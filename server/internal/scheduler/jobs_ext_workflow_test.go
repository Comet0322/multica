package scheduler

import (
	"context"
	"testing"
	"time"
)

type countingReconciler struct{ calls int }

func (c *countingReconciler) Reconcile(context.Context) error {
	c.calls++
	return nil
}

func TestExtWorkflowReconcileJob(t *testing.T) {
	r := &countingReconciler{}
	job := ExtWorkflowReconcileJob(r)
	if err := job.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if job.Name != "ext_workflow_reconcile" || job.Cadence != 30*time.Second || job.CatchUpMode != CatchUpLatestOnly || job.MaxAttempts != 1 {
		t.Fatalf("job = %s every %s, catch-up %v, attempts %d", job.Name, job.Cadence, job.CatchUpMode, job.MaxAttempts)
	}
	if _, err := job.Handler(context.Background(), HandlerInput{}); err != nil || r.calls != 1 {
		t.Fatalf("handler err=%v calls=%d", err, r.calls)
	}
}
