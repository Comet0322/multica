package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type apiServer struct {
	*httptest.Server
	calls   atomic.Int32
	lastReq atomic.Value // []byte
	lastHdr atomic.Value // http.Header
}

// newAPIServer answers each call from handler(callNumber, w).
func newAPIServer(t *testing.T, handler func(n int, w http.ResponseWriter)) *apiServer {
	t.Helper()
	s := &apiServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.lastReq.Store(b)
		s.lastHdr.Store(r.Header.Clone())
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		handler(int(s.calls.Add(1)), w)
	}))
	t.Cleanup(s.Close)
	return s
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func client(t *testing.T, s *apiServer) (*Anthropic, *[]time.Duration) {
	t.Helper()
	a, err := NewAnthropic(AnthropicConfig{APIKey: "sk-test", BaseURL: s.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	a.cfg.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	return a, &slept
}

const okBody = `{"model":"claude-haiku-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":7,"output_tokens":3}}`

func TestRequestCarriesTheConversationAndCacheBreakpoints(t *testing.T) {
	s := newAPIServer(t, func(_ int, w http.ResponseWriter) { reply(w, 200, okBody) })
	a, _ := client(t, s)

	_, err := a.Next(context.Background(), Request{
		System: "be brief",
		Tools:  []ToolSpec{{Name: "a", Description: "first", Schema: map[string]any{"type": "object"}}, {Name: "b", Description: "second", Schema: map[string]any{"type": "object"}}},
		Messages: []Message{
			{Role: User, Text: "do it"},
			{Role: Assistant, Text: "ok", ToolCalls: []ToolCall{{ID: "t1", Name: "a", Input: map[string]any{"x": 1}}}},
			{Role: User, ToolResults: []ToolResult{{CallID: "t1", Output: "result", IsError: true}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	hdr := s.lastHdr.Load().(http.Header)
	if hdr.Get("x-api-key") != "sk-test" || hdr.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("headers = %v", hdr)
	}
	var got apiRequest
	if err := json.Unmarshal(s.lastReq.Load().([]byte), &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != DefaultAnthropicModel || got.MaxTokens != 16000 {
		t.Errorf("model/max_tokens = %q/%d", got.Model, got.MaxTokens)
	}
	if len(got.System) != 1 || got.System[0].CacheControl == nil {
		t.Errorf("system should be one cached block: %+v", got.System)
	}
	if got.Tools[0].CacheControl != nil || got.Tools[1].CacheControl == nil {
		t.Errorf("only the last tool carries the cache breakpoint: %+v", got.Tools)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	tu := got.Messages[1].Content[1]
	if tu.Type != "tool_use" || tu.ID != "t1" || string(tu.Input) != `{"x":1}` {
		t.Errorf("tool_use block = %+v", tu)
	}
	tr := got.Messages[2].Content[0]
	if tr.Type != "tool_result" || tr.ToolUseID != "t1" || !tr.IsError || tr.Content != "result" || tr.CacheControl == nil {
		t.Errorf("tool_result block = %+v (should end the cached prefix)", tr)
	}
}

func TestReplyIsMappedToATurn(t *testing.T) {
	s := newAPIServer(t, func(_ int, w http.ResponseWriter) {
		reply(w, 200, `{"model":"claude-haiku-5-5","stop_reason":"tool_use","content":[
			{"type":"text","text":"checking"},
			{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"ls"}}],
			"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":80,"cache_creation_input_tokens":5}}`)
	})
	a, _ := client(t, s)
	turn, err := a.Next(context.Background(), Request{Messages: []Message{{Role: User, Text: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "checking" || len(turn.ToolCalls) != 1 || turn.ToolCalls[0].ID != "tu_1" || turn.ToolCalls[0].Input["command"] != "ls" {
		t.Errorf("turn = %+v", turn)
	}
	if turn.Usage != (Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 80, CacheWriteTokens: 5}) || turn.Model != "claude-haiku-5-5" {
		t.Errorf("usage/model = %+v %q", turn.Usage, turn.Model)
	}
}

func TestRateLimitsAreRetriedHonouringRetryAfter(t *testing.T) {
	s := newAPIServer(t, func(n int, w http.ResponseWriter) {
		if n < 3 {
			w.Header().Set("retry-after", "7")
			reply(w, 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		reply(w, 200, okBody)
	})
	a, slept := client(t, s)
	turn, err := a.Next(context.Background(), Request{Messages: []Message{{Role: User, Text: "x"}}})
	if err != nil || turn.Text != "done" {
		t.Fatalf("turn = %+v, err = %v", turn, err)
	}
	if s.calls.Load() != 3 || len(*slept) != 2 || (*slept)[0] != 7*time.Second {
		t.Errorf("calls = %d, slept = %v", s.calls.Load(), *slept)
	}
}

func TestOverloadAndServerErrorsAreRetriedThenGiveUp(t *testing.T) {
	s := newAPIServer(t, func(n int, w http.ResponseWriter) {
		reply(w, 529, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
	})
	a, slept := client(t, s)
	a.cfg.MaxRetries = 2
	_, err := a.Next(context.Background(), Request{Messages: []Message{{Role: User, Text: "x"}}})
	ae, ok := err.(*APIError)
	if !ok || ae.Status != 529 || ae.Type != "overloaded_error" {
		t.Fatalf("err = %v", err)
	}
	if s.calls.Load() != 3 || len(*slept) != 2 {
		t.Errorf("want 1 call + 2 retries, got %d calls, %d sleeps", s.calls.Load(), len(*slept))
	}
}

func TestARequestTheServerRejectsIsNotRetried(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		s := newAPIServer(t, func(_ int, w http.ResponseWriter) {
			reply(w, status, `{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`)
		})
		a, _ := client(t, s)
		_, err := a.Next(context.Background(), Request{Messages: []Message{{Role: User, Text: "x"}}})
		if err == nil || !strings.Contains(err.Error(), "nope") || s.calls.Load() != 1 {
			t.Errorf("status %d: err = %v after %d calls", status, err, s.calls.Load())
		}
	}
}

func TestACutOffReplyIsAnErrorNotAnAnswer(t *testing.T) {
	s := newAPIServer(t, func(_ int, w http.ResponseWriter) {
		reply(w, 200, `{"model":"m","stop_reason":"max_tokens","content":[{"type":"text","text":"half an ans"}],"usage":{}}`)
	})
	a, _ := client(t, s)
	if _, err := a.Next(context.Background(), Request{Messages: []Message{{Role: User, Text: "x"}}}); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("err = %v", err)
	}
}

func TestCancellationStopsRetrying(t *testing.T) {
	s := newAPIServer(t, func(_ int, w http.ResponseWriter) {
		reply(w, 429, `{"error":{"type":"rate_limit_error","message":"x"}}`)
	})
	a, _ := client(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	a.cfg.sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	if _, err := a.Next(ctx, Request{Messages: []Message{{Role: User, Text: "x"}}}); err == nil {
		t.Fatal("want an error")
	}
	if s.calls.Load() != 1 {
		t.Errorf("kept calling after cancellation: %d", s.calls.Load())
	}
}

func TestNewAnthropicNeedsAKey(t *testing.T) {
	if _, err := NewAnthropic(AnthropicConfig{}); err == nil {
		t.Error("an empty key was accepted")
	}
}
