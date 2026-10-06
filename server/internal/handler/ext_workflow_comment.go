package handler

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow (fork): comment-side glue for the workflow engine (spec §6.3,
// §6.4). The hooks in comment.go stay one line each.

// isExtWorkflowBlockComment: the comment carries an ext-workflow decision
// block (valid or not). isNoteComment treats it as a note, so it wakes no
// agent; the engine reads it from comment:created.
func isExtWorkflowBlockComment(content string) bool { return extworkflow.ContainsBlock(content) }

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
