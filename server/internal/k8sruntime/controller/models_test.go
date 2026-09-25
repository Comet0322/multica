package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon"
)

type gw struct {
	mu      sync.Mutex
	hits    []string
	headers []http.Header
	handler http.HandlerFunc
}

func newGW(t *testing.T, h http.HandlerFunc) (*gw, string) {
	t.Helper()
	g := &gw{handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.hits = append(g.hits, r.URL.RequestURI())
		g.headers = append(g.headers, r.Header.Clone())
		g.mu.Unlock()
		g.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return g, srv.URL
}

func ask(t *testing.T, h *harness) map[string]any {
	t.Helper()
	h.c.handleAck(context.Background(), "rt-1", &daemon.HeartbeatResponse{PendingModelList: &daemon.PendingModelList{ID: "q"}})
	return h.srv.waitReport(t, "models:q")
}

func TestScanReadsTheGatewaysOwnModelList(t *testing.T) {
	g, url := newGW(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"corp-large","display_name":"Corp Large"},{"id":"corp-small"},{"id":""},{"id":"corp-small"}],"has_more":false}`))
	})
	h := newHarness(t, func(c *Config) { c.ModelsURL = url + "/"; c.ModelsAPIKey = "secret-key" })
	r := ask(t, h)

	if r["status"] != "completed" || r["fallback"] != false {
		t.Fatalf("answer = %v", r)
	}
	m := modelsOf(t, r)
	if len(m) != 2 || m[0].ID != "corp-large" || m[0].Label != "Corp Large" || !m[0].Default || m[1].ID != "corp-small" || m[1].Label != "corp-small" || m[1].Default {
		t.Fatalf("models = %+v (empty ids and duplicates must be dropped)", m)
	}
	if !strings.HasPrefix(g.hits[0], "/v1/models") {
		t.Fatalf("asked %q", g.hits[0])
	}
	// The same key is offered both ways so either kind of gateway accepts it.
	if got := g.headers[0].Get("Authorization"); got != "Bearer secret-key" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := g.headers[0].Get("X-Api-Key"); got != "secret-key" {
		t.Fatalf("x-api-key = %q", got)
	}
}

func TestScanFollowsPagination(t *testing.T) {
	_, url := newGW(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}],"has_more":true,"last_id":"b"}`))
			return
		}
		if r.URL.Query().Get("after_id") != "b" {
			t.Errorf("after_id = %q", r.URL.Query().Get("after_id"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"c"}],"has_more":false}`))
	})
	h := newHarness(t, func(c *Config) { c.ModelsURL = url })
	m := modelsOf(t, ask(t, h))
	if len(m) != 3 || m[2].ID != "c" {
		t.Fatalf("models = %+v", m)
	}
}

func TestScanAcceptsAnOpenAICompatibleList(t *testing.T) {
	_, url := newGW(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"llama-70b","object":"model","owned_by":"corp"}]}`))
	})
	h := newHarness(t, func(c *Config) { c.ModelsURL = url })
	if m := modelsOf(t, ask(t, h)); len(m) != 1 || m[0].ID != "llama-70b" {
		t.Fatalf("models = %+v", m)
	}
}

func TestScanFailureIsReportedWithoutLeakingTheKey(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"unauthorized": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "bad key secret-key", http.StatusUnauthorized)
		},
		"not a list": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<html>hi secret-key</html>`)) },
		"empty":      func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) },
	} {
		_, url := newGW(t, handler)
		h := newHarness(t, func(c *Config) { c.ModelsURL = url; c.ModelsAPIKey = "secret-key" })
		r := ask(t, h)
		if r["status"] != "failed" {
			t.Fatalf("%s: answer = %v", name, r)
		}
		if msg, _ := r["error"].(string); msg == "" || strings.Contains(msg, "secret-key") {
			t.Fatalf("%s: error must be set and must not echo the key or the gateway's body: %q", name, msg)
		}
	}
}

func TestUnreachableGatewayIsReportedAsAFailure(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ModelsURL = "http://127.0.0.1:1" })
	if r := ask(t, h); r["status"] != "failed" {
		t.Fatalf("answer = %v", r)
	}
}

func TestExplicitModelsWinOverAScan(t *testing.T) {
	g, url := newGW(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[{"id":"scanned"}]}`)) })
	h := newHarness(t, func(c *Config) { c.ModelsURL = url; c.Models = []string{"pinned"} })
	if m := modelsOf(t, ask(t, h)); len(m) != 1 || m[0].ID != "pinned" {
		t.Fatalf("models = %+v", m)
	}
	if len(g.hits) != 0 {
		t.Fatalf("the gateway must not be asked when the list is explicit: %v", g.hits)
	}
}

func TestDefaultModelIsHonouredWhenPresent(t *testing.T) {
	_, url := newGW(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`))
	})
	h := newHarness(t, func(c *Config) { c.ModelsURL = url; c.DefaultModel = "c" })
	m := modelsOf(t, ask(t, h))
	if m[0].Default || m[1].Default || !m[2].Default {
		t.Fatalf("models = %+v", m)
	}
	h2 := newHarness(t, func(c *Config) { c.Models = []string{"a", "b"}; c.DefaultModel = "not-in-list" })
	if m := modelsOf(t, ask(t, h2)); !m[0].Default || m[1].Default {
		t.Fatalf("an unknown default must fall back to the first: %+v", m)
	}
}
