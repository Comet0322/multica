package sandboxmanager_test

import (
	"testing"
	"time"
)

// The manager keeps its leases in memory. A task that was running when the
// manager restarted is gone from the registry, its pod is reset, and nothing
// would ever finish it: Multica kept it "running" until its own timeout (hours).
// The first time the restarted manager registers the pool's runtime it asks
// Multica to fail what is still running on it (the same call a restarted
// daemon makes), which also lets Multica retry the task.
func TestFirstRegistrationAsksMulticaToRecoverOrphanedTasks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	const call = "POST /api/daemon/runtimes/runtime-a/recover-orphans"

	h.newPod("127.0.0.2", "pool-0").start()
	deadline := time.Now().Add(2 * time.Second)
	for h.upstream.callsTo(call) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.upstream.callsTo(call); got != 1 {
		t.Fatalf("recover-orphans calls after the first pod registered: want 1, got %d", got)
	}

	// Pods that join later, or register again, must not fail tasks that are
	// running on their neighbours.
	h.newPod("127.0.0.3", "pool-1").start()
	time.Sleep(200 * time.Millisecond)
	if got := h.upstream.callsTo(call); got != 1 {
		t.Fatalf("recover-orphans calls after a second pod registered: want 1, got %d", got)
	}
}
