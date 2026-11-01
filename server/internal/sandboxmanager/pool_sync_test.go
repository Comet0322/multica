package sandboxmanager_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

const patPool = "mul_pat_pool_account"

// patchCall is one PATCH /api/runtimes/{id} the manager sent.
type patchCall struct {
	RuntimeID   string
	WorkspaceID string
	Body        string
}

// syncMultica is a fake Multica server for a PAT-discovered pool: it lists the
// account's workspaces, takes registrations and runtime updates, and has
// nothing to claim.
type syncMultica struct {
	*httptest.Server
	mu sync.Mutex
	// workspaces is what the account belongs to.
	workspaces []string
	// listStatus, when non-zero, makes the listing answer with that status.
	listStatus int
	// noDaemonList makes /api/daemon/workspaces answer 404, as an older server.
	noDaemonList bool
	paths        []string
	registered   map[string][]string // workspace -> runtime_mode of each registered runtime
	patches      []patchCall
	// visibility is what GET /api/runtimes reports for every registered
	// runtime, "private" until a PATCH sets it.
	visibility string
	// patchFails makes that many PATCH calls answer 500 without applying them.
	patchFails int
	// hook, when set, answers a request before the defaults: a non-zero status
	// is sent with the body. It runs with the fake's lock held.
	hook func(r *http.Request) (status int, body string)
}

func newSyncMultica(t *testing.T, workspaces ...string) *syncMultica {
	t.Helper()
	m := &syncMultica{workspaces: workspaces, registered: map[string][]string{}, visibility: "private"}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.Close)
	return m
}

func (m *syncMultica) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+patPool {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	m.paths = append(m.paths, r.Method+" "+r.URL.Path)
	if m.hook != nil {
		if status, body := m.hook(r); status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
	}
	switch {
	case r.URL.Path == "/api/runtimes" && r.Method == http.MethodGet:
		list := []map[string]any{}
		for ws := range m.registered {
			if r.Header.Get("X-Workspace-ID") == ws {
				list = append(list, map[string]any{"id": "rt-" + ws[:8], "visibility": m.visibility})
			}
		}
		writeJSON(w, list)
	case r.URL.Path == "/api/daemon/workspaces" || r.URL.Path == "/api/workspaces":
		if r.URL.Path == "/api/daemon/workspaces" && m.noDaemonList {
			http.NotFound(w, r)
			return
		}
		if m.listStatus != 0 {
			http.Error(w, "boom", m.listStatus)
			return
		}
		list := []map[string]any{}
		for _, id := range m.workspaces {
			list = append(list, map[string]any{"id": id, "name": "ws " + id[:4]})
		}
		writeJSON(w, list)
	case r.URL.Path == "/api/daemon/register":
		var body struct {
			WorkspaceID string `json:"workspace_id"`
			Runtimes    []struct {
				Type string `json:"type"`
				Mode string `json:"runtime_mode"`
			} `json:"runtimes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var out []map[string]any
		for _, rt := range body.Runtimes {
			m.registered[body.WorkspaceID] = append(m.registered[body.WorkspaceID], rt.Mode)
			out = append(out, map[string]any{"id": "rt-" + body.WorkspaceID[:8], "provider": rt.Type})
		}
		writeJSON(w, map[string]any{"runtimes": out})
	case strings.HasPrefix(r.URL.Path, "/api/runtimes/") && r.Method == http.MethodPatch:
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		if m.patchFails > 0 {
			m.patchFails--
			m.patches = append(m.patches, patchCall{RuntimeID: strings.TrimPrefix(r.URL.Path, "/api/runtimes/"), WorkspaceID: r.Header.Get("X-Workspace-ID"), Body: "FAILED " + string(raw)})
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if strings.Contains(string(raw), `"public"`) {
			m.visibility = "public"
		}
		m.patches = append(m.patches, patchCall{
			RuntimeID:   strings.TrimPrefix(r.URL.Path, "/api/runtimes/"),
			WorkspaceID: r.Header.Get("X-Workspace-ID"),
			Body:        string(raw),
		})
		writeJSON(w, map[string]any{"ok": true})
	case r.URL.Path == "/api/daemon/tasks/claim":
		writeJSON(w, map[string]any{"tasks": []any{}})
	default:
		writeJSON(w, map[string]any{"status": "ok"})
	}
}

func (m *syncMultica) setWorkspaces(ids ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workspaces = ids
}

func (m *syncMultica) setListStatus(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listStatus = status
}

func (m *syncMultica) sawPath(p string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Contains(m.paths, p)
}

func (m *syncMultica) patchList() []patchCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.patches)
}

func (m *syncMultica) modes(workspaceID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.registered[workspaceID])
}

func newSyncHarness(t *testing.T, up *syncMultica, mutate ...func(*sandboxmanager.PoolConfig)) *harness {
	t.Helper()
	logs, audit := &syncBuffer{}, &syncBuffer{}
	cfg := sandboxmanager.PoolConfig{
		MulticaURL: up.URL, Token: patPool, Name: "Pool",
		UpstreamBackoff: time.Minute, AuditLog: audit,
		DrainTimeout: time.Minute, DrainPoll: 5 * time.Millisecond,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	pool, err := sandboxmanager.NewPool(cfg, slog.New(slog.NewTextHandler(logs, nil)))
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

func TestSyncServesTheAccountsWorkspaces(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up)
	if got := workspaceIDs(h.inventory().Workspaces); len(got) != 0 {
		t.Fatalf("serving %v before the first sync", got)
	}

	res, err := h.pool.Sync(context.Background())
	if err != nil || !slices.Equal(res.Added, []string{wsA}) {
		t.Fatalf("first sync: %+v, %v", res, err)
	}
	up.setWorkspaces(wsA, wsB)
	res, err = h.pool.Sync(context.Background())
	if err != nil || !slices.Equal(res.Added, []string{wsB}) || len(res.Removed) != 0 {
		t.Fatalf("second sync: %+v, %v", res, err)
	}
	if got := workspaceIDs(h.inventory().Workspaces); !slices.Equal(got, []string{wsA, wsB}) {
		t.Fatalf("inventory lists %v", got)
	}
	// Syncing again with nothing new changes nothing.
	if res, err = h.pool.Sync(context.Background()); err != nil || len(res.Added)+len(res.Removed)+len(res.Updated) != 0 {
		t.Fatalf("idle sync: %+v, %v", res, err)
	}
}

func TestSyncDrainsAWorkspaceTheAccountLeft(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	h := newSyncHarness(t, up)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	up.setWorkspaces(wsA)
	// A workspace missing once is kept (a listing can lose a row for a moment);
	// the second sync without it removes it.
	res, err := h.pool.Sync(context.Background())
	if err != nil || len(res.Removed) != 0 {
		t.Fatalf("first sync without B must keep it: %+v, %v", res, err)
	}
	res, err = h.pool.Sync(context.Background())
	if err != nil || !slices.Equal(res.Removed, []string{wsB}) {
		t.Fatalf("sync after leaving B: %+v, %v", res, err)
	}
	if got := workspaceIDs(h.inventory().Workspaces); !slices.Equal(got, []string{wsA}) {
		t.Fatalf("still serving %v", got)
	}
}

func TestSyncKeepsWhatItHasWhenTheListingFailsOrIsEmpty(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	h := newSyncHarness(t, up)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	unchanged := func(what string) {
		t.Helper()
		if got := workspaceIDs(h.inventory().Workspaces); !slices.Equal(got, []string{wsA, wsB}) || len(h.inventory().Draining) != 0 {
			t.Fatalf("%s changed the configuration: %v", what, got)
		}
	}

	up.setListStatus(http.StatusInternalServerError)
	if _, err := h.pool.Sync(context.Background()); err == nil {
		t.Fatal("a failed listing must be reported")
	}
	unchanged("a failed listing")

	up.setListStatus(0)
	up.setWorkspaces()
	res, err := h.pool.Sync(context.Background())
	if err != nil || len(res.Removed) != 0 {
		t.Fatalf("an empty listing: %+v, %v", res, err)
	}
	unchanged("an empty listing")
	if logs := h.logs.String(); strings.Contains(logs, patPool) {
		t.Fatalf("log leaks the PAT:\n%s", logs)
	}
}

func TestSyncFallsBackToTheOlderWorkspaceListing(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	up.noDaemonList = true
	h := newSyncHarness(t, up)

	res, err := h.pool.Sync(context.Background())
	if err != nil || !slices.Equal(res.Added, []string{wsA}) {
		t.Fatalf("sync: %+v, %v", res, err)
	}
	if !up.sawPath("GET /api/daemon/workspaces") || !up.sawPath("GET /api/workspaces") {
		t.Fatalf("want the daemon listing tried first and the old one used, saw %v", up.paths)
	}
}

func TestSyncUsesTheDaemonListingWhenItExists(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if up.sawPath("GET /api/workspaces") {
		t.Fatal("the older listing was used though the daemon one answered")
	}
}

func TestSyncAppliesTheDefaultLeaseCapToEveryWorkspace(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	h := newSyncHarness(t, up, func(c *sandboxmanager.PoolConfig) { c.MaxLeases = 3 })
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	inv := h.inventory()
	if len(inv.Workspaces) != 2 {
		t.Fatalf("serving %+v", inv.Workspaces)
	}
	for _, w := range inv.Workspaces {
		if w.MaxLeases != 3 {
			t.Fatalf("workspace %s has cap %d, want 3", w.WorkspaceID, w.MaxLeases)
		}
	}
}

func TestSyncNeedsAToken(t *testing.T) {
	t.Parallel()
	up := newMultiMultica(t, &fakeWorkspace{id: wsA, pat: patA})
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil)[:1])
	if _, err := h.pool.Sync(context.Background()); err == nil {
		t.Fatal("a static pool has nothing to discover")
	}
}

func TestPoolTokenCannotBeCombinedWithWorkspaceSettings(t *testing.T) {
	t.Parallel()
	cases := map[string]sandboxmanager.PoolConfig{
		"workspace list": {Workspaces: []sandboxmanager.WorkspaceConfig{{WorkspaceID: wsA, Token: patA}}},
		"workspace ID":   {WorkspaceID: wsA},
		"upstream token": {MulticaToken: patA},
		"manager ID":     {ManagerID: "manager"},
	}
	for name, cfg := range cases {
		cfg.MulticaURL, cfg.Token = "http://127.0.0.1:1", patPool
		if _, err := sandboxmanager.NewPool(cfg, nil); err == nil {
			t.Errorf("%s with a Token: want an error", name)
		}
	}
	if _, err := sandboxmanager.NewPool(sandboxmanager.PoolConfig{MulticaURL: "http://127.0.0.1:1", Token: patPool}, nil); err != nil {
		t.Fatalf("a Token alone must be valid: %v", err)
	}
}

func TestPoolRefusesBadSyncSettings(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*sandboxmanager.PoolConfig){
		"negative max leases":    func(c *sandboxmanager.PoolConfig) { c.MaxLeases = -1 },
		"negative sync":          func(c *sandboxmanager.PoolConfig) { c.SyncInterval = -time.Second },
		"unknown visibility":     func(c *sandboxmanager.PoolConfig) { c.RuntimeVisibility = "workspace" },
		"visibility capitalised": func(c *sandboxmanager.PoolConfig) { c.RuntimeVisibility = "Public" },
	} {
		cfg := sandboxmanager.PoolConfig{MulticaURL: "http://127.0.0.1:1", Token: patPool}
		mutate(&cfg)
		if _, err := sandboxmanager.NewPool(cfg, nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestTokenPoolRegistersLocalRuntimesAndSharesThem(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	pod.registerFor(wsA)

	if got := up.modes(wsA); !slices.Equal(got, []string{"local"}) {
		t.Fatalf("runtime_mode registered as %v, want [local]", got)
	}
	patches := up.patchList()
	if len(patches) != 1 {
		t.Fatalf("want one visibility update, got %+v", patches)
	}
	if p := patches[0]; p.RuntimeID != "rt-"+wsA[:8] || p.WorkspaceID != wsA || p.Body != `{"visibility":"public"}` {
		t.Fatalf("bad visibility update: %+v", p)
	}
}

func TestPrivateVisibilityLeavesRuntimesUnshared(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up, func(c *sandboxmanager.PoolConfig) { c.RuntimeVisibility = "private" })
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	pod.registerFor(wsA)

	if got := up.modes(wsA); !slices.Equal(got, []string{"local"}) {
		t.Fatalf("runtime_mode registered as %v, want [local]", got)
	}
	if p := up.patchList(); len(p) != 0 {
		t.Fatalf("private runtimes must not be updated, got %+v", p)
	}
}

func TestStaticWorkspacesStayCloudAndAreNeverPatched(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	h.upstream.mu.Lock()
	defer h.upstream.mu.Unlock()
	for _, c := range h.upstream.calls {
		if strings.HasPrefix(c, "PATCH ") {
			t.Fatalf("a static pool sent %s", c)
		}
	}
	if len(h.upstream.registrations) == 0 {
		t.Fatal("nothing was registered")
	}
	for _, reg := range h.upstream.registrations {
		for _, rt := range reg["runtimes"].([]any) {
			if mode := rt.(map[string]any)["runtime_mode"]; mode != "cloud" {
				t.Fatalf("static workspaces must register as cloud, got %v", mode)
			}
		}
	}
}

func TestSyncLoopSyncsOnItsIntervalAndOnRequest(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up, func(c *sandboxmanager.PoolConfig) { c.SyncInterval = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	now := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() { defer close(done); h.pool.SyncLoop(ctx, now) }()
	t.Cleanup(func() { cancel(); <-done })

	// Nothing happens on its own with a long interval.
	time.Sleep(100 * time.Millisecond)
	if got := workspaceIDs(h.inventory().Workspaces); len(got) != 0 {
		t.Fatalf("synced without a trigger: %v", got)
	}
	now <- struct{}{}
	awaitReload(t, "the requested sync", func() bool { return slices.Equal(workspaceIDs(h.inventory().Workspaces), []string{wsA}) })

	up.setWorkspaces(wsA, wsB)
	now <- struct{}{}
	awaitReload(t, "the second requested sync", func() bool { return len(h.inventory().Workspaces) == 2 })
}

func TestSyncLoopTicks(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up, func(c *sandboxmanager.PoolConfig) { c.SyncInterval = 20 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.pool.SyncLoop(ctx, nil) }()
	t.Cleanup(func() { cancel(); <-done })

	awaitReload(t, "the first tick", func() bool { return len(h.inventory().Workspaces) == 1 })
	up.setWorkspaces(wsA, wsB)
	awaitReload(t, "a later tick", func() bool { return len(h.inventory().Workspaces) == 2 })
}

func TestSyncLoopReturnsAtOnceWithoutAToken(t *testing.T) {
	t.Parallel()
	up := newMultiMultica(t, &fakeWorkspace{id: wsA, pat: patA})
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil)[:1])
	done := make(chan struct{})
	go func() { defer close(done); h.pool.SyncLoop(context.Background(), nil) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SyncLoop blocked without a Token")
	}
}

func (m *syncMultica) setHook(h func(r *http.Request) (int, string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hook = h
}

func (m *syncMultica) pathCount(p string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, q := range m.paths {
		if q == p {
			n++
		}
	}
	return n
}
