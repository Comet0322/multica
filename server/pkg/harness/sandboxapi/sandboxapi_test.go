package sandboxapi_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
	"github.com/multica-ai/multica/server/pkg/harness/sandbox/sandboxtest"
	"github.com/multica-ai/multica/server/pkg/harness/sandboxapi"
)

const token = "s3cret-api-token"

type fixture struct {
	srv    *sandboxapi.Server
	ts     *httptest.Server
	closed atomic.Int32
	now    atomic.Int64 // unix nanos
}

type countingSandbox struct {
	sandbox.Sandbox
	f *fixture
}

func (c countingSandbox) Close(ctx context.Context) error {
	c.f.closed.Add(1)
	return c.Sandbox.Close(ctx)
}

// newFixture serves local-directory sandboxes, so no container runtime is needed.
func newFixture(t *testing.T, mutate func(*sandboxapi.Config)) *fixture {
	t.Helper()
	f := &fixture{}
	f.now.Store(time.Now().UnixNano())
	cfg := sandboxapi.Config{
		Token: token,
		New: func(_ context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
			if opts.Image == "forbidden" {
				return nil, errors.New("image not allowed")
			}
			l, err := sandbox.NewLocal(t.TempDir())
			return countingSandbox{l, f}, err
		},
		Now: func() time.Time { return time.Unix(0, f.now.Load()) },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := sandboxapi.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.srv, f.ts = srv, httptest.NewServer(srv)
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fixture) remote(t *testing.T) *sandbox.Remote {
	t.Helper()
	r, err := sandbox.NewRemote(context.Background(), sandbox.RemoteConfig{BaseURL: f.ts.URL, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRemoteSandboxMeetsTheSandboxContract(t *testing.T) {
	f := newFixture(t, nil)
	r := f.remote(t)
	sandboxtest.Contract(t, r, "work")

	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.srv.Count() != 0 || f.closed.Load() != 1 {
		t.Errorf("after Close: %d sandboxes, %d closed", f.srv.Count(), f.closed.Load())
	}
	if err := r.Close(context.Background()); err != nil {
		t.Errorf("closing twice should be harmless: %v", err)
	}
	if _, err := r.Exec(context.Background(), sandbox.ExecRequest{Command: "true"}); err == nil {
		t.Error("a deleted sandbox still ran a command")
	}
}

func TestEveryEndpointNeedsTheToken(t *testing.T) {
	f := newFixture(t, nil)
	id := f.remote(t).ID()

	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/sandboxes"},
		{"POST", "/v1/sandboxes/" + id + "/exec"},
		{"PUT", "/v1/sandboxes/" + id + "/files?path=/x"},
		{"GET", "/v1/sandboxes/" + id + "/files?path=/x"},
		{"DELETE", "/v1/sandboxes/" + id},
	} {
		for name, auth := range map[string]string{"none": "", "wrong": "Bearer nope", "other scheme": "Basic " + token, "bare": token} {
			req, _ := http.NewRequest(c.method, f.ts.URL+c.path, strings.NewReader(`{"command":"true"}`))
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s credentials: status %d, want 401", c.method, c.path, name, resp.StatusCode)
			}
		}
	}
	if resp, err := http.Get(f.ts.URL + "/healthz"); err != nil || resp.StatusCode != 200 {
		t.Errorf("healthz should need no token: %v %v", resp, err)
	}
	if f.srv.Count() != 1 {
		t.Errorf("unauthenticated calls changed the sandboxes: %d", f.srv.Count())
	}
}

func TestServerWillNotStartWithoutATokenOrAFactory(t *testing.T) {
	if _, err := sandboxapi.New(sandboxapi.Config{New: func(context.Context, sandbox.CreateOptions) (sandbox.Sandbox, error) { return nil, nil }}); err == nil {
		t.Error("started without a token")
	}
	if _, err := sandboxapi.New(sandboxapi.Config{Token: token}); err == nil {
		t.Error("started without a factory")
	}
}

func TestTheSandboxLimitIsHeldUnderConcurrentCreates(t *testing.T) {
	f := newFixture(t, func(c *sandboxapi.Config) { c.MaxSandboxes = 5 })
	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := sandbox.NewRemote(context.Background(), sandbox.RemoteConfig{BaseURL: f.ts.URL, Token: token})
			var ae *sandbox.APIError
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests:
				refused.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 5 || refused.Load() != 15 || f.srv.Count() != 5 {
		t.Errorf("created %d, refused %d, held %d; want 5, 15, 5", ok.Load(), refused.Load(), f.srv.Count())
	}
}

func TestAFailedCreateFreesItsSlot(t *testing.T) {
	f := newFixture(t, func(c *sandboxapi.Config) { c.MaxSandboxes = 1 })
	_, err := sandbox.NewRemote(context.Background(), sandbox.RemoteConfig{BaseURL: f.ts.URL, Token: token, Options: sandbox.CreateOptions{Image: "forbidden"}})
	if err == nil || !strings.Contains(err.Error(), "image not allowed") {
		t.Fatalf("err = %v", err)
	}
	f.remote(t) // would be refused if the failed attempt still held the only slot
}

func TestIdleSandboxesAreDeletedAndUsedOnesAreKept(t *testing.T) {
	f := newFixture(t, func(c *sandboxapi.Config) { c.IdleTTL = time.Hour })
	idle, busy := f.remote(t), f.remote(t)

	f.now.Add(int64(40 * time.Minute))
	if _, err := busy.Exec(context.Background(), sandbox.ExecRequest{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	f.now.Add(int64(30 * time.Minute)) // idle: 70 min since use; busy: 30 min

	if n := f.srv.Reap(context.Background()); n != 1 || f.srv.Count() != 1 {
		t.Fatalf("reaped %d, %d left; want 1 and 1", n, f.srv.Count())
	}
	if _, err := idle.Exec(context.Background(), sandbox.ExecRequest{Command: "true"}); err == nil {
		t.Error("the idle sandbox survived")
	}
	if _, err := busy.Exec(context.Background(), sandbox.ExecRequest{Command: "true"}); err != nil {
		t.Errorf("the used sandbox was reaped: %v", err)
	}
}

func TestShutdownDeletesEverySandbox(t *testing.T) {
	f := newFixture(t, nil)
	f.remote(t)
	f.remote(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.srv.Run(ctx, time.Hour); close(done) }()
	cancel()
	<-done
	if f.srv.Count() != 0 || f.closed.Load() != 2 {
		t.Errorf("%d left, %d closed", f.srv.Count(), f.closed.Load())
	}
}

func TestSizeLimits(t *testing.T) {
	f := newFixture(t, func(c *sandboxapi.Config) { c.MaxFileBytes = 1024; c.MaxOutputBytes = 100 })
	r := f.remote(t)
	ctx := context.Background()

	if err := r.Put(ctx, "/ok", bytes.Repeat([]byte("a"), 1024), 0o644); err != nil {
		t.Errorf("a file at the limit: %v", err)
	}
	err := r.Put(ctx, "/big", bytes.Repeat([]byte("a"), 1025), 0o644)
	var ae *sandbox.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusRequestEntityTooLarge {
		t.Errorf("a file over the limit: %v", err)
	}
	res, err := r.Exec(ctx, sandbox.ExecRequest{Command: "yes x | head -c 5000"})
	if err != nil || len(res.Stdout) != 100 {
		t.Errorf("output not capped: %d bytes, %v", len(res.Stdout), err)
	}
}

func TestBadRequestsAreRefusedNotCrashed(t *testing.T) {
	f := newFixture(t, nil)
	r := f.remote(t)
	post := func(path, body string) int {
		req, _ := http.NewRequest("POST", f.ts.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	exec := "/v1/sandboxes/" + r.ID() + "/exec"
	for name, c := range map[string]struct {
		path, body string
		want       int
	}{
		"empty command":   {exec, `{"command":"  "}`, 400},
		"not json":        {exec, `nope`, 400},
		"unknown sandbox": {"/v1/sandboxes/sbx_nope/exec", `{"command":"true"}`, 404},
	} {
		if got := post(c.path, c.body); got != c.want {
			t.Errorf("%s: status %d, want %d", name, got, c.want)
		}
	}
	for name, q := range map[string]string{"no path": "", "nul in path": "?path=a%00b", "bad mode": "?path=/x&mode=999", "setuid mode": "?path=/x&mode=4755"} {
		req, _ := http.NewRequest("PUT", f.ts.URL+"/v1/sandboxes/"+r.ID()+"/files"+q, strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestACanceledCallDoesNotLeaveTheCommandRunning(t *testing.T) {
	f := newFixture(t, nil)
	r := f.remote(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Exec(ctx, sandbox.ExecRequest{Command: "sleep 30"})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("a canceled exec returned %v after %v", err, time.Since(start))
	}
}

// TestRemoteInFrontOfDocker is the deployment shape: the API in front of real
// containers with no network.
func TestRemoteInFrontOfDocker(t *testing.T) {
	image := os.Getenv("HARNESS_DOCKER_IMAGE")
	if image == "" {
		t.Skip("HARNESS_DOCKER_IMAGE not set")
	}
	f := newFixture(t, func(c *sandboxapi.Config) {
		c.New = func(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
			if opts.Image != "" && opts.Image != image {
				return nil, errors.New("image not allowed")
			}
			return sandbox.NewDocker(ctx, sandbox.DockerOptions{Image: image, MemoryMB: opts.MemoryMB, Pids: opts.Pids})
		}
	})
	r := f.remote(t)
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	sandboxtest.Contract(t, r, "/tmp/work")
}
