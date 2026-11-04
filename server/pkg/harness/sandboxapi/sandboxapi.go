// Package sandboxapi serves sandboxes over HTTP: create one, run a command in
// it, move files in and out, delete it. The wire format is documented in
// package sandbox, where its client (sandbox.Remote) lives.
//
// The server owns every sandbox it creates. It caps how many exist, deletes
// the ones nobody has touched for a while, and deletes all of them when it is
// shut down, so a crashed or forgetful client cannot leave machines behind.
package sandboxapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
)

// Config configures a Server.
type Config struct {
	// Token is the bearer token every client must present. Required: there is no
	// unauthenticated mode.
	Token string
	// New makes the sandbox behind one Create call. Required. It is where a
	// provider (Docker, a vendor API) plugs in, and where an image is checked.
	New func(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error)
	// MaxSandboxes bounds how many exist at once; zero means 50.
	MaxSandboxes int
	// IdleTTL is how long a sandbox may go unused before it is deleted; zero means 2h.
	IdleTTL time.Duration
	// MaxFileBytes bounds one uploaded or downloaded file; zero means 64 MiB.
	MaxFileBytes int64
	// MaxOutputBytes bounds stdout and stderr of one command each; zero means 8 MiB.
	MaxOutputBytes int
	// DefaultExecTimeout applies to a command that names none; zero means 10 min.
	DefaultExecTimeout time.Duration
	// MaxExecTimeout caps what a command may ask for; zero means 1h.
	MaxExecTimeout time.Duration
	Logger         *slog.Logger
	// Now is the clock, replaced in tests.
	Now func() time.Time
}

type entry struct {
	sb       sandbox.Sandbox
	lastUsed time.Time
}

// Server is the sandbox API.
type Server struct {
	cfg Config
	mux *http.ServeMux

	mu    sync.Mutex
	boxes map[string]*entry
}

// New returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Token == "" {
		return nil, errors.New("sandboxapi: a token is required")
	}
	if cfg.New == nil {
		return nil, errors.New("sandboxapi: a sandbox factory is required")
	}
	if cfg.MaxSandboxes <= 0 {
		cfg.MaxSandboxes = 50
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 2 * time.Hour
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 64 << 20
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = 8 << 20
	}
	if cfg.DefaultExecTimeout <= 0 {
		cfg.DefaultExecTimeout = 10 * time.Minute
	}
	if cfg.MaxExecTimeout <= 0 {
		cfg.MaxExecTimeout = time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), boxes: map[string]*entry{}}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s.mux.HandleFunc("POST /v1/sandboxes", s.auth(s.create))
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.auth(s.exec))
	s.mux.HandleFunc("PUT /v1/sandboxes/{id}/files", s.auth(s.putFile))
	s.mux.HandleFunc("GET /v1/sandboxes/{id}/files", s.auth(s.getFile))
	s.mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.auth(s.remove))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, format string, a ...any) {
	reply(w, status, sandbox.WireError{Error: fmt.Sprintf(format, a...)})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == r.Header.Get("Authorization") || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			fail(w, http.StatusUnauthorized, "invalid or missing bearer token")
			return
		}
		next(w, r)
	}
}

// lookup finds a sandbox and marks it used.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (sandbox.Sandbox, bool) {
	s.mu.Lock()
	e := s.boxes[r.PathValue("id")]
	var sb sandbox.Sandbox
	if e != nil {
		e.lastUsed = s.cfg.Now()
		sb = e.sb // nil while the sandbox is still being created
	}
	s.mu.Unlock()
	if sb == nil {
		fail(w, http.StatusNotFound, "no such sandbox")
		return nil, false
	}
	return sb, true
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var opts sandbox.CreateOptions
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&opts); err != nil && !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "bad request body: %v", err)
		return
	}
	// Reserve the slot before the slow part, or concurrent creates overshoot the cap.
	id := newID()
	s.mu.Lock()
	if len(s.boxes) >= s.cfg.MaxSandboxes {
		s.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "at the limit of %d sandboxes", s.cfg.MaxSandboxes)
		return
	}
	s.boxes[id] = &entry{lastUsed: s.cfg.Now()}
	s.mu.Unlock()

	sb, err := s.cfg.New(r.Context(), opts)
	if err != nil {
		s.mu.Lock()
		delete(s.boxes, id)
		s.mu.Unlock()
		s.cfg.Logger.Warn("sandbox create failed", "error", err)
		fail(w, http.StatusBadRequest, "could not create the sandbox: %v", err)
		return
	}
	s.mu.Lock()
	s.boxes[id].sb = sb
	s.mu.Unlock()
	s.cfg.Logger.Info("sandbox created", "id", id)
	reply(w, http.StatusCreated, sandbox.CreatedSandbox{ID: id})
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "sbx_" + hex.EncodeToString(b)
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var req sandbox.WireExecRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil || strings.TrimSpace(req.Command) == "" {
		fail(w, http.StatusBadRequest, "a command is required")
		return
	}
	timeout := s.cfg.DefaultExecTimeout
	if req.TimeoutSec > 0 {
		timeout = min(time.Duration(req.TimeoutSec)*time.Second, s.cfg.MaxExecTimeout)
	}
	// r.Context() ends when the client goes away, which kills the command.
	res, err := sb.Exec(r.Context(), sandbox.ExecRequest{Command: req.Command, Cwd: req.Cwd, Env: req.Env, Timeout: timeout})
	if err != nil {
		fail(w, http.StatusInternalServerError, "exec: %v", err)
		return
	}
	out := sandbox.WireExecResult{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode, TimedOut: res.TimedOut}
	if len(out.Stdout) > s.cfg.MaxOutputBytes {
		out.Stdout, out.Truncated = out.Stdout[:s.cfg.MaxOutputBytes], true
	}
	if len(out.Stderr) > s.cfg.MaxOutputBytes {
		out.Stderr, out.Truncated = out.Stderr[:s.cfg.MaxOutputBytes], true
	}
	reply(w, http.StatusOK, out)
}

func (s *Server) putFile(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" || strings.ContainsRune(p, 0) {
		fail(w, http.StatusBadRequest, "a path is required")
		return
	}
	mode := uint64(0o644)
	if m := r.URL.Query().Get("mode"); m != "" {
		v, err := strconv.ParseUint(m, 8, 32)
		if err != nil || v > 0o777 {
			fail(w, http.StatusBadRequest, "mode must be octal permission bits")
			return
		}
		mode = v
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxFileBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusRequestEntityTooLarge, "file is larger than %d bytes", s.cfg.MaxFileBytes)
			return
		}
		fail(w, http.StatusBadRequest, "reading the body: %v", err)
		return
	}
	if err := sb.Put(r.Context(), p, data, fs.FileMode(mode)); err != nil {
		fail(w, http.StatusInternalServerError, "put: %v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" || strings.ContainsRune(p, 0) {
		fail(w, http.StatusBadRequest, "a path is required")
		return
	}
	data, err := sb.Get(r.Context(), p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fail(w, http.StatusNotFound, "no such file")
	case err != nil:
		fail(w, http.StatusInternalServerError, "get: %v", err)
	case int64(len(data)) > s.cfg.MaxFileBytes:
		fail(w, http.StatusRequestEntityTooLarge, "file is larger than %d bytes", s.cfg.MaxFileBytes)
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	e := s.boxes[id]
	delete(s.boxes, id)
	s.mu.Unlock()
	if e == nil {
		fail(w, http.StatusNotFound, "no such sandbox")
		return
	}
	if err := s.closeEntry(r.Context(), id, e); err != nil {
		fail(w, http.StatusInternalServerError, "delete: %v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) closeEntry(ctx context.Context, id string, e *entry) error {
	if e.sb == nil { // still being created
		return nil
	}
	err := e.sb.Close(ctx)
	s.cfg.Logger.Info("sandbox deleted", "id", id, "error", err)
	return err
}

// Reap deletes every sandbox idle for longer than IdleTTL and returns how many.
func (s *Server) Reap(ctx context.Context) int {
	now := s.cfg.Now()
	s.mu.Lock()
	var dead = map[string]*entry{}
	for id, e := range s.boxes {
		if e.sb != nil && now.Sub(e.lastUsed) > s.cfg.IdleTTL {
			dead[id] = e
			delete(s.boxes, id)
		}
	}
	s.mu.Unlock()
	for id, e := range dead {
		_ = s.closeEntry(ctx, id, e)
	}
	return len(dead)
}

// Run reaps idle sandboxes until ctx ends, then deletes every sandbox left.
func (s *Server) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.CloseAll(context.WithoutCancel(ctx))
			return
		case <-t.C:
			s.Reap(ctx)
		}
	}
}

// CloseAll deletes every sandbox.
func (s *Server) CloseAll(ctx context.Context) {
	s.mu.Lock()
	all := s.boxes
	s.boxes = map[string]*entry{}
	s.mu.Unlock()
	for id, e := range all {
		_ = s.closeEntry(ctx, id, e)
	}
}

// Count is the number of sandboxes that exist.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.boxes)
}
