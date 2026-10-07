package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow (fork): comment-side glue for the workflow engine (spec §6.3,
// §6.4). The hooks in comment.go stay one line each.

// isNoteCommentOn is isNoteComment in the context of the comment's issue: a
// decision block (valid or not) is also a note, so it wakes no agent and the
// engine reads it from comment:created, but only while the engine is on and
// the issue is a workflow parent or step child. Anywhere else the fence is
// ordinary text. Every site that decides "does this comment trigger agents"
// calls this, so create, edit, replay and end-of-run re-evaluation agree.
func (h *Handler) isNoteCommentOn(ctx context.Context, issue db.Issue, content string) bool {
	if isNoteComment(content) {
		return true
	}
	if !extworkflow.ContainsBlock(content) || !h.ExtWorkflow.Enabled() {
		return false
	}
	if _, err := h.Queries.GetExtWorkflowRunStepByIssue(ctx, db.GetExtWorkflowRunStepByIssueParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID}); err == nil {
		return true
	}
	isParent, err := h.Queries.ExtWorkflowRunExistsForIssue(ctx, db.ExtWorkflowRunExistsForIssueParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	return err == nil && isParent
}

type extWorkflowCommentKey struct{}

// withExtWorkflowCommentTrigger marks ctx as the trigger pass of a stored
// comment. Only that pass may wake the supervisor: previews and the
// re-evaluation of old comments route the same comment without the mark.
func withExtWorkflowCommentTrigger(ctx context.Context, commentID pgtype.UUID) context.Context {
	return context.WithValue(ctx, extWorkflowCommentKey{}, commentID)
}

// extWorkflowParentComment is the assignee fallback for a workflow assignee:
// a person's comment that names nobody wakes the supervisor with a
// conversation turn. It never returns a platform trigger.
func (h *Handler) extWorkflowParentComment(ctx context.Context, issue db.Issue, authorType, authorID string) {
	commentID, _ := ctx.Value(extWorkflowCommentKey{}).(pgtype.UUID)
	if authorType != "member" || !commentID.Valid || !h.ExtWorkflow.Enabled() {
		return
	}
	memberID, err := util.ParseUUID(authorID)
	if err != nil {
		return
	}
	if err := h.ExtWorkflow.OnMemberParentComment(ctx, issue.ID, commentID, memberID); err != nil {
		slog.Warn("ext-workflow: conversation wake failed", "issue_id", uuidToString(issue.ID), "comment_id", uuidToString(commentID), "error", err)
	}
}

// ExtCommentFromEvent reads the comment id and author type of a
// comment:created event. The payload carries a CommentResponse from the HTTP
// handler and a map from the agent comment path in the task service.
func ExtCommentFromEvent(e events.Event) (pgtype.UUID, string, bool) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return pgtype.UUID{}, "", false
	}
	var id, authorType string
	switch c := payload["comment"].(type) {
	case CommentResponse:
		id, authorType = c.ID, c.AuthorType
	case map[string]any:
		id, _ = c["id"].(string)
		authorType, _ = c["author_type"].(string)
	default:
		return pgtype.UUID{}, "", false
	}
	commentID, err := util.ParseUUID(id)
	if err != nil {
		return pgtype.UUID{}, "", false
	}
	return commentID, authorType, true
}

// refuseExtWorkflowChildWakeup answers 409 when the issue is a step's child
// issue in a workflow run: the engine schedules that work, and a wakeup could
// start a task it does not track.
func (h *Handler) refuseExtWorkflowChildWakeup(w http.ResponseWriter, r *http.Request, issue db.Issue) bool {
	if _, err := h.Queries.GetExtWorkflowRunStepByIssue(r.Context(), db.GetExtWorkflowRunStepByIssueParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID}); err != nil {
		return false
	}
	writeError(w, http.StatusConflict, "this issue is a workflow step; the workflow engine schedules its work, so wakeups cannot be created on it")
	return true
}
