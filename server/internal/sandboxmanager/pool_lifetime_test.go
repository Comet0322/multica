package sandboxmanager_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

// lifetimes sets the shared pod's idle timeout, maximum lifetime and drain
// grace.
func lifetimes(idle, life, grace time.Duration) func(*sandboxmanager.PoolConfig) {
	return func(c *sandboxmanager.PoolConfig) {
		c.PodIdleTimeout = idle
		c.PodMaxLifetime = life
		c.PodDrainGrace = grace
	}
}

// shared combines shared placement with lifetime settings.
func shared(concurrency int, opts ...func(*sandboxmanager.PoolConfig)) func(*sandboxmanager.PoolConfig) {
	return func(c *sandboxmanager.PoolConfig) {
		sharedPods(concurrency, 0)(c)
		for _, opt := range opts {
			opt(c)
		}
	}
}

func TestSharedPodStaysBoundBetweenTasksWithoutReset(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shared(3, lifetimes(time.Hour, 2*time.Hour, time.Hour)))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-1", "")
	a := h.newPod("127.0.0.2", "pool-0")
	a.start()
	a.claimBatch()
	a.report("t1", "complete")

	if inv := h.inventory(); inv.Leased != 1 || inv.Resetting != 0 {
		t.Fatalf("a shared pod between tasks must stay bound, not reset: %+v", inv)
	}
	if cmd := a.pollControl(); cmd != nil {
		t.Fatalf("no reset between tasks of one lease: %+v", cmd)
	}
	if got := a.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("the follow-up task should reuse the bound pod: %v", got)
	}
}

func TestIdleSharedPodIsReleasedAndResetAfterTheIdleTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shared(3, lifetimes(150*time.Millisecond, time.Hour, time.Hour)))
	h.upstream.enqueueIn("t1", "issue-1", "")
	a := h.newPod("127.0.0.2", "pool-0")
	a.nonce = "boot-1"
	a.start()
	a.claimBatch()
	a.report("t1", "complete")

	time.Sleep(300 * time.Millisecond)
	if inv := h.inventory(); inv.Resetting != 1 || inv.Leased != 0 {
		t.Fatalf("an idle shared pod should be resetting: %+v", inv)
	}
	h.upstream.enqueueIn("t2", "issue-1", "")
	if got := a.claimBatch(); len(got) != 0 {
		t.Fatalf("an expired pod took a new task: %v", got)
	}
	a.completeReset("boot-2")
	// The binding is gone: the same issue's next task binds a pod anew.
	if got := a.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("the next task of the key should bind a pod again: %v", got)
	}
	if got := leasePods(h); got["t2"] != "127.0.0.2" {
		t.Fatalf("unexpected placement: %v", got)
	}
}

func TestActivityKeepsASharedPodFromGoingIdle(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shared(3, lifetimes(400*time.Millisecond, time.Hour, time.Hour)))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-1", "")
	a := h.newPod("127.0.0.2", "pool-0")
	a.start()
	a.claimBatch()
	time.Sleep(600 * time.Millisecond) // a running task is not idleness
	if inv := h.inventory(); inv.Leased != 1 || inv.Resetting != 0 {
		t.Fatalf("a pod with a running task must not time out as idle: %+v", inv)
	}
	a.report("t1", "complete")
	time.Sleep(250 * time.Millisecond) // the idle clock starts at the last end
	if got := a.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("the pod should still be bound: %v", got)
	}
}

func TestSharedPodPastItsLifetimeTakesNoNewTasksAndResetsWhenDrained(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, shared(3, lifetimes(time.Hour, 200*time.Millisecond, time.Hour)))
	h.upstream.enqueueIn("t1", "issue-1", "")
	a, b := h.newPod("127.0.0.2", "pool-0"), h.newPod("127.0.0.3", "pool-1")
	a.nonce = "boot-1"
	a.start()
	b.start()
	a.claimBatch()

	time.Sleep(350 * time.Millisecond)
	h.upstream.enqueueIn("t2", "issue-1", "")
	if got := a.claimBatch(); len(got) != 0 {
		t.Fatalf("a pod past its lifetime took a new task: %v", got)
	}
	if got := b.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("the new task should bind another pod: %v", got)
	}
	// The running task is not interrupted within the grace period.
	if _, failed := h.upstream.failureOf("t1"); failed {
		t.Fatal("running task was failed inside the grace period")
	}
	if inv := h.inventory(); inv.Leased != 2 {
		t.Fatalf("the draining pod keeps its running task: %+v", inv)
	}
	a.report("t1", "complete")
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("the drained pod should reset: %+v", inv)
	}
	a.completeReset("boot-2")
	if inv := h.inventory(); inv.Idle != 1 {
		t.Fatalf("a reset pod rejoins the pool: %+v", inv)
	}
}

func TestSharedPodStillRunningAfterTheGraceIsReleasedAndItsTasksFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shared(3, lifetimes(time.Hour, 100*time.Millisecond, 150*time.Millisecond)))
	h.upstream.enqueueIn("t1", "issue-1", "")
	a := h.newPod("127.0.0.2", "pool-0")
	a.start()
	a.claimBatch()

	time.Sleep(400 * time.Millisecond)
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("the pod should be released after the grace: %+v", inv)
	}
	a.heartbeat()
	why, ok := h.upstream.failureOf("t1")
	if !ok || !strings.Contains(why, "lifetime") {
		t.Fatalf("the unfinished task should be failed with the reason, got %q (%v)", why, ok)
	}
	if got := leasePods(h); len(got) != 0 {
		t.Fatalf("leases should be closed: %v", got)
	}
}

func TestIdleBoundPodIsReleasedForAWaitingTaskOfAnotherKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shared(3, lifetimes(time.Hour, 2*time.Hour, time.Hour)))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-2", "")
	a := h.newPod("127.0.0.2", "pool-0")
	a.nonce = "boot-1"
	a.start()
	a.claimBatch()
	a.report("t1", "complete")

	// The only pod is bound to issue-1 but has nothing running; issue-2 must
	// not starve until the idle timeout.
	a.claimBatch()
	a.claimBatch()
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("the idle bound pod should be released for the waiting task: %+v", inv)
	}
	a.completeReset("boot-2")
	if got := a.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("the waiting task should get the released pod: %v", got)
	}
}

func TestTaskPlacementIgnoresSharedLifetimes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, lifetimes(time.Millisecond, time.Millisecond, time.Millisecond))
	h.upstream.enqueue("t1")
	a := h.newPod("127.0.0.2", "pool-0")
	a.start()
	a.claimBatch()
	time.Sleep(50 * time.Millisecond)
	if inv := h.inventory(); inv.Leased != 1 {
		t.Fatalf("one-task pods have no lifetime: %+v", inv)
	}
	if status := a.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: %d", status)
	}
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("a one-task pod resets as soon as its task ends: %+v", inv)
	}
}

func TestNegativeLifetimeConfigIsRejected(t *testing.T) {
	t.Parallel()
	for name, opt := range map[string]func(*sandboxmanager.PoolConfig){
		"idle":  func(c *sandboxmanager.PoolConfig) { c.PodIdleTimeout = -time.Second },
		"life":  func(c *sandboxmanager.PoolConfig) { c.PodMaxLifetime = -time.Second },
		"grace": func(c *sandboxmanager.PoolConfig) { c.PodDrainGrace = -time.Second },
	} {
		cfg := sandboxmanager.PoolConfig{
			MulticaURL: "http://127.0.0.1:1", MulticaToken: harnessPAT, WorkspaceID: harnessWorkspaceID,
		}
		opt(&cfg)
		if _, err := sandboxmanager.NewPool(cfg, nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
