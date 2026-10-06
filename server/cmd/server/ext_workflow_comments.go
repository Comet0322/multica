package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ext-workflow (fork): the comment protocol listener (spec §6.3).

type extWorkflowCommentObserver interface {
	OnComment(ctx context.Context, commentID pgtype.UUID) error
}

// registerExtWorkflowCommentListener forwards agent comments to the engine,
// which applies the ext-workflow decision block they may carry. People's and
// system comments never decide through this path.
func registerExtWorkflowCommentListener(bus *events.Bus, engine extWorkflowCommentObserver) {
	bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
		commentID, authorType, ok := handler.ExtCommentFromEvent(e)
		if !ok || authorType != "agent" {
			return
		}
		// bus.Publish is synchronous on the publisher's goroutine: bound the work.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := engine.OnComment(ctx, commentID); err != nil {
			slog.Warn("ext-workflow: comment protocol failed", "comment_id", util.UUIDToString(commentID), "error", err)
		}
	})
}
