// server/internal/workflow/engine.go
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Tick expands eligible definitions, then advances every running workflow.
func (e *Engine) Tick(ctx context.Context) error {
	var errs []error
	if err := e.ExpandCandidates(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := e.advanceRuns(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (e *Engine) advanceRuns(ctx context.Context) error {
	defs, err := e.Q.ListRunningWorkflowDefinitions(ctx, 200)
	if err != nil {
		return fmt.Errorf("list running workflows: %w", err)
	}
	var errs []error
	for _, def := range defs {
		if err := e.advance(ctx, def); err != nil {
			slog.Warn("workflow advance failed", "issue_id", def.ID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type stepRow struct {
	issue db.Issue
	meta  StepMeta
}

// loadSteps finds a run's steps by the run id in metadata, not by
// parent_issue_id, so a step whose parent link is lost is still tracked.
func (e *Engine) loadSteps(ctx context.Context, def db.Issue) ([]stepRow, error) {
	steps, err := e.Q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidString(def.ID)})
	if err != nil {
		return nil, err
	}
	var rows []stepRow
	for _, k := range steps {
		if m, ok := readStepMeta(k); ok {
			rows = append(rows, stepRow{k, m})
		}
	}
	return rows, nil
}

func (e *Engine) advance(ctx context.Context, def db.Issue) error {
	dm, ok := readDefMeta(def)
	if !ok || dm.State != RunRunning {
		return nil
	}
	// Expand writes the running metadata before moving the issue to
	// in_progress; finish that move if a crash left the status behind.
	if def.Status == "todo" {
		updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: def.ID, WorkspaceID: def.WorkspaceID, Status: "in_progress"})
		if err != nil {
			return err
		}
		e.Events.IssueUpdated(ctx, def, updated)
		def = updated
	}
	steps, err := e.loadSteps(ctx, def)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if err := e.observe(ctx, s, steps); err != nil {
			return err
		}
	}
	out, steps, err := e.evaluate(ctx, def, dm)
	if err != nil {
		return err
	}
	switch out {
	case OutcomeDone:
		return e.closeDefinition(ctx, def, RunDone, "done", "")
	case OutcomeBlocked, OutcomeBroken:
		// Re-read the steps right before closing: a /retry or a late event may
		// have changed the picture since the first evaluation.
		again, steps2, err := e.evaluate(ctx, def, dm)
		if err != nil {
			return err
		}
		if again != out {
			return nil
		}
		if out == OutcomeBlocked {
			return e.closeDefinition(ctx, def, RunBlocked, "blocked", failureSummary(steps2))
		}
		return e.closeDefinition(ctx, def, RunBlocked, "blocked",
			"Workflow stopped: a step issue is missing (it may have been deleted). Restore it or recreate the workflow.")
	}
	return nil
}

func (e *Engine) evaluate(ctx context.Context, def db.Issue, dm DefMeta) (Outcome, []stepRow, error) {
	steps, err := e.loadSteps(ctx, def)
	if err != nil {
		return "", nil, err
	}
	metas := make([]StepMeta, len(steps))
	for i, s := range steps {
		metas[i] = s.meta
	}
	return Evaluate(metas, dm.Total), steps, nil
}

// observe derives at most one event for a step from the world and applies it.
// When no event applies it puts the issue status back in line with the phase.
func (e *Engine) observe(ctx context.Context, s stepRow, all []stepRow) error {
	switch s.meta.Phase {
	case PhasePending:
		done := map[string]bool{}
		for _, o := range all {
			if o.meta.Phase == PhaseDone {
				done[o.meta.Node] = true
			}
		}
		ready := true
		for _, d := range s.meta.Deps {
			if !done[d] {
				ready = false
			}
		}
		if ready {
			_, err := e.ApplyEvent(ctx, s.issue, EventDepsMet, pgtype.UUID{})
			return err
		}
	case PhaseRunning:
		if s.issue.Status == "done" || s.issue.Status == "in_review" {
			_, err := e.ApplyEvent(ctx, s.issue, EventAgentFinished, pgtype.UUID{})
			return err
		}
		ev, err := e.runningEvent(ctx, s)
		if err != nil {
			return err
		}
		if ev != "" {
			_, err := e.ApplyEvent(ctx, s.issue, ev, pgtype.UUID{})
			return err
		}
	}
	return e.reconcileStatus(ctx, s.issue, s.meta.Phase)
}

// clock returns the time used to stamp and age dispatches: the injected Now in
// tests, otherwise the database clock, so stamps compare against task
// created_at on one clock.
func (e *Engine) clock(ctx context.Context) (time.Time, error) {
	if e.Now != nil {
		return e.Now(), nil
	}
	t, err := e.Q.WorkflowDBNow(ctx)
	if err != nil {
		return time.Time{}, err
	}
	return t.Time, nil
}

// targetAgent is the agent a step's tasks belong to: the issue's current agent
// assignee, else the agent recorded in the step metadata.
func targetAgent(issue db.Issue, meta StepMeta) (pgtype.UUID, bool) {
	if issue.AssigneeType.String == "agent" && issue.AssigneeID.Valid {
		return issue.AssigneeID, true
	}
	id, err := util.ParseUUID(meta.AgentID)
	return id, err == nil
}

// dispatchGrace is how long a step may sit in running with no agent task
// before the dispatch is considered lost (a crash between claim and enqueue).
const dispatchGrace = 90 * time.Second

// runningEvent maps the latest agent task of a running step (created since its
// latest dispatch) to an event, or "" while the task is queued or in flight.
func (e *Engine) runningEvent(ctx context.Context, s stepRow) (Event, error) {
	agentID, ok := targetAgent(s.issue, s.meta)
	if !ok {
		return "", nil
	}
	// A pending or running task is in flight whenever it was created.
	inFlight, err := e.Q.HasInFlightWorkflowTask(ctx, db.HasInFlightWorkflowTaskParams{IssueID: s.issue.ID, AgentID: agentID})
	if err != nil {
		return "", err
	}
	if inFlight {
		return "", nil
	}
	since := pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true}
	var dispatched time.Time
	if s.meta.DispatchedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, s.meta.DispatchedAt); err == nil {
			dispatched = t
			since.Time = t
		}
	}
	var ev Event
	status, err := e.Q.LatestWorkflowTaskStatus(ctx, db.LatestWorkflowTaskStatusParams{IssueID: s.issue.ID, AgentID: agentID, Since: since})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		ref := dispatched
		if ref.IsZero() { // legacy step without a stamp: use the last issue write
			ref = s.issue.UpdatedAt.Time
		}
		now, cerr := e.clock(ctx)
		if cerr != nil {
			return "", cerr
		}
		if now.Sub(ref) <= dispatchGrace {
			return "", nil // dispatch still in flight
		}
		ev = EventDispatchLost
	case err != nil:
		return "", err
	case status == "failed" || status == "completed": // completed without done/in_review is a failed attempt
		ev = EventAgentFailed
	case status == "cancelled":
		ev = EventAgentCancelled
	default:
		return "", nil
	}
	// The agent may have finished the issue while we were looking.
	fresh, err := e.Q.GetIssue(ctx, s.issue.ID)
	if err != nil {
		return "", err
	}
	if fresh.Status == "done" || fresh.Status == "in_review" {
		return EventAgentFinished, nil
	}
	return ev, nil
}

// reconcileStatus puts the issue back on the status its phase maps to when
// something else moved it (for example a crash between status and metadata
// writes). It never changes the phase.
func (e *Engine) reconcileStatus(ctx context.Context, issue db.Issue, phase Phase) error {
	want := PhaseStatus[phase]
	if issue.Status == want {
		return nil
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: want})
	if err != nil {
		return err
	}
	e.Events.IssueUpdated(ctx, issue, updated)
	return nil
}

// ApplyEvent runs one event through the transition table for step. It
// returns false when no rule matches the step's current phase. trigger, when
// valid, is the reviewer's comment passed to the agent on a redo.
func (e *Engine) ApplyEvent(ctx context.Context, step db.Issue, ev Event, trigger pgtype.UUID) (bool, error) {
	// The expected phase/attempts/dispatch generation come from the step as the
	// caller observed it, never from a re-read, so a stale observation loses.
	meta, ok := readStepMeta(step)
	if !ok {
		return false, nil
	}
	if ev == EventDispatchLost || ev == EventAgentFailed || ev == EventAgentCancelled {
		// The agent moves the issue status itself before its task ends; do not
		// overwrite a finished step with a failure or a re-run.
		fresh, err := e.Q.GetIssue(ctx, step.ID)
		if err != nil {
			return false, err
		}
		if fresh.Status == "done" || fresh.Status == "in_review" {
			ev = EventAgentFinished
		}
	}
	rule, ok := Next(meta, ev)
	if !ok {
		return false, nil
	}
	next := meta.Apply(rule)
	for _, a := range rule.Actions {
		if a == ActionDispatch || a == ActionDispatchWithFeedback {
			now, cerr := e.clock(ctx)
			if cerr != nil {
				return false, cerr
			}
			next.DispatchedAt = now.UTC().Format(time.RFC3339Nano)
		}
	}

	// Claim the transition first: the metadata write only succeeds while the
	// step is still in the observed phase, attempt count and dispatch
	// generation, so a concurrent instance cannot apply the same or a
	// conflicting event.
	raw, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	cur, err := e.Q.ClaimWorkflowStepTransition(ctx, db.ClaimWorkflowStepTransitionParams{
		Value: raw, ID: step.ID, WorkspaceID: step.WorkspaceID,
		ExpectedPhase: string(meta.Phase), ExpectedAttempts: int32(meta.Attempts),
		ExpectedDispatchedAt: meta.DispatchedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if want := PhaseStatus[next.Phase]; cur.Status != want {
		prev := cur
		cur, err = e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: step.ID, WorkspaceID: step.WorkspaceID, Status: want})
		if err != nil {
			return true, err
		}
		e.Events.IssueUpdated(ctx, prev, cur)
	}
	for _, a := range rule.Actions {
		switch a {
		case ActionDispatch:
			if err := e.dispatch(ctx, cur, next, pgtype.UUID{}); err != nil {
				return true, e.failDispatch(ctx, cur, next, err)
			}
		case ActionDispatchWithFeedback:
			if err := e.dispatch(ctx, cur, next, trigger); err != nil {
				return true, e.failDispatch(ctx, cur, next, err)
			}
		case ActionRequestReview:
			if _, err := e.systemComment(ctx, cur, "Waiting for review. The workflow creator can reply `/accept` to approve or `/reject <feedback>` to send it back to the agent."); err != nil {
				return true, err
			}
		case ActionCommentFailure:
			if _, err := e.systemComment(ctx, cur, fmt.Sprintf("Step ended without finishing after %d attempt(s). Reply `/retry` to run it again.", next.Attempts+1)); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

func (e *Engine) dispatch(ctx context.Context, issue db.Issue, meta StepMeta, trigger pgtype.UUID) error {
	var err error
	if trigger.Valid {
		agentID, ok := targetAgent(issue, meta)
		if !ok {
			return fmt.Errorf("step %s has no valid agent", meta.Node)
		}
		_, err = e.Tasks.EnqueueTaskForMention(ctx, issue, agentID, trigger, service.OriginDerived)
	} else {
		_, err = e.Tasks.EnqueueTaskForIssue(ctx, issue)
	}
	if errors.Is(err, service.ErrDuplicatePendingTask) {
		return nil
	}
	return err
}

// failDispatch marks a step failed when its agent task could not be queued
// (for example the agent was archived) and says why on the issue.
func (e *Engine) failDispatch(ctx context.Context, issue db.Issue, meta StepMeta, cause error) error {
	failed := meta
	failed.Phase = PhaseFailed
	raw, err := json.Marshal(failed)
	if err != nil {
		return err
	}
	cur, err := e.Q.ClaimWorkflowStepTransition(ctx, db.ClaimWorkflowStepTransitionParams{
		Value: raw, ID: issue.ID, WorkspaceID: issue.WorkspaceID,
		ExpectedPhase: string(meta.Phase), ExpectedAttempts: int32(meta.Attempts),
		ExpectedDispatchedAt: meta.DispatchedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // another instance already moved the step
	}
	if err != nil {
		return err
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: PhaseStatus[PhaseFailed]})
	if err == nil {
		e.Events.IssueUpdated(ctx, cur, updated)
	}
	_, cerr := e.systemComment(ctx, issue, fmt.Sprintf("Could not start this step: %v. Fix the cause, then reply `/retry`.", cause))
	return errors.Join(err, cerr)
}

func failureSummary(steps []stepRow) string {
	var failed []string
	for _, s := range steps {
		if s.meta.Phase == PhaseFailed {
			failed = append(failed, s.meta.Node)
		}
	}
	sort.Strings(failed)
	return "Workflow blocked: failed step(s): " + strings.Join(failed, ", ") + ". Reply `/retry` here to rerun them."
}

// closeDefinition records the run's end state on the definition issue. The
// issue status is written first, then the state through a write that only
// applies while the definition is still running, so a stale close (for example
// racing a /retry) changes nothing.
func (e *Engine) closeDefinition(ctx context.Context, def db.Issue, state RunState, status, comment string) error {
	fresh, err := e.Q.GetIssue(ctx, def.ID)
	if err != nil {
		return err
	}
	dm, ok := readDefMeta(fresh)
	if !ok || dm.State != RunRunning {
		return nil
	}
	updated := fresh
	if fresh.Status != status {
		updated, err = e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: def.ID, WorkspaceID: def.WorkspaceID, Status: status})
		if err != nil {
			return err
		}
	}
	if e.beforeClose != nil {
		e.beforeClose()
	}
	dm.State = state
	raw, err := json.Marshal(dm)
	if err != nil {
		return err
	}
	_, err = e.Q.SetWorkflowDefinitionState(ctx, db.SetWorkflowDefinitionStateParams{
		Value: raw, ID: def.ID, WorkspaceID: def.WorkspaceID, ExpectedState: string(RunRunning),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Fence lost: never restore an old status. Converge on the winner's
		// state below, which also repairs a status a concurrent closer with a
		// different outcome wrote.
		return e.convergeStatus(ctx, def)
	}
	if err != nil {
		return err
	}
	if err := e.convergeStatus(ctx, def); err != nil {
		return err
	}
	if updated.Status != fresh.Status {
		e.Events.IssueUpdated(ctx, fresh, updated)
	}
	if comment != "" {
		_, err = e.systemComment(ctx, updated, comment)
	}
	return err
}

// reopenDefinition returns a blocked workflow to running after a retry. It
// never overwrites a state that is already running.
func (e *Engine) reopenDefinition(ctx context.Context, def db.Issue) error {
	cur, err := e.Q.GetIssue(ctx, def.ID)
	if err != nil {
		return err
	}
	dm, ok := readDefMeta(cur)
	if !ok || dm.State != RunBlocked {
		return nil
	}
	dm.State = RunRunning
	raw, err := json.Marshal(dm)
	if err != nil {
		return err
	}
	if _, err := e.Q.SetWorkflowDefinitionState(ctx, db.SetWorkflowDefinitionStateParams{
		Value: raw, ID: cur.ID, WorkspaceID: cur.WorkspaceID, ExpectedState: string(RunBlocked),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: cur.ID, WorkspaceID: cur.WorkspaceID, Status: "in_progress"})
	if err != nil {
		return err
	}
	e.Events.IssueUpdated(ctx, cur, updated)
	return nil
}

// convergeStatus makes a closed definition's issue status match its state
// (done => done, blocked => blocked).
func (e *Engine) convergeStatus(ctx context.Context, def db.Issue) error {
	cur, err := e.Q.GetIssue(ctx, def.ID)
	if err != nil {
		return err
	}
	dm, ok := readDefMeta(cur)
	if !ok {
		return nil
	}
	want := ""
	switch dm.State {
	case RunDone:
		want = "done"
	case RunBlocked:
		want = "blocked"
	default:
		return nil
	}
	if cur.Status == want {
		return nil
	}
	if e.beforeConvergeWrite != nil {
		e.beforeConvergeWrite()
	}
	// Conditional on the state still being the one read, so a reopen that lands
	// in between is not overwritten.
	updated, err := e.Q.SetWorkflowDefinitionStatusIfState(ctx, db.SetWorkflowDefinitionStatusIfStateParams{
		ID: def.ID, WorkspaceID: def.WorkspaceID, Status: want, WantState: string(dm.State),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	e.Events.IssueUpdated(ctx, cur, updated)
	return nil
}
