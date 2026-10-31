package sandboxbroker_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxbroker"
)

const (
	wsA  = "aaaaaaaa-2222-4333-8444-555555555555"
	wsB  = "bbbbbbbb-2222-4333-8444-555555555555"
	patA = "mul_pat_workspace_a"
	patB = "mul_pat_workspace_b"
)

// fakeWorkspace is one workspace's side of the fake Multica server.
type fakeWorkspace struct {
	id, pat, runtimeID string
	queue              []map[string]any
	claims             int
	// failStatus, when non-zero, makes every claim answer with that status.
	failStatus int
	// completedWith records the PAT used for each forwarded task report.
	reports []string
}

// multiMultica is a fake Multica server hosting several workspaces, told apart
// by the PAT the manager presents.
type multiMultica struct {
	*httptest.Server
	mu  sync.Mutex
	wss map[string]*fakeWorkspace // by PAT
}

func newMultiMultica(t *testing.T, wss ...*fakeWorkspace) *multiMultica {
	t.Helper()
	m := &multiMultica{wss: map[string]*fakeWorkspace{}}
	for _, ws := range wss {
		ws.runtimeID = "rt-" + ws.id[:8]
		m.wss[ws.pat] = ws
	}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.Close)
	return m
}

func (m *multiMultica) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.wss[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if ws == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/api/daemon/register":
		var body struct {
			WorkspaceID string `json:"workspace_id"`
			Runtimes    []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"runtimes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.WorkspaceID != ws.id {
			http.Error(w, "wrong workspace for this PAT", http.StatusForbidden)
			return
		}
		var runtimes []map[string]any
		for _, rt := range body.Runtimes {
			runtimes = append(runtimes, map[string]any{"id": ws.runtimeID, "provider": rt.Type, "name": rt.Name})
		}
		writeJSON(w, map[string]any{"runtimes": runtimes})
	case r.URL.Path == "/api/daemon/tasks/claim":
		ws.claims++
		if ws.failStatus != 0 {
			http.Error(w, "boom", ws.failStatus)
			return
		}
		tasks := []map[string]any{}
		if len(ws.queue) > 0 {
			tasks = append(tasks, ws.queue[0])
			ws.queue = ws.queue[1:]
		}
		writeJSON(w, map[string]any{"tasks": tasks})
	case strings.HasPrefix(r.URL.Path, "/api/daemon/tasks/"):
		ws.reports = append(ws.reports, strings.TrimPrefix(r.URL.Path, "/api/daemon/tasks/"))
		writeJSON(w, map[string]any{"status": "ok"})
	default:
		http.NotFound(w, r)
	}
}

func (m *multiMultica) enqueue(ws *fakeWorkspace, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws.queue = append(ws.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": ws.runtimeID,
		"workspace_id": ws.id, "issue_id": "issue-" + id, "initiator_email": "alice@example.com",
	})
}

func (m *multiMultica) claimCount(ws *fakeWorkspace) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ws.claims
}

func (m *multiMultica) setFail(ws *fakeWorkspace, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws.failStatus = status
}

func (m *multiMultica) reportsOf(ws *fakeWorkspace) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(ws.reports)
}

func newMultiHarness(t *testing.T, up *multiMultica, workspaces []sandboxbroker.WorkspaceConfig) *harness {
	t.Helper()
	logs, audit := &syncBuffer{}, &syncBuffer{}
	pool, err := sandboxbroker.NewPool(sandboxbroker.PoolConfig{
		MulticaURL: up.URL, Name: "Shared Pool", Workspaces: workspaces,
		UpstreamBackoff: time.Minute, AuditLog: audit,
	}, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, pool: pool, logs: logs, audit: audit}
	h.podAPI = httptest.NewServer(pool.PodHandler())
	h.admin = httptest.NewServer(pool.AdminHandler())
	t.Cleanup(h.podAPI.Close)
	t.Cleanup(h.admin.Close)
	return h
}

// registerFor registers the pod's daemon for one workspace and returns the
// runtime IDs it was given.
func (p *simPod) registerFor(workspaceID string) []string {
	p.h.t.Helper()
	body := map[string]any{
		"workspace_id": workspaceID, "daemon_id": "pod-" + p.name, "device_name": p.name,
		"cli_version": "0.0.0-test", "boot_nonce": p.nonce,
		"runtimes": []map[string]any{{"name": "claude", "type": "claude", "version": "1.0.0", "status": "online"}},
	}
	status, raw := p.call(http.MethodPost, "/api/daemon/register", body)
	if status != http.StatusOK {
		p.h.t.Fatalf("register for %s: status %d: %s", workspaceID, status, raw)
	}
	var resp struct {
		Runtimes []struct {
			ID string `json:"id"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		p.h.t.Fatal(err)
	}
	var ids []string
	for _, rt := range resp.Runtimes {
		ids = append(ids, rt.ID)
	}
	return ids
}

// startAll plays a daemon that watches every workspace the manager lists.
func (p *simPod) startAll() []string {
	p.h.t.Helper()
	ids := p.startAllRaw()
	if p.settleFirstSeen() {
		ids = p.startAllRaw()
	}
	return ids
}

func (p *simPod) startAllRaw() []string {
	p.h.t.Helper()
	status, raw := p.call(http.MethodGet, "/api/workspaces", nil)
	if status != http.StatusOK {
		p.h.t.Fatalf("list workspaces: status %d", status)
	}
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		p.h.t.Fatal(err)
	}
	var ids []string
	for _, ws := range list {
		ids = append(ids, p.registerFor(ws.ID)...)
	}
	return ids
}

func (p *simPod) claimFrom(runtimeIDs []string) []string {
	p.h.t.Helper()
	status, raw := p.call(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{
		"daemon_id": "pod-" + p.name, "runtime_ids": runtimeIDs, "max_tasks": 4,
	})
	if status != http.StatusOK {
		p.h.t.Fatalf("claim: status %d: %s", status, raw)
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

func twoWorkspaces(a, b func(*sandboxbroker.WorkspaceConfig)) []sandboxbroker.WorkspaceConfig {
	ca := sandboxbroker.WorkspaceConfig{WorkspaceID: wsA, Token: patA, RuntimeName: "Pool A"}
	cb := sandboxbroker.WorkspaceConfig{WorkspaceID: wsB, Token: patB, RuntimeName: "Pool B"}
	if a != nil {
		a(&ca)
	}
	if b != nil {
		b(&cb)
	}
	return []sandboxbroker.WorkspaceConfig{ca, cb}
}

func TestPodSeesEveryWorkspaceAndTasksAreClaimedWithEachOwnPAT(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	up.enqueue(b, "tb")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"

	status, raw := pod.call(http.MethodGet, "/api/workspaces", nil)
	if status != http.StatusOK || !strings.Contains(string(raw), wsA) || !strings.Contains(string(raw), wsB) {
		t.Fatalf("pod must see both workspaces, got %d %s", status, raw)
	}
	ids := pod.startAll()
	if len(ids) != 2 {
		t.Fatalf("want a runtime per workspace, got %v", ids)
	}
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"tb"}) {
		t.Fatalf("want [tb], got %v", got)
	}
	if status := pod.report("tb", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	// The report reached B's upstream under B's PAT (the fake answers A's PAT
	// with A's state).
	if got := up.reportsOf(b); !slices.Equal(got, []string{"tb/complete"}) {
		t.Fatalf("workspace B should see the report, got %v", got)
	}
	if got := up.reportsOf(a); len(got) != 0 {
		t.Fatalf("workspace A must see nothing, got %v", got)
	}
}

func TestPodLentToAIsNotLentToBUntilResetAndRegisteredAgain(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	ids := pod.startAll()
	up.enqueue(a, "ta")
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"ta"}) {
		t.Fatalf("want [ta], got %v", got)
	}
	if status := pod.report("ta", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}

	// B has a task the moment A's lease ends.
	up.enqueue(b, "tb")
	before := up.claimCount(b)
	if got := pod.claimFrom(ids); len(got) != 0 {
		t.Fatalf("pod still resetting was handed %v", got)
	}
	cmd := pod.pollControl()
	if cmd == nil {
		t.Fatal("want a reset command")
	}
	pod.reportControl(cmd.ID, "done")
	if got := pod.claimFrom(ids); len(got) != 0 {
		t.Fatalf("pod reset but not registered again was handed %v", got)
	}
	if n := up.claimCount(b) - before; n != 0 {
		t.Fatalf("B's queue must not be touched before the pod registered again, saw %d claims", n)
	}

	// Registered for A only: B is still out of reach.
	pod.nonce = "boot-2"
	idsA := pod.registerFor(wsA)
	if got := pod.claimFrom(append(idsA, ids...)); len(got) != 0 {
		t.Fatalf("pod not registered for B was handed %v", got)
	}
	idsB := pod.registerFor(wsB)
	if got := pod.claimFrom(append(idsA, idsB...)); !slices.Equal(got, []string{"tb"}) {
		t.Fatalf("want [tb] after registering for B, got %v", got)
	}
}

func TestWorkspaceUpstreamFailureDoesNotAffectOthers(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
			up := newMultiMultica(t, a, b)
			h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
			up.setFail(a, status)
			up.enqueue(b, "tb1")
			up.enqueue(b, "tb2")
			p1, p2 := h.newPod("127.0.0.2", "pool-0"), h.newPod("127.0.0.3", "pool-1")
			p1.nonce, p2.nonce = "n1", "n2"
			ids1, ids2 := p1.startAll(), p2.startAll()

			if got := p1.claimFrom(ids1); !slices.Equal(got, []string{"tb1"}) {
				t.Fatalf("B must be served while A fails, got %v", got)
			}
			// A is left alone for the backoff; B keeps being served.
			failedClaims := up.claimCount(a)
			if got := p2.claimFrom(ids2); !slices.Equal(got, []string{"tb2"}) {
				t.Fatalf("B must keep being served, got %v", got)
			}
			if n := up.claimCount(a); n != failedClaims {
				t.Fatalf("failing workspace was polled again within the backoff: %d -> %d", failedClaims, n)
			}
		})
	}
}

func TestFailingUpstreamAloneStillAnswersBadGateway(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "n1"
	ids := pod.startAll()
	up.setFail(a, http.StatusInternalServerError)
	up.setFail(b, http.StatusInternalServerError)
	status, _ := pod.call(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{"runtime_ids": ids, "max_tasks": 1})
	if status != http.StatusBadGateway {
		t.Fatalf("want 502 when every upstream fails, got %d", status)
	}
}

func TestWorkspaceAtItsLeaseCapIsNotPolled(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(func(c *sandboxbroker.WorkspaceConfig) { c.MaxLeases = 1 }, nil))
	p1, p2 := h.newPod("127.0.0.2", "pool-0"), h.newPod("127.0.0.3", "pool-1")
	p1.nonce, p2.nonce = "n1", "n2"
	ids1, ids2 := p1.startAll(), p2.startAll()
	up.enqueue(a, "ta1")
	up.enqueue(a, "ta2")
	up.enqueue(b, "tb1")

	if got := p1.claimFrom(ids1); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want [ta1], got %v", got)
	}
	aClaims := up.claimCount(a)
	if got := p2.claimFrom(ids2); !slices.Equal(got, []string{"tb1"}) {
		t.Fatalf("A is capped, so B's task goes to the free pod; got %v", got)
	}
	if n := up.claimCount(a); n != aClaims {
		t.Fatalf("capped workspace was polled: %d -> %d", aClaims, n)
	}
	if up.queuedOf(a) != 1 {
		t.Fatal("A's second task must stay queued in Multica")
	}
}

func (m *multiMultica) queuedOf(ws *fakeWorkspace) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(ws.queue)
}

func TestWorkspacesTakeTurnsWhenBothHaveTasks(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	for _, id := range []string{"a1", "a2", "a3"} {
		up.enqueue(a, id)
	}
	for _, id := range []string{"b1", "b2", "b3"} {
		up.enqueue(b, id)
	}
	var got []string
	for i, ip := range []string{"127.0.0.2", "127.0.0.3", "127.0.0.4", "127.0.0.5"} {
		pod := h.newPod(ip, "pool-"+ip)
		pod.nonce = "n" + ip
		_ = i
		got = append(got, pod.claimFrom(pod.startAll())...)
	}
	var fromA, fromB int
	for _, id := range got {
		if strings.HasPrefix(id, "a") {
			fromA++
		} else {
			fromB++
		}
	}
	if fromA != 2 || fromB != 2 {
		t.Fatalf("want 2 tasks from each workspace, got %v", got)
	}
}

func TestInventoryLeasesAndAuditCarryTheWorkspace(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "n1"
	ids := pod.startAll()
	up.enqueue(b, "tb")
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"tb"}) {
		t.Fatalf("want [tb], got %v", got)
	}

	inv := h.inventory()
	if len(inv.Pods) != 1 || inv.Pods[0].WorkspaceID != wsB {
		t.Fatalf("leased pod must show workspace B: %+v", inv.Pods)
	}
	counts := map[string]int{}
	for _, ws := range inv.Workspaces {
		counts[ws.WorkspaceID] = ws.Leased
	}
	if counts[wsA] != 0 || counts[wsB] != 1 {
		t.Fatalf("per-workspace lease counts wrong: %+v", inv.Workspaces)
	}
	if ls := h.leases(); len(ls) != 1 || ls[0].WorkspaceID != wsB {
		t.Fatalf("lease record must carry workspace B: %+v", ls)
	}

	pod.report("tb", "complete")
	for _, rec := range h.auditRecords() {
		if rec.TaskID == "" {
			continue // a pod-level reset record, tied to no workspace
		}
		if rec.WorkspaceID != wsB {
			t.Fatalf("audit record without workspace B: %+v", rec)
		}
	}
	if recs := h.auditRecords(); len(recs) < 2 {
		t.Fatalf("want lease and release records, got %+v", recs)
	}
}

func TestPoolRejectsBadWorkspaceEntries(t *testing.T) {
	t.Parallel()
	cases := map[string][]sandboxbroker.WorkspaceConfig{
		"duplicate":      {{WorkspaceID: wsA, Token: patA}, {WorkspaceID: wsA, Token: patB}},
		"missing PAT":    {{WorkspaceID: wsA}},
		"bad ID":         {{WorkspaceID: "nope", Token: patA}},
		"negative quota": {{WorkspaceID: wsA, Token: patA, MaxLeases: -1}},
		"bad placement":  {{WorkspaceID: wsA, Token: patA, Placement: "pod"}},
	}
	for name, ws := range cases {
		if _, err := sandboxbroker.NewPool(sandboxbroker.PoolConfig{MulticaURL: "http://127.0.0.1:1", Workspaces: ws}, nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
