package agentsvc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness"
	"github.com/multica-ai/multica/server/pkg/harness/gittools"
	"github.com/multica-ai/multica/server/pkg/harness/model"
	"github.com/multica-ai/multica/server/pkg/harness/multicatools"
	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
	"github.com/multica-ai/multica/server/pkg/harness/tools"
)

// Config configures a Server.
type Config struct {
	// Token is the bearer token daemons present. Required.
	Token string
	// NewSandbox makes the sandbox for one run and names its working directory.
	// Required. In production it creates a sandbox.Remote.
	NewSandbox func(ctx context.Context) (sb sandbox.Sandbox, workdir string, err error)
	// NewModel returns the model for a run; an empty id means the default.
	// Required. The credentials it uses never leave the service.
	NewModel func(modelID string) (model.Model, error)
	// GitRepos enables git_clone and deliver for these repositories; GitUser and
	// GitToken are the https credential, used on this host only.
	GitRepos          []string
	GitUser, GitToken string
	// MulticaURL, when set, is the Multica server the tools talk to, replacing the
	// one in the request (a daemon's view of the server need not be the service's).
	MulticaURL string
	// MaxRuns bounds concurrent runs; zero means 20.
	MaxRuns int
	// Heartbeat is how often an idle stream says it is alive; zero means 15s.
	Heartbeat time.Duration
	// DefaultTimeout applies to a run that names none, MaxTimeout caps what it may
	// ask for; zero means 1h and 4h.
	DefaultTimeout, MaxTimeout time.Duration
	// MaxRequestBytes bounds a run request, files included; zero means 96 MiB.
	MaxRequestBytes int64
	Logger          *slog.Logger
}

// Server is the agent service.
type Server struct {
	cfg Config
	mux *http.ServeMux

	mu   sync.Mutex
	runs map[string]context.CancelFunc
}

// New returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Token == "" || cfg.NewSandbox == nil || cfg.NewModel == nil {
		return nil, errors.New("agentsvc: a token, a sandbox factory and a model factory are required")
	}
	if cfg.MaxRuns <= 0 {
		cfg.MaxRuns = 20
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 15 * time.Second
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = time.Hour
	}
	if cfg.MaxTimeout <= 0 {
		cfg.MaxTimeout = 4 * time.Hour
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = 96 << 20
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), runs: map[string]context.CancelFunc{}}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s.mux.HandleFunc("POST /v1/runs", s.auth(s.run))
	s.mux.HandleFunc("DELETE /v1/runs/{id}", s.auth(s.cancel))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Active is how many runs are in progress.
func (s *Server) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

func fail(w http.ResponseWriter, status int, format string, a ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(WireError{Error: fmt.Sprintf(format, a...)})
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

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cancel := s.runs[r.PathValue("id")]
	s.mu.Unlock()
	if cancel == nil {
		fail(w, http.StatusNotFound, "no such run")
		return
	}
	cancel()
	w.WriteHeader(http.StatusAccepted)
}

// stream writes events to one response, one at a time.
type stream struct {
	mu  sync.Mutex
	w   http.ResponseWriter
	enc *json.Encoder
	rc  *http.ResponseController
}

func (st *stream) send(e Event) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.enc.Encode(e); err == nil {
		_ = st.rc.Flush()
	}
}

var issueRE = regexp.MustCompile(`assigned issue ID is: ([0-9a-fA-F-]{36})`)

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "run_" + hex.EncodeToString(b)
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusRequestEntityTooLarge, "request is larger than %d bytes", s.cfg.MaxRequestBytes)
			return
		}
		fail(w, http.StatusBadRequest, "bad request: %v", err)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		fail(w, http.StatusBadRequest, "prompt is required")
		return
	}
	for _, f := range req.Files {
		if clean := path.Clean("/" + f.Path); f.Path == "" || strings.Contains(f.Path, "\x00") || clean == "/" || strings.HasPrefix(path.Clean(f.Path), "..") {
			fail(w, http.StatusBadRequest, "bad file path %q", f.Path)
			return
		}
	}

	id := newRunID()
	timeout := s.cfg.DefaultTimeout
	if req.TimeoutSec > 0 {
		timeout = min(time.Duration(req.TimeoutSec)*time.Second, s.cfg.MaxTimeout)
	}
	runCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// Reserve the slot before the slow work, as the sandbox API does.
	s.mu.Lock()
	if len(s.runs) >= s.cfg.MaxRuns {
		s.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "too many runs in progress (limit %d)", s.cfg.MaxRuns)
		return
	}
	s.runs[id] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.runs, id)
		s.mu.Unlock()
	}()

	mdl, err := s.cfg.NewModel(req.Model)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	sb, workdir, err := s.cfg.NewSandbox(runCtx)
	if err != nil {
		s.cfg.Logger.Error("run: sandbox", "run", id, "error", err)
		fail(w, http.StatusServiceUnavailable, "could not get a sandbox: %v", err)
		return
	}
	// Whatever happens next, the sandbox is gone before the result is sent: a task
	// that reports completion must not leave a container behind.
	closed := false
	closeSandbox := func() {
		if !closed {
			closed = true
			if err := sb.Close(context.WithoutCancel(r.Context())); err != nil {
				s.cfg.Logger.Warn("run: closing the sandbox failed", "run", id, "error", err)
			}
		}
	}
	defer closeSandbox()

	for _, f := range req.Files {
		mode := os.FileMode(f.Mode).Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := sb.Put(runCtx, path.Join(workdir, f.Path), f.Data, mode); err != nil {
			fail(w, http.StatusInternalServerError, "could not place %s in the sandbox: %v", f.Path, err)
			return
		}
	}

	reg := tools.NewRegistry(tools.Sandboxed{SB: sb, Workdir: workdir}.Tools()...)
	env := map[string]string{}
	for _, k := range TaskEnvKeys {
		env[k] = req.Env[k]
	}
	if s.cfg.MulticaURL != "" {
		env["MULTICA_SERVER_URL"] = s.cfg.MulticaURL
	}
	if m := issueRE.FindStringSubmatch(req.Prompt); m != nil && env["MULTICA_TOKEN"] != "" {
		client := &multicatools.Client{
			BaseURL: env["MULTICA_SERVER_URL"], Token: env["MULTICA_TOKEN"], WorkspaceID: env["MULTICA_WORKSPACE_ID"],
			AgentID: env["MULTICA_AGENT_ID"], TaskID: env["MULTICA_TASK_ID"],
		}
		for _, t := range multicatools.Tools(client, multicatools.NewScope(m[1])) {
			reg.Add(t)
		}
	}
	hasGit := len(s.cfg.GitRepos) > 0
	if hasGit {
		dir, err := os.MkdirTemp("", "agentsvc-git-")
		if err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		defer os.RemoveAll(dir)
		for _, t := range gittools.Tools(gittools.Config{
			SB: sb, Workdir: workdir, HostDir: dir, Allowed: s.cfg.GitRepos, TaskID: env["MULTICA_TASK_ID"],
			User: s.cfg.GitUser, Token: s.cfg.GitToken,
		}) {
			reg.Add(t)
		}
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	st := &stream{w: w, enc: json.NewEncoder(w), rc: http.NewResponseController(w)}
	st.send(Event{Type: EventStarted, RunID: id})
	s.cfg.Logger.Info("run started", "run", id, "task", env["MULTICA_TASK_ID"])

	hbDone := make(chan struct{})
	go func() {
		t := time.NewTicker(s.cfg.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				st.send(Event{Type: EventHeartbeat})
			case <-hbDone:
				return
			}
		}
	}()

	start := time.Now()
	out := harness.Run(runCtx, harness.Config{
		Model: mdl, Tools: reg, System: SystemPrompt(req.SystemPrompt, hasGit), MaxTurns: req.MaxTurns,
		Emit: func(e harness.Event) { st.send(Event{Type: EventMessage, Message: message(e)}) },
	}, req.Prompt)
	close(hbDone)
	closeSandbox()

	res := &Result{Status: out.Status, Output: out.Output, Error: out.Error, DurationMs: time.Since(start).Milliseconds(), Usage: map[string]Usage{}}
	if out.Status != harness.StatusCompleted && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.Status = "timeout"
	}
	for name, u := range out.Usage {
		res.Usage[name] = Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens}
	}
	st.send(Event{Type: EventResult, Result: res})
	s.cfg.Logger.Info("run finished", "run", id, "status", res.Status, "ms", res.DurationMs)
}

func message(e harness.Event) *Message {
	switch e.Kind {
	case harness.EventText:
		return &Message{Type: "text", Content: e.Text}
	case harness.EventToolUse:
		return &Message{Type: "tool-use", Tool: e.Tool, CallID: e.CallID, Input: e.Input}
	case harness.EventToolResult:
		return &Message{Type: "tool-result", Tool: e.Tool, CallID: e.CallID, Output: e.Output}
	default:
		return &Message{Type: "error", Content: e.Text}
	}
}

// SystemPrompt is the Multica runtime brief followed by what is true of this
// environment. The brief describes the multica CLI and a networked machine;
// neither exists here, so the environment note goes last and wins.
func SystemPrompt(brief string, hasGit bool) string {
	parts := []string{}
	if strings.TrimSpace(brief) != "" {
		parts = append(parts, brief)
	}
	env := "## Environment\n\n" +
		"You work in a sandbox with no network. The `multica` CLI and any other command that needs the network are not available. " +
		"Use the issue_* and comment_* tools to read and update your issue, create sub-issues and post comments."
	if hasGit {
		env += " Code comes in with git_clone and goes out with deliver: clone, edit and commit with git in bash, " +
			"then call deliver with a branch under agent/<task id>/. Never try to push from the sandbox."
	}
	return strings.Join(append(parts, env), "\n\n")
}
