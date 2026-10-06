package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow (fork): handler-side glue for the workflow engine. The engine
// cannot import handler, so the invoke gate and the event publisher reach it
// through ExtWorkflowBridge.

// ExtWorkflowBridge implements extworkflow.AgentAccess and extworkflow.Publisher.
type ExtWorkflowBridge struct{ h *Handler }

func NewExtWorkflowBridge(h *Handler) *ExtWorkflowBridge { return &ExtWorkflowBridge{h: h} }

// CanInvokeAgent applies canInvokeAgent to a stored actor. A member is judged
// as themselves; an agent actor has no request-scoped originator here, so it
// is judged like an unattributed agent (workspace-invocable agents only).
func (b *ExtWorkflowBridge) CanInvokeAgent(ctx context.Context, workspaceID pgtype.UUID, actorType string, actorID pgtype.UUID, agentID pgtype.UUID) (bool, error) {
	agent, err := b.h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if agent.ArchivedAt.Valid {
		return false, nil
	}
	actor, originator := uuidToString(actorID), ""
	if actorType == "member" {
		originator = actor
	}
	return b.h.canInvokeAgent(ctx, agent, actorType, actor, originator, uuidToString(workspaceID)), nil
}

func (b *ExtWorkflowBridge) Publish(eventType, workspaceID, actorType, actorID string, payload map[string]any) {
	b.h.publish(eventType, workspaceID, actorType, actorID, payload)
}

// validateExtWorkflowAssignee is validateAssigneePair for a workflow.
func (h *Handler) validateExtWorkflowAssignee(ctx context.Context, r *http.Request, workspaceID string, wsUUID, workflowID pgtype.UUID) (int, string) {
	if !h.ExtWorkflow.Enabled() {
		return http.StatusConflict, "workflow_engine_disabled"
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	actor, _ := util.ParseUUID(actorID)
	err := h.ExtWorkflow.ValidateAssignment(ctx, wsUUID, workflowID, actorType, actor)
	var refused *extworkflow.AssignError
	if errors.As(err, &refused) {
		return refused.Status, refused.Message
	}
	if err != nil {
		slog.Warn("ext-workflow: validate assignment failed", "workflow_id", uuidToString(workflowID), "error", err)
		return http.StatusInternalServerError, "failed to validate workflow assignment"
	}
	return 0, ""
}

// startExtWorkflowRun is dispatchIssueRun for a workflow assignee. The
// assignment already succeeded; a failed start is logged.
func (h *Handler) startExtWorkflowRun(ctx context.Context, issue db.Issue, actorType, actorID string) {
	if !h.ExtWorkflow.Enabled() {
		return
	}
	actor, _ := util.ParseUUID(actorID)
	if err := h.ExtWorkflow.StartRun(ctx, issue.ID, actorType, actor); err != nil {
		slog.Warn("ext-workflow: start run failed", "issue_id", uuidToString(issue.ID), "error", err)
	}
}

// notifyExtWorkflowParentChanged lets the engine stop a run whose parent was
// cancelled or reassigned away from its workflow.
func (h *Handler) notifyExtWorkflowParentChanged(ctx context.Context, prev db.Issue, statusChanged, assigneeChanged bool) {
	if !h.ExtWorkflow.Enabled() || prev.AssigneeType.String != "workflow" || (!statusChanged && !assigneeChanged) {
		return
	}
	if err := h.ExtWorkflow.OnParentChanged(ctx, prev.ID); err != nil {
		slog.Warn("ext-workflow: parent change failed", "issue_id", uuidToString(prev.ID), "error", err)
	}
}

// extWorkflowParentDeleting cancels the run of an issue about to be deleted.
func (h *Handler) extWorkflowParentDeleting(ctx context.Context, issue db.Issue) {
	if !h.ExtWorkflow.Enabled() || issue.AssigneeType.String != "workflow" {
		return
	}
	if err := h.ExtWorkflow.OnParentDeleted(ctx, issue.ID); err != nil {
		slog.Warn("ext-workflow: parent delete failed", "issue_id", uuidToString(issue.ID), "error", err)
	}
}
