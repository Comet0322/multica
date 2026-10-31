package sandboxbroker_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/sandboxbroker"
)

func (h *harness) auditRecords() []sandboxbroker.AuditRecord {
	h.t.Helper()
	var out []sandboxbroker.AuditRecord
	sc := bufio.NewScanner(strings.NewReader(h.audit.String()))
	for sc.Scan() {
		var rec sandboxbroker.AuditRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			h.t.Fatalf("audit line is not standalone JSON: %q: %v", sc.Text(), err)
		}
		out = append(out, rec)
	}
	return out
}

func TestTaskWithoutInitiatorIsRejectedAndNeverLeased(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueueNoInitiator("t1")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	if got := pod.claimBatch(); len(got) != 0 {
		t.Fatalf("pod was handed %v", got)
	}
	msg, ok := h.upstream.failureOf("t1")
	if !ok || !strings.Contains(msg, "initiator") {
		t.Fatalf("task should fail upstream naming the missing initiator, got %q (failed=%v)", msg, ok)
	}
	if inv := h.inventory(); inv.Leased != 0 || inv.Idle != 1 {
		t.Fatalf("want the pod idle, got %+v", inv)
	}
	if len(h.leases()) != 0 {
		t.Fatalf("a rejected task must not create a lease: %+v", h.leases())
	}
	var recs []sandboxbroker.AuditRecord
	for _, r := range h.auditRecords() {
		if r.Action != sandboxbroker.AuditReset {
			recs = append(recs, r)
		}
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 audit record, got %+v", recs)
	}
	r := recs[0]
	if r.Decision != sandboxbroker.AuditDeny || r.TaskID != "t1" || r.PodIP != "" || !strings.Contains(r.Reason, "initiator") {
		t.Fatalf("bad denial record: %+v", r)
	}

	// The pod is still usable by the next task.
	h.upstream.enqueue("t2")
	if got := pod.claimBatch(); len(got) != 1 || got[0] != "t2" {
		t.Fatalf("want t2 after the rejection, got %v", got)
	}
}

func TestAuditTracesLeaseAndReleaseByTaskID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	pod.claimBatch()
	if status := pod.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: %d", status)
	}

	var seq []string
	for _, r := range h.auditRecords() {
		if r.TaskID != "t1" {
			continue
		}
		if r.Time.IsZero() || r.Reason == "" || r.Decision != sandboxbroker.AuditAllow ||
			r.WorkspaceID != harnessWorkspaceID || r.Initiator != "alice@example.com" ||
			r.PodIP != "127.0.0.2" || r.PodName != "pool-0" {
			t.Fatalf("incomplete record: %+v", r)
		}
		seq = append(seq, r.Action)
	}
	if strings.Join(seq, ",") != "lease,release,reset" {
		t.Fatalf("want lease, release, then the reset it causes for t1, got %v", seq)
	}
}

func TestAuditIsJSONLinesWithoutCredentials(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	h.upstream.enqueueNoInitiator("t2")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	pod.claimBatch()
	pod.report("t1", "fail")
	pod.completeReset("boot-2")
	pod.claimBatch()

	raw := h.audit.String()
	if len(h.auditRecords()) < 3 {
		t.Fatalf("expected several records, got %q", raw)
	}
	for _, secret := range []string{harnessPAT, podPlaceholder, "Bearer"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("audit log contains %q: %s", secret, raw)
		}
	}
}
