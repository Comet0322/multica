package sandboxmanager_test

import (
	"net/http"
	"slices"
	"testing"
)

// The new daemon can register before the supervisor's "done" report arrives:
// the supervisor starts the daemon first and reports afterwards. That
// registration must not be lost.
func TestRegistrationBeforeDoneCompletesTheResetWhenDoneArrives(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseAndComplete(t, h, pod, "t1")

	cmd := pod.pollControl() // the supervisor has the command; the old daemon is stopped
	pod.start()              // the new daemon registers before the report
	if inv := h.inventory(); inv.Resetting != 1 || inv.Idle != 0 {
		t.Fatalf("pod left the reset before the acknowledgement: %+v", inv)
	}
	h.upstream.enqueue("t2")
	if got := pod.claimBatch(); len(got) != 0 {
		t.Fatalf("pod was handed %v before the acknowledgement", got)
	}

	if code := pod.reportControl(cmd.ID, "done"); code != http.StatusOK {
		t.Fatalf("report done: status %d", code)
	}
	if inv := h.inventory(); inv.Idle != 1 || inv.Resetting != 0 {
		t.Fatalf("want idle once done arrives, got %+v", inv)
	}
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("want t2, got %v", got)
	}
}

// Before the supervisor has even taken the command, a registration can only be
// the old daemon's; it must not complete a later acknowledgement.
func TestRegistrationBeforeTheCommandWasTakenDoesNotCountAsTheNewDaemon(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseAndComplete(t, h, pod, "t1")

	pod.start() // the old daemon, command not yet delivered
	cmd := pod.pollControl()
	if code := pod.reportControl(cmd.ID, "done"); code != http.StatusOK {
		t.Fatalf("report done: status %d", code)
	}
	if inv := h.inventory(); inv.Resetting != 1 || inv.Idle != 0 {
		t.Fatalf("an old-daemon registration completed the reset: %+v", inv)
	}
	pod.start() // the new daemon
	if inv := h.inventory(); inv.Idle != 1 {
		t.Fatalf("want idle after the new daemon registered, got %+v", inv)
	}
}

// A failed reset must not keep the early registration: the daemon was not
// started again.
func TestEarlyRegistrationIsDroppedWhenTheResetFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseAndComplete(t, h, pod, "t1")

	cmd := pod.pollControl()
	pod.start()
	if code := pod.reportControl(cmd.ID, "failed"); code != http.StatusOK {
		t.Fatalf("report failed: status %d", code)
	}
	if inv := h.inventory(); inv.Idle != 0 || inv.Resetting != 0 {
		t.Fatalf("want the pod neither idle nor resetting, got %+v", inv)
	}
}
