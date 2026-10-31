package sandboxbroker_test

// The task-lifecycle harness: a fake Multica server, simulated pod daemons that
// connect from distinct loopback addresses (127.0.0.2, 127.0.0.3, ...; Linux
// only) and the pool manager under test, all inside one test process.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxbroker"
)

const (
	harnessWorkspaceID = "11111111-2222-4333-8444-555555555555"
	harnessPAT         = "mul_test_manager_pat"
	podPlaceholder     = "mul_placeholder"
)

// fakeMultica stands in for the Multica server the manager talks to as a daemon.
type fakeMultica struct {
	*httptest.Server

	mu    sync.Mutex
	calls []string
	// registrations holds the bodies of /api/daemon/register calls received.
	registrations []map[string]any
}

func newFakeMultica(t *testing.T) *fakeMultica {
	t.Helper()
	f := &fakeMultica{}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeMultica) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+harnessPAT {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/api/daemon/register":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.registrations = append(f.registrations, body)
		f.mu.Unlock()
		var runtimes []map[string]any
		for i, rt := range body["runtimes"].([]any) {
			m := rt.(map[string]any)
			runtimes = append(runtimes, map[string]any{
				"id":       "runtime-" + string(rune('a'+i)),
				"provider": m["type"],
				"name":     m["name"],
			})
		}
		writeJSON(w, map[string]any{"runtimes": runtimes})
	case "/api/daemon/heartbeat":
		writeJSON(w, map[string]any{"status": "ok"})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeMultica) registrationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.registrations)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// harness wires the fake server and a pool manager together.
type harness struct {
	t        *testing.T
	upstream *fakeMultica
	pool     *sandboxbroker.Pool
	podAPI   *httptest.Server
	admin    *httptest.Server
}

func newHarness(t *testing.T, expectedSize int) *harness {
	t.Helper()
	up := newFakeMultica(t)
	pool, err := sandboxbroker.NewPool(sandboxbroker.PoolConfig{
		MulticaURL:   up.URL,
		MulticaToken: harnessPAT,
		WorkspaceID:  harnessWorkspaceID,
		Name:         "Test Pool",
		ExpectedSize: expectedSize,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, upstream: up, pool: pool}
	h.podAPI = httptest.NewServer(pool.PodHandler())
	h.admin = httptest.NewServer(pool.AdminHandler())
	t.Cleanup(h.podAPI.Close)
	t.Cleanup(h.admin.Close)
	return h
}

// inventory queries the manager's read-only inventory endpoint.
func (h *harness) inventory() sandboxbroker.Inventory {
	h.t.Helper()
	resp, err := http.Get(h.admin.URL + "/v1/inventory")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("inventory status %d", resp.StatusCode)
	}
	var inv sandboxbroker.Inventory
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		h.t.Fatal(err)
	}
	return inv
}

// simPod plays a pod's standard daemon: it reaches the manager from a chosen
// source address, as the pod IP would.
type simPod struct {
	h      *harness
	ip     string
	name   string
	client *http.Client
}

func (h *harness) newPod(ip, name string) *simPod {
	transport := &http.Transport{
		DialContext: (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}}).DialContext,
	}
	h.t.Cleanup(transport.CloseIdleConnections)
	return &simPod{h: h, ip: ip, name: name, client: &http.Client{Transport: transport, Timeout: 10 * time.Second}}
}

func (p *simPod) call(method, path string, body any) (int, []byte) {
	p.h.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, p.h.podAPI.URL+path, rd)
	if err != nil {
		p.h.t.Fatal(err)
	}
	// The pod only holds a placeholder token; the manager ignores its value.
	req.Header.Set("Authorization", "Bearer "+podPlaceholder)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// start runs the calls a daemon makes while starting up: renew its token, list
// workspaces, then register its runtimes. It returns the register response.
func (p *simPod) start() map[string]any {
	p.h.t.Helper()
	if status, _ := p.call(http.MethodPost, "/api/tokens/current/renew", map[string]any{}); status != http.StatusOK {
		p.h.t.Fatalf("renew token: status %d", status)
	}
	status, raw := p.call(http.MethodGet, "/api/workspaces", nil)
	if status != http.StatusOK {
		p.h.t.Fatalf("list workspaces: status %d", status)
	}
	if !strings.Contains(string(raw), harnessWorkspaceID) {
		p.h.t.Fatalf("workspace list %s does not offer %s", raw, harnessWorkspaceID)
	}
	status, raw = p.call(http.MethodPost, "/api/daemon/register", map[string]any{
		"workspace_id": harnessWorkspaceID,
		"daemon_id":    "pod-" + p.name,
		"device_name":  p.name,
		"cli_version":  "0.0.0-test",
		"runtimes": []map[string]any{
			{"name": "claude", "type": "claude", "version": "1.0.0", "status": "online"},
		},
	})
	if status != http.StatusOK {
		p.h.t.Fatalf("register: status %d: %s", status, raw)
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		p.h.t.Fatal(err)
	}
	return resp
}
