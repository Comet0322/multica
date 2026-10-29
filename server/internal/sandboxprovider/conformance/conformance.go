// Package conformance checks a sandbox provider against
// deploy/sandbox/CONTRACT.md by running the probe image
// (deploy/sandbox/conformance) and reading what it reports from inside.
package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxprovider"
)

type Config struct {
	// Image is the probe image, pullable by the service.
	Image string
	// CallbackURL is where sandboxes reach the Collector.
	CallbackURL string
	// EgressURL, when set, must be unreachable from sandboxes.
	EgressURL string
	// StartTimeout bounds PUT-to-running; StartTarget is the contract's
	// expected p95 and only warns.
	StartTimeout time.Duration
	StartTarget  time.Duration
	// ReportTimeout bounds running-to-report.
	ReportTimeout time.Duration
	// IdleWindow is how long a repeated PUT must leave the sandbox alone.
	IdleWindow time.Duration
}

func (c *Config) defaults() {
	if c.StartTimeout == 0 {
		c.StartTimeout = 3 * time.Minute
	}
	if c.StartTarget == 0 {
		c.StartTarget = 45 * time.Second
	}
	if c.ReportTimeout == 0 {
		c.ReportTimeout = 2 * time.Minute
	}
	if c.IdleWindow == 0 {
		c.IdleWindow = 5 * time.Second
	}
}

// Report is what the probe sends from inside a sandbox.
type Report struct {
	Key        string `json:"key"`
	Nonce      string `json:"nonce"`
	Echo       string `json:"echo"`
	Previous   string `json:"previous"`
	UID        string `json:"uid"`
	Volume     string `json:"volume"`
	Tmp        string `json:"tmp"`
	Rootfs     string `json:"rootfs"`
	Egress     string `json:"egress"`
	CapBnd     string `json:"cap_bnd"`
	NoNewPrivs string `json:"no_new_privs"`
}

// Collector receives probe reports; serve it at Config.CallbackURL.
type Collector struct {
	mu      sync.Mutex
	reports []Report
	changed chan struct{}
}

func NewCollector() *Collector { return &Collector{changed: make(chan struct{})} }

func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/report") {
		http.NotFound(w, r)
		return
	}
	var rep Report
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&rep); err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.reports = append(c.reports, rep)
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (c *Collector) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.reports {
		if r.Key == key {
			n++
		}
	}
	return n
}

func (c *Collector) wait(ctx context.Context, key, nonce string, timeout time.Duration) (Report, error) {
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		for _, r := range c.reports {
			if r.Key == key && r.Nonce == nonce {
				c.mu.Unlock()
				return r, nil
			}
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			return Report{}, fmt.Errorf("no report from the sandbox within %s; can it reach %s?", timeout, "the callback URL")
		case <-ctx.Done():
			return Report{}, ctx.Err()
		}
	}
}

type Level string

const (
	Pass Level = "PASS"
	Warn Level = "WARN"
	Fail Level = "FAIL"
)

type Result struct {
	Name    string
	Level   Level
	Message string
}

// Failed reports whether any result is a failure.
func Failed(results []Result) bool {
	for _, r := range results {
		if r.Level == Fail {
			return true
		}
	}
	return false
}

type suite struct {
	ctx     context.Context
	p       sandboxprovider.Provider
	c       *Collector
	cfg     Config
	key     string
	echoVar string
	echo    string
	results []Result
}

// errStop ends the run after a failure later checks depend on.
var errStop = errors.New("stop")

func (s *suite) check(name string, fn func() (Level, string)) {
	level, msg := fn()
	s.results = append(s.results, Result{name, level, msg})
}

func (s *suite) must(name string, fn func() error) error {
	if err := fn(); err != nil {
		s.results = append(s.results, Result{name, Fail, err.Error()})
		return errStop
	}
	s.results = append(s.results, Result{name, Pass, ""})
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Run executes the suite against p. It creates and deletes sandboxes whose
// keys start with "conformance-", and always cleans them up.
func Run(ctx context.Context, p sandboxprovider.Provider, c *Collector, cfg Config) []Result {
	cfg.defaults()
	s := &suite{ctx: ctx, p: p, c: c, cfg: cfg, key: "conformance-" + randomHex(6),
		echoVar: "ZZ_FUTURE_" + strings.ToUpper(randomHex(4)), echo: randomHex(8)}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = p.Delete(cleanupCtx, s.key)
	}()
	_ = s.run()
	return s.results
}

func (s *suite) spec(nonce string) sandboxprovider.Spec {
	env := map[string]string{
		"CONFORMANCE_CALLBACK": strings.TrimRight(s.cfg.CallbackURL, "/"),
		"CONFORMANCE_KEY":      s.key,
		"CONFORMANCE_NONCE":    nonce,
		"CONFORMANCE_ECHO_VAR": s.echoVar,
		s.echoVar:              s.echo,
	}
	if s.cfg.EgressURL != "" {
		env["CONFORMANCE_EGRESS_URL"] = s.cfg.EgressURL
	}
	return sandboxprovider.Spec{Key: s.key, Image: s.cfg.Image, Env: env, Labels: map[string]string{"ai.multica.conformance": "1"}}
}

// start ensures the spec and waits for running, returning the elapsed time.
func (s *suite) start(nonce string) (time.Duration, error) {
	begin := time.Now()
	if _, err := s.p.Ensure(s.ctx, s.spec(nonce)); err != nil {
		return 0, fmt.Errorf("PUT: %w", err)
	}
	return time.Since(begin), s.waitStatus(s.cfg.StartTimeout, sandboxprovider.StatusRunning)
}

func (s *suite) waitStatus(timeout time.Duration, want ...sandboxprovider.Status) error {
	deadline := time.Now().Add(timeout)
	var last sandboxprovider.Sandbox
	for time.Now().Before(deadline) {
		got, err := s.p.Get(s.ctx, s.key)
		if err != nil {
			return fmt.Errorf("GET: %w", err)
		}
		last = got
		for _, w := range want {
			if got.Status == w {
				return nil
			}
		}
		if got.Status == sandboxprovider.StatusFailed {
			return fmt.Errorf("sandbox failed: %s", got.Reason)
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("status is %q (%s) after %s, want %v", last.Status, last.Reason, timeout, want)
}

func (s *suite) run() error {
	unknown := "conformance-unknown-" + randomHex(4)
	if err := s.must("unknown key: GET reports missing, stop and delete succeed", func() error {
		got, err := s.p.Get(s.ctx, unknown)
		if err != nil {
			return err
		}
		if got.Status != sandboxprovider.StatusMissing {
			return fmt.Errorf("status = %q, want missing", got.Status)
		}
		if err := s.p.Stop(s.ctx, unknown); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		return s.p.Delete(s.ctx, unknown)
	}); err != nil {
		return err
	}

	nonce := randomHex(4)
	var elapsed time.Duration
	if err := s.must("PUT starts a new sandbox", func() error {
		var err error
		begin := time.Now()
		_, err = s.start(nonce)
		elapsed = time.Since(begin)
		return err
	}); err != nil {
		return err
	}
	s.check("time to running is within the target", func() (Level, string) {
		if elapsed > s.cfg.StartTarget {
			return Warn, fmt.Sprintf("%s, target %s", elapsed.Round(time.Second), s.cfg.StartTarget)
		}
		return Pass, elapsed.Round(time.Second).String()
	})

	var rep Report
	if err := s.must("sandbox reaches the callback URL and reports", func() error {
		var err error
		rep, err = s.c.wait(s.ctx, s.key, nonce, s.cfg.ReportTimeout)
		return err
	}); err != nil {
		return err
	}
	s.runtimeChecks(rep)
	s.check("new sandbox starts with an empty volume", func() (Level, string) {
		if rep.Previous != "none" {
			return Fail, "found data from an earlier sandbox: " + rep.Previous
		}
		return Pass, ""
	})

	if err := s.must("repeated PUT with the same spec leaves the sandbox running", func() error {
		before := s.c.count(s.key)
		if _, err := s.p.Ensure(s.ctx, s.spec(nonce)); err != nil {
			return fmt.Errorf("PUT: %w", err)
		}
		time.Sleep(s.cfg.IdleWindow)
		if got, err := s.p.Get(s.ctx, s.key); err != nil || got.Status != sandboxprovider.StatusRunning {
			return fmt.Errorf("status after repeated PUT = %q (%v)", got.Status, err)
		}
		if s.c.count(s.key) != before {
			return errors.New("the sandbox restarted")
		}
		return nil
	}); err != nil {
		return err
	}

	s.check("list includes the sandbox", func() (Level, string) {
		list, err := s.p.List(s.ctx)
		if err != nil {
			return Fail, err.Error()
		}
		for _, sb := range list {
			if sb.Key == s.key {
				return Pass, ""
			}
		}
		return Fail, "missing from GET /v1/sandboxes"
	})

	if err := s.volumeKept("stop then PUT keeps the volume", &nonce, func() error {
		if err := s.p.Stop(s.ctx, s.key); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		return s.waitStatus(s.cfg.StartTimeout, sandboxprovider.StatusStopped, sandboxprovider.StatusMissing)
	}); err != nil {
		return err
	}
	if err := s.volumeKept("PUT with a changed spec replaces the running sandbox and keeps the volume", &nonce, nil); err != nil {
		return err
	}

	return s.must("delete removes the volume", func() error {
		if err := s.p.Delete(s.ctx, s.key); err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		if err := s.waitStatus(s.cfg.StartTimeout, sandboxprovider.StatusMissing); err != nil {
			return err
		}
		fresh := randomHex(4)
		if _, err := s.start(fresh); err != nil {
			return err
		}
		rep, err := s.c.wait(s.ctx, s.key, fresh, s.cfg.ReportTimeout)
		if err != nil {
			return err
		}
		if rep.Previous != "none" {
			return fmt.Errorf("volume survived delete: found %q", rep.Previous)
		}
		return nil
	})
}

// volumeKept runs between, starts the sandbox with a new spec and checks the
// probe finds the previous run's marker.
func (s *suite) volumeKept(name string, nonce *string, between func() error) error {
	return s.must(name, func() error {
		if between != nil {
			if err := between(); err != nil {
				return err
			}
		}
		prev, next := *nonce, randomHex(4)
		if _, err := s.start(next); err != nil {
			return err
		}
		rep, err := s.c.wait(s.ctx, s.key, next, s.cfg.ReportTimeout)
		if err != nil {
			return err
		}
		*nonce = next
		if rep.Previous != prev {
			return fmt.Errorf("volume content = %q, want %q", rep.Previous, prev)
		}
		return nil
	})
}

func (s *suite) runtimeChecks(rep Report) {
	s.check("env is passed through verbatim", func() (Level, string) {
		if rep.Echo != s.echo {
			return Fail, fmt.Sprintf("%s = %q, want %q", s.echoVar, rep.Echo, s.echo)
		}
		return Pass, ""
	})
	s.check("runs as uid 1000", func() (Level, string) {
		if rep.UID != "1000" {
			return Fail, "uid " + rep.UID
		}
		return Pass, ""
	})
	s.check("/data is a writable volume", func() (Level, string) {
		if rep.Volume != "ok" {
			return Fail, "cannot write /data"
		}
		return Pass, ""
	})
	s.check("/tmp is writable", func() (Level, string) {
		if rep.Tmp != "ok" {
			return Fail, "cannot write /tmp"
		}
		return Pass, ""
	})
	s.check("all capabilities dropped", func() (Level, string) {
		if strings.Trim(rep.CapBnd, "0") != "" {
			return Fail, "bounding set " + rep.CapBnd
		}
		return Pass, ""
	})
	s.check("no privilege escalation", func() (Level, string) {
		if rep.NoNewPrivs != "1" {
			return Fail, "NoNewPrivs " + rep.NoNewPrivs
		}
		return Pass, ""
	})
	s.check("root filesystem is read-only", func() (Level, string) {
		if rep.Rootfs != "read-only" {
			return Warn, "writable (allowed, read-only recommended)"
		}
		return Pass, ""
	})
	s.check("denied egress is blocked", func() (Level, string) {
		switch rep.Egress {
		case "blocked":
			return Pass, s.cfg.EgressURL
		case "open":
			return Fail, "reached " + s.cfg.EgressURL
		}
		return Warn, "not checked; pass -egress-url"
	})
}

// CheckHTTPAPI covers what the Provider interface cannot express: auth and
// server-side key validation.
func CheckHTTPAPI(ctx context.Context, baseURL, token string) []Result {
	base := strings.TrimRight(baseURL, "/") + "/v1/sandboxes"
	call := func(method, path, auth, body string) (int, error) {
		req, err := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
		if err != nil {
			return 0, err
		}
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	var out []Result
	expect := func(name string, want int, method, path, auth, body string) {
		got, err := call(method, path, auth, body)
		switch {
		case err != nil:
			out = append(out, Result{name, Fail, err.Error()})
		case got != want:
			out = append(out, Result{name, Fail, fmt.Sprintf("HTTP %d, want %d", got, want)})
		default:
			out = append(out, Result{name, Pass, ""})
		}
	}
	expect("missing token is rejected", http.StatusUnauthorized, http.MethodGet, "", "", "")
	expect("wrong token is rejected", http.StatusUnauthorized, http.MethodGet, "", "wrong-"+randomHex(4), "")
	expect("invalid key is rejected", http.StatusBadRequest, http.MethodPut, "/Not_A_Key", token, `{"image":"x"}`)
	return out
}
