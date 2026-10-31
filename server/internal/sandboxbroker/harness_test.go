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
	// queue holds tasks the fake hands out on claim; claims counts claim
	// requests of either flavour that reached the server.
	queue  []map[string]any
	claims int
	// taskCalls records forwarded task reports as "<task id> <action>"; failures
	// maps a failed task id to the error text reported.
	taskCalls []string
	failures  map[string]string
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
	case "/api/daemon/tasks/claim":
		var body struct {
			MaxTasks int `json:"max_tasks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		tasks := []map[string]any{}
		for len(tasks) < body.MaxTasks {
			task := f.pop()
			if task == nil {
				break
			}
			tasks = append(tasks, task)
		}
		writeJSON(w, map[string]any{"tasks": tasks})
	case "/api/daemon/runtimes/runtime-a/tasks/claim":
		writeJSON(w, map[string]any{"task": f.pop()})
	default:
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/daemon/tasks/"), "/")
		if !strings.HasPrefix(r.URL.Path, "/api/daemon/tasks/") || len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.taskCalls = append(f.taskCalls, parts[0]+" "+parts[1])
		if parts[1] == "fail" {
			var body struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if f.failures == nil {
				f.failures = map[string]string{}
			}
			f.failures[parts[0]] = body.Error
		}
		f.mu.Unlock()
		writeJSON(w, map[string]any{"status": "ok"})
	}
}

// enqueue adds a task the next claim will return.
func (f *fakeMultica) enqueue(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID, "issue_id": "issue-" + id,
		"initiator_email": "alice@example.com",
	})
}

// enqueueNoInitiator adds a task that no human triggered.
func (f *fakeMultica) enqueueNoInitiator(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID, "issue_id": "issue-" + id,
	})
}

func (f *fakeMultica) pop() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if len(f.queue) == 0 {
		return nil
	}
	task := f.queue[0]
	f.queue = f.queue[1:]
	return task
}

func (f *fakeMultica) claimCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims
}

func (f *fakeMultica) queued() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue)
}

func (f *fakeMultica) taskCallsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.taskCalls...)
}

func (f *fakeMultica) failureOf(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msg, ok := f.failures[id]
	return msg, ok
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
	// logs captures the manager's log output.
	logs *syncBuffer
	// audit captures the manager's audit log.
	audit *syncBuffer
}

// syncBuffer is a log sink safe for the manager's concurrent handlers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHarness(t *testing.T, expectedSize int, opts ...func(*sandboxbroker.PoolConfig)) *harness {
	t.Helper()
	up := newFakeMultica(t)
	cfg := sandboxbroker.PoolConfig{
		MulticaURL:   up.URL,
		MulticaToken: harnessPAT,
		WorkspaceID:  harnessWorkspaceID,
		Name:         "Test Pool",
		ExpectedSize: expectedSize,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	logs := &syncBuffer{}
	audit := &syncBuffer{}
	cfg.AuditLog = audit
	pool, err := sandboxbroker.NewPool(cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, upstream: up, pool: pool, logs: logs, audit: audit}
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
	// nonce is the boot nonce sent on registration; reportedIP is the address
	// the daemon claims for itself. Both are omitted when empty.
	nonce      string
	reportedIP string
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

// register sends the daemon's register request alone and returns the raw reply.
func (p *simPod) register() (int, []byte) {
	p.h.t.Helper()
	body := map[string]any{
		"workspace_id": harnessWorkspaceID,
		"daemon_id":    "pod-" + p.name,
		"device_name":  p.name,
		"cli_version":  "0.0.0-test",
		"runtimes": []map[string]any{
			{"name": "claude", "type": "claude", "version": "1.0.0", "status": "online"},
		},
	}
	if p.nonce != "" {
		body["boot_nonce"] = p.nonce
	}
	if p.reportedIP != "" {
		body["pod_ip"] = p.reportedIP
	}
	return p.call(http.MethodPost, "/api/daemon/register", body)
}

// heartbeat sends one daemon heartbeat and returns the status code.
func (p *simPod) heartbeat() int {
	p.h.t.Helper()
	status, _ := p.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	return status
}

// claimBatch plays the daemon's machine-level claim and returns the task IDs
// it was handed.
func (p *simPod) claimBatch() []string {
	p.h.t.Helper()
	status, raw := p.call(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{
		"daemon_id": "pod-" + p.name, "runtime_ids": []string{"runtime-a"}, "max_tasks": 4,
	})
	if status != http.StatusOK {
		p.h.t.Fatalf("batch claim: status %d: %s", status, raw)
	}
	var resp struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		p.h.t.Fatal(err)
	}
	ids := []string{}
	for _, task := range resp.Tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

// claimLegacy plays the single-runtime claim older servers need.
func (p *simPod) claimLegacy() string {
	p.h.t.Helper()
	status, raw := p.call(http.MethodPost, "/api/daemon/runtimes/runtime-a/tasks/claim", map[string]any{})
	if status != http.StatusOK {
		p.h.t.Fatalf("legacy claim: status %d: %s", status, raw)
	}
	var resp struct {
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		p.h.t.Fatal(err)
	}
	if resp.Task == nil {
		return ""
	}
	return resp.Task.ID
}

// report sends a task report (start, progress, complete, fail) and returns the
// status code.
func (p *simPod) report(taskID, action string) int {
	p.h.t.Helper()
	status, _ := p.call(http.MethodPost, "/api/daemon/tasks/"+taskID+"/"+action, map[string]any{"error": "boom"})
	return status
}

// leases queries the manager's lease records.
func (h *harness) leases() []sandboxbroker.LeaseInfo {
	h.t.Helper()
	resp, err := http.Get(h.admin.URL + "/v1/leases")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("leases status %d", resp.StatusCode)
	}
	var out []sandboxbroker.LeaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return out
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
	status, raw = p.register()
	if status != http.StatusOK {
		p.h.t.Fatalf("register: status %d: %s", status, raw)
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		p.h.t.Fatal(err)
	}
	return resp
}
