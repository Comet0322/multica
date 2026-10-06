package extworkflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Decisions and cancellation from people (the run API) and from agents (the
// comment protocol). Both end in Advance; this file only resolves the step,
// checks who may act and which actions they may take (spec §6.3, §7.2).

// ErrStepNotFound: the step id does not belong to the run.
var ErrStepNotFound = errors.New("workflow run step not found")

// DecideInput is one decision on a run step.
//
// Authorization: member actors are authorized inside Decide through
// MemberCanDecide (current workspace membership plus trigger, creator or
// admin). Decide is a low-level gate for agent actors: a caller passing
// ActorType "agent" must already have bound that agent to this run, meaning it
// is the run's supervisor or the step's agent with an in-flight ext task (the
// comment protocol in OnComment enforces this). Only OnBehalfOf is checked here.
type DecideInput struct {
	RunID pgtype.UUID
	// StepID may be invalid only for abort.
	StepID   pgtype.UUID
	Decision Decision
	// ActorType is "member" (the run API) or "agent" (the comment protocol).
	ActorType string
	ActorID   pgtype.UUID
	// OnBehalfOf is the member a supervisor acts for in a conversation turn;
	// the permission check is made against that member.
	OnBehalfOf pgtype.UUID
	// TaskID is the agent task that decided; cancellations spare it.
	TaskID pgtype.UUID
	// ExpectedStatus guards against a stale UI ("" = no check).
	ExpectedStatus StepStatus
}

// HumanActions are the decisions a person (or a supervisor acting for one)
// may take. escalate and request-rewind belong to the agents.
var HumanActions = []DecisionAction{ActionApprove, ActionRedo, ActionRetry, ActionSkip, ActionRewind, ActionAbort}

func isHumanAction(a DecisionAction) bool {
	for _, h := range HumanActions {
		if h == a {
			return true
		}
	}
	return false
}

// Decide applies a decision. Errors: ErrEngineDisabled, ErrRunNotFound,
// ErrStepNotFound, ErrForbidden, ErrIllegalDecision, ErrStatusMismatch.
func (e *Engine) Decide(ctx context.Context, in DecideInput) error {
	if !e.Enabled() {
		return ErrEngineDisabled
	}
	if err := ValidateDecision(in.Decision); err != nil {
		return err
	}
	snap, err := e.LoadRun(ctx, in.RunID)
	if err != nil {
		return err
	}
	key := ""
	if in.StepID.Valid {
		k, ok := snap.KeyOf(in.StepID)
		if !ok {
			return ErrStepNotFound
		}
		key = k
	} else if in.Decision.Action != ActionAbort {
		return fmt.Errorf("%w: %s needs a step", ErrIllegalDecision, in.Decision.Action)
	}
	switch in.ActorType {
	case "member":
		if !isHumanAction(in.Decision.Action) {
			return fmt.Errorf("%w: %s is reserved for agents", ErrIllegalDecision, in.Decision.Action)
		}
		if err := e.requireDecider(ctx, snap.Run, in.ActorID); err != nil {
			return err
		}
	case "agent":
		if in.OnBehalfOf.Valid {
			if !isHumanAction(in.Decision.Action) {
				return fmt.Errorf("%w: %s cannot be taken on behalf of a person", ErrIllegalDecision, in.Decision.Action)
			}
			if err := e.requireDecider(ctx, snap.Run, in.OnBehalfOf); err != nil {
				return err
			}
		}
	default:
		return ErrForbidden
	}
	d := in.Decision
	d.Step = key
	return e.Advance(ctx, in.RunID, AdvanceInput{
		StepKey:        key,
		Event:          Event{Kind: EvDecision, Decision: d},
		Actor:          Actor{Type: in.ActorType, ID: in.ActorID, OnBehalfOf: in.OnBehalfOf, TaskID: in.TaskID},
		ExpectedStatus: in.ExpectedStatus,
	})
}

// CancelRun stops an active run for a person (spec §5.6, §7.2).
func (e *Engine) CancelRun(ctx context.Context, runID pgtype.UUID, actorType string, actorID pgtype.UUID) error {
	if !e.Enabled() {
		return ErrEngineDisabled
	}
	snap, err := e.LoadRun(ctx, runID)
	if err != nil {
		return err
	}
	if actorType != "member" {
		return ErrForbidden
	}
	if err := e.requireDecider(ctx, snap.Run, actorID); err != nil {
		return err
	}
	if !RunStatus(snap.Run.Status).Active() {
		return fmt.Errorf("%w: the run is %s", ErrStatusMismatch, snap.Run.Status)
	}
	return e.Advance(ctx, runID, AdvanceInput{
		Event: Event{Kind: EvCancelRun, Reason: "a person cancelled the run"},
		Actor: Actor{Type: "member", ID: actorID},
	})
}

func (e *Engine) requireDecider(ctx context.Context, run db.ExtWorkflowRun, userID pgtype.UUID) error {
	ok, err := e.MemberCanDecide(ctx, run, userID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// MemberCanDecide reports whether a member may decide on, or cancel, a run:
// the member who triggered it, the workflow's creator, or a workspace
// owner/admin (spec §7.2).
func (e *Engine) MemberCanDecide(ctx context.Context, run db.ExtWorkflowRun, userID pgtype.UUID) (bool, error) {
	if !userID.Valid {
		return false, nil
	}
	member, err := e.q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: userID, WorkspaceID: run.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load member: %w", err)
	}
	if run.TriggeredByType == "member" && run.TriggeredByID == userID {
		return true, nil
	}
	if member.Role == "owner" || member.Role == "admin" {
		return true, nil
	}
	wf, err := e.q.GetExtWorkflowInWorkspace(ctx, db.GetExtWorkflowInWorkspaceParams{ID: run.WorkflowID, WorkspaceID: run.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load workflow: %w", err)
	}
	return wf.CreatorID == userID, nil
}
