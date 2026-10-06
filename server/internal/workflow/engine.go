// server/internal/workflow/engine.go
package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

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
	steps, err = e.loadSteps(ctx, def)
	if err != nil {
		return err
	}
	metas := make([]StepMeta, len(steps))
	for i, s := range steps {
		metas[i] = s.meta
	}
	switch Evaluate(metas, dm.Total) {
	case OutcomeDone:
		return e.closeDefinition(ctx, def, RunDone, "done", "")
	case OutcomeBlocked:
		return e.closeDefinition(ctx, def, RunBlocked, "blocked", failureSummary(steps))
	case OutcomeBroken:
		return e.closeDefinition(ctx, def, RunBlocked, "blocked",
			"Workflow stopped: a step issue is missing (it may have been deleted). Restore it or recreate the workflow.")
	}
	return nil
}

// observe derives at most one event for a step from the world and applies it.
func (e *Engine) observe(ctx context.Context, s stepRow, all []stepRow) error {
	switch s.meta.Phase {
	case PhasePending:
		done := map[string]bool{}
		for _, o := range all {
			if o.meta.Phase == PhaseDone {
				done[o.meta.Node] = true
			}
		}
		for _, d := range s.meta.Deps {
			if !done[d] {
				return nil
			}
		}
		_, err := e.ApplyEvent(ctx, s.issue, EventDepsMet, pgtype.UUID{})
		return err
	case PhaseRunning:
		if s.issue.Status == "done" || s.issue.Status == "in_review" {
			_, err := e.ApplyEvent(ctx, s.issue, EventAgentFinished, pgtype.UUID{})
			return err
		}
		agentID, perr := util.ParseUUID(s.meta.AgentID)
		if perr != nil {
			return nil
		}
		status, err := e.Q.LatestWorkflowTaskStatus(ctx, db.LatestWorkflowTaskStatusParams{IssueID: s.issue.ID, AgentID: agentID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if status == "failed" {
			_, err := e.ApplyEvent(ctx, s.issue, EventAgentFailed, pgtype.UUID{})
			return err
		}
		return e.reconcileStatus(ctx, s.issue, s.meta.Phase)
	}
	return nil
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
	fresh, err := e.Q.GetIssue(ctx, step.ID)
	if err != nil {
		return false, err
	}
	step = fresh
	meta, ok := readStepMeta(step)
	if !ok {
		return false, nil
	}
	rule, ok := Next(meta, ev)
	if !ok {
		return false, nil
	}
	next := meta.Apply(rule)

	cur := step
	if want := PhaseStatus[next.Phase]; step.Status != want {
		cur, err = e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: step.ID, WorkspaceID: step.WorkspaceID, Status: want})
		if err != nil {
			return false, err
		}
		e.Events.IssueUpdated(ctx, step, cur)
	}
	if err := e.writeMeta(ctx, cur, next); err != nil {
		return false, err
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
			if _, err := e.systemComment(ctx, cur, fmt.Sprintf("Step failed after %d attempt(s). Reply `/retry` to run it again.", next.Attempts+1)); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

func (e *Engine) dispatch(ctx context.Context, issue db.Issue, meta StepMeta, trigger pgtype.UUID) error {
	var err error
	if trigger.Valid {
		agentID, perr := util.ParseUUID(meta.AgentID)
		if perr != nil {
			return perr
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
	meta.Phase = PhaseFailed
	if err := e.writeMeta(ctx, issue, meta); err != nil {
		return err
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: PhaseStatus[PhaseFailed]})
	if err == nil {
		e.Events.IssueUpdated(ctx, issue, updated)
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

// closeDefinition records the run's end state on the definition issue.
func (e *Engine) closeDefinition(ctx context.Context, def db.Issue, state RunState, status, comment string) error {
	dm, _ := readDefMeta(def)
	dm.State = state
	if err := e.writeMeta(ctx, def, dm); err != nil {
		return err
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: def.ID, WorkspaceID: def.WorkspaceID, Status: status})
	if err != nil {
		return err
	}
	e.Events.IssueUpdated(ctx, def, updated)
	if comment != "" {
		_, err = e.systemComment(ctx, updated, comment)
	}
	return err
}

// reopenDefinition returns a blocked workflow to running after a retry.
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
	if err := e.writeMeta(ctx, cur, dm); err != nil {
		return err
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: cur.ID, WorkspaceID: cur.WorkspaceID, Status: "in_progress"})
	if err != nil {
		return err
	}
	e.Events.IssueUpdated(ctx, cur, updated)
	return nil
}
