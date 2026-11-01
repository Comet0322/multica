package sandboxmanager_test

import (
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

func pruneTimings(c *sandboxmanager.PoolConfig) {
	c.HeartbeatTimeout = 200 * time.Millisecond
	c.LostRetention = 600 * time.Millisecond
}

func TestLostPodStaysListedUntilRetentionThenIsPruned(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, pruneTimings)
	h.newPod("127.0.0.2", "pool-0").start()

	time.Sleep(300 * time.Millisecond)
	inv := h.inventory()
	if inv.Lost != 1 || len(inv.Pods) != 1 || inv.PrunedTotal != 0 {
		t.Fatalf("a freshly lost pod must stay listed, got %+v", inv)
	}

	time.Sleep(800 * time.Millisecond)
	inv = h.inventory()
	if inv.Lost != 0 || len(inv.Pods) != 0 || inv.PrunedTotal != 1 {
		t.Fatalf("want the lost pod pruned and counted, got %+v", inv)
	}
}

func TestPodRegisteringAfterPruneIsFirstSeenAndReset(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, pruneTimings)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	time.Sleep(300 * time.Millisecond)
	h.inventory() // marks it lost
	time.Sleep(800 * time.Millisecond)

	pod.start() // same IP and boot nonce, after the retention ran out

	inv := h.inventory()
	if len(inv.Pods) != 1 || inv.Pods[0].State != sandboxmanager.PodResetting || inv.Idle != 0 {
		t.Fatalf("a pruned pod registering again must be reset first, got %+v", inv)
	}
	if inv.PrunedTotal != 1 {
		t.Fatalf("pruned_total = %d, want 1", inv.PrunedTotal)
	}
}

func TestNegativeLostRetentionIsRejected(t *testing.T) {
	t.Parallel()
	_, err := sandboxmanager.NewPool(sandboxmanager.PoolConfig{
		MulticaURL: "http://multica:8080", MulticaToken: harnessPAT, WorkspaceID: harnessWorkspaceID,
		LostRetention: -time.Second,
	}, nil)
	if err == nil {
		t.Fatal("negative lost retention must be rejected")
	}
}

func TestDefaultLostRetentionKeepsALostPodListed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, func(c *sandboxmanager.PoolConfig) { c.HeartbeatTimeout = 200 * time.Millisecond })
	h.newPod("127.0.0.2", "pool-0").start()
	time.Sleep(300 * time.Millisecond)
	h.inventory()
	time.Sleep(500 * time.Millisecond)
	if inv := h.inventory(); inv.Lost != 1 || inv.PrunedTotal != 0 {
		t.Fatalf("default retention must be minutes, got %+v", inv)
	}
}
