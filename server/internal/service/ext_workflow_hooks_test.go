package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type extChildCall struct {
	parent pgtype.UUID
	events []ExtChildEvent
}

// fakeExtHooks records engine calls.
type fakeExtHooks struct {
	mu         sync.Mutex
	childCalls []extChildCall
	starts     []pgtype.UUID
	afterRuns  int
	childErr   error
}

func (f *fakeExtHooks) ValidateAssignment(context.Context, pgtype.UUID, pgtype.UUID, string, pgtype.UUID) error {
	return nil
}

func (f *fakeExtHooks) StartRun(_ context.Context, issueID pgtype.UUID, _ string, _ pgtype.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, issueID)
	return nil
}

func (f *fakeExtHooks) OnChildEvents(ctx context.Context, _ pgx.Tx, parentID pgtype.UUID, evs []ExtChildEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.childErr != nil {
		return f.childErr
	}
	f.childCalls = append(f.childCalls, extChildCall{parent: parentID, events: evs})
	if after := ExtAfterCommitFrom(ctx); after != nil {
		after.Add(func() { f.mu.Lock(); f.afterRuns++; f.mu.Unlock() })
	}
	return nil
}

// workflowFamily is a parent assigned to a (fake) workflow and one child that
// just closed, with the wakeup rows processing creates cleaned up.
func workflowFamily(t *testing.T, f principalFixture, assigneeType string) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	parent := f.Issue(t, "Workflow parent", testutil.Cols{"assignee_type": assigneeType, "assignee_id": dbid.NewV7()})
	child := f.Issue(t, "Workflow child", testutil.Cols{"parent_issue_id": parent})
	f.Cleanup(t, `DELETE FROM issue_child_event WHERE parent_id = $1`, parent)
	f.Cleanup(t, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id = $1)`, parent)
	f.Cleanup(t, `DELETE FROM issue_wakeup WHERE issue_id = $1`, parent)
	f.Exec(t, `UPDATE issue SET status = 'done' WHERE id = $1`, child)
	return parseTestUUID(t, parent), parseTestUUID(t, child)
}

func TestProcessChildEventsHandsWorkflowParentToEngine(t *testing.T) {
	f, _ := newPrincipalFixture(t)
	parent, child := workflowFamily(t, f, "workflow")
	hooks := &fakeExtHooks{}
	f.svc.TaskSvc.ExtWorkflow = hooks

	if err := (&IssueWakeupService{Tasks: f.svc.TaskSvc}).ProcessChildEvents(context.Background(), parent); err != nil {
		t.Fatalf("ProcessChildEvents: %v", err)
	}
	if len(hooks.childCalls) != 1 || hooks.childCalls[0].parent != parent {
		t.Fatalf("engine calls = %+v", hooks.childCalls)
	}
	kinds := map[string]bool{}
	for _, ev := range hooks.childCalls[0].events {
		if ev.ChildID != child {
			t.Fatalf("event for %v, want child %v", ev.ChildID, child)
		}
		kinds[ev.Kind] = true
	}
	if !kinds["attached"] || !kinds["closed"] {
		t.Fatalf("kinds = %v, want attached and closed", kinds)
	}
	if hooks.afterRuns != 1 {
		t.Fatalf("after-commit ran %d times, want 1", hooks.afterRuns)
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_child_event WHERE parent_id = $1 AND processed_at IS NULL`, parent); n != 0 {
		t.Fatalf("%d events left unprocessed", n)
	}
}

func TestProcessChildEventsKeepsEventsWhenTheEngineFails(t *testing.T) {
	f, _ := newPrincipalFixture(t)
	parent, _ := workflowFamily(t, f, "workflow")
	hooks := &fakeExtHooks{childErr: errors.New("engine down")}
	f.svc.TaskSvc.ExtWorkflow = hooks

	if err := (&IssueWakeupService{Tasks: f.svc.TaskSvc}).ProcessChildEvents(context.Background(), parent); err == nil {
		t.Fatal("ProcessChildEvents succeeded, want the engine error")
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_child_event WHERE parent_id = $1 AND processed_at IS NULL`, parent); n == 0 {
		t.Fatal("events were consumed although the engine failed")
	}
}

func TestProcessChildEventsSkipsOtherParents(t *testing.T) {
	f, owner := newPrincipalFixture(t)
	agent := f.privateAgentOwnedBy(t, owner, "plain-parent")
	parent := parseTestUUID(t, f.Issue(t, "Agent parent", testutil.Cols{"assignee_type": "agent", "assignee_id": agent}))
	f.Cleanup(t, `DELETE FROM issue_child_event WHERE parent_id = $1`, parent)
	f.Cleanup(t, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id = $1)`, parent)
	f.Cleanup(t, `DELETE FROM issue_wakeup WHERE issue_id = $1`, parent)
	f.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, parent)
	f.Issue(t, "Child", testutil.Cols{"parent_issue_id": util.UUIDToString(parent)})
	hooks := &fakeExtHooks{}
	f.svc.TaskSvc.ExtWorkflow = hooks
	if err := (&IssueWakeupService{Tasks: f.svc.TaskSvc}).ProcessChildEvents(context.Background(), parent); err != nil {
		t.Fatalf("ProcessChildEvents: %v", err)
	}
	if len(hooks.childCalls) != 0 {
		t.Fatalf("engine called for an agent parent: %+v", hooks.childCalls)
	}
}

func extWorkflowServiceFixture(t *testing.T) (principalFixture, *IssueService, *fakeExtHooks, pgtype.UUID, pgtype.UUID) {
	t.Helper()
	f, owner := newPrincipalFixture(t)
	supervisor := f.privateAgentOwnedBy(t, owner, "supervisor")
	workflow := f.Insert(t, "ext_workflow", testutil.Cols{
		"workspace_id": f.WorkspaceID, "name": "Ship", "supervisor_agent_id": supervisor, "creator_id": owner,
	})
	hooks := &fakeExtHooks{}
	f.svc.TaskSvc.ExtWorkflow = hooks
	issues := NewIssueService(f.q, f.Pool, events.New(), analytics.NoopClient{}, f.svc.TaskSvc)
	return f, issues, hooks, parseTestUUID(t, workflow), parseTestUUID(t, supervisor)
}

func TestWillEnqueueRunStartsAWorkflowRun(t *testing.T) {
	f, issues, _, workflow, supervisor := extWorkflowServiceFixture(t)
	ctx := context.Background()
	id := f.Issue(t, "Parked", testutil.Cols{"status": "backlog", "assignee_type": "workflow", "assignee_id": workflow})
	issue, err := f.q.GetIssue(ctx, parseTestUUID(t, id))
	if err != nil {
		t.Fatal(err)
	}
	issue.Status = "todo"
	in := IssueTriggerInput{Issue: issue, PrevStatus: "backlog", StatusChanged: true}

	trigger, ok := issues.WillEnqueueRun(ctx, in, IssueTriggerProbe{})
	if !ok || trigger.AssigneeType != "workflow" || trigger.AgentID != supervisor || trigger.Source != RunSourceStatus {
		t.Fatalf("trigger = %+v, %v", trigger, ok)
	}

	f.Insert(t, "ext_workflow_run", testutil.Cols{
		"workspace_id": f.WorkspaceID, "workflow_id": workflow, "issue_id": id, "triggered_by_type": "member",
		"triggered_by_id": f.UserID, "definition": testutil.Raw(`'{}'::jsonb`),
	})
	if _, ok := issues.WillEnqueueRun(ctx, in, IssueTriggerProbe{}); ok {
		t.Fatal("a second run was promised while one is active")
	}

	f.svc.TaskSvc.ExtWorkflow = nil
	f.Exec(t, `DELETE FROM ext_workflow_run WHERE issue_id = $1`, id)
	if _, ok := issues.WillEnqueueRun(ctx, in, IssueTriggerProbe{}); ok {
		t.Fatal("a run was promised with the engine off")
	}
}

func TestCreateWorkflowIssueStartsRunOutsideBacklog(t *testing.T) {
	f, issues, hooks, workflow, _ := extWorkflowServiceFixture(t)
	ctx := context.Background()
	create := func(status string) db.Issue {
		res, err := issues.Create(ctx, IssueCreateParams{
			WorkspaceID: parseTestUUID(t, f.WorkspaceID), Title: "Ship " + status, Status: status, Priority: "none",
			AssigneeType: pgtype.Text{String: "workflow", Valid: true}, AssigneeID: workflow,
			CreatorType: "member", CreatorID: parseTestUUID(t, f.UserID),
		}, IssueCreateOpts{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return res.Issue
	}
	parked := create("backlog")
	started := create("todo")
	if len(hooks.starts) != 1 || hooks.starts[0] != started.ID || hooks.starts[0] == parked.ID {
		t.Fatalf("StartRun calls = %v, want only %v", hooks.starts, started.ID)
	}
}

func TestRerunIssueRefusesWorkflowWork(t *testing.T) {
	f, _, _, workflow, supervisor := extWorkflowServiceFixture(t)
	ctx := context.Background()
	issue := f.Issue(t, "Run by workflow", testutil.Cols{"assignee_type": "workflow", "assignee_id": workflow})
	if _, err := f.svc.TaskSvc.RerunIssue(ctx, parseTestUUID(t, issue), pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, nil); !errors.Is(err, ErrRerunWorkflowIssue) {
		t.Fatalf("rerun workflow issue: %v", err)
	}
	agent := util.UUIDToString(supervisor)
	var runtimeID string
	f.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, agent).Scan(&runtimeID)
	child := f.Issue(t, "Workflow step", testutil.Cols{"assignee_type": "agent", "assignee_id": agent})
	task := f.Task(t, agent, testutil.Cols{"issue_id": child, "runtime_id": runtimeID, "status": "completed", "ext_workflow_run_id": dbid.NewV7(), "ext_workflow_role": "step"})
	if _, err := f.svc.TaskSvc.RerunIssue(ctx, parseTestUUID(t, child), parseTestUUID(t, task), pgtype.UUID{}, pgtype.UUID{}, nil); !errors.Is(err, ErrRerunWorkflowIssue) {
		t.Fatalf("rerun workflow task: %v", err)
	}
}

// The child_done system rule must wake nobody for a workflow parent: the
// engine owns it. resolveWakeTarget's default branch already answers "none".
func TestResolveWakeTargetIgnoresWorkflowParents(t *testing.T) {
	f, _ := newPrincipalFixture(t)
	issue, err := f.q.GetIssue(context.Background(), parseTestUUID(t, f.Issue(t, "Workflow parent", testutil.Cols{"assignee_type": "workflow", "assignee_id": dbid.NewV7()})))
	if err != nil {
		t.Fatal(err)
	}
	target, err := resolveWakeTarget(context.Background(), f.q, issue)
	if err != nil || target.Type != "none" || target.Agent.ID.Valid {
		t.Fatalf("target = %+v, %v; want none", target, err)
	}
}
