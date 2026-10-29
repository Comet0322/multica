package sandboxbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxprovider"
)

const (
	testWorkspace = "6f1c2a51-0000-4000-8000-000000000001"
	testAgent     = "6f1c2a51-0000-4000-8000-0000000000a1"
	testRuntime   = "6f1c2a51-0000-4000-8000-0000000000r1"
	testTask      = "6f1c2a51-0000-4000-8000-0000000000t1"
	brokerToken   = "mul_broker"
	image         = "registry.example/multica-sandbox:1"
)

type call struct {
	Method, Path, Auth string
	Body               map[string]any
}

// fakeServer is the subset of the Multica daemon API the broker talks to.
type fakeServer struct {
	mu        sync.Mutex
	calls     []call
	claims    []string
	status    map[string]string
	heartbeat map[string]any
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
	switch {
	case r.URL.Path == "/api/daemon/register":
		writeJSON(w, http.StatusOK, map[string]any{
			"runtimes": []map[string]any{{"id": testRuntime, "name": "Managed", "provider": "claude", "status": "online"}},
			"repos":    []any{}, "repos_version": "v1",
		})
	case r.URL.Path == "/api/daemon/heartbeat":
		ack := map[string]any{"runtime_id": testRuntime, "status": "ok", "server_capabilities": []string{"cap-a"}}
		for k, v := range f.heartbeat {
			ack[k] = v
		}
		f.heartbeat = nil
		writeJSON(w, http.StatusOK, ack)
	case r.URL.Path == "/api/daemon/tasks/claim":
		var tasks []json.RawMessage
		for _, id := range f.claims {
			tasks = append(tasks, json.RawMessage(`{"id":"`+id+`","agent_id":"`+testAgent+`","runtime_id":"`+testRuntime+`","workspace_id":"`+testWorkspace+`","auth_token":"mat_task"}`))
		}
		f.claims = nil
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
	case strings.HasSuffix(r.URL.Path, "/status"):
		status := f.status[strings.Split(r.URL.Path, "/")[4]]
		if status == "" {
			status = "dispatched"
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": status})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func (f *fakeServer) find(path string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

type fakeProvider struct {
	mu        sync.Mutex
	sandboxes map[string]sandboxprovider.Sandbox
	ensured   []sandboxprovider.Spec
}

func (p *fakeProvider) Ensure(_ context.Context, spec sandboxprovider.Spec) (sandboxprovider.Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensured = append(p.ensured, spec)
	s := sandboxprovider.Sandbox{Key: spec.Key, Status: sandboxprovider.StatusRunning, Image: spec.Image}
	p.sandboxes[spec.Key] = s
	return s, nil
}

func (p *fakeProvider) Get(_ context.Context, key string) (sandboxprovider.Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sandboxes[key]; ok {
		return s, nil
	}
	return sandboxprovider.Sandbox{Key: key, Status: sandboxprovider.StatusMissing}, nil
}

func (p *fakeProvider) set(key string, status sandboxprovider.Status) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sandboxes[key]
	s.Status = status
	p.sandboxes[key] = s
}

func (p *fakeProvider) Stop(_ context.Context, key string) error {
	p.set(key, sandboxprovider.StatusStopped)
	return nil
}

func (p *fakeProvider) Delete(_ context.Context, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sandboxes, key)
	return nil
}

func (p *fakeProvider) List(context.Context) ([]sandboxprovider.Sandbox, error) { return nil, nil }

type harness struct {
	t        *testing.T
	server   *fakeServer
	provider *fakeProvider
	broker   *Broker
	api      http.Handler
	clock    time.Time
	dir      string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	server := &fakeServer{status: map[string]string{}}
	upstreamSrv := httptest.NewServer(server)
	t.Cleanup(upstreamSrv.Close)
	h := &harness{t: t, server: server, provider: &fakeProvider{sandboxes: map[string]sandboxprovider.Sandbox{}}, clock: time.Unix(1_800_000_000, 0), dir: t.TempDir()}
	h.start(upstreamSrv.URL)
	return h
}

func (h *harness) start(upstreamURL string) {
	h.t.Helper()
	b, err := New(Config{
		MulticaURL: upstreamURL, MulticaToken: brokerToken, WorkspaceID: testWorkspace,
		SandboxURL: "http://broker.internal:8790", Image: image, Providers: []string{"claude"},
		StateDir: h.dir, Name: "Managed",
	}, h.provider, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		h.t.Fatal(err)
	}
	b.now = func() time.Time { return h.clock }
	if err := b.load(); err != nil {
		h.t.Fatal(err)
	}
	h.broker, h.api = b, b.Handler()
}

func (h *harness) step() {
	h.t.Helper()
	if err := h.broker.Step(context.Background()); err != nil {
		h.t.Fatalf("step: %v", err)
	}
}

func (h *harness) advance(d time.Duration) { h.clock = h.clock.Add(d) }

func (h *harness) token(key string) string {
	h.broker.mu.Lock()
	defer h.broker.mu.Unlock()
	sb := h.broker.state.Sandboxes[key]
	if sb == nil {
		h.t.Fatalf("sandbox %s not created", key)
	}
	return sb.Token
}

func (h *harness) do(method, path, token string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Client-Version", "0.7.0")
	req.Header.Set("X-Client-Capabilities", "skill-bundles-v1")
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	return rec
}

var sandboxRegistration = map[string]any{
	"workspace_id": testWorkspace, "daemon_id": "sandbox-control", "device_name": "abc123", "cli_version": "0.7.0",
	"legacy_daemon_ids": []string{"old"}, "launched_by": "",
	"runtimes": []map[string]any{
		{"name": "Claude", "type": "claude", "version": "2.1.0", "status": "online"},
		{"name": "Codex", "type": "codex", "version": "0.9", "status": "online"},
		{"name": "Custom", "type": "claude", "profile_id": "p1", "status": "online"},
	},
}

// registered runs the bootstrap: the control sandbox's daemon registers and
// the broker replays its identity upstream.
func (h *harness) registered() {
	h.t.Helper()
	h.step()
	rec := h.do(http.MethodPost, "/api/daemon/register", h.token(controlKey), sandboxRegistration)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("sandbox register: %d %s", rec.Code, rec.Body)
	}
}

func TestBootstrapReplaysSandboxIdentityAsCloudRuntime(t *testing.T) {
	h := newHarness(t)
	h.step()
	if len(h.provider.ensured) != 1 || h.provider.ensured[0].Key != controlKey {
		t.Fatalf("bootstrap should start only the control sandbox, got %+v", h.provider.ensured)
	}
	env := h.provider.ensured[0].Env
	if env["MULTICA_SANDBOX_BROKER_URL"] != "http://broker.internal:8790" || !strings.HasPrefix(env["MULTICA_SANDBOX_TOKEN"], "msb_") {
		t.Fatalf("sandbox env = %v", env)
	}
	for _, v := range env {
		if v == brokerToken {
			t.Fatal("the broker's Multica token must never reach a sandbox")
		}
	}

	rec := h.do(http.MethodPost, "/api/daemon/register", h.token(controlKey), sandboxRegistration)
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	regs := h.server.find("/api/daemon/register")
	if len(regs) != 1 {
		t.Fatalf("upstream registrations = %d", len(regs))
	}
	up := regs[0]
	if up.Auth != "Bearer "+brokerToken || up.Body["daemon_id"] != h.broker.cfg.BrokerID || up.Body["device_name"] != "Managed" {
		t.Fatalf("upstream registration identity = %+v", up)
	}
	if _, ok := up.Body["legacy_daemon_ids"]; ok {
		t.Fatal("legacy daemon IDs must not be replayed")
	}
	runtimes := up.Body["runtimes"].([]any)
	if len(runtimes) != 1 {
		t.Fatalf("only the allowed built-in provider should register, got %v", runtimes)
	}
	rt := runtimes[0].(map[string]any)
	if rt["type"] != "claude" || rt["runtime_mode"] != "cloud" || rt["name"] != "Managed" || rt["version"] != "2.1.0" {
		t.Fatalf("runtime = %v", rt)
	}
	var resp struct {
		Runtimes []runtimeRef `json:"runtimes"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Runtimes) != 1 || resp.Runtimes[0].ID != testRuntime {
		t.Fatalf("sandbox register response = %s", rec.Body)
	}

	// A second sandbox with the same image must not re-register upstream.
	h.server.claims = []string{testTask}
	h.step()
	second := maps.Clone(sandboxRegistration)
	second["daemon_id"], second["device_name"] = "sandbox-agent", "other-host"
	if rec := h.do(http.MethodPost, "/api/daemon/register", h.token("agent-"+testAgent), second); rec.Code != http.StatusOK {
		t.Fatalf("second register: %d", rec.Code)
	}
	if n := len(h.server.find("/api/daemon/register")); n != 1 {
		t.Fatalf("upstream registrations after second sandbox = %d", n)
	}

	// Later steps replay the captured daemon headers upstream.
	h.advance(time.Minute)
	h.step()
	hb := h.server.find("/api/daemon/heartbeat")
	if len(hb) == 0 {
		t.Fatal("broker should heartbeat its runtime")
	}
}

func TestTaskRunsInItsAgentSandbox(t *testing.T) {
	h := newHarness(t)
	h.registered()
	h.server.claims = []string{testTask}
	h.step()
	key := "agent-" + testAgent
	if _, err := h.provider.Get(context.Background(), key); err != nil || h.provider.sandboxes[key].Status != sandboxprovider.StatusRunning {
		t.Fatalf("agent sandbox not started: %+v", h.provider.sandboxes)
	}
	claim := h.server.find("/api/daemon/tasks/claim")[0]
	if claim.Body["daemon_id"] != h.broker.cfg.BrokerID {
		t.Fatalf("claim body = %v", claim.Body)
	}

	// Another sandbox (here: control) cannot see or touch the task.
	other := h.token(controlKey)
	var empty struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	_ = json.Unmarshal(h.do(http.MethodPost, "/api/daemon/tasks/claim", other, map[string]any{"max_tasks": 5}).Body.Bytes(), &empty)
	if len(empty.Tasks) != 0 {
		t.Fatal("task leaked to another sandbox")
	}
	if rec := h.do(http.MethodPost, "/api/daemon/tasks/"+testTask+"/start", other, map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign start = %d", rec.Code)
	}

	token := h.token(key)
	var got struct {
		Tasks []map[string]any `json:"tasks"`
	}
	_ = json.Unmarshal(h.do(http.MethodPost, "/api/daemon/tasks/claim", token, map[string]any{"max_tasks": 2}).Body.Bytes(), &got)
	if len(got.Tasks) != 1 || got.Tasks[0]["id"] != testTask || got.Tasks[0]["auth_token"] != "mat_task" {
		t.Fatalf("delivered = %v", got.Tasks)
	}

	if rec := h.do(http.MethodPost, "/api/daemon/tasks/"+testTask+"/start", token, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("start = %d", rec.Code)
	}
	start := h.server.find("/api/daemon/tasks/" + testTask + "/start")
	if len(start) != 1 || start[0].Auth != "Bearer "+brokerToken {
		t.Fatalf("start must be forwarded with the broker credential, got %+v", start)
	}
	if h.broker.state.Tasks[testTask].State != taskStarted {
		t.Fatal("task should be marked started")
	}
	if rec := h.do(http.MethodPost, "/api/daemon/runtimes/"+testRuntime+"/tasks/"+testTask+"/skill-bundles/resolve", token, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("skill bundle forward = %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/daemon/tasks/"+testTask+"/complete", token, map[string]any{"output": "done"}); rec.Code != http.StatusOK {
		t.Fatalf("complete = %d", rec.Code)
	}
	if _, ok := h.broker.state.Tasks[testTask]; ok {
		t.Fatal("completed task should be released")
	}

	// Without work the sandbox is stopped after the idle timeout.
	h.advance(h.broker.cfg.IdleTimeout / 2)
	h.step()
	h.advance(h.broker.cfg.IdleTimeout)
	h.step()
	if h.provider.sandboxes[key].Status != sandboxprovider.StatusStopped {
		t.Fatalf("idle sandbox status = %s", h.provider.sandboxes[key].Status)
	}
}

func TestSandboxRoutingBoundaries(t *testing.T) {
	h := newHarness(t)
	h.registered()
	token := h.token(controlKey)

	if rec := h.do(http.MethodPost, "/api/daemon/runtimes/"+testRuntime+"/recover-orphans", token, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("recover-orphans = %d", rec.Code)
	}
	if len(h.server.find("/api/daemon/runtimes/"+testRuntime+"/recover-orphans")) != 0 {
		t.Fatal("recover-orphans must not reach the shared upstream runtime")
	}
	rec := h.do(http.MethodGet, "/api/daemon/workspaces/"+testWorkspace+"/runtime-profiles", token, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"runtime_profiles":[]`) {
		t.Fatalf("runtime profiles = %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/daemon/workspaces/6f1c2a51-0000-4000-8000-00000000ffff/repos", token, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign workspace repos = %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/daemon/something-new", token, map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("unknown daemon endpoint = %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/daemon/deregister", token, map[string]any{"runtime_ids": []string{testRuntime}}); rec.Code != http.StatusOK || len(h.server.find("/api/daemon/deregister")) != 0 {
		t.Fatal("deregister must be absorbed")
	}
	if rec := h.do(http.MethodGet, "/api/issues/MUL-1", token, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("sandbox credential on the public API = %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/daemon/heartbeat", "", map[string]any{}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated daemon call = %d", rec.Code)
	}

	// Agent CLI traffic keeps its own task token.
	if rec := h.do(http.MethodGet, "/api/issues/MUL-1", "mat_task", nil); rec.Code != http.StatusOK {
		t.Fatalf("agent pass-through = %d", rec.Code)
	}
	if got := h.server.find("/api/issues/MUL-1"); len(got) != 1 || got[0].Auth != "Bearer mat_task" {
		t.Fatalf("agent request auth = %+v", got)
	}
}

func TestHeartbeatHidesImageSkillsAndRelaysModelList(t *testing.T) {
	h := newHarness(t)
	h.registered()
	h.server.heartbeat = map[string]any{
		"pending_local_skills": map[string]string{"id": "ls1"},
		"pending_model_list":   map[string]string{"id": "ml1"},
		"pending_update":       map[string]string{"id": "up1", "target_version": "9.9.9"},
	}
	h.step()
	skills := h.server.find("/api/daemon/runtimes/" + testRuntime + "/local-skills/ls1/result")
	if len(skills) != 1 || skills[0].Body["supported"] != false || skills[0].Body["mcp_supported"] != false {
		t.Fatalf("local skills result = %+v", skills)
	}
	if up := h.server.find("/api/daemon/runtimes/" + testRuntime + "/update/up1/result"); len(up) != 1 || up[0].Body["status"] != "failed" {
		t.Fatalf("update result = %+v", up)
	}

	token := h.token(controlKey)
	rec := h.do(http.MethodPost, "/api/daemon/heartbeat", token, map[string]any{"runtime_id": testRuntime})
	if !strings.Contains(rec.Body.String(), `"pending_model_list":{"id":"ml1"}`) || !strings.Contains(rec.Body.String(), "cap-a") {
		t.Fatalf("control heartbeat = %s", rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/daemon/runtimes/"+testRuntime+"/models/ml1/result", token, map[string]any{"status": "completed"}); rec.Code != http.StatusOK {
		t.Fatalf("model result = %d", rec.Code)
	}
	if len(h.server.find("/api/daemon/runtimes/"+testRuntime+"/models/ml1/result")) != 1 {
		t.Fatal("model list result should be forwarded once")
	}
	if rec := h.do(http.MethodPost, "/api/daemon/runtimes/"+testRuntime+"/models/ml1/result", token, map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("replayed model result = %d", rec.Code)
	}
}

func TestLeaseAndRecovery(t *testing.T) {
	h := newHarness(t)
	h.registered()
	h.server.claims = []string{testTask, "6f1c2a51-0000-4000-8000-0000000000t2"}
	h.step()
	key := "agent-" + testAgent
	token := h.token(key)

	h.advance(h.broker.cfg.LeaseInterval)
	h.step()
	if n := len(h.server.find("/api/daemon/runtimes/" + testRuntime + "/tasks/" + testTask + "/prepare-lease")); n == 0 {
		t.Fatal("queued task lease should be extended")
	}

	h.do(http.MethodPost, "/api/daemon/tasks/claim", token, map[string]any{"max_tasks": 2})
	h.do(http.MethodPost, "/api/daemon/tasks/"+testTask+"/start", token, map[string]any{})

	// The sandbox dies: the started task fails, the undelivered one requeues.
	h.provider.set(key, sandboxprovider.StatusStopped)
	h.advance(sandboxStartGrace)
	h.step()
	fails := h.server.find("/api/daemon/tasks/" + testTask + "/fail")
	if len(fails) != 1 || !strings.Contains(fails[0].Body["error"].(string), "stopped while running") {
		t.Fatalf("lost started task should fail, got %+v", fails)
	}
	t2 := h.broker.state.Tasks["6f1c2a51-0000-4000-8000-0000000000t2"]
	if t2 == nil || t2.State != taskQueued {
		t.Fatalf("delivered task should requeue, got %+v", t2)
	}
	if h.provider.sandboxes[key].Status != sandboxprovider.StatusRunning {
		t.Fatal("sandbox should be restarted for the requeued task")
	}

	// The server cancelled it meanwhile.
	h.server.status["6f1c2a51-0000-4000-8000-0000000000t2"] = "cancelled"
	h.advance(statusCheckInterval)
	h.step()
	if len(h.broker.state.Tasks) != 0 {
		t.Fatalf("cancelled task should be released, got %v", h.broker.state.Tasks)
	}
}

func TestStartTimeoutFailsTask(t *testing.T) {
	h := newHarness(t)
	h.registered()
	h.server.claims = []string{testTask}
	h.step()
	h.advance(h.broker.cfg.StartTimeout + time.Second)
	h.step()
	if fails := h.server.find("/api/daemon/tasks/" + testTask + "/fail"); len(fails) != 1 {
		t.Fatalf("expired task should fail once, got %d", len(fails))
	}
}

func TestJournalSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.registered()
	h.server.claims = []string{testTask}
	h.step()
	key := "agent-" + testAgent
	token := h.token(key)

	h.start(h.broker.cfg.MulticaURL)
	if !h.broker.registered() {
		t.Fatal("restarted broker should reuse its registration")
	}
	var got struct {
		Tasks []map[string]any `json:"tasks"`
	}
	_ = json.Unmarshal(h.do(http.MethodPost, "/api/daemon/tasks/claim", token, map[string]any{"max_tasks": 1}).Body.Bytes(), &got)
	if len(got.Tasks) != 1 {
		t.Fatal("running sandbox should keep its credential and queued task across restarts")
	}
}
