package sandboxbroker

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestResetIsAuditedWithTheLeaseIdentity(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p, err := NewPool(PoolConfig{
		MulticaURL: "http://127.0.0.1:1", MulticaToken: "x",
		WorkspaceID: "11111111-2222-4333-8444-555555555555", AuditLog: &buf,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := &lease{TaskID: "t1", PodIP: "10.0.0.1", PodName: "pool-0", Initiator: "alice@example.com"}
	p.auditLease(AuditReset, l, "pod reset after task")

	var rec AuditRecord
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Action != AuditReset || rec.TaskID != "t1" || rec.Initiator != "alice@example.com" || rec.PodIP != "10.0.0.1" {
		t.Fatalf("bad reset record: %+v", rec)
	}
}
