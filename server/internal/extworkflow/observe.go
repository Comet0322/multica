package extworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The engine's observation entry points (spec §5.2). Each re-derives events
// from current task and issue state under the run lock, so a late, repeated
// or lost signal converges to the same result.

var _ service.ExtWorkflowHooks = (*Engine)(nil)

// ReconcileBatch bounds one reconcile tick (spec §5.2).
const ReconcileBatch = 200

// OnTaskTerminal handles task:completed / task:failed / task:cancelled for a
// workflow task. A failed task whose platform retry is pending is ignored:
// the retry carries the same ext columns.
func (e *Engine) OnTaskTerminal(ctx context.Context, taskID pgtype.UUID) error {
	if !e.Enabled() {
		return nil
	}
	task, err := e.q.GetAgentTask(ctx, taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load task: %w", err)
	}
	if !task.ExtWorkflowRunID.Valid || !isTerminalTask(task.Status) {
		return nil
	}
	if task.Status == "failed" {
		retried, err := e.q.HasRetryTaskForParent(ctx, task.ID)
		if err != nil {
			return fmt.Errorf("check retry: %w", err)
		}
		if retried {
			return nil
		}
	}
	err = e.advance(ctx, nil, task.ExtWorkflowRunID, func(ctx context.Context, q *db.Queries, snap *RunSnapshot) ([]stepEvent, error) {
		if !snap.State.Status.Active() {
			return nil, nil
		}
		if task.ExtWorkflowKind.String == KindSummary {
			ev, ok, err := deriveSummary(ctx, q, snap)
			if err != nil || !ok {
				return nil, err
			}
			return []stepEvent{ev}, nil
		}
		key, ok := snap.KeyOf(task.ExtWorkflowStepID)
		if !ok {
			return nil, nil
		}
		ev, ok, err := deriveStep(ctx, q, snap, key)
		if err != nil || !ok {
			return nil, err
		}
		return []stepEvent{ev}, nil
	})
	if errors.Is(err, ErrRunNotFound) {
		return nil
	}
	return err
}

// OnChildEvents runs inside processChildEvents' transaction for a workflow
// parent (spec §5.2). Notifications are deferred to the caller's commit.
func (e *Engine) OnChildEvents(ctx context.Context, tx pgx.Tx, parentID pgtype.UUID, events []service.ExtChildEvent) error {
	if !e.Enabled() {
		return nil
	}
	run, err := e.q.WithTx(tx).GetActiveExtWorkflowRunByIssue(ctx, parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load active run: %w", err)
	}
	children := make(map[pgtype.UUID]bool, len(events))
	for _, ev := range events {
		children[ev.ChildID] = true
	}
	return e.advance(ctx, tx, run.ID, func(ctx context.Context, q *db.Queries, snap *RunSnapshot) ([]stepEvent, error) {
		var out []stepEvent
		for _, row := range snap.Steps {
			if !children[row.IssueID] {
				continue
			}
			ev, ok, err := deriveStep(ctx, q, snap, row.NodeKey)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, ev)
			}
		}
		return out, nil
	})
}

// OnParentChanged stops the active run when its parent was cancelled,
// deleted, or reassigned away from the run's workflow.
func (e *Engine) OnParentChanged(ctx context.Context, issueID pgtype.UUID) error {
	if !e.Enabled() {
		return nil
	}
	run, err := e.q.GetActiveExtWorkflowRunByIssue(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load active run: %w", err)
	}
	return e.ignoreGone(e.advance(ctx, nil, run.ID, func(ctx context.Context, q *db.Queries, snap *RunSnapshot) ([]stepEvent, error) {
		ev, ok, err := deriveParent(ctx, q, snap)
		if err != nil || !ok {
			return nil, err
		}
		return []stepEvent{ev}, nil
	}))
}

// OnParentDeleted cancels the active run before the parent's tasks are
// cancelled and the row is removed, so the platform's own cancellation of
// supervisor tasks is not read as a silent supervisor.
func (e *Engine) OnParentDeleted(ctx context.Context, issueID pgtype.UUID) error {
	if !e.Enabled() {
		return nil
	}
	run, err := e.q.GetActiveExtWorkflowRunByIssue(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load active run: %w", err)
	}
	return e.ignoreGone(e.advance(ctx, nil, run.ID, func(context.Context, *db.Queries, *RunSnapshot) ([]stepEvent, error) {
		return []stepEvent{{Event: Event{Kind: EvCancelRun, Reason: "the parent issue was deleted"}, Actor: EngineActor}}, nil
	}))
}

// Reconcile is the safety net (scheduler job ext_workflow_reconcile): it
// re-derives events for up to ReconcileBatch active runs, least recently
// touched first, and touches each so the next tick moves on.
func (e *Engine) Reconcile(ctx context.Context) error {
	if !e.Enabled() {
		return nil
	}
	runs, err := e.q.ListExtWorkflowRunsForReconcile(ctx, ReconcileBatch)
	if err != nil {
		return fmt.Errorf("list active runs: %w", err)
	}
	var errs []error
	for _, run := range runs {
		if err := e.ignoreGone(e.advance(ctx, nil, run.ID, deriveReconcile)); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", run.ID.String(), err))
		}
		if err := e.q.TouchExtWorkflowRun(ctx, run.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) ignoreGone(err error) error {
	if errors.Is(err, ErrRunNotFound) {
		return nil
	}
	return err
}

func deriveReconcile(ctx context.Context, q *db.Queries, snap *RunSnapshot) ([]stepEvent, error) {
	if !snap.State.Status.Active() {
		return nil, nil
	}
	if ev, ok, err := deriveParent(ctx, q, snap); err != nil || ok {
		if ok {
			return []stepEvent{ev}, nil
		}
		return nil, err
	}
	var out []stepEvent
	for _, row := range snap.Steps {
		ev, ok, err := deriveStep(ctx, q, snap, row.NodeKey)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ev)
		}
	}
	ev, ok, err := deriveSummary(ctx, q, snap)
	if err != nil {
		return nil, err
	}
	if ok {
		out = append(out, ev)
	}
	return out, nil
}

// deriveParent: the parent was deleted, cancelled, or no longer assigned to
// this run's workflow.
func deriveParent(ctx context.Context, q *db.Queries, snap *RunSnapshot) (stepEvent, bool, error) {
	cancel := func(reason string) (stepEvent, bool, error) {
		return stepEvent{Event: Event{Kind: EvCancelRun, Reason: reason}, Actor: EngineActor}, true, nil
	}
	parent, err := q.GetIssue(ctx, snap.Run.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return cancel("the parent issue was deleted")
	}
	if err != nil {
		return stepEvent{}, false, fmt.Errorf("load parent: %w", err)
	}
	if issuestatus.Effective(ctx, q, parent.WorkspaceID, parent.Status) == issuestatus.Cancelled {
		return cancel("the parent issue was cancelled")
	}
	if parent.AssigneeType.String != "workflow" || parent.AssigneeID != snap.Run.WorkflowID {
		return cancel("the parent issue was reassigned")
	}
	return stepEvent{}, false, nil
}

// deriveStep reads what a step's child issue and newest tasks say happened
// (spec §5.3). Only tasks stamped with the step count; a child moved to done
// or in_review finishes the step whatever ran it.
func deriveStep(ctx context.Context, q *db.Queries, snap *RunSnapshot, key string) (stepEvent, bool, error) {
	none := func() (stepEvent, bool, error) { return stepEvent{}, false, nil }
	st, row := snap.State.Steps[key], snap.ByKey[key]
	if st.Status.Terminal() {
		return none()
	}
	observed := func(ev Event) (stepEvent, bool, error) {
		return stepEvent{Key: key, Event: ev, Actor: EngineActor}, true, nil
	}
	child, err := q.GetIssue(ctx, row.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return observed(Event{Kind: EvChildCancelled, Detail: map[string]any{"child": "deleted"}})
	}
	if err != nil {
		return stepEvent{}, false, fmt.Errorf("load child issue: %w", err)
	}
	childStatus := issuestatus.Effective(ctx, q, child.WorkspaceID, child.Status)
	if childStatus == issuestatus.Cancelled {
		return observed(Event{Kind: EvChildCancelled})
	}
	switch st.Status {
	case StepRunning:
		if childStatus == issuestatus.Done || childStatus == issuestatus.InReview {
			return observed(Event{Kind: EvStepFinished, Detail: map[string]any{"child_status": child.Status}})
		}
		task, found, err := latestTask(ctx, q, row.ID, RoleStep)
		if err != nil || !found {
			return stepEvent{}, false, err
		}
		if settled, err := taskSettled(ctx, q, task); err != nil || !settled {
			return stepEvent{}, false, err
		}
		return observed(stepTaskOutcome(task))
	case StepAwaitingSupervisor:
		if snap.State.SupervisorFor[key] || st.SupervisorWakes == 0 {
			return none()
		}
		task, found, err := latestTask(ctx, q, row.ID, RoleSupervisor)
		if err != nil || !found {
			return stepEvent{}, false, err
		}
		if settled, err := taskSettled(ctx, q, task); err != nil || !settled {
			return stepEvent{}, false, err
		}
		// Only a task started for this pending decision counts: the step row
		// and that task were written in one transaction.
		if task.CreatedAt.Time.Before(row.UpdatedAt.Time) {
			return none()
		}
		// A turn's no-decision is applied once: the first one only records a
		// protocol error and leaves the step row (and updated_at) untouched,
		// so the task id in that event is what marks the turn as handled.
		if handled, err := noDecisionRecorded(ctx, q, snap.Run.ID, task.ID); err != nil || handled {
			return stepEvent{}, false, err
		}
		return observed(Event{Kind: EvSupervisorNoDecision, Reason: describeSupervisorEnd(task), Detail: map[string]any{"task_id": task.ID.String()}})
	}
	return none()
}

// noDecisionRecorded reports whether a timeline event already carries this
// supervisor task as the turn that ended without a decision.
func noDecisionRecorded(ctx context.Context, q *db.Queries, runID, taskID pgtype.UUID) (bool, error) {
	rows, err := q.ListExtWorkflowRunEvents(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("list run events: %w", err)
	}
	want := taskID.String()
	for _, r := range rows {
		if r.Kind != RunEventProtocolError && r.Kind != RunEventEscalated {
			continue
		}
		var p struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(r.Payload, &p) == nil && p.TaskID == want {
			return true, nil
		}
	}
	return false, nil
}

// deriveSummary: the summary task ended, whether it completed or not.
func deriveSummary(ctx context.Context, q *db.Queries, snap *RunSnapshot) (stepEvent, bool, error) {
	if !snap.State.SummaryRequested || !snap.State.AllSettled() {
		return stepEvent{}, false, nil
	}
	task, err := q.GetLatestExtWorkflowSummaryTask(ctx, snap.Run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return stepEvent{}, false, nil
	}
	if err != nil {
		return stepEvent{}, false, fmt.Errorf("load summary task: %w", err)
	}
	if settled, err := taskSettled(ctx, q, task); err != nil || !settled {
		return stepEvent{}, false, err
	}
	return stepEvent{Event: Event{Kind: EvSummaryEnded}, Actor: EngineActor}, true, nil
}

func latestTask(ctx context.Context, q *db.Queries, stepID pgtype.UUID, role string) (db.AgentTaskQueue, bool, error) {
	task, err := q.GetLatestExtWorkflowTaskForStep(ctx, db.GetLatestExtWorkflowTaskForStepParams{
		StepID: stepID, Role: pgtype.Text{String: role, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return task, false, nil
	}
	if err != nil {
		return task, false, fmt.Errorf("load latest task: %w", err)
	}
	return task, true, nil
}

// taskSettled: the task ended for good, and not because the engine itself
// cancelled it.
func taskSettled(ctx context.Context, q *db.Queries, task db.AgentTaskQueue) (bool, error) {
	switch task.Status {
	case "completed":
		return true, nil
	case "cancelled":
		return task.FailureReason.String != service.ExtWorkflowEngineCancelReason, nil
	case "failed":
		retried, err := q.HasRetryTaskForParent(ctx, task.ID)
		return !retried, err
	}
	return false, nil
}

func isTerminalTask(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

// stepTaskOutcome maps a settled step task whose child is not finished.
func stepTaskOutcome(task db.AgentTaskQueue) Event {
	detail := map[string]any{"task_id": task.ID.String()}
	switch task.Status {
	case "completed":
		return Event{Kind: EvStepFailed, Reason: "ended_without_finishing", Detail: detail}
	case "cancelled":
		return Event{Kind: EvStepFailed, Reason: "cancelled", Detail: detail}
	}
	reason := task.FailureReason.String
	if reason == "" {
		reason = "failed"
	}
	if task.Error.String != "" {
		detail["error"] = task.Error.String
	}
	return Event{Kind: EvStepFailed, Reason: reason, Detail: detail}
}

func describeSupervisorEnd(task db.AgentTaskQueue) string {
	switch task.Status {
	case "completed":
		return "the supervisor's turn ended without a valid decision"
	case "cancelled":
		return "the supervisor's turn was cancelled"
	}
	if task.Error.String != "" {
		return "the supervisor's turn failed: " + task.Error.String
	}
	return "the supervisor's turn failed"
}
