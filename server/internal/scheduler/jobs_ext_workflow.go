package scheduler

import (
	"context"
	"time"
)

// ExtWorkflowReconciler is *extworkflow.Engine.
type ExtWorkflowReconciler interface{ Reconcile(context.Context) error }

// ExtWorkflowReconcileJob is the workflow engine's safety net (fork,
// ext-workflow): it re-derives events that a crash or a lost bus event
// dropped, for at most 200 active runs per tick.
func ExtWorkflowReconcileJob(r ExtWorkflowReconciler) JobSpec {
	return JobSpec{
		Name: "ext_workflow_reconcile", Cadence: 30 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 45 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, r.Reconcile(ctx)
		},
	}
}
