package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ext-workflow (fork): the queue entry point for workflow step and supervisor
// tasks, plus the post-commit broadcasts the engine needs from this package.

// ExtWorkflowEngineCancelReason is the failure_reason the engine stamps on the
// tasks it cancels itself, so their terminal event is not read as an agent
// outcome. Keep in sync with the Cancel*ExtWorkflow* queries.
const ExtWorkflowEngineCancelReason = "ext_workflow_engine"

var (
	// ErrExtAgentUnavailable: the target agent is gone, archived or has no runtime.
	ErrExtAgentUnavailable = errors.New("ext-workflow: agent is archived or has no runtime")
	// ErrExtTaskSlotBusy: the agent already holds this issue's pending slot
	// with a task the engine cannot reuse (claimed, or another run's).
	ErrExtTaskSlotBusy = errors.New("ext-workflow: the agent already holds a pending task on this issue")
)

// ExtWorkflowTaskParams describes one workflow task.
type ExtWorkflowTaskParams struct {
	IssueID, AgentID, RunID pgtype.UUID
	StepID                  pgtype.UUID // invalid for summary/conversation
	Role                    string      // "step" | "supervisor"
	Kind                    string      // "step","review","failure","rewind_request","summary","conversation"
	HandoffNote             string
	// ActorUserID is the accountable member (the run's triggering member);
	// invalid falls back to the issue's attribution chain.
	ActorUserID pgtype.UUID
	// TriggerCommentID is set for conversation tasks: the member comment that
	// woke the supervisor. It also gives the task that comment's thread slot.
	TriggerCommentID pgtype.UUID
}

// EnqueueExtWorkflowTask inserts a workflow task inside the engine's
// transaction, stamped with the four ext columns. Pending-task dedup applies
// per (issue, agent, role): an identical pending workflow task is returned as
// is, and a plain queued run of the same agent on the issue is adopted.
// Callers publish with PublishExtWorkflowTaskQueued after their commit.
func (s *TaskService) EnqueueExtWorkflowTask(ctx context.Context, tx pgx.Tx, p ExtWorkflowTaskParams) (db.AgentTaskQueue, error) {
	if !p.IssueID.Valid || !p.AgentID.Valid || !p.RunID.Valid || p.Role == "" || p.Kind == "" {
		return db.AgentTaskQueue{}, errors.New("ext-workflow: incomplete task params")
	}
	q := s.Queries.WithTx(tx)
	issue, err := q.GetIssue(ctx, p.IssueID)
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("load issue: %w", err)
	}
	agent, err := q.GetAgent(ctx, p.AgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTaskQueue{}, ErrExtAgentUnavailable
	}
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("load agent: %w", err)
	}
	if agent.ArchivedAt.Valid || !agent.RuntimeID.Valid {
		return db.AgentTaskQueue{}, ErrExtAgentUnavailable
	}
	attr := s.attributionForIssueTask(ctx, issue, p.TriggerCommentID, attribution.SourceDelegation, p.ActorUserID)
	if attr, err = s.applyAttributionFallback(ctx, attr, agent); err != nil {
		return db.AgentTaskQueue{}, err
	}
	overlay := s.buildRuntimeMCPOverlay(ctx, attr.UserID, agent)
	source, delegatedFrom, evidenceKind, evidenceRef := attributionCreateParams(attr)
	summary := pgtype.Text{String: "Workflow " + strings.ReplaceAll(p.Kind, "_", " "), Valid: true}
	if p.TriggerCommentID.Valid {
		summary = s.buildCommentTriggerSummary(ctx, issue.WorkspaceID, p.TriggerCommentID)
	}
	role := pgtype.Text{String: p.Role, Valid: true}
	kind := pgtype.Text{String: p.Kind, Valid: true}
	task, err := q.CreateExtWorkflowTask(ctx, db.CreateExtWorkflowTaskParams{
		ID:                   dbid.NewV7(),
		AgentID:              agent.ID,
		RuntimeID:            agent.RuntimeID,
		IssueID:              issue.ID,
		Priority:             priorityToInt(issue.Priority),
		TriggerCommentID:     p.TriggerCommentID,
		TriggerSummary:       summary,
		HandoffNote:          pgtype.Text{String: p.HandoffNote, Valid: p.HandoffNote != ""},
		OriginatorUserID:     attr.UserID,
		AccountableUserID:    attr.AccountableUserID,
		RuntimeMcpOverlay:    overlay.Overlay,
		RuntimeConnectedApps: overlay.ConnectedApps,
		OriginatorSource:     source,
		DelegatedFromTaskID:  delegatedFrom,
		RuleVersionID:        attr.RuleVersionID,
		TriggerEvidenceKind:  evidenceKind,
		TriggerEvidenceRefID: evidenceRef,
		ExtWorkflowRunID:     p.RunID,
		ExtWorkflowStepID:    p.StepID,
		ExtWorkflowRole:      role,
		ExtWorkflowKind:      kind,
	})
	if err == nil {
		return task, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTaskQueue{}, fmt.Errorf("create ext workflow task: %w", err)
	}
	holders, err := q.ListPendingSlotTasksForIssueAgent(ctx, db.ListPendingSlotTasksForIssueAgentParams{IssueID: issue.ID, AgentID: agent.ID})
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("list pending slot tasks: %w", err)
	}
	for _, h := range holders {
		if h.ExtWorkflowRunID == p.RunID && h.ExtWorkflowStepID == p.StepID && h.ExtWorkflowRole == role && h.ExtWorkflowKind == kind {
			return h, nil
		}
	}
	if !p.TriggerCommentID.Valid {
		for _, h := range holders {
			if h.Status != "queued" || h.CommentThreadID.Valid || h.ExtWorkflowRunID.Valid {
				continue
			}
			adopted, err := q.AdoptTaskForExtWorkflow(ctx, db.AdoptTaskForExtWorkflowParams{
				ID: h.ID, RunID: p.RunID, StepID: p.StepID, Role: role, Kind: kind, HandoffNote: p.HandoffNote,
			})
			if err == nil {
				return adopted, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return db.AgentTaskQueue{}, fmt.Errorf("adopt pending task: %w", err)
			}
		}
	}
	return db.AgentTaskQueue{}, ErrExtTaskSlotBusy
}

// PublishExtWorkflowTaskQueued announces a committed workflow task: the
// task:queued event and the runtime wakeup, as every enqueue path does.
func (s *TaskService) PublishExtWorkflowTaskQueued(ctx context.Context, task db.AgentTaskQueue) {
	s.broadcastTaskEvent(ctx, protocol.EventTaskQueued, task)
	s.NotifyTaskEnqueued(ctx, task)
}

// ExtBroadcastIssueUpdated emits issue:updated for a status the engine wrote
// directly (no HTTP handler involved).
func (s *TaskService) ExtBroadcastIssueUpdated(ctx context.Context, issue db.Issue, prevStatus string) {
	if s.Bus == nil {
		return
	}
	s.broadcastIssueUpdated(ctx, issue, prevStatus)
}

// ExtPublishSystemComment emits comment:created for a system comment the
// engine wrote in its transaction.
func (s *TaskService) ExtPublishSystemComment(issue db.Issue, created db.CreateCommentRow) {
	if s.Bus == nil {
		return
	}
	comment := created.Comment()
	fields := commentEventFields(comment)
	fields["revision"] = comment.Revision
	s.Bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   "system",
		Payload: map[string]any{
			"comment":        fields,
			"issue_title":    issue.Title,
			"issue_status":   issue.Status,
			"issue_revision": created.IssueRevision,
		},
	})
}

// ExtPublishInboxBatchArchived emits inbox:batch-archived for inbox items the
// engine archived, in the shape of the task_failed auto-archive.
func (s *TaskService) ExtPublishInboxBatchArchived(workspaceID, recipientID, issueID string, count int64) {
	if s.Bus == nil {
		return
	}
	s.Bus.Publish(events.Event{
		Type: protocol.EventInboxBatchArchived, WorkspaceID: workspaceID, ActorType: "system",
		Payload: map[string]any{"recipient_id": recipientID, "count": count, "issue_id": issueID, "reason": "ext_workflow_escalation_resolved"},
	})
}

// ExtPublishInbox emits inbox:new for an inbox item the engine wrote, in the
// same shape as the system wakeup notifications.
func (s *TaskService) ExtPublishInbox(item db.InboxItem, issueStatus string) {
	(&IssueWakeupService{Tasks: s}).publishSystemInbox(item, issueStatus)
}
