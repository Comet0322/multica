package extworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// OnMemberParentComment wakes the supervisor with a conversation turn when a
// person comments on a workflow parent without mentioning anyone (spec §6.4).
// As with any comment that wakes an agent, the commenter must be allowed to
// invoke the supervisor. A comment wakes at most one conversation turn.
//
// The turn's task carries the comment as its trigger: a decision block the
// supervisor posts during the turn is attributed to that comment's author
// (OnComment → conversationCommenter), and permission is checked against
// them, not the supervisor.
func (e *Engine) OnMemberParentComment(ctx context.Context, issueID, commentID, memberID pgtype.UUID) error {
	if !e.Enabled() || !commentID.Valid || !memberID.Valid {
		return nil
	}
	run, err := e.q.GetActiveExtWorkflowRunByIssue(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load active run: %w", err)
	}
	woken, err := e.q.HasExtWorkflowConversationTaskForComment(ctx, commentID)
	if err != nil {
		return fmt.Errorf("check conversation task: %w", err)
	}
	if woken {
		return nil
	}
	var def Definition
	if err := json.Unmarshal(run.Definition, &def); err != nil {
		return fmt.Errorf("decode run definition: %w", err)
	}
	supervisor, err := util.ParseUUID(def.SupervisorAgentID)
	if err != nil {
		return nil
	}
	if e.access == nil {
		return nil
	}
	ok, err := e.access.CanInvokeAgent(ctx, run.WorkspaceID, "member", memberID, supervisor)
	if err != nil {
		return fmt.Errorf("check supervisor access: %w", err)
	}
	if !ok {
		return nil
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	task, err := e.tasks.EnqueueExtWorkflowTask(ctx, tx, service.ExtWorkflowTaskParams{
		IssueID: issueID, AgentID: supervisor, RunID: run.ID, Role: RoleSupervisor, Kind: KindConversation,
		TriggerCommentID: commentID, ActorUserID: memberID,
		HandoffNote: "Workflow supervisor: a person commented on this workflow issue. Your instructions carry the run briefing.",
	})
	if errors.Is(err, service.ErrExtAgentUnavailable) || errors.Is(err, service.ErrExtTaskSlotBusy) {
		slog.Warn("ext-workflow: conversation wake dropped", "run_id", util.UUIDToString(run.ID),
			"comment_id", util.UUIDToString(commentID), "error", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("enqueue conversation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	// Enqueue may hand back an existing task; announce only this comment's.
	if task.TriggerCommentID != commentID {
		slog.Warn("ext-workflow: conversation enqueue returned another comment's task",
			"run_id", util.UUIDToString(run.ID), "comment_id", util.UUIDToString(commentID),
			"task_id", util.UUIDToString(task.ID))
		return nil
	}
	e.tasks.PublishExtWorkflowTaskQueued(ctx, task)
	return nil
}
