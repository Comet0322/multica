package service

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow (fork): the seam between upstream services and the workflow
// engine in internal/extworkflow. service must not import extworkflow, so the
// engine is reached only through this interface.

// ExtChildEvent is one claimed issue_child_event row of a workflow parent.
type ExtChildEvent struct {
	ChildID pgtype.UUID
	Kind    string
}

// ExtWorkflowHooks is implemented by *extworkflow.Engine.
type ExtWorkflowHooks interface {
	ValidateAssignment(ctx context.Context, workspaceID, workflowID pgtype.UUID, actorType string, actorID pgtype.UUID) error
	StartRun(ctx context.Context, issueID pgtype.UUID, actorType string, actorID pgtype.UUID) error
	// OnChildEvents runs inside the caller's transaction. Notifications it
	// produces are registered on ExtAfterCommitFrom(ctx) when present.
	OnChildEvents(ctx context.Context, tx pgx.Tx, parentID pgtype.UUID, events []ExtChildEvent) error
}

// ExtAfterCommit collects work a hook produced inside a caller's transaction
// that may only run once that transaction committed (bus events, daemon
// wakeups).
type ExtAfterCommit struct {
	mu  sync.Mutex
	fns []func()
}

type extAfterCommitKey struct{}

// WithExtAfterCommit returns a context carrying a fresh collector.
func WithExtAfterCommit(ctx context.Context) (context.Context, *ExtAfterCommit) {
	a := &ExtAfterCommit{}
	return context.WithValue(ctx, extAfterCommitKey{}, a), a
}

// ExtAfterCommitFrom returns the collector on ctx, or nil.
func ExtAfterCommitFrom(ctx context.Context) *ExtAfterCommit {
	a, _ := ctx.Value(extAfterCommitKey{}).(*ExtAfterCommit)
	return a
}

func (a *ExtAfterCommit) Add(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fns = append(a.fns, fn)
}

// Run executes and clears the collected functions in order.
func (a *ExtAfterCommit) Run() {
	a.mu.Lock()
	fns := a.fns
	a.fns = nil
	a.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// runExtWorkflowChildEvents hands the claimed sub-issue changes of a workflow
// parent to the engine inside tx. The returned func publishes what the engine
// produced and must run only after tx committed.
func (s *TaskService) runExtWorkflowChildEvents(ctx context.Context, tx pgx.Tx, parent db.Issue, claimed []db.IssueChildEvent) (func(), error) {
	if s == nil || s.ExtWorkflow == nil || parent.AssigneeType.String != "workflow" {
		return func() {}, nil
	}
	hctx, after := WithExtAfterCommit(ctx)
	events := make([]ExtChildEvent, 0, len(claimed))
	for _, e := range claimed {
		events = append(events, ExtChildEvent{ChildID: e.ChildID, Kind: e.Kind})
	}
	if err := s.ExtWorkflow.OnChildEvents(hctx, tx, parent.ID, events); err != nil {
		return nil, err
	}
	return after.Run, nil
}
