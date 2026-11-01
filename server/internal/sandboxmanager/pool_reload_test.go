package sandboxmanager_test

import (
	"context"
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

func newReloadHarness(t *testing.T, up *multiMultica, workspaces []sandboxmanager.WorkspaceConfig, drainTimeout time.Duration) *harness {
	t.Helper()
	logs, audit := &syncBuffer{}, &syncBuffer{}
	pool, err := sandboxmanager.NewPool(sandboxmanager.PoolConfig{
		MulticaURL: up.URL, Name: "Shared Pool", Workspaces: workspaces,
		UpstreamBackoff: time.Minute, AuditLog: audit,
		DrainTimeout: drainTimeout, DrainPoll: 5 * time.Millisecond,
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

func awaitReload(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func workspaceIDs(list []sandboxmanager.WorkspaceInfo) []string {
	ids := []string{}
	for _, w := range list {
		ids = append(ids, w.WorkspaceID)
	}
	return ids
}

func TestReloadAddedWorkspaceIsClaimedFromAtOnce(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	all := twoWorkspaces(nil, nil)
	h := newReloadHarness(t, up, all[:1], time.Minute)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	pod.startAll()
	up.enqueue(b, "tb")

	res, err := h.pool.Reload(context.Background(), all)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Added, []string{wsB}) || len(res.Removed) != 0 {
		t.Fatalf("unexpected result %+v", res)
	}
	if got := workspaceIDs(h.inventory().Workspaces); !slices.Equal(got, []string{wsA, wsB}) {
		t.Fatalf("inventory lists %v", got)
	}
	ids := pod.startAll()
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"tb"}) {
		t.Fatalf("the added workspace was not claimed from: %v", got)
	}
	if reports := up.reportsOf(a); len(reports) != 0 {
		t.Fatalf("A saw reports: %v", reports)
	}
}

func TestReloadRemovedWorkspaceDrainsWithoutDisturbingOthers(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	all := twoWorkspaces(nil, nil)
	h := newReloadHarness(t, up, all, time.Minute)
	p1, p2 := h.newPod("127.0.0.2", "pool-0"), h.newPod("127.0.0.3", "pool-1")
	p1.nonce, p2.nonce = "n1", "n2"
	ids1, ids2 := p1.startAll(), p2.startAll()
	up.enqueue(a, "ta1")
	up.enqueue(b, "tb1")
	if got := p1.claimFrom(ids1); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want [ta1], got %v", got)
	}

	res, err := h.pool.Reload(context.Background(), all[1:])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Removed, []string{wsA}) {
		t.Fatalf("unexpected result %+v", res)
	}
	inv := h.inventory()
	if got := workspaceIDs(inv.Workspaces); !slices.Equal(got, []string{wsB}) {
		t.Fatalf("serving %v, want only B", got)
	}
	if len(inv.Draining) != 1 || inv.Draining[0].WorkspaceID != wsA || inv.Draining[0].Leased != 1 {
		t.Fatalf("A must be draining with its lease: %+v", inv.Draining)
	}

	// A no longer yields tasks, B is unaffected, and A's lease keeps working.
	up.enqueue(a, "ta2")
	claims := up.claimCount(a)
	if got := p2.claimFrom(ids2); !slices.Equal(got, []string{"tb1"}) {
		t.Fatalf("B's task must still run: %v", got)
	}
	if n := up.claimCount(a); n != claims {
		t.Fatalf("removed workspace was polled: %d -> %d", claims, n)
	}
	if status := p1.report("ta1", "start"); status != http.StatusOK {
		t.Fatalf("draining lease can no longer report: status %d", status)
	}
	if got := h.inventory().Draining; len(got) != 1 {
		t.Fatalf("A disconnected while its task still ran: %+v", got)
	}

	if status := p1.report("ta1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	awaitReload(t, "A to finish draining", func() bool { return len(h.inventory().Draining) == 0 })
	if reports := up.reportsOf(a); !slices.Contains(reports, "ta1/complete") {
		t.Fatalf("A's completion was not forwarded: %v", reports)
	}
	if q := up.queuedOf(a); q != 1 {
		t.Fatalf("A's second task must stay queued in Multica, %d queued", q)
	}
}

func TestReloadDrainTimeoutEndsLeasesAndFailsTasks(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	all := twoWorkspaces(nil, nil)
	h := newReloadHarness(t, up, all, 50*time.Millisecond)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "n1"
	ids := pod.startAll()
	up.enqueue(a, "ta1")
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want [ta1], got %v", got)
	}

	if _, err := h.pool.Reload(context.Background(), all[1:]); err != nil {
		t.Fatal(err)
	}

	awaitReload(t, "the drain to give up", func() bool { return len(h.inventory().Draining) == 0 })
	if reports := up.reportsOf(a); !slices.Contains(reports, "ta1/fail") {
		t.Fatalf("the abandoned task was not failed upstream: %v", reports)
	}
	var found bool
	for _, l := range h.leases() {
		if l.TaskID == "ta1" {
			found = true
			if l.EndedAt == nil || !strings.Contains(l.EndReason, "removed") {
				t.Fatalf("lease not ended for the removal: %+v", l)
			}
		}
	}
	if !found {
		t.Fatal("lease record missing")
	}
}

func TestReloadRefusesInvalidConfigAndKeepsTheCurrentOne(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	h := newReloadHarness(t, up, twoWorkspaces(nil, nil), time.Minute)
	ctx := context.Background()

	bad := map[string][]sandboxmanager.WorkspaceConfig{
		"empty":      {},
		"duplicate":  {{WorkspaceID: wsA, Ticket: patA}, {WorkspaceID: wsA, Ticket: patB}},
		"missing":    {{WorkspaceID: wsB, Ticket: patB}, {WorkspaceID: wsA}},
		"bad ID":     {{WorkspaceID: "nope", Ticket: patA}},
		"placement":  {{WorkspaceID: wsA, Ticket: patA, Placement: "pod"}},
		"bad quota":  {{WorkspaceID: wsA, Ticket: patA, MaxLeases: -1}},
		"drops both": nil,
	}
	for name, list := range bad {
		if _, err := h.pool.Reload(ctx, list); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	inv := h.inventory()
	if got := workspaceIDs(inv.Workspaces); !slices.Equal(got, []string{wsA, wsB}) || len(inv.Draining) != 0 {
		t.Fatalf("a refused reload changed the configuration: %+v", inv)
	}
}

func TestReloadAppliesLeaseCapAndFlagsChangesThatNeedARestart(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	h := newReloadHarness(t, up, twoWorkspaces(nil, nil), time.Minute)

	next := twoWorkspaces(func(c *sandboxmanager.WorkspaceConfig) { c.MaxLeases = 3 }, func(c *sandboxmanager.WorkspaceConfig) { c.Ticket = "rotated" })
	res, err := h.pool.Reload(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Updated, []string{wsA}) || !slices.Equal(res.Unapplied, []string{wsB}) {
		t.Fatalf("unexpected result %+v", res)
	}
	for _, w := range h.inventory().Workspaces {
		if w.WorkspaceID == wsA && w.MaxLeases != 3 {
			t.Fatalf("lease cap not applied: %+v", w)
		}
	}
}

func TestReloadWhileClaimingIsRaceFree(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	all := twoWorkspaces(nil, nil)
	h := newReloadHarness(t, up, all, time.Minute)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "n1"
	ids := pod.startAll()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			list := all
			if i%2 == 1 {
				list = all[:1]
			}
			if _, err := h.pool.Reload(context.Background(), list); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			pod.claimFrom(ids)
			h.inventory()
		}
	}()
	wg.Wait()
}
