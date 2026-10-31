package sandboxbroker_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxbroker"
)

// Reset timeout 3s, as the ticket specifies.
func shortReset(c *sandboxbroker.PoolConfig) { c.ResetTimeout = 3 * time.Second }

type resetCommand struct {
	ID      string `json:"id"`
	Command string `json:"command"`
}

// pollControl plays the supervisor's poll; it returns nil when the manager has
// no command for this pod.
func (p *simPod) pollControl() *resetCommand {
	p.h.t.Helper()
	status, raw := p.call(http.MethodGet, "/pod/control", nil)
	if status == http.StatusNoContent {
		return nil
	}
	if status != http.StatusOK {
		p.h.t.Fatalf("poll control: status %d: %s", status, raw)
	}
	var cmd resetCommand
	if err := json.Unmarshal(raw, &cmd); err != nil {
		p.h.t.Fatal(err)
	}
	return &cmd
}

func (p *simPod) reportControl(id, status string) int {
	p.h.t.Helper()
	code, _ := p.call(http.MethodPost, "/pod/control", map[string]string{"id": id, "status": status, "error": "disk gone"})
	return code
}

// completeReset plays the supervisor and the restarted daemon: take the reset
// command, report it done and register again under a new boot nonce.
func (p *simPod) completeReset(nonce string) {
	p.h.t.Helper()
	cmd := p.pollControl()
	if cmd == nil || cmd.Command != "reset" {
		p.h.t.Fatalf("want a reset command, got %+v", cmd)
	}
	if code := p.reportControl(cmd.ID, "done"); code != http.StatusOK {
		p.h.t.Fatalf("report done: status %d", code)
	}
	p.nonce = nonce
	p.start()
}

func leaseAndComplete(t *testing.T, h *harness, p *simPod, task string) {
	t.Helper()
	h.upstream.enqueue(task)
	p.start()
	if got := p.claimBatch(); !slices.Equal(got, []string{task}) {
		t.Fatalf("want %s, got %v", task, got)
	}
	if status := p.report(task, "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
}

func TestReleasedPodIsResettingAndGetsNoTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	leaseAndComplete(t, h, pod, "t1")

	if inv := h.inventory(); inv.Resetting != 1 || inv.Idle != 0 || inv.Leased != 0 {
		t.Fatalf("want 1 resetting, got %+v", inv)
	}
	h.upstream.enqueue("t2")
	if got := pod.claimBatch(); len(got) != 0 {
		t.Fatalf("resetting pod was handed %v", got)
	}
	if cmd := pod.pollControl(); cmd == nil || cmd.Command != "reset" || cmd.ID == "" {
		t.Fatalf("want a reset command, got %+v", cmd)
	}
}

func TestPodReturnsToIdleAfterRegisteringWithNewNonce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	leaseAndComplete(t, h, pod, "t1")

	cmd := pod.pollControl()
	if code := pod.reportControl(cmd.ID, "done"); code != http.StatusOK {
		t.Fatalf("report done: status %d", code)
	}
	// Done alone does not free the pod; the daemon has to be back.
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("pod left reset before registering: %+v", inv)
	}
	if again := pod.pollControl(); again != nil {
		t.Fatalf("command still offered after done: %+v", again)
	}
	pod.nonce = "boot-2"
	pod.start()
	if inv := h.inventory(); inv.Idle != 1 || inv.Resetting != 0 {
		t.Fatalf("want idle after re-registering, got %+v", inv)
	}
	h.upstream.enqueue("t2")
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("want t2, got %v", got)
	}
}

func TestOldDaemonRegisteringAgainDoesNotEndTheReset(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	leaseAndComplete(t, h, pod, "t1")

	pod.start() // same nonce: the old daemon, not yet reset
	if inv := h.inventory(); inv.Resetting != 1 || inv.Idle != 0 {
		t.Fatalf("same-nonce registration ended the reset: %+v", inv)
	}
}

func TestResetTimeoutMarksPodLostUntilItRegistersAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shortReset)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	leaseAndComplete(t, h, pod, "t1")

	time.Sleep(3500 * time.Millisecond)
	if inv := h.inventory(); inv.Lost != 1 || inv.Resetting != 0 {
		t.Fatalf("want lost after reset timeout, got %+v", inv)
	}
	h.upstream.enqueue("t2")
	if got := pod.claimBatch(); len(got) != 0 {
		t.Fatalf("lost pod was handed %v", got)
	}
	pod.nonce = "boot-2"
	pod.start()
	if inv := h.inventory(); inv.Idle != 1 || inv.Lost != 0 {
		t.Fatalf("want idle after registering again, got %+v", inv)
	}
}

func TestResetStaysWithinTimeoutWhileSilent(t *testing.T) {
	t.Parallel()
	// The daemon is stopped during a reset, so heartbeat silence alone must
	// not mark the pod lost.
	h := newHarness(t, 1, func(c *sandboxbroker.PoolConfig) {
		c.HeartbeatTimeout = time.Second
		c.ResetTimeout = 10 * time.Second
	})
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	leaseAndComplete(t, h, pod, "t1")

	time.Sleep(1500 * time.Millisecond)
	if inv := h.inventory(); inv.Resetting != 1 || inv.Lost != 0 {
		t.Fatalf("want still resetting, got %+v", inv)
	}
}

func TestFailedResetReportMarksPodLostAndNeverLends(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	leaseAndComplete(t, h, pod, "t1")

	cmd := pod.pollControl()
	if code := pod.reportControl(cmd.ID, "failed"); code != http.StatusOK {
		t.Fatalf("report failed: status %d", code)
	}
	if inv := h.inventory(); inv.Lost != 1 || inv.Resetting != 0 {
		t.Fatalf("want lost after failed reset, got %+v", inv)
	}
	h.upstream.enqueue("t2")
	if got := pod.claimBatch(); len(got) != 0 {
		t.Fatalf("lost pod was handed %v", got)
	}
	if h.upstream.queued() != 1 {
		t.Fatal("a task was claimed for a lost pod")
	}
}

func TestControlChannelIsPerSourceIP(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	a := h.newPod("127.0.0.2", "pool-0")
	b := h.newPod("127.0.0.3", "pool-1")
	a.nonce, b.nonce = "a-1", "b-1"
	b.start()
	leaseAndComplete(t, h, a, "t1")

	cmd := a.pollControl()
	if cmd == nil {
		t.Fatal("pod A got no command")
	}
	if got := b.pollControl(); got != nil {
		t.Fatalf("pod B received a command: %+v", got)
	}
	// B cannot answer A's command either.
	b.reportControl(cmd.ID, "failed")
	if inv := h.inventory(); inv.Resetting != 1 || inv.Lost != 0 || inv.Idle != 1 {
		t.Fatalf("B's report affected the pool: %+v", inv)
	}
	stranger := h.newPod("127.0.0.9", "stranger")
	if status, _ := stranger.call(http.MethodGet, "/pod/control", nil); status != http.StatusNotFound {
		t.Fatalf("unregistered caller: want 404, got %d", status)
	}
}
