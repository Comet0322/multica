package agentsvc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness/agentsvc"
	"github.com/multica-ai/multica/server/pkg/harness/model"
	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
)

const token = "svc-token"

type fixture struct {
	srv       *agentsvc.Server
	ts        *httptest.Server
	client    *agentsvc.Client
	opened    atomic.Int32
	closed    atomic.Int32
	heartbeat time.Duration
}

type countedSandbox struct {
	sandbox.Sandbox
	f *fixture
}

func (c countedSandbox) Close(ctx context.Context) error {
	c.f.closed.Add(1)
	return c.Sandbox.Close(ctx)
}

func newFixture(t *testing.T, steps func() []model.Step, mutate func(*agentsvc.Config)) *fixture {
	t.Helper()
	f := &fixture{}
	cfg := agentsvc.Config{
		Token: token,
		NewModel: func(id string) (model.Model, error) {
			if id == "nope" {
				return nil, errors.New("no such model")
			}
			return model.NewMock(steps()...), nil
		},
		NewSandbox: func(context.Context) (sandbox.Sandbox, string, error) {
			f.opened.Add(1)
			l, err := sandbox.NewLocal(t.TempDir())
			return countedSandbox{l, f}, "/", err
		},
		Heartbeat: 50 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := agentsvc.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.srv, f.ts = srv, httptest.NewServer(srv)
	t.Cleanup(f.ts.Close)
	f.client = &agentsvc.Client{BaseURL: f.ts.URL, Token: token}
	return f
}

func say(text string) func() []model.Step {
	return func() []model.Step { return []model.Step{model.Say(text)} }
}

func TestARunStreamsStartedMessagesAndOneResult(t *testing.T) {
	f := newFixture(t, func() []model.Step {
		return []model.Step{
			model.Call("c1", "write_file", map[string]any{"path": "a.txt", "content": "hi"}),
			model.Call("c2", "bash", map[string]any{"command": "cat a.txt; cat seeded.txt"}),
			model.Say("done"),
		}
	}, nil)

	var types []string
	var sawFiles bool
	res, err := f.client.Run(context.Background(), agentsvc.RunRequest{
		Prompt: "work",
		Files:  []agentsvc.File{{Path: "seeded.txt", Data: []byte("seed")}},
	}, func(e agentsvc.Event) {
		types = append(types, e.Type)
		if e.Message != nil && e.Message.Tool == "bash" && e.Message.Output != "" && strings.Contains(e.Message.Output, "hiseed") {
			sawFiles = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" || res.Output != "done" || res.Usage["mock"].InputTokens == 0 {
		t.Errorf("result = %+v", res)
	}
	if types[0] != agentsvc.EventStarted || types[len(types)-1] != agentsvc.EventResult {
		t.Errorf("event order = %v", types)
	}
	if !sawFiles {
		t.Error("the run did not see both its own file and the seeded one")
	}
	// The result is the last thing sent, so the handler may not have returned yet.
	waitFor(t, func() bool { return f.srv.Active() == 0 }, "the run to be forgotten")
	if f.opened.Load() != 1 || f.closed.Load() != 1 {
		t.Errorf("sandboxes opened %d closed %d", f.opened.Load(), f.closed.Load())
	}
}

func TestTheSandboxIsClosedBeforeTheResultIsSent(t *testing.T) {
	f := newFixture(t, say("ok"), nil)
	_, err := f.client.Run(context.Background(), agentsvc.RunRequest{Prompt: "x"}, func(e agentsvc.Event) {
		if e.Type == agentsvc.EventResult && f.closed.Load() != 1 {
			t.Error("the result arrived while the sandbox was still open")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEveryEndpointNeedsTheToken(t *testing.T) {
	f := newFixture(t, say("ok"), nil)
	for _, c := range []struct{ method, path string }{{"POST", "/v1/runs"}, {"DELETE", "/v1/runs/run_x"}} {
		for _, auth := range []string{"", "Bearer nope", "Basic " + token, token} {
			req, _ := http.NewRequest(c.method, f.ts.URL+c.path, strings.NewReader(`{"prompt":"x"}`))
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Errorf("%s %s with %q: %d, want 401", c.method, c.path, auth, resp.StatusCode)
			}
		}
	}
	if f.opened.Load() != 0 {
		t.Error("an unauthenticated request created a sandbox")
	}
}

func TestRefusedRequestsDoNotLeaveASandboxBehind(t *testing.T) {
	f := newFixture(t, say("ok"), nil)
	for name, c := range map[string]struct {
		req  agentsvc.RunRequest
		want int
	}{
		"no prompt":     {agentsvc.RunRequest{}, 400},
		"unknown model": {agentsvc.RunRequest{Prompt: "x", Model: "nope"}, 400},
		"path escape":   {agentsvc.RunRequest{Prompt: "x", Files: []agentsvc.File{{Path: "../x"}}}, 400},
		"absolute path": {agentsvc.RunRequest{Prompt: "x", Files: []agentsvc.File{{Path: ""}}}, 400},
	} {
		_, err := f.client.Run(context.Background(), c.req, nil)
		var ae *agentsvc.APIError
		if !errors.As(err, &ae) || ae.Status != c.want {
			t.Errorf("%s: err = %v, want status %d", name, err, c.want)
		}
	}
	if f.opened.Load() != f.closed.Load() {
		t.Errorf("sandboxes opened %d, closed %d", f.opened.Load(), f.closed.Load())
	}
}

func TestTheRunLimitIsHeld(t *testing.T) {
	block := make(chan struct{})
	f := newFixture(t, func() []model.Step {
		return []model.Step{model.CallWith(func(model.Request) model.ToolCall {
			<-block
			return model.ToolCall{ID: "c", Name: "bash", Input: map[string]any{"command": "true"}}
		}), model.Say("ok")}
	}, func(c *agentsvc.Config) { c.MaxRuns = 1 })

	done := make(chan error, 1)
	go func() {
		_, err := f.client.Run(context.Background(), agentsvc.RunRequest{Prompt: "first"}, nil)
		done <- err
	}()
	for i := 0; f.srv.Active() == 0 && i < 200; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	_, err := f.client.Run(context.Background(), agentsvc.RunRequest{Prompt: "second"}, nil)
	var ae *agentsvc.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusTooManyRequests {
		t.Errorf("second run: %v, want 429", err)
	}
	close(block)
	if err := <-done; err != nil {
		t.Errorf("first run: %v", err)
	}
	if _, err := f.client.Run(context.Background(), agentsvc.RunRequest{Prompt: "third"}, nil); err != nil {
		t.Errorf("after the first finished: %v", err)
	}
}

func longCommand() []model.Step {
	return []model.Step{model.Call("c", "bash", map[string]any{"command": "sleep 30"}), model.Say("never")}
}

func TestClosingTheConnectionCancelsTheRunAndFreesTheSandbox(t *testing.T) {
	f := newFixture(t, longCommand, nil)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.client.Run(ctx, agentsvc.RunRequest{Prompt: "x"}, func(e agentsvc.Event) {
			if e.Message != nil && e.Message.Type == "tool-use" {
				close(started)
			}
		})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("client err = %v", err)
	}
	waitFor(t, func() bool { return f.srv.Active() == 0 && f.closed.Load() == 1 }, "the run to be torn down")
}

func TestDeleteCancelsARun(t *testing.T) {
	f := newFixture(t, longCommand, nil)
	var id string
	started := make(chan struct{})
	go func() {
		_, _ = f.client.Run(context.Background(), agentsvc.RunRequest{Prompt: "x"}, func(e agentsvc.Event) {
			if e.Type == agentsvc.EventStarted {
				id = e.RunID
			}
			if e.Message != nil && e.Message.Type == "tool-use" {
				close(started)
			}
		})
	}()
	<-started
	req, _ := http.NewRequest("DELETE", f.ts.URL+"/v1/runs/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete: %v %v", resp, err)
	}
	resp.Body.Close()
	waitFor(t, func() bool { return f.srv.Active() == 0 }, "the run to end")
}

func TestARunThatOutlivesItsTimeoutEndsWithTimeoutStatus(t *testing.T) {
	f := newFixture(t, longCommand, nil)
	res, err := f.client.Run(context.Background(), agentsvc.RunRequest{Prompt: "x", TimeoutSec: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "timeout" {
		t.Errorf("result = %+v", res)
	}
	if f.closed.Load() != 1 {
		t.Error("sandbox not closed after a timeout")
	}
}

func TestAnIdleStreamSendsHeartbeats(t *testing.T) {
	f := newFixture(t, func() []model.Step {
		return []model.Step{model.Call("c", "bash", map[string]any{"command": "sleep 1"}), model.Say("ok")}
	}, nil)
	beats := 0
	if _, err := f.client.Run(context.Background(), agentsvc.RunRequest{Prompt: "x"}, func(e agentsvc.Event) {
		if e.Type == agentsvc.EventHeartbeat {
			beats++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if beats < 5 {
		t.Errorf("%d heartbeats during a 1s command at 50ms", beats)
	}
}

func TestOnlyTheTaskIdentityIsReadFromTheRequestEnv(t *testing.T) {
	var seen model.Request
	f := newFixture(t, func() []model.Step {
		return []model.Step{model.CallWith(func(r model.Request) model.ToolCall {
			seen = r
			return model.ToolCall{ID: "c", Name: "bash", Input: map[string]any{"command": "env"}}
		}), model.Say("ok")}
	}, nil)
	var out string
	_, err := f.client.Run(context.Background(), agentsvc.RunRequest{
		Prompt: "x", Env: map[string]string{"HARNESS_ANTHROPIC_API_KEY": "stolen", "PATH": "/evil", "MULTICA_TASK_ID": "t1"},
	}, func(e agentsvc.Event) {
		if e.Message != nil && e.Message.Type == "tool-result" {
			out = e.Message.Output
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "stolen") || strings.Contains(out, "/evil") {
		t.Errorf("request env reached the sandbox:\n%s", out)
	}
	_ = seen
}

func TestTheSystemPromptEndsWithTheEnvironmentNote(t *testing.T) {
	got := agentsvc.SystemPrompt("brief mentions the multica CLI", true)
	if !strings.HasPrefix(got, "brief mentions") || !strings.Contains(got[strings.Index(got, "## Environment"):], "no network") || !strings.Contains(got, "deliver") {
		t.Errorf("system prompt:\n%s", got)
	}
	if strings.Contains(agentsvc.SystemPrompt("", false), "deliver") {
		t.Error("git instructions without git")
	}
}

func TestAServiceThatDiesMidRunLeavesTheClientWithoutAResult(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"type":"started","run_id":"run_x"}` + "\n"))
	}))
	defer ts.Close()
	c := &agentsvc.Client{BaseURL: ts.URL, Token: token}
	if _, err := c.Run(context.Background(), agentsvc.RunRequest{Prompt: "x"}, nil); !errors.Is(err, agentsvc.ErrNoResult) {
		t.Errorf("err = %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
