package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type allowAllAgents struct{}

func (allowAllAgents) CanInvokeAgent(context.Context, pgtype.UUID, string, pgtype.UUID, pgtype.UUID) (bool, error) {
	return true, nil
}

// TestExtWorkflowChildActivitySkipsTheTriggerInbox runs one step with the
// production subscriber and notification listeners: the step agent's comment
// and status moves on its child issue must not reach the triggering member's
// inbox (spec §5.6: step progress belongs in the run panel).
func TestExtWorkflowChildActivitySkipsTheTriggerInbox(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := testutil.New(testPool, "", "")
	user := fx.User(t, "Workflow Inbox User", fmt.Sprintf("extwf-inbox-%d@multica.test", os.Getpid()))
	ws := fx.Workspace(t, "Workflow Inbox WS", fmt.Sprintf("extwf-inbox-%d", os.Getpid()), testutil.Cols{"issue_prefix": "WFI"})
	fx.WorkspaceID, fx.UserID = ws, user
	fx.Member(t, ws, user, "owner")
	rt := fx.Runtime(t, "wf-inbox-runtime")
	for _, stmt := range []string{
		`DELETE FROM issue_subscriber WHERE issue_id IN (SELECT id FROM issue WHERE workspace_id = $1)`,
		`DELETE FROM issue WHERE workspace_id = $1`,
		`DELETE FROM agent_task_queue WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1)`,
		`DELETE FROM comment WHERE workspace_id = $1`,
		`DELETE FROM inbox_item WHERE workspace_id = $1`,
		`DELETE FROM activity_log WHERE workspace_id = $1`,
		`DELETE FROM issue_child_event WHERE workspace_id = $1`,
		`DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE workspace_id = $1)`,
		`DELETE FROM issue_wakeup WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_run_event WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_run_step WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_run WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_node WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow WHERE workspace_id = $1`,
	} {
		fx.Cleanup(t, stmt, ws)
	}

	q := db.New(testPool)
	bus := events.New()
	registerSubscriberListeners(bus, testPool)
	registerNotificationListeners(bus, q)
	tasks := service.NewTaskService(q, testPool, nil, bus)
	issues := service.NewIssueService(q, testPool, bus, analytics.NoopClient{}, tasks)
	engine := extworkflow.NewEngine(extworkflow.Deps{Pool: testPool, Queries: q, Issues: issues, Tasks: tasks, Access: allowAllAgents{}, Enabled: true, AccessCacheTTL: -1})
	tasks.ExtWorkflow = engine

	supervisor, writer := fx.Agent(t, "Supervisor", rt), fx.Agent(t, "Writer", rt)
	wf := fx.Insert(t, "ext_workflow", testutil.Cols{"workspace_id": ws, "name": "Ship", "supervisor_agent_id": supervisor, "max_rewinds": 1, "creator_id": user})
	fx.Insert(t, "ext_workflow_node", testutil.Cols{
		"workflow_id": wf, "workspace_id": ws, "key": "draft", "title": "Draft", "agent_id": writer,
		"prompt": "Write it", "requires_review": false, "max_attempts": 2, "depends_on": []string{}, "position": 0,
	})
	parent := fx.Issue(t, "Ship feature", testutil.Cols{"status": "todo", "assignee_type": "workflow", "assignee_id": wf})
	fx.Exec(t, `UPDATE workspace SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) WHERE id = $1`, ws)
	addTestSubscriber(t, parent, "member", user, "creator")

	if err := engine.StartRun(ctx, util.MustParseUUID(parent), "member", util.MustParseUUID(user)); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	var child string
	fx.QueryRow(t, `SELECT issue_id FROM ext_workflow_run_step WHERE workspace_id = $1 AND node_key = 'draft'`, ws).Scan(&child)

	agentMoves := func(status, prev string) {
		fx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, child, status)
		bus.Publish(events.Event{
			Type: protocol.EventIssueUpdated, WorkspaceID: ws, ActorType: "agent", ActorID: writer,
			Payload: map[string]any{
				"issue": handler.IssueResponse{
					ID: child, WorkspaceID: ws, Title: "Ship feature · Draft", Status: status, Priority: "none",
					CreatorType: "agent", CreatorID: supervisor, ParentIssueID: &parent,
				},
				"assignee_changed": false, "status_changed": true, "prev_status": prev,
			},
		})
	}
	agentMoves("in_progress", "todo")
	comment := fx.Comment(t, child, "Drafted it.", testutil.Cols{"author_type": "agent", "author_id": writer})
	bus.Publish(events.Event{
		Type: protocol.EventCommentCreated, WorkspaceID: ws, ActorType: "agent", ActorID: writer,
		Payload: map[string]any{
			"comment":     map[string]any{"id": comment, "issue_id": child, "content": "Drafted it.", "author_type": "agent", "author_id": writer},
			"issue_title": "Ship feature · Draft", "issue_status": "in_progress",
		},
	})
	agentMoves("done", "in_progress")
	if err := (&service.IssueWakeupService{Tasks: tasks}).ProcessChildEvents(ctx, util.MustParseUUID(parent)); err != nil {
		t.Fatalf("ProcessChildEvents: %v", err)
	}

	var stepStatus string
	fx.QueryRow(t, `SELECT status FROM ext_workflow_run_step WHERE issue_id = $1`, child).Scan(&stepStatus)
	if stepStatus != "done" {
		t.Fatalf("step status = %s, want done", stepStatus)
	}
	if n := fx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_type = 'member' AND recipient_id = $1 AND issue_id = $2`, user, child); n != 0 {
		var types []string
		rows, _ := testPool.Query(ctx, `SELECT type FROM inbox_item WHERE recipient_id = $1 AND issue_id = $2`, user, child)
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			types = append(types, s)
		}
		rows.Close()
		t.Fatalf("trigger member has %d inbox items for the child issue (%v), want 0", n, types)
	}
}
