package scheduler

import (
	"context"
	"time"
)

type WorkflowTicker interface{ Tick(context.Context) error }

// WorkflowTickJob advances YAML-defined workflows. All state lives in issue
// metadata, so a missed bucket only delays the next step; nothing is lost.
func WorkflowTickJob(t WorkflowTicker) JobSpec {
	return JobSpec{
		Name: "workflow_tick", Cadence: 15 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 45 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, t.Tick(ctx)
		},
	}
}
