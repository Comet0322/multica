package sandboxmanager_test

import (
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

// enqueueIn adds a task with the given issue and chat session IDs.
func (f *fakeMultica) enqueueIn(id, issueID, chatID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID, "issue_id": issueID, "chat_session_id": chatID,
		"initiator_email": "alice@example.com",
	})
}

func sharedPods(concurrency int, wait time.Duration) func(*sandboxmanager.PoolConfig) {
	return func(c *sandboxmanager.PoolConfig) {
		c.Placement = sandboxmanager.PlacementIssue
		c.PodConcurrency = concurrency
		c.PlacementWait = wait
	}
}

func leasePods(h *harness) map[string]string {
	out := map[string]string{}
	for _, l := range h.leases() {
		if l.EndedAt == nil {
			out[l.TaskID] = l.PodIP
		}
	}
	return out
}

func TestSameIssueTasksRunInParallelOnTheBoundPod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, sharedPods(3, 0))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-1", "")
	a, b := h.newPod("127.0.0.2", "pool-0"), h.newPod("127.0.0.3", "pool-1")
	a.start()
	b.start()

	if got := a.claimBatch(); len(got) != 1 || got[0] != "t1" {
		t.Fatalf("first claim: %v", got)
	}
	if got := a.claimBatch(); len(got) != 1 || got[0] != "t2" {
		t.Fatalf("bound pod should take the second task of its issue: %v", got)
	}
	got := leasePods(h)
	if got["t1"] != "127.0.0.2" || got["t2"] != "127.0.0.2" {
		t.Fatalf("both tasks should lease the first pod: %v", got)
	}
	if inv := h.inventory(); inv.Idle != 1 || inv.Leased != 1 {
		t.Fatalf("second pod should stay idle: %+v", inv)
	}
}

func TestChatTasksWithoutIssueShareByConversation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, sharedPods(3, 0))
	h.upstream.enqueueIn("t1", "", "chat-1")
	h.upstream.enqueueIn("t2", "", "chat-1")
	h.upstream.enqueueIn("t3", "", "chat-2")
	a, b := h.newPod("127.0.0.2", "pool-0"), h.newPod("127.0.0.3", "pool-1")
	a.start()
	b.start()

	a.claimBatch()
	a.claimBatch()
	// t3 belongs to another conversation: the bound pod cannot take it, so the
	// idle pod ends up with it.
	if got := a.claimBatch(); len(got) != 0 {
		t.Fatalf("bound pod was handed another conversation's task: %v", got)
	}
	if got := b.claimBatch(); len(got) != 1 || got[0] != "t3" {
		t.Fatalf("idle pod should get the other conversation's task: %v", got)
	}
	got := leasePods(h)
	if got["t1"] != "127.0.0.2" || got["t2"] != "127.0.0.2" || got["t3"] != "127.0.0.3" {
		t.Fatalf("unexpected placement: %v", got)
	}
}

func TestFullBoundPodHandsTheTaskToAnotherIdlePod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, sharedPods(2, 0))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-1", "")
	h.upstream.enqueueIn("t3", "issue-1", "")
	a, b := h.newPod("127.0.0.2", "pool-0"), h.newPod("127.0.0.3", "pool-1")
	a.start()
	b.start()
	a.claimBatch()
	a.claimBatch()

	if got := a.claimBatch(); len(got) != 0 {
		t.Fatalf("full pod claimed another task: %v", got)
	}
	if got := b.claimBatch(); len(got) != 1 || got[0] != "t3" {
		t.Fatalf("idle pod should take the overflow: %v", got)
	}
	if h.upstream.queued() != 0 {
		t.Fatal("overflow task should have been claimed")
	}
}

func TestTaskForAnotherKeyWaitsThenGoesToAnIdlePodThatAppears(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, sharedPods(3, 0))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-2", "")
	a := h.newPod("127.0.0.2", "pool-0")
	a.start()
	a.claimBatch()

	if got := a.claimBatch(); len(got) != 0 {
		t.Fatalf("pod bound to issue-1 was handed an issue-2 task: %v", got)
	}
	if h.upstream.queued() != 0 {
		t.Fatal("the issue-2 task should be held by the manager")
	}
	b := h.newPod("127.0.0.3", "pool-1")
	b.start()
	if got := b.claimBatch(); len(got) != 1 || got[0] != "t2" {
		t.Fatalf("new idle pod should get the waiting task: %v", got)
	}
	if got := leasePods(h); got["t2"] != "127.0.0.3" {
		t.Fatalf("unexpected placement: %v", got)
	}
}

func TestTaskWaitingPastTheLimitIsFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, sharedPods(3, 100*time.Millisecond))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-2", "")
	a := h.newPod("127.0.0.2", "pool-0")
	a.start()
	a.claimBatch()
	a.claimBatch()

	time.Sleep(200 * time.Millisecond)
	a.claimBatch()
	why, ok := h.upstream.failureOf("t2")
	if !ok || !strings.Contains(why, "in time") {
		t.Fatalf("waiting task should be failed after the limit, got %q (%v)", why, ok)
	}
}

func TestSharedPodResetsOnlyAfterItsLastTaskEnds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, sharedPods(3, 0), lifetimes(150*time.Millisecond, time.Hour, time.Hour))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-1", "")
	a := h.newPod("127.0.0.2", "pool-0")
	a.start()
	a.claimBatch()
	a.claimBatch()

	if status := a.report("t1", "complete"); status != 200 {
		t.Fatalf("complete t1: %d", status)
	}
	if inv := h.inventory(); inv.Leased != 1 || inv.Resetting != 0 {
		t.Fatalf("pod with a running task must not reset: %+v", inv)
	}
	if status := a.report("t2", "complete"); status != 200 {
		t.Fatalf("complete t2: %d", status)
	}
	// The shared pod stays bound for a follow-up task until it idles out.
	if inv := h.inventory(); inv.Leased != 1 || inv.Resetting != 0 {
		t.Fatalf("pod should stay bound right after its last task: %+v", inv)
	}
	time.Sleep(300 * time.Millisecond)
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("pod should reset after its last task and the idle timeout: %+v", inv)
	}
}

func TestInvalidPlacementConfigIsRejected(t *testing.T) {
	t.Parallel()
	for name, opt := range map[string]func(*sandboxmanager.PoolConfig){
		"unknown mode":     func(c *sandboxmanager.PoolConfig) { c.Placement = "agent" },
		"negative size":    func(c *sandboxmanager.PoolConfig) { c.PodConcurrency = -1 },
		"negative waiting": func(c *sandboxmanager.PoolConfig) { c.PlacementWait = -time.Second },
	} {
		cfg := sandboxmanager.PoolConfig{
			MulticaURL: "http://127.0.0.1:1", MulticaTicket: harnessPAT, WorkspaceID: harnessWorkspaceID,
		}
		opt(&cfg)
		if _, err := sandboxmanager.NewPool(cfg, nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
