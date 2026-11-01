package sandboxmanager_test

// Registration, discovery and authorization defects of the design review: a
// runtime deleted upstream (H-3), who may read the workspace list and the repos
// (H-4), a listing that loses a workspace for a moment (H-5), runtime
// visibility (M-2), a registration that failed (M-3), what the inventory and
// readiness say (M-12, M-1), and a manager that stalled (L-3).

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

const runtimeGoneBody = `{"error":"runtime not found"}`

// heartbeatAnswer makes the fake answer the manager's upstream heartbeats with
// the given status and body while on is true.
func heartbeatAnswer(f *fakeMultica, on *atomic.Bool, status int, body string) {
	f.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/daemon/heartbeat" && on.Load() {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return true
		}
		return false
	})
}

// H-3: Multica answers a heartbeat with "runtime not found" once the runtime was
// deleted. The manager must forget the registration and make the stock daemon
// register again, which it does only after a 404 from its own heartbeat.
func TestRuntimeDeletedUpstreamMakesThePodRegisterAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	var gone atomic.Bool
	gone.Store(true)
	heartbeatAnswer(h.upstream, &gone, http.StatusNotFound, runtimeGoneBody)
	registered := h.upstream.registrationCount()

	pod.heartbeat() // the manager relays it upstream and learns the runtime is gone
	eventually(t, "the pod's heartbeat to be refused", func() bool { return pod.heartbeat() == http.StatusNotFound })
	status, body := pod.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	if status != http.StatusNotFound || !strings.Contains(string(body), "runtime not found") {
		t.Fatalf("the daemon needs a 404 saying the runtime is gone, got %d %s", status, body)
	}

	// The daemon registers again; the manager registers upstream again.
	gone.Store(false)
	if status, raw := pod.register(); status != http.StatusOK {
		t.Fatalf("register: status %d: %s", status, raw)
	}
	if n := h.upstream.registrationCount(); n != registered+1 {
		t.Fatalf("the workspace must register upstream again: %d -> %d", registered, n)
	}
	// The same runtime id came back; the gone marker must not outlive it, or
	// the daemon would loop on 404.
	if status := pod.heartbeat(); status != http.StatusOK {
		t.Fatalf("heartbeat after the runtime is back: status %d", status)
	}
}

func TestRuntimeDeletedUpstreamComesBackWithANewID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	var gone atomic.Bool
	gone.Store(true)
	var renamed atomic.Bool
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/api/daemon/heartbeat" && gone.Load():
			http.Error(w, runtimeGoneBody, http.StatusNotFound)
			return true
		case r.URL.Path == "/api/daemon/register" && renamed.Load():
			writeJSON(w, map[string]any{"runtimes": []map[string]any{{"id": "runtime-b", "provider": "claude", "name": "claude"}}})
			return true
		}
		return false
	})
	pod.heartbeat()
	eventually(t, "the runtime to be dropped", func() bool { return pod.heartbeat() == http.StatusNotFound })

	gone.Store(false)
	renamed.Store(true)
	pod.register()
	status, _ := pod.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-b"})
	if status != http.StatusOK {
		t.Fatalf("heartbeat for the new runtime: status %d", status)
	}
	if status := pod.heartbeat(); status != http.StatusNotFound {
		t.Fatalf("the deleted runtime stays refused, got %d", status)
	}
}

func TestAnotherNotFoundDoesNotDropTheRegistration(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	var on atomic.Bool
	on.Store(true)
	heartbeatAnswer(h.upstream, &on, http.StatusNotFound, `{"error":"not found"}`)
	registered := h.upstream.registrationCount()

	pod.heartbeat()
	time.Sleep(300 * time.Millisecond)
	if status := pod.heartbeat(); status != http.StatusOK {
		t.Fatalf("a 404 that does not say the runtime is gone must not drop it: status %d", status)
	}
	pod.register()
	if n := h.upstream.registrationCount(); n != registered {
		t.Fatalf("the workspace registered upstream again without cause: %d -> %d", registered, n)
	}
}

func TestDroppedRegistrationTellsPodsToLookAgainOnceInAWhile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withWake(time.Hour), farHeartbeat)
	clock := h.withClock()
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	sock := pod.openWake()
	var gone atomic.Bool
	gone.Store(true)
	heartbeatAnswer(h.upstream, &gone, http.StatusNotFound, runtimeGoneBody)

	pod.heartbeat()
	if got := sock.next(5 * time.Second); got != workspacesChangedEvent {
		t.Fatalf("want %s, got %q", workspacesChangedEvent, got)
	}
	// Registered again, then gone again 11s later: the relay is due, the hint
	// is not (once per 30s).
	gone.Store(false)
	pod.register()
	clock.advance(11 * time.Second)
	gone.Store(true)
	pod.heartbeat()
	time.Sleep(300 * time.Millisecond)
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("a second hint within 30s: %q", got)
	}
	// Later, the hint goes out again.
	gone.Store(false)
	pod.register()
	clock.advance(31 * time.Second)
	gone.Store(true)
	pod.heartbeat()
	if got := sock.next(5 * time.Second); got != workspacesChangedEvent {
		t.Fatalf("want a hint after 30s, got %q", got)
	}
}

// H-4: the workspace list goes to registered pods and to IPs whose supervisor
// has used the control channel, nobody else.
func TestWorkspaceListIsServedOnlyToKnownPods(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, farHeartbeat)
	clock := h.withClock()
	lists := []string{"/api/workspaces", "/api/daemon/workspaces"}
	list := func(p *simPod, path string) int {
		status, _ := p.call(http.MethodGet, path, nil)
		return status
	}

	stranger := h.newPod("127.0.0.9", "stranger")
	for _, path := range lists {
		if status := list(stranger, path); status != http.StatusForbidden {
			t.Fatalf("%s for an unknown address: status %d", path, status)
		}
	}

	// The supervisor reaches the control channel before it starts the daemon,
	// with the started event or with its first poll (which the manager answers
	// 404 for a pod it does not know).
	viaEvent := h.newPod("127.0.0.3", "via-event")
	viaEvent.event("started")
	viaPoll := h.newPod("127.0.0.4", "via-poll")
	if status, _ := viaPoll.call(http.MethodGet, "/pod/control", nil); status != http.StatusNotFound {
		t.Fatalf("poll of an unknown pod: status %d", status)
	}
	for _, p := range []*simPod{viaEvent, viaPoll} {
		for _, path := range lists {
			if status := list(p, path); status != http.StatusOK {
				t.Fatalf("%s for %s after it announced itself: status %d", path, p.name, status)
			}
		}
	}

	registered := h.newPod("127.0.0.5", "registered")
	registered.register()
	for _, path := range lists {
		if status := list(registered, path); status != http.StatusOK {
			t.Fatalf("%s for a registered pod: status %d", path, status)
		}
	}

	// The announcement lapses; registration does not.
	clock.advance(11 * time.Minute)
	for _, path := range lists {
		if status := list(viaEvent, path); status != http.StatusForbidden {
			t.Fatalf("%s after the announcement lapsed: status %d", path, status)
		}
		if status := list(registered, path); status != http.StatusOK {
			t.Fatalf("%s for a registered pod later: status %d", path, status)
		}
	}
}

// The daemon lists the workspaces before it registers anywhere; that start-up
// flow must not be refused.
func TestStartUpFlowIsNotBlockedByTheListAuthorization(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	if status := pod.event("started"); status != http.StatusOK { // the supervisor
		t.Fatalf("started: status %d", status)
	}
	if status, _ := pod.call(http.MethodPost, "/api/tokens/current/renew", map[string]any{}); status != http.StatusOK {
		t.Fatalf("renew: status %d", status)
	}
	status, raw := pod.call(http.MethodGet, "/api/workspaces", nil)
	if status != http.StatusOK || !strings.Contains(string(raw), harnessWorkspaceID) {
		t.Fatalf("list: status %d %s", status, raw)
	}
	if status, raw := pod.register(); status != http.StatusOK {
		t.Fatalf("register: status %d %s", status, raw)
	}
}

func repoStatus(p *simPod, workspaceID string) int {
	p.h.t.Helper()
	status, _ := p.call(http.MethodGet, "/api/daemon/workspaces/"+workspaceID+"/repos", nil)
	return status
}

// H-4: the repo list is read with the manager's PAT, so it goes only to a pod
// working for that workspace.
func TestRepoListIsServedOnlyToAPodWorkingForTheWorkspace(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))

	stranger := h.newPod("127.0.0.9", "stranger")
	for _, ws := range []string{wsA, wsB} {
		if status := repoStatus(stranger, ws); status != http.StatusForbidden {
			t.Fatalf("unregistered address read %s: status %d", ws, status)
		}
	}

	// A pod that registered for both workspaces and is idle may read both.
	idle := h.newPod("127.0.0.2", "pool-0")
	idle.nonce = "n1"
	ids := idle.startAll()
	for _, ws := range []string{wsA, wsB} {
		if status := repoStatus(idle, ws); status != http.StatusOK {
			t.Fatalf("idle pod, %s: status %d", ws, status)
		}
	}

	// Leased for A, it reads A and not B.
	up.enqueue(a, "ta1")
	if got := idle.claimFrom(ids); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want ta1, got %v", got)
	}
	if status := repoStatus(idle, wsA); status != http.StatusOK {
		t.Fatalf("leased pod reading its own workspace: status %d", status)
	}
	if status := repoStatus(idle, wsB); status != http.StatusForbidden {
		t.Fatalf("leased pod reading another workspace: status %d", status)
	}

	// Once the lease ends the pod is resetting and reads nothing.
	if status := idle.report("ta1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	if status := repoStatus(idle, wsA); status != http.StatusForbidden {
		t.Fatalf("resetting pod: status %d", status)
	}
}

func TestRepoListOfAPodThatRegisteredForOneWorkspaceStopsThere(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	pod := h.newPod("127.0.0.3", "pool-1")
	pod.nonce = "n2"

	// First sight: the manager orders a reset, the pod is resetting.
	pod.announce()
	pod.registerFor(wsA)
	if status := repoStatus(pod, wsA); status != http.StatusForbidden {
		t.Fatalf("resetting pod without a registration after the reset: status %d", status)
	}
	// The supervisor takes the command; the new daemon registers before its
	// report arrives.
	cmd := pod.pollControl()
	if cmd == nil {
		t.Fatal("no reset command")
	}
	pod.registerFor(wsA)
	if status := repoStatus(pod, wsA); status != http.StatusOK {
		t.Fatalf("resetting pod, workspace it registered: status %d", status)
	}
	if status := repoStatus(pod, wsB); status != http.StatusForbidden {
		t.Fatalf("resetting pod, workspace it did not register: status %d", status)
	}
	// Then the report: idle with the same workspace set.
	pod.reportControl(cmd.ID, "done")
	if status := repoStatus(pod, wsA); status != http.StatusOK {
		t.Fatalf("idle pod, workspace it registered: status %d", status)
	}
	if status := repoStatus(pod, wsB); status != http.StatusForbidden {
		t.Fatalf("idle pod, workspace it did not register: status %d", status)
	}
}

// H-5: a listing that loses a workspace once must not drain it.
func TestSyncRemovesAWorkspaceOnlyAfterTwoMissesInARow(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	h := newSyncHarness(t, up)
	served := func() []string { return workspaceIDs(h.inventory().Workspaces) }
	sync := func() sandboxmanager.ReloadResult {
		t.Helper()
		res, err := h.pool.Sync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	sync()

	up.setWorkspaces(wsA)
	if res := sync(); len(res.Removed) != 0 || !slices.Equal(served(), []string{wsA, wsB}) {
		t.Fatalf("first miss: %+v, serving %v", res, served())
	}
	if !strings.Contains(h.logs.String(), "missing from the listing") {
		t.Fatalf("the kept workspace must be logged:\n%s", h.logs.String())
	}

	// It is listed again: the count starts over.
	up.setWorkspaces(wsA, wsB)
	sync()
	up.setWorkspaces(wsA)
	if res := sync(); len(res.Removed) != 0 || !slices.Equal(served(), []string{wsA, wsB}) {
		t.Fatalf("a miss after a hit counts as the first again: %+v, serving %v", res, served())
	}
	if res := sync(); !slices.Equal(res.Removed, []string{wsB}) || !slices.Equal(served(), []string{wsA}) {
		t.Fatalf("second miss in a row: %+v, serving %v", res, served())
	}
}

// A failed listing does not count as a miss.
func TestSyncFailedListingsAreNotMisses(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	h := newSyncHarness(t, up)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	up.setWorkspaces(wsA)
	h.pool.Sync(context.Background()) // first miss
	up.setListStatus(http.StatusInternalServerError)
	for range 3 {
		h.pool.Sync(context.Background())
	}
	up.setListStatus(0)
	up.setWorkspaces(wsA, wsB)
	h.pool.Sync(context.Background())
	if got := workspaceIDs(h.inventory().Workspaces); !slices.Equal(got, []string{wsA, wsB}) {
		t.Fatalf("serving %v", got)
	}
}

// H-5: a workspace back before its drain ended is the same object: its
// registration and its running tasks carry on, and the drain stops.
func TestWorkspaceAddedBackWhileDrainingKeepsItsLeasesAndRegistration(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	all := twoWorkspaces(nil, nil)
	h := newReloadHarness(t, up, all, 250*time.Millisecond)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "n1"
	ids := pod.startAll()
	up.enqueue(a, "ta1")
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want ta1, got %v", got)
	}
	registers := up.callCount(a, "POST /api/daemon/register")
	recovers := up.callCount(a, "POST /api/daemon/runtimes/"+a.runtimeID+"/recover-orphans")

	if res, err := h.pool.Reload(context.Background(), all[1:]); err != nil || !slices.Equal(res.Removed, []string{wsA}) {
		t.Fatalf("remove A: %+v, %v", res, err)
	}
	if got := h.inventory().Draining; len(got) != 1 {
		t.Fatalf("A must be draining: %+v", got)
	}
	res, err := h.pool.Reload(context.Background(), all)
	if err != nil || !slices.Equal(res.Added, []string{wsA}) {
		t.Fatalf("add A back: %+v, %v", res, err)
	}
	inv := h.inventory()
	if len(inv.Draining) != 0 {
		t.Fatalf("A is served again, it cannot still be draining: %+v", inv.Draining)
	}
	var leased int
	for _, w := range inv.Workspaces {
		if w.WorkspaceID == wsA {
			leased = w.Leased
		}
	}
	if leased != 1 {
		t.Fatalf("A must keep its lease: %+v", inv.Workspaces)
	}

	// The drain of the first removal would have ended the lease by now.
	time.Sleep(600 * time.Millisecond)
	h.wantHeld("ta1")
	if reports := up.reportsOf(a); slices.Contains(reports, "ta1/fail") {
		t.Fatalf("the running task was failed: %v", reports)
	}

	// The daemon registers again: no new upstream registration and no orphan
	// recovery that would fail the running task.
	pod.registerFor(wsA)
	if n := up.callCount(a, "POST /api/daemon/register"); n != registers {
		t.Fatalf("A registered upstream again: %d -> %d", registers, n)
	}
	if n := up.callCount(a, "POST /api/daemon/runtimes/"+a.runtimeID+"/recover-orphans"); n != recovers {
		t.Fatalf("orphans were recovered under a running lease: %d -> %d", recovers, n)
	}
	if status := pod.report("ta1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	h.wantEnded("ta1", "pod reported complete")
}

func TestRemovedAddedAndRemovedAgainDrainsOnce(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	all := twoWorkspaces(nil, nil)
	h := newReloadHarness(t, up, all, 300*time.Millisecond)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "n1"
	ids := pod.startAll()
	up.enqueue(a, "ta1")
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want ta1, got %v", got)
	}

	ctx := context.Background()
	h.pool.Reload(ctx, all[1:])
	h.pool.Reload(ctx, all)
	h.pool.Reload(ctx, all[1:])

	awaitReload(t, "the drain to give up", func() bool { return len(h.inventory().Draining) == 0 })
	h.wantEnded("ta1", "removed")
	time.Sleep(200 * time.Millisecond) // a stale drain would act now
	fails := 0
	for _, r := range up.reportsOf(a) {
		if r == "ta1/fail" {
			fails++
		}
	}
	if fails != 1 {
		t.Fatalf("the abandoned task must be failed once, got %d", fails)
	}
}

// H-5: orphans are recovered only for a workspace that holds nothing here.
func TestOrphanRecoveryIsSkippedForAWorkspaceWithAnActiveLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	recover := "POST /api/daemon/runtimes/runtime-a/recover-orphans"

	// The first registration finds nothing running here: orphans are
	// recovered, after the registration and before any claim.
	var order []string
	var claimsAtRecovery int
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		h.upstream.mu.Lock()
		defer h.upstream.mu.Unlock()
		order = append(order, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/api/daemon/runtimes/runtime-a/recover-orphans" {
			claimsAtRecovery = h.upstream.claims
		}
		return false
	})
	leaseOne(t, h, pod, "t1")
	if n := h.upstream.callCount(recover); n != 1 {
		t.Fatalf("recover-orphans calls at first registration: %d", n)
	}
	h.upstream.mu.Lock()
	reg, rec := slices.Index(order, "POST /api/daemon/register"), slices.Index(order, recover)
	claims := claimsAtRecovery
	h.upstream.mu.Unlock()
	if reg < 0 || rec < reg {
		t.Fatalf("recover-orphans must follow the registration: %v", order)
	}
	if claims != 0 {
		t.Fatalf("a task was claimed before orphans were recovered: %d claims", claims)
	}

	// The runtime is deleted upstream while t1 runs; the registration is
	// dropped and made again with a live lease in the workspace.
	var gone atomic.Bool
	gone.Store(true)
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/daemon/heartbeat" && gone.Load() {
			http.Error(w, runtimeGoneBody, http.StatusNotFound)
			return true
		}
		return false
	})
	registered := h.upstream.registrationCount()
	pod.heartbeat()
	eventually(t, "the registration to be dropped", func() bool { return pod.heartbeat() == http.StatusNotFound })
	gone.Store(false)
	pod.register()
	if n := h.upstream.registrationCount(); n != registered+1 {
		t.Fatalf("the workspace did not register again: %d -> %d", registered, n)
	}
	if n := h.upstream.callCount(recover); n != 1 {
		t.Fatalf("orphans were recovered under a live lease (%d calls): t1 would have been failed", n)
	}
	h.wantHeld("t1")
}

// M-2: a failed visibility update is repaired by a later Sync, and left alone
// once seen right.
func TestVisibilityUpdateThatFailedIsRepairedBySync(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	up.mu.Lock()
	up.patchFails = 1
	up.mu.Unlock()
	h := newSyncHarness(t, up)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	pod.registerFor(wsA)
	if p := up.patchList(); len(p) != 1 || !strings.HasPrefix(p[0].Body, "FAILED") {
		t.Fatalf("the first update must have failed: %+v", p)
	}

	h.pool.Sync(context.Background()) // sees private, shares again
	if p := up.patchList(); len(p) != 2 || p[1].Body != `{"visibility":"public"}` {
		t.Fatalf("Sync must repair the visibility: %+v", p)
	}
	h.pool.Sync(context.Background()) // sees public, remembers
	gets := up.pathCount("GET /api/runtimes")
	h.pool.Sync(context.Background())
	h.pool.Sync(context.Background())
	if n := up.pathCount("GET /api/runtimes"); n != gets {
		t.Fatalf("a workspace seen right is not checked again: %d -> %d", gets, n)
	}
	if n := len(up.patchList()); n != 2 {
		t.Fatalf("no more updates once public: %d", n)
	}
}

func TestPrivateVisibilityMakesNoVisibilityCalls(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up, func(c *sandboxmanager.PoolConfig) { c.RuntimeVisibility = "private" })
	h.pool.Sync(context.Background())
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	pod.registerFor(wsA)
	h.pool.Sync(context.Background())
	if n := up.pathCount("GET /api/runtimes"); n != 0 {
		t.Fatalf("private runtimes are never checked: %d calls", n)
	}
	if p := up.patchList(); len(p) != 0 {
		t.Fatalf("private runtimes are never updated: %+v", p)
	}
}

// M-2: the registration request may end (the pod gave up waiting) while the
// manager is still sharing the runtime and recovering orphans; both go on.
func TestSharingAndRecoverySurviveTheRegistrationRequestEnding(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	var delivered atomic.Bool
	up.setHook(func(r *http.Request) (int, string) {
		if r.Method == http.MethodPatch {
			time.Sleep(400 * time.Millisecond)
			if r.Context().Err() == nil {
				delivered.Store(true)
			}
		}
		return 0, ""
	})
	h := newSyncHarness(t, up)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	short := &http.Client{Transport: pod.client.Transport, Timeout: 200 * time.Millisecond}
	body := `{"workspace_id":"` + wsA + `","daemon_id":"pod-pool-0","device_name":"pool-0","boot_nonce":"boot-1","runtimes":[{"name":"claude","type":"claude","version":"1","status":"online"}]}`
	req, _ := http.NewRequest(http.MethodPost, h.podAPI.URL+"/api/daemon/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := short.Do(req); err == nil { // the client gives up first
		resp.Body.Close()
	}
	eventually(t, "the visibility update to reach Multica", func() bool { return delivered.Load() })
	if n := up.pathCount("POST /api/daemon/runtimes/rt-" + wsA[:8] + "/recover-orphans"); n != 1 {
		t.Fatalf("orphan recovery calls: %d", n)
	}
}

// M-3: a workspace no pod registered for yet is announced again by Sync, at
// most every 30 seconds, and only while it is missing.
func TestSyncHintsPodsToRegisterAWorkspaceThatHasNoRegistration(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	h := newSyncHarness(t, up, withWake(time.Hour), farHeartbeat)
	clock := h.withClock()
	sync := func() {
		t.Helper()
		if _, err := h.pool.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	sync() // both workspaces are new
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	pod.registerFor(wsA) // B's registration never happened
	sock := pod.openWake()

	sync() // within 30s of the hint that announced the workspaces
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("too early for another hint: %q", got)
	}
	clock.advance(31 * time.Second)
	sync()
	if got := sock.next(5 * time.Second); got != workspacesChangedEvent {
		t.Fatalf("want %s for the unregistered workspace, got %q", workspacesChangedEvent, got)
	}
	clock.advance(time.Second)
	sync()
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("once per 30s: %q", got)
	}
	clock.advance(31 * time.Second)
	sync()
	if got := sock.next(5 * time.Second); got != workspacesChangedEvent {
		t.Fatalf("it repeats while the workspace stays unregistered, got %q", got)
	}

	pod.registerFor(wsB)
	clock.advance(31 * time.Second)
	sync()
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("nothing is missing any more: %q", got)
	}
}

func TestSyncSendsNoHintWithoutAPod(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up, withWake(time.Hour))
	clock := h.withClock()
	h.pool.Sync(context.Background())
	// No pod holds a socket and none is registered: nothing to tell and
	// nothing breaks.
	clock.advance(time.Minute)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// M-12: what the inventory tells an operator.
func TestInventoryReportsTheLatestSync(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up)
	if inv := h.inventory(); inv.LastSync != nil {
		t.Fatalf("no sync has run: %+v", inv.LastSync)
	}
	h.pool.Sync(context.Background())
	if ls := h.inventory().LastSync; ls == nil || !ls.OK || ls.Error != "" || ls.At.IsZero() {
		t.Fatalf("after a good sync: %+v", ls)
	}
	up.setListStatus(http.StatusInternalServerError)
	h.pool.Sync(context.Background())
	ls := h.inventory().LastSync
	if ls == nil || ls.OK || ls.Error == "" {
		t.Fatalf("after a failed listing: %+v", ls)
	}
	if strings.Contains(ls.Error, "boom") {
		t.Fatalf("the response body leaked into the inventory: %q", ls.Error)
	}
	up.setListStatus(0)
	up.setWorkspaces()
	h.pool.Sync(context.Background())
	if ls := h.inventory().LastSync; ls == nil || ls.OK || !strings.Contains(ls.Error, "no workspace") {
		t.Fatalf("an empty listing is a failure: %+v", ls)
	}
}

func getStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

// M-1: readiness follows the latest successful listing; liveness does not.
func TestReadyzFollowsTheLatestSuccessfulSync(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up, func(c *sandboxmanager.PoolConfig) { c.SyncInterval = 10 * time.Second })
	clock := h.withClock()
	ready := func() int { s, _ := getStatus(t, h.podAPI.URL+"/readyz"); return s }
	alive := func() int { s, _ := getStatus(t, h.podAPI.URL+"/healthz"); return s }

	if s := ready(); s != http.StatusServiceUnavailable {
		t.Fatalf("before the first sync: readyz %d", s)
	}
	if _, why := getStatus(t, h.podAPI.URL+"/readyz"); why == "" {
		t.Fatal("the answer must say why")
	}
	h.pool.Sync(context.Background())
	if s := ready(); s != http.StatusOK {
		t.Fatalf("after a sync: readyz %d", s)
	}

	clock.advance(25 * time.Second)
	if s := ready(); s != http.StatusOK {
		t.Fatalf("within three intervals: readyz %d", s)
	}
	// An empty listing is a failed sync but does not make the manager unready
	// before the window ends.
	up.setWorkspaces()
	h.pool.Sync(context.Background())
	if s := ready(); s != http.StatusOK {
		t.Fatalf("one empty listing: readyz %d", s)
	}
	clock.advance(6 * time.Second)
	if s := ready(); s != http.StatusServiceUnavailable {
		t.Fatalf("no success for three intervals: readyz %d", s)
	}
	if s := alive(); s != http.StatusOK {
		t.Fatalf("liveness must not follow readiness: healthz %d", s)
	}
	up.setWorkspaces(wsA)
	h.pool.Sync(context.Background())
	if s := ready(); s != http.StatusOK {
		t.Fatalf("after recovering: readyz %d", s)
	}
}

func TestReadyzIsAlwaysOKWithoutDiscovery(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	if s, _ := getStatus(t, h.podAPI.URL+"/readyz"); s != http.StatusOK {
		t.Fatalf("static workspaces: readyz %d", s)
	}
}

func TestInventoryReportsLeaseAgeAndUpstreamHealth(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, farHeartbeat)
	clock := h.withClock()
	if age := h.inventory().OldestLeaseAgeSeconds; age != 0 {
		t.Fatalf("no lease: age %d", age)
	}
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")
	clock.advance(90 * time.Second)
	if age := h.inventory().OldestLeaseAgeSeconds; age < 90 || age > 100 {
		t.Fatalf("oldest lease age %d, want about 90", age)
	}

	pod.heartbeat()
	eventually(t, "the upstream heartbeat to be recorded", func() bool {
		ws := h.inventory().Workspaces
		return len(ws) == 1 && ws[0].Upstream != nil && ws[0].Upstream.OK && ws[0].Upstream.Op == "heartbeat"
	})

	pod.report("t1", "complete")
	if age := h.inventory().OldestLeaseAgeSeconds; age != 0 {
		t.Fatalf("lease over: age %d", age)
	}
}

func TestInventoryReportsAFailedClaim(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/daemon/tasks/claim" {
			http.Error(w, "database is down", http.StatusInternalServerError)
			return true
		}
		return false
	})
	pod.call(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{"daemon_id": "pod-pool-0", "runtime_ids": []string{"runtime-a"}, "max_tasks": 1})
	ws := h.inventory().Workspaces
	if len(ws) != 1 || ws[0].Upstream == nil || ws[0].Upstream.OK || ws[0].Upstream.Op != "claim" || ws[0].Upstream.Error == "" {
		t.Fatalf("workspace upstream = %+v", ws)
	}
	if strings.Contains(ws[0].Upstream.Error, "database") {
		t.Fatalf("the response body leaked into the inventory: %q", ws[0].Upstream.Error)
	}
}

func TestPendingFailReportsShowInTheInventory(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	if n := h.inventory().PendingFailReports; n != 0 {
		t.Fatalf("pending %d", n)
	}
}

// M-12: a reload that changes nothing is not worth an Info line.
func TestUnchangedSyncLogsNoReload(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up)
	const msg = "workspace configuration reloaded"
	h.pool.Sync(context.Background())
	if n := strings.Count(h.logs.String(), msg); n != 1 {
		t.Fatalf("the first sync adds a workspace and says so: %d lines\n%s", n, h.logs.String())
	}
	for range 3 {
		h.pool.Sync(context.Background())
	}
	if n := strings.Count(h.logs.String(), msg); n != 1 {
		t.Fatalf("unchanged syncs must stay quiet at Info: %d lines", n)
	}
	up.setWorkspaces(wsA, wsB)
	h.pool.Sync(context.Background())
	if n := strings.Count(h.logs.String(), msg); n != 2 {
		t.Fatalf("a real change is logged: %d lines", n)
	}
}

// L-3: a manager that was itself stalled must not take every pod for silent.
func TestManagerStallDoesNotMakeEveryPodLost(t *testing.T) {
	t.Parallel()
	t.Run("a stall gives every pod a grace period", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, 1)
		clock := h.withClock()
		pod := h.newPod("127.0.0.2", "pool-0")
		pod.start()
		clock.advance(60 * time.Second) // longer than the heartbeat timeout
		h.pool.SetLastMarkLost(time.Now().Add(-60 * time.Second))
		h.pool.Touch()
		if inv := h.inventory(); inv.Lost != 0 || inv.Idle != 1 {
			t.Fatalf("the pods' silence is the manager's own: %+v", inv)
		}
		if !strings.Contains(h.logs.String(), "manager stalled") {
			t.Fatalf("the stall must be logged:\n%s", h.logs.String())
		}
	})
	t.Run("without a stall a silent pod is lost", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, 1)
		clock := h.withClock()
		pod := h.newPod("127.0.0.2", "pool-0")
		pod.start()
		clock.advance(60 * time.Second)
		h.pool.Touch()
		if inv := h.inventory(); inv.Lost != 1 {
			t.Fatalf("a pod silent for 60s while the manager ran must be lost: %+v", inv)
		}
	})
}
