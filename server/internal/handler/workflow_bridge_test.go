package handler

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestWorkflowEventsIssueUpdatedShape(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Workflow bridge")
	cur, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	prev := cur
	prev.Status = "todo"
	cur.Status = "in_progress"

	got := make(chan events.Event, 1)
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		if p, ok := e.Payload.(map[string]any); ok {
			if r, ok := p["issue"].(IssueResponse); ok && r.ID == issueID {
				got <- e
			}
		}
	})
	testHandler.WorkflowEvents().IssueUpdated(context.Background(), prev, cur)

	select {
	case e := <-got:
		p := e.Payload.(map[string]any)
		if p["status_changed"] != true || p["prev_status"] != "todo" {
			t.Fatalf("payload = %+v", p)
		}
		if e.ActorType != "system" {
			t.Fatalf("actor type = %q, want system", e.ActorType)
		}
	case <-time.After(time.Second):
		t.Fatal("issue:updated was not published with an IssueResponse payload")
	}
}

func TestWorkflowEventsCommentCreated(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Workflow bridge comment")
	issue, _ := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	got := make(chan events.Event, 1)
	testHandler.Bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
		if p, ok := e.Payload.(map[string]any); ok {
			if c, ok := p["comment"].(CommentResponse); ok && c.IssueID == issueID {
				got <- e
			}
		}
	})
	testHandler.WorkflowEvents().CommentCreated(context.Background(), issue, db.Comment{
		ID: issue.ID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "system", Content: "hi", Type: "system",
	})
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("comment:created was not published with a CommentResponse payload")
	}
}
