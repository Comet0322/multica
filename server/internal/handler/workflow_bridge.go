package handler

import (
	"context"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// WorkflowEvents lets the workflow engine publish events in the exact shapes
// the realtime hub, activity log and listeners already expect, without the
// workflow package importing handler.
type WorkflowEvents struct{ h *Handler }

func (h *Handler) WorkflowEvents() *WorkflowEvents { return &WorkflowEvents{h: h} }

// IssuePayload is the IssueCreateOpts.BroadcastPayload hook.
func (w *WorkflowEvents) IssuePayload(issue db.Issue, atts []db.Attachment, labels []db.IssueLabel) map[string]any {
	ctx := context.Background()
	resp := issueToResponse(issue, w.h.getIssuePrefix(ctx, issue.WorkspaceID))
	w.h.fillStatusCategory(ctx, issue.WorkspaceID, &resp)
	if len(atts) > 0 {
		resp.Attachments = make([]AttachmentResponse, len(atts))
		for i, a := range atts {
			// The engine has no request, so use the server default (signed) mode.
			resp.Attachments[i] = w.h.attachmentToResponse(a, attachmentURLModeSigned)
		}
	}
	lr := labelsToResponse(labels)
	resp.Labels = &lr
	return map[string]any{"issue": resp}
}

// IssueUpdated publishes issue:updated for a status change made by the engine.
func (w *WorkflowEvents) IssueUpdated(ctx context.Context, prev, cur db.Issue) {
	resp := issueToResponse(cur, w.h.getIssuePrefix(ctx, cur.WorkspaceID))
	w.h.fillStatusCategory(ctx, cur.WorkspaceID, &resp)
	w.h.publish(protocol.EventIssueUpdated, uuidToString(cur.WorkspaceID), "system", "", map[string]any{
		"issue":               resp,
		"assignee_changed":    false,
		"status_changed":      prev.Status != cur.Status,
		"priority_changed":    false,
		"project_changed":     false,
		"start_date_changed":  false,
		"due_date_changed":    false,
		"description_changed": false,
		"title_changed":       false,
		"prev_title":          prev.Title,
		"prev_assignee_type":  textToPtr(prev.AssigneeType),
		"prev_assignee_id":    uuidToPtr(prev.AssigneeID),
		"prev_status":         prev.Status,
		"prev_priority":       prev.Priority,
		"prev_description":    textToPtr(prev.Description),
		"creator_type":        prev.CreatorType,
		"creator_id":          uuidToString(prev.CreatorID),
	})
}

// CommentCreated publishes comment:created for a system comment the engine
// inserted. It does not trigger agents (that happens only in CreateComment).
func (w *WorkflowEvents) CommentCreated(ctx context.Context, issue db.Issue, c db.Comment) {
	resp := commentToResponse(c, nil, nil)
	resp.IssueRevision = issue.Revision
	w.h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment":             resp,
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      issue.Revision,
	})
}
