package extworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The comment decision protocol (spec §6.3). Agents decide by posting one
// ext-workflow block with the existing CLI. A comment names no workflow task,
// so the engine identifies the deciding task from the comment's source task
// (stamped by the CLI's X-Task-ID) or, failing that, from the author's
// in-flight tasks in the run, and authorizes by role:
//
//	supervisor task focused on step S → on S's child issue: every action but request-rewind
//	supervisor conversation task      → on the parent, naming step: every human action, for the commenter
//	step task of step S               → on S's child issue: request-rewind only
//
// Other authors are ignored. A run participant whose block is invalid gets a
// protocol_error event and a system reply under its comment.

// ProtocolErrorReason is the protocol_error payload reason for a rejected block.
const ProtocolErrorReason = "invalid_block"

// NoDecisionReason is the protocol_error payload reason for a supervisor turn
// that ended without a decision.
const NoDecisionReason = "no_decision"

// OnComment handles comment:created for an agent comment that carries an
// ext-workflow block.
func (e *Engine) OnComment(ctx context.Context, commentID pgtype.UUID) error {
	if !e.Enabled() {
		return nil
	}
	c, err := e.q.GetComment(ctx, commentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load comment: %w", err)
	}
	if c.AuthorType != "agent" || c.DeletedAt.Valid || !ContainsBlock(c.Content) {
		return nil
	}
	call, ok, err := e.protocolCall(ctx, c)
	if err != nil || !ok {
		return err
	}
	d, _, perr := ParseBlock(c.Content)
	if perr == nil {
		var in DecideInput
		if in, perr = e.decideInput(ctx, call, d); perr == nil {
			perr = e.Decide(ctx, in)
		}
	}
	switch {
	case perr == nil, errors.Is(perr, ErrEngineDisabled):
		return nil
	case isProtocolError(perr):
		return e.rejectBlock(ctx, call, d, perr)
	}
	return perr
}

// protocolCall is a block posted by a run participant.
type protocolCall struct {
	snap    *RunSnapshot
	comment db.Comment
	task    db.AgentTaskQueue // the deciding task
	// childStep is the step whose child issue carries the comment; "" when
	// the comment is on the parent issue.
	childStep string
}

// protocolCall finds the run the commented issue belongs to and the author's
// deciding task. ok=false: not a run participant, ignore the comment.
func (e *Engine) protocolCall(ctx context.Context, c db.Comment) (protocolCall, bool, error) {
	call := protocolCall{comment: c}
	var runID pgtype.UUID
	step, err := e.q.GetExtWorkflowRunStepByIssue(ctx, db.GetExtWorkflowRunStepByIssueParams{IssueID: c.IssueID, WorkspaceID: c.WorkspaceID})
	switch {
	case err == nil:
		runID, call.childStep = step.RunID, step.NodeKey
	case errors.Is(err, pgx.ErrNoRows):
		run, err := e.q.GetActiveExtWorkflowRunByIssue(ctx, c.IssueID)
		if errors.Is(err, pgx.ErrNoRows) {
			return call, false, nil
		}
		if err != nil {
			return call, false, fmt.Errorf("load active run: %w", err)
		}
		runID = run.ID
	default:
		return call, false, fmt.Errorf("load step for issue: %w", err)
	}
	snap, err := e.LoadRun(ctx, runID)
	if errors.Is(err, ErrRunNotFound) {
		return call, false, nil
	}
	if err != nil {
		return call, false, err
	}
	call.snap = snap
	task, ok := pickDecidingTask(snap, c, call.childStep)
	if !ok {
		return call, false, nil
	}
	call.task = task
	return call, true, nil
}

// pickDecidingTask chooses among the author's in-flight tasks of the run: the
// comment's source task when it carries one, else the task whose role fits
// the issue the comment is on.
func pickDecidingTask(snap *RunSnapshot, c db.Comment, childStep string) (db.AgentTaskQueue, bool) {
	var mine []db.AgentTaskQueue
	for _, t := range snap.Active {
		if t.AgentID != c.AuthorID || (c.SourceTaskID.Valid && t.ID != c.SourceTaskID) {
			continue
		}
		mine = append(mine, t)
	}
	if len(mine) == 0 {
		return db.AgentTaskQueue{}, false
	}
	var prefer []func(db.AgentTaskQueue) bool
	if childStep != "" {
		stepID := snap.ByKey[childStep].ID
		prefer = append(prefer,
			func(t db.AgentTaskQueue) bool { return isFocusSupervisorTask(t) && t.ExtWorkflowStepID == stepID },
			func(t db.AgentTaskQueue) bool {
				return t.ExtWorkflowRole.String == RoleStep && t.ExtWorkflowStepID == stepID
			})
	} else {
		prefer = append(prefer, func(t db.AgentTaskQueue) bool { return t.ExtWorkflowKind.String == KindConversation })
	}
	for _, match := range prefer {
		for _, t := range mine {
			if match(t) {
				return t, true
			}
		}
	}
	return mine[0], true
}

func isFocusSupervisorTask(t db.AgentTaskQueue) bool {
	if t.ExtWorkflowRole.String != RoleSupervisor {
		return false
	}
	switch t.ExtWorkflowKind.String {
	case KindReview, KindFailure, KindRewindRequest:
		return true
	}
	return false
}

// decideInput authorizes the block for the deciding task's role (the table
// in spec §6.3) and turns it into a decision.
func (e *Engine) decideInput(ctx context.Context, call protocolCall, d Decision) (DecideInput, error) {
	snap, t := call.snap, call.task
	in := DecideInput{RunID: snap.Run.ID, Decision: d, ActorType: "agent", ActorID: call.comment.AuthorID, TaskID: t.ID}
	illegal := func(format string, args ...any) (DecideInput, error) {
		return in, fmt.Errorf("%w: "+format, append([]any{ErrIllegalDecision}, args...)...)
	}
	stepTitle := func(key string) string {
		if n, ok := snap.Def.NodeByKey(key); ok {
			return fmt.Sprintf("%q (%s)", n.Title, key)
		}
		return fmt.Sprintf("%q", key)
	}
	focus, _ := snap.KeyOf(t.ExtWorkflowStepID)
	key := ""
	switch {
	case call.childStep != "" && t.ExtWorkflowRole.String == RoleStep && focus == call.childStep:
		if d.Action != ActionRequestRewind {
			return illegal("a step agent may only post request-rewind")
		}
		if d.Step != "" && d.Step != call.childStep {
			return illegal("this is the child issue of step %s, not %q", stepTitle(call.childStep), d.Step)
		}
		key = call.childStep
	case call.childStep != "" && isFocusSupervisorTask(t) && focus == call.childStep:
		if d.Action == ActionRequestRewind {
			return illegal("request-rewind is for step agents; the supervisor decides with rewind")
		}
		if d.Step != "" && d.Step != call.childStep {
			return illegal("this is the child issue of step %s, not %q", stepTitle(call.childStep), d.Step)
		}
		key = call.childStep
	case call.childStep == "" && t.ExtWorkflowKind.String == KindConversation:
		if d.Action == ActionRequestRewind {
			return illegal("request-rewind is for step agents")
		}
		if d.Step == "" && d.Action != ActionAbort {
			return illegal("a decision on the parent issue must name the step with `step:`")
		}
		if d.Step != "" {
			if _, ok := snap.ByKey[d.Step]; !ok {
				return illegal("unknown step %q", d.Step)
			}
		}
		commenter, err := e.conversationCommenter(ctx, t)
		if err != nil {
			return in, err
		}
		in.OnBehalfOf = commenter
		key = d.Step
	case t.ExtWorkflowKind.String == KindSummary:
		return illegal("the summary takes no decision block; write a plain comment on the parent issue")
	case t.ExtWorkflowRole.String == RoleStep:
		return illegal("post request-rewind on the child issue of your own step %s", stepTitle(focus))
	case isFocusSupervisorTask(t):
		return illegal("post the decision on the child issue of step %s", stepTitle(focus))
	default:
		return illegal("this turn cannot decide here")
	}
	if key != "" {
		in.StepID = snap.ByKey[key].ID
		in.ExpectedStatus = StepStatus(snap.ByKey[key].Status)
	}
	return in, nil
}

// conversationCommenter is the member whose comment woke a conversation
// turn; the supervisor acts on that member's behalf.
func (e *Engine) conversationCommenter(ctx context.Context, t db.AgentTaskQueue) (pgtype.UUID, error) {
	if !t.TriggerCommentID.Valid {
		return pgtype.UUID{}, fmt.Errorf("%w: the conversation has no triggering comment", ErrForbidden)
	}
	trigger, err := e.q.GetComment(ctx, t.TriggerCommentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, fmt.Errorf("%w: the triggering comment is gone", ErrForbidden)
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("load triggering comment: %w", err)
	}
	if trigger.AuthorType != "member" {
		return pgtype.UUID{}, fmt.Errorf("%w: the conversation was not started by a person", ErrForbidden)
	}
	return trigger.AuthorID, nil
}

func isProtocolError(err error) bool {
	for _, target := range []error{
		ErrIllegalDecision, ErrStatusMismatch, ErrForbidden, ErrStepNotFound, ErrRunNotFound,
		ErrBlockMultiple, ErrBlockUnclosed, ErrBlockEmpty, ErrBlockInvalid,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// rejectBlock records a protocol_error and replies under the comment.
func (e *Engine) rejectBlock(ctx context.Context, call protocolCall, d Decision, cause error) error {
	c, snap := call.comment, call.snap
	issue, err := e.q.GetIssue(ctx, c.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load issue: %w", err)
	}
	stepKey := call.childStep
	if stepKey == "" {
		stepKey = d.Step
	}
	payload := map[string]any{
		"reason": ProtocolErrorReason, "error": cause.Error(),
		"comment_id": util.UUIDToString(c.ID), "issue_id": util.UUIDToString(c.IssueID),
		"task_id": util.UUIDToString(call.task.ID),
	}
	if d.Action != "" {
		payload["action"] = string(d.Action)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode protocol error: %w", err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := e.q.WithTx(tx)
	if _, err := q.CreateExtWorkflowRunEvent(ctx, db.CreateExtWorkflowRunEventParams{
		ID: dbid.NewV7(), RunID: snap.Run.ID, WorkspaceID: snap.Run.WorkspaceID, StepID: snap.ByKey[stepKey].ID,
		Kind: RunEventProtocolError, ActorType: "agent", ActorID: c.AuthorID, Payload: raw,
	}); err != nil {
		return fmt.Errorf("record protocol error: %w", err)
	}
	reply, err := q.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "system", ParentID: c.ID,
		Content: protocolErrorText(cause),
	})
	if err != nil {
		return fmt.Errorf("create protocol error reply: %w", err)
	}
	if err := q.TouchExtWorkflowRun(ctx, snap.Run.ID); err != nil {
		return fmt.Errorf("touch run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	e.tasks.ExtPublishSystemComment(issue, reply)
	if e.pub != nil {
		e.pub.Publish(protocol.EventExtWorkflowRunUpdated, util.UUIDToString(snap.Run.WorkspaceID), "system", "", map[string]any{
			"run_id": util.UUIDToString(snap.Run.ID), "issue_id": util.UUIDToString(snap.Run.IssueID), "workflow_id": util.UUIDToString(snap.Run.WorkflowID),
		})
	}
	return nil
}

func protocolErrorText(cause error) string {
	return fmt.Sprintf("The workflow did not apply the `%s` block above: %s.\n\nNothing changed. Fix the block and post it again in a new comment.", BlockLang, cause.Error())
}
