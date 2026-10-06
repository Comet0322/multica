package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// withExtWorkflowEngine wires a live engine into the shared test handler for
// one test, the way cmd/server does, and restores the previous wiring.
func withExtWorkflowEngine(t *testing.T) *extworkflow.Engine {
	t.Helper()
	bridge := NewExtWorkflowBridge(testHandler)
	engine := extworkflow.NewEngine(extworkflow.Deps{
		Pool: testPool, Queries: testHandler.Queries, Issues: testHandler.IssueService, Tasks: testHandler.TaskService,
		Access: bridge, Publisher: bridge, Enabled: true,
	})
	setExtWorkflowEngine(t, engine)
	return engine
}

func setExtWorkflowEngine(t *testing.T, engine *extworkflow.Engine) {
	t.Helper()
	prevHandler, prevTasks := testHandler.ExtWorkflow, testHandler.TaskService.ExtWorkflow
	testHandler.ExtWorkflow = engine
	testHandler.TaskService.ExtWorkflow = nil
	if engine != nil {
		testHandler.TaskService.ExtWorkflow = engine
	}
	t.Cleanup(func() {
		testHandler.ExtWorkflow = prevHandler
		testHandler.TaskService.ExtWorkflow = prevTasks
	})
}

var hookWorkflowSeq atomic.Int64

// hookWorkflow creates a one-node workflow whose node runs nodeAgent.
func hookWorkflow(t *testing.T, nodeAgent string) string {
	t.Helper()
	// A test may build several workflows; names are unique per workspace.
	name := fmt.Sprintf("%s-%d", t.Name(), hookWorkflowSeq.Add(1))
	supervisor := createHandlerTestAgent(t, name+"-supervisor", nil)
	if nodeAgent == "" {
		nodeAgent = createHandlerTestAgent(t, name+"-worker", nil)
	}
	wf := createExtWorkflowAs(t, "", name, supervisor)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"nodes": []any{extWFNode("build", nodeAgent)}}).Want(http.StatusOK)
	return wf.ID
}

// cleanupWorkflowIssue removes an issue the handler created and everything a
// run hung off it. Registered statements run in reverse order.
func cleanupWorkflowIssue(t *testing.T, issueID string) {
	t.Helper()
	dbfx.Cleanup(t, `DELETE FROM issue WHERE id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE parent_issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM ext_workflow_run WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1 OR issue_id IN (SELECT id FROM issue WHERE parent_issue_id = $1)`, issueID)
	dbfx.Cleanup(t, `DELETE FROM ext_workflow_run_step WHERE run_id IN (SELECT id FROM ext_workflow_run WHERE issue_id = $1)`, issueID)
	dbfx.Cleanup(t, `DELETE FROM ext_workflow_run_event WHERE run_id IN (SELECT id FROM ext_workflow_run WHERE issue_id = $1)`, issueID)
	dbfx.Cleanup(t, `DELETE FROM issue_child_event WHERE parent_id = $1`, issueID)
}

func createWorkflowIssue(t *testing.T, workflowID, status string) *testutil.Response {
	t.Helper()
	resp := testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": t.Name(), "status": status, "assignee_type": "workflow", "assignee_id": workflowID,
	}))
	if resp.Code == http.StatusCreated {
		var issue IssueResponse
		resp.JSON(&issue)
		cleanupWorkflowIssue(t, issue.ID)
	}
	return resp
}

func activeRunStatus(t *testing.T, issueID string) string {
	t.Helper()
	status := ""
	_ = testPool.QueryRow(context.Background(), `SELECT status FROM ext_workflow_run WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1`, issueID).Scan(&status)
	return status
}

func TestExtWorkflowAssignRequiresTheEngine(t *testing.T) {
	requireExtWorkflowDB(t)
	setExtWorkflowEngine(t, nil)
	wf := hookWorkflow(t, "")
	resp := createWorkflowIssue(t, wf, "todo").Want(http.StatusConflict)
	if !strings.Contains(resp.Text(), "workflow_engine_disabled") {
		t.Fatalf("body = %s", resp.Text())
	}
}

func TestExtWorkflowAssignStartsARun(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	wf := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wf, "todo").Want(http.StatusCreated).JSON(&issue)

	if got := activeRunStatus(t, issue.ID); got != "running" {
		t.Fatalf("run status = %q, want running", got)
	}
	var kind, role string
	dbfx.QueryRow(t, `
		SELECT t.ext_workflow_kind, t.ext_workflow_role FROM agent_task_queue t
		JOIN issue c ON c.id = t.issue_id
		WHERE c.parent_issue_id = $1 AND t.status = 'queued'`, issue.ID).Scan(&kind, &role)
	if kind != "step" || role != "step" {
		t.Fatalf("child task kind=%q role=%q", kind, role)
	}
}

func TestExtWorkflowBacklogStartsOnPromotion(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	wf := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wf, "backlog").Want(http.StatusCreated).JSON(&issue)
	if got := activeRunStatus(t, issue.ID); got != "" {
		t.Fatalf("backlog started a run (%s)", got)
	}
	req := withURLParam(newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"status": "todo"}), "id", issue.ID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	if got := activeRunStatus(t, issue.ID); got != "running" {
		t.Fatalf("run status after promotion = %q, want running", got)
	}
}

func TestExtWorkflowParentCancelStopsTheRun(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	wf := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wf, "todo").Want(http.StatusCreated).JSON(&issue)

	req := withURLParam(newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"status": "cancelled"}), "id", issue.ID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	if got := activeRunStatus(t, issue.ID); got != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", got)
	}
	if n := dbfx.Count(t, `
		SELECT count(*) FROM agent_task_queue t JOIN issue c ON c.id = t.issue_id
		WHERE c.parent_issue_id = $1 AND t.status <> 'cancelled'`, issue.ID); n != 0 {
		t.Fatalf("%d step tasks still active", n)
	}
}

func TestExtWorkflowAssignNeedsInvokeOnEveryAgent(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	privateAgent, _, member := privateAgentTestFixture(t)
	wf := hookWorkflow(t, privateAgent)
	resp := testutil.Call(t, testHandler.CreateIssue, newRequestAs(member, "POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": t.Name(), "status": "todo", "assignee_type": "workflow", "assignee_id": wf,
	}))
	resp.Want(http.StatusForbidden)
}

func TestExtWorkflowRerunIsAConflict(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	wf := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wf, "todo").Want(http.StatusCreated).JSON(&issue)
	req := withURLParam(newRequest("POST", "/api/issues/"+issue.ID+"/rerun", nil), "id", issue.ID)
	testutil.Call(t, testHandler.RerunIssue, req).Want(http.StatusConflict)
}

func TestIsIssueActorTypeAcceptsWorkflow(t *testing.T) {
	if !isIssueActorType("workflow") || isIssueActorType("robot") {
		t.Fatal("isIssueActorType must accept workflow and only the known types")
	}
}

func TestInvolvesUserIncludesWorkflowsTheyCreated(t *testing.T) {
	requireExtWorkflowDB(t)
	setExtWorkflowEngine(t, nil)
	wf := hookWorkflow(t, "")
	issueID := dbfx.Issue(t, t.Name(), testutil.Cols{"assignee_type": "workflow", "assignee_id": wf})
	for _, query := range []string{"", "&open_only=true"} {
		req := newRequest("GET", "/api/issues?workspace_id="+testWorkspaceID+"&involves_user_id="+testUserID+query, nil)
		var body struct {
			Issues []IssueResponse `json:"issues"`
		}
		testutil.Call(t, testHandler.ListIssues, req).Want(http.StatusOK).JSON(&body)
		found := false
		for _, is := range body.Issues {
			found = found || is.ID == issueID
		}
		if !found {
			t.Fatalf("involves_user_id%s did not list the workflow issue %s", query, issueID)
		}
	}
}

func TestExtWorkflowReassignCancelsTheRunAndStartsTheNewOne(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	wfA := hookWorkflow(t, "")
	wfB := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wfA, "todo").Want(http.StatusCreated).JSON(&issue)
	req := withURLParam(newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"assignee_type": "workflow", "assignee_id": wfB}), "id", issue.ID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	if n := dbfx.Count(t, `SELECT count(*) FROM ext_workflow_run WHERE issue_id=$1 AND workflow_id=$2 AND status='cancelled'`, issue.ID, wfA); n != 1 {
		t.Fatalf("old run not cancelled (%d)", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ext_workflow_run WHERE issue_id=$1 AND workflow_id=$2 AND status IN ('running','waiting_human')`, issue.ID, wfB); n != 1 {
		t.Fatalf("want exactly one active run of the new workflow, got %d", n)
	}
	if n := dbfx.Count(t, `
		SELECT count(*) FROM agent_task_queue t
		JOIN ext_workflow_run r ON r.id = t.ext_workflow_run_id
		WHERE r.issue_id = $1 AND r.workflow_id = $2 AND t.status NOT IN ('cancelled','completed','failed')`, issue.ID, wfA); n != 0 {
		t.Fatalf("%d tasks of the old run still active", n)
	}
}

func TestExtWorkflowReassignToAnAgentCancelsTheRun(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	wf := hookWorkflow(t, "")
	agent := createHandlerTestAgent(t, t.Name()+"-agent", nil)
	var issue IssueResponse
	createWorkflowIssue(t, wf, "todo").Want(http.StatusCreated).JSON(&issue)
	req := withURLParam(newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"assignee_type": "agent", "assignee_id": agent}), "id", issue.ID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	if got := activeRunStatus(t, issue.ID); got != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", got)
	}
}

// Moving a finished workflow issue back through backlog starts a fresh run:
// the one-active-run index only covers active runs.
func TestExtWorkflowRerunAfterDoneStartsAFreshRun(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	wf := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wf, "todo").Want(http.StatusCreated).JSON(&issue)
	ctx := context.Background()

	// Drive the run to done: finish every queued task, move the step's child
	// issue to done through the handler (real child-event hook), repeat until
	// the run settles.
	for i := 0; i < 6 && activeRunStatus(t, issue.ID) == "running"; i++ {
		rows, err := testPool.Query(ctx, `
			SELECT t.id, t.issue_id FROM agent_task_queue t
			JOIN ext_workflow_run r ON r.id = t.ext_workflow_run_id
			WHERE r.issue_id = $1 AND t.status IN ('queued','dispatched','running')`, issue.ID)
		if err != nil {
			t.Fatal(err)
		}
		type pending struct{ task, issue pgtype.UUID }
		var todo []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.task, &p.issue); err != nil {
				t.Fatal(err)
			}
			todo = append(todo, p)
		}
		rows.Close()
		for _, p := range todo {
			dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', started_at=COALESCE(started_at, now()), completed_at=now() WHERE id=$1`, p.task)
			if uuidToString(p.issue) != issue.ID {
				id := uuidToString(p.issue)
				req := withURLParam(newRequest("PUT", "/api/issues/"+id, map[string]any{"status": "done"}), "id", id)
				testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
			}
			if err := engine.OnTaskTerminal(ctx, p.task); err != nil {
				t.Fatalf("OnTaskTerminal: %v", err)
			}
		}
	}
	if got := activeRunStatus(t, issue.ID); got != "done" {
		t.Fatalf("first run status = %q, want done", got)
	}

	for _, status := range []string{"backlog", "todo"} {
		req := withURLParam(newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"status": status}), "id", issue.ID)
		testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ext_workflow_run WHERE issue_id=$1`, issue.ID); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
	if got := activeRunStatus(t, issue.ID); got != "running" {
		t.Fatalf("newest run status = %q, want running", got)
	}
}
