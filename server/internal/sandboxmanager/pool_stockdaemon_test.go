package sandboxmanager_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

// The stock daemon never sends a boot nonce or its own IP, so none of these
// tests set pod.nonce: the reset has to complete from the supervisor's report
// and the registration that follows it.

func TestStockDaemonLeaseResetIdleCycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseAndComplete(t, h, pod, "t1")
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("want resetting after the task, got %+v", inv)
	}

	// A registration before the acknowledgement is the old daemon's.
	pod.startRaw()
	if inv := h.inventory(); inv.Resetting != 1 || inv.Idle != 0 {
		t.Fatalf("a registration before the supervisor reported ended the reset: %+v", inv)
	}

	cmd := pod.pollControl()
	if cmd == nil || cmd.Command != "reset" {
		t.Fatalf("want a reset command, got %+v", cmd)
	}
	if code := pod.reportControl(cmd.ID, "done"); code != http.StatusOK {
		t.Fatalf("report done: status %d", code)
	}
	// The acknowledgement alone does not free the pod.
	if inv := h.inventory(); inv.Resetting != 1 || inv.Idle != 0 {
		t.Fatalf("pod left reset on the report alone: %+v", inv)
	}
	h.upstream.enqueue("t2")
	if got := pod.claimBatch(); len(got) != 0 {
		t.Fatalf("pod was lent before the new daemon registered: %v", got)
	}

	pod.startRaw() // the new daemon
	if inv := h.inventory(); inv.Idle != 1 || inv.Resetting != 0 {
		t.Fatalf("want idle after the next registration, got %+v", inv)
	}
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("want t2, got %v", got)
	}
}

func TestStockDaemonWithMatchingPodIPCompletesTheSameCycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.reportedIP = "127.0.0.2"
	leaseAndComplete(t, h, pod, "t1")

	cmd := pod.pollControl()
	pod.reportControl(cmd.ID, "done")
	pod.startRaw()
	if inv := h.inventory(); inv.Idle != 1 {
		t.Fatalf("want idle, got %+v", inv)
	}
	// The SNAT check still applies to every registration that carries the IP.
	pod.reportedIP = "10.9.9.9"
	if status, _ := pod.register(); status != http.StatusForbidden {
		t.Fatalf("mismatched pod_ip: want 403, got %d", status)
	}
}

func TestPodSeenForTheFirstTimeIsResetBeforeItIsLent(t *testing.T) {
	t.Parallel()
	// A busy pod re-registering after the manager restarted looks like this:
	// the registry is empty and nothing proves the pod clean.
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	h.upstream.enqueue("t1")
	pod.startRaw()

	if inv := h.inventory(); inv.Resetting != 1 || inv.Idle != 0 {
		t.Fatalf("an unseen pod must start in reset, got %+v", inv)
	}
	if got := pod.claimBatch(); len(got) != 0 {
		t.Fatalf("an unseen pod was handed %v", got)
	}
	if h.upstream.queued() != 1 {
		t.Fatal("a task was claimed for an unseen pod")
	}
	cmd := pod.pollControl()
	if cmd == nil || cmd.Command != "reset" {
		t.Fatalf("the supervisor must be told to reset the pod, got %+v", cmd)
	}
	pod.reportControl(cmd.ID, "done")
	pod.startRaw()
	if inv := h.inventory(); inv.Idle != 1 {
		t.Fatalf("want idle once reset, got %+v", inv)
	}
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1, got %v", got)
	}
}

func TestLostPodEndsItsLeases(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t, "alice@example.com")
	h := newHarness(t, 2, shortTimeout, withLiteLLM(llm), withGateway())
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	healthy := h.newPod("127.0.0.3", "pool-1")
	pod.start()
	healthy.start()
	task := pod.claimTask()
	key := llm.keyOf("t1")
	ticket := envOf(t, task)["GIT_GATEWAY_TOKEN"]
	if key == "" || ticket == "" {
		t.Fatalf("task got no credentials: key %q token %q", key, ticket)
	}

	time.Sleep(3500 * time.Millisecond)
	healthy.heartbeat() // any pod's traffic notices the silence and reports it

	eventually(t, "key revoked", func() bool { return llm.isDeleted(key) })
	if h.verifyToken(ticket) != http.StatusNotFound {
		t.Fatal("git token survived the pod being lost")
	}
	eventually(t, "task failed upstream", func() bool {
		msg, ok := h.upstream.failureOf("t1")
		return ok && strings.Contains(msg, "lost")
	})
	ls := h.leases()
	if len(ls) != 1 || ls[0].EndedAt == nil || ls[0].EndReason != "pod lost" {
		t.Fatalf("lease should be closed with the reason, got %+v", ls)
	}
	var released bool
	for _, r := range h.auditRecords() {
		released = released || (r.Action == sandboxmanager.AuditRelease && r.TaskID == "t1" && r.Reason == "pod lost")
	}
	if !released {
		t.Fatalf("no release audit for the lost pod: %+v", h.auditRecords())
	}

	// If it comes back it carries the dead task's files: it is reset first.
	pod.startRaw()
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("a lost pod that returns must be reset before it is lent, got %+v", inv)
	}
}

func TestResetIsAudited(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseAndComplete(t, h, pod, "t1")
	cmd := pod.pollControl()
	pod.reportControl(cmd.ID, "done")
	pod.startRaw()

	var got []string
	for _, r := range h.auditRecords() {
		if r.Action != sandboxmanager.AuditReset || r.TaskID != "t1" {
			continue
		}
		if r.Decision != sandboxmanager.AuditAllow || r.Initiator != "alice@example.com" || r.PodIP != "127.0.0.2" || r.WorkspaceID != harnessWorkspaceID {
			t.Fatalf("incomplete reset record: %+v", r)
		}
		got = append(got, strings.SplitN(r.Reason, ":", 2)[0])
	}
	if !slices.Equal(got, []string{"reset started", "reset completed"}) {
		t.Fatalf("want a start and a completion record for t1, got %v", got)
	}
}

func TestResetTimeoutAndFailureAreAudited(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, shortReset)
	timedOut := h.newPod("127.0.0.2", "pool-0")
	failed := h.newPod("127.0.0.3", "pool-1")
	failed.start()
	leaseAndComplete(t, h, timedOut, "t1")
	h.upstream.enqueue("t2")
	if got := failed.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("want t2, got %v", got)
	}
	failed.report("t2", "complete")
	cmd := failed.pollControl()
	failed.reportControl(cmd.ID, "failed")

	time.Sleep(3500 * time.Millisecond)
	h.inventory()

	deny := map[string]string{}
	for _, r := range h.auditRecords() {
		if r.Action == sandboxmanager.AuditReset && r.Decision == sandboxmanager.AuditDeny {
			deny[r.PodIP] = r.Reason
		}
	}
	if !strings.Contains(deny["127.0.0.2"], "timed out") {
		t.Fatalf("want a timeout record for the silent pod, got %v", deny)
	}
	if !strings.Contains(deny["127.0.0.3"], "failed") || !strings.Contains(deny["127.0.0.3"], "disk gone") {
		t.Fatalf("want a failure record with the supervisor's error, got %v", deny)
	}
}

func TestLeasesAndInventoryNameTheInitiators(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	pod.claimBatch()

	ls := h.leases()
	if len(ls) != 1 || ls[0].Initiator != "alice@example.com" {
		t.Fatalf("lease should name its initiator, got %+v", ls)
	}
	inv := h.inventory()
	if len(inv.Pods) != 1 || !slices.Equal(inv.Pods[0].Initiators, []string{"alice@example.com"}) {
		t.Fatalf("pod should list the initiators it serves, got %+v", inv.Pods)
	}
}
