package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type recordingCommentObserver struct{ ids []pgtype.UUID }

func (r *recordingCommentObserver) OnComment(_ context.Context, id pgtype.UUID) error {
	r.ids = append(r.ids, id)
	return nil
}

func TestExtWorkflowCommentListenerForwardsAgentComments(t *testing.T) {
	bus := events.New()
	obs := &recordingCommentObserver{}
	registerExtWorkflowCommentListener(bus, obs)
	fromHTTP, fromTask, byMember, bySystem := dbid.NewV7(), dbid.NewV7(), dbid.NewV7(), dbid.NewV7()
	publish := func(comment any) {
		bus.Publish(events.Event{Type: protocol.EventCommentCreated, Payload: map[string]any{"comment": comment}})
	}
	publish(handler.CommentResponse{ID: util.UUIDToString(fromHTTP), AuthorType: "agent"})
	publish(map[string]any{"id": util.UUIDToString(fromTask), "author_type": "agent"})
	publish(handler.CommentResponse{ID: util.UUIDToString(byMember), AuthorType: "member"})
	publish(map[string]any{"id": util.UUIDToString(bySystem), "author_type": "system"})
	publish("not a comment")
	if len(obs.ids) != 2 || obs.ids[0] != fromHTTP || obs.ids[1] != fromTask {
		t.Fatalf("forwarded %v, want the two agent comments", obs.ids)
	}
}
