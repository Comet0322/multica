package sandboxbroker_test

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxbroker"
)

func TestNoClaimWithoutIdlePodPolling(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")

	// Ten poll periods with no pod asking: the manager never goes upstream.
	time.Sleep(10 * 100 * time.Millisecond)
	if n := h.upstream.claimCount(); n != 0 {
		t.Fatalf("want 0 claims with no polling pod, got %d", n)
	}
	if h.upstream.queued() != 1 {
		t.Fatal("task left the upstream queue")
	}
}

func TestIdlePodPollingGetsExactlyOneClaimedTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	h.upstream.enqueue("t2")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	began := time.Now()
	got := pod.claimBatch()
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("handing the task to the pod took %s", elapsed)
	}
	if !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want [t1], got %v", got)
	}
	if n := h.upstream.claimCount(); n != 1 {
		t.Fatalf("want exactly 1 upstream claim, got %d", n)
	}
	if inv := h.inventory(); inv.Leased != 1 || inv.Idle != 0 {
		t.Fatalf("want the pod leased, got %+v", inv)
	}
}

func TestTwoIdlePodsTwoTasksEachGetOneWithoutDuplicates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	h.upstream.enqueue("t1")
	h.upstream.enqueue("t2")
	a := h.newPod("127.0.0.2", "pool-0")
	b := h.newPod("127.0.0.3", "pool-1")
	a.start()
	b.start()

	var wg sync.WaitGroup
	var gotA, gotB []string
	wg.Add(2)
	go func() { defer wg.Done(); gotA = a.claimBatch() }()
	go func() { defer wg.Done(); gotB = b.claimBatch() }()
	wg.Wait()

	if len(gotA) != 1 || len(gotB) != 1 || gotA[0] == gotB[0] {
		t.Fatalf("want one distinct task each, got %v and %v", gotA, gotB)
	}
	if inv := h.inventory(); inv.Leased != 2 {
		t.Fatalf("want 2 leased, got %+v", inv)
	}
}

func TestLegacySingleRuntimeClaimIsServed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	if id := pod.claimLegacy(); id != "t1" {
		t.Fatalf("want t1, got %q", id)
	}
	if inv := h.inventory(); inv.Leased != 1 {
		t.Fatalf("want leased, got %+v", inv)
	}
}

func TestLeasedPodCannotClaimASecondTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	h.upstream.enqueue("t2")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	pod.claimBatch()

	if got := pod.claimBatch(); len(got) != 0 {
		t.Fatalf("leased pod was handed %v", got)
	}
	if id := pod.claimLegacy(); id != "" {
		t.Fatalf("leased pod was handed %q on the legacy endpoint", id)
	}
	if n := h.upstream.claimCount(); n != 1 {
		t.Fatalf("want 1 upstream claim, got %d", n)
	}
	if h.upstream.queued() != 1 {
		t.Fatal("second task should still be queued upstream")
	}
}

func TestPodReportsAreForwardedAndForeignTasksRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	h.upstream.enqueue("t1")
	owner := h.newPod("127.0.0.2", "pool-0")
	other := h.newPod("127.0.0.3", "pool-1")
	owner.start()
	other.start()
	owner.claimBatch()

	if status := other.report("t1", "progress"); status != http.StatusForbidden {
		t.Fatalf("foreign report: want 403, got %d", status)
	}
	if status := owner.report("unclaimed", "start"); status != http.StatusForbidden {
		t.Fatalf("unknown task report: want 403, got %d", status)
	}
	for _, action := range []string{"start", "progress"} {
		if status := owner.report("t1", action); status != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", action, status)
		}
	}
	want := []string{"t1 start", "t1 progress"}
	if got := h.upstream.taskCallsSeen(); !slices.Equal(got, want) {
		t.Fatalf("upstream saw %v, want %v", got, want)
	}
}

func TestLeaseIsRecordedAndReleasedOnCompleteOrFail(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	h.upstream.enqueue("t1")
	h.upstream.enqueue("t2")
	a := h.newPod("127.0.0.2", "pool-0")
	b := h.newPod("127.0.0.3", "pool-1")
	a.start()
	b.start()
	a.claimBatch()
	b.claimBatch()

	byTask := map[string]sandboxbroker.LeaseInfo{}
	for _, l := range h.leases() {
		byTask[l.TaskID] = l
	}
	if l := byTask["t1"]; l.PodIP != "127.0.0.2" || l.StartedAt.IsZero() || l.EndedAt != nil {
		t.Fatalf("bad active lease for t1: %+v", l)
	}
	if l := byTask["t2"]; l.PodIP != "127.0.0.3" {
		t.Fatalf("bad lease for t2: %+v", l)
	}

	if status := a.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	if status := b.report("t2", "fail"); status != http.StatusOK {
		t.Fatalf("fail: status %d", status)
	}
	for _, l := range h.leases() {
		if l.EndedAt == nil || l.EndedAt.Before(l.StartedAt) {
			t.Fatalf("lease %s not closed: %+v", l.TaskID, l)
		}
	}
	if inv := h.inventory(); inv.Leased != 0 || inv.Idle != 2 {
		t.Fatalf("want both pods released, got %+v", inv)
	}
	// A released pod can take new work, and the old task is no longer its own.
	h.upstream.enqueue("t3")
	if got := a.claimBatch(); !slices.Equal(got, []string{"t3"}) {
		t.Fatalf("want t3 after release, got %v", got)
	}
	if status := a.report("t1", "progress"); status != http.StatusForbidden {
		t.Fatalf("report on a released task: want 403, got %d", status)
	}
}

func TestReregisterWithNewNonceFailsTheLeasedTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	pod.start()
	pod.claimBatch()

	pod.nonce = "boot-2"
	pod.start()

	msg, ok := h.upstream.failureOf("t1")
	if !ok {
		t.Fatal("leased task was not failed upstream")
	}
	if !strings.Contains(msg, "replaced") {
		t.Fatalf("failure reason should say the pod was replaced, got %q", msg)
	}
	if inv := h.inventory(); inv.Leased != 0 || inv.Idle != 1 {
		t.Fatalf("want a fresh idle pod, got %+v", inv)
	}
	leases := h.leases()
	if len(leases) != 1 || leases[0].EndedAt == nil {
		t.Fatalf("lease should be closed, got %+v", leases)
	}
	if status := pod.report("t1", "progress"); status != http.StatusForbidden {
		t.Fatalf("new instance reported on the old lease: status %d", status)
	}
}
