package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// ext-workflow: agents may not steer a run around the engine by editing its
// issues. Only a step's own agent, while it works on the step, may move that
// step's issue; people keep every power.

func (a extWFAgent) update(taskID, issueID string, body map[string]any) *testutil.Response {
	a.t.Helper()
	return testutil.Call(a.t, testHandler.UpdateIssue, withURLParam(a.request("PUT", "/api/issues/"+issueID, body, taskID), "id", issueID))
}

func (a extWFAgent) delete(taskID, issueID string) *testutil.Response {
	a.t.Helper()
	return testutil.Call(a.t, testHandler.DeleteIssue, withURLParam(a.request("DELETE", "/api/issues/"+issueID, nil, taskID), "id", issueID))
}

func memberUpdate(t *testing.T, issueID string, body map[string]any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest("PUT", "/api/issues/"+issueID, body), "id", issueID))
}

// wantWorkflowRefusal asserts a 409 that tells the agent about the workflow.
func wantWorkflowRefusal(t *testing.T, resp *testutil.Response, what string) {
	t.Helper()
	resp.Want(http.StatusConflict)
	if !strings.Contains(resp.Text(), "workflow") {
		t.Fatalf("%s: refusal does not mention the workflow: %s", what, resp.Text())
	}
}

func issueStatus(t *testing.T, issueID string) string {
	t.Helper()
	var status string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status)
	return status
}

// liveTask is a running platform or supervisor task for agent on issueID,
// enough for the handler to treat the agent's requests as the agent's.
func liveTask(t *testing.T, agentID, issueID string, over testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{"issue_id": issueID, "runtime_id": testRuntimeID, "status": "running", "started_at": testutil.Raw("now()")}
	for k, v := range over {
		cols[k] = v
	}
	return dbfx.Task(t, agentID, cols)
}

func TestExtWorkflowGuardStepAgentMovesItsOwnChild(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	_, planner, _, _, runID := e2eWorkflow(t)
	spec := extStep(t, runID, "spec")
	t1 := extTask(t, spec.IssueID, "step")

	planner.update(t1, spec.IssueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
	planner.update(t1, spec.IssueID, map[string]any{"status": "done"}).Want(http.StatusOK)
	endExtTask(t, t1)
	wantExtStep(t, runID, "spec", "done", 1)
	wantExtStep(t, runID, "build", "running", 1)
}

func TestExtWorkflowGuardAgentCannotTouchAnotherStep(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	_, planner, _, _, runID := e2eWorkflow(t)
	spec, build := extStep(t, runID, "spec"), extStep(t, runID, "build")
	t1 := extTask(t, spec.IssueID, "step")
	before := issueStatus(t, build.IssueID)

	wantWorkflowRefusal(t, planner.update(t1, build.IssueID, map[string]any{"status": "done"}), "status")
	wantWorkflowRefusal(t, planner.update(t1, build.IssueID, map[string]any{"status": "cancelled"}), "cancel")
	wantWorkflowRefusal(t, planner.update(t1, build.IssueID, map[string]any{"assignee_type": "member", "assignee_id": testUserID}), "assignee")
	wantWorkflowRefusal(t, planner.update(t1, build.IssueID, map[string]any{"parent_issue_id": nil}), "parent")
	wantWorkflowRefusal(t, planner.delete(t1, build.IssueID), "delete")
	// The move endpoint writes through the same path.
	move := withURLParam(planner.request("POST", "/api/issues/"+build.IssueID+"/move", map[string]any{"status": "done", "before_id": nil, "after_id": nil}, t1), "id", build.IssueID)
	wantWorkflowRefusal(t, testutil.Call(t, testHandler.MoveIssue, move), "move")

	if got := issueStatus(t, build.IssueID); got != before {
		t.Fatalf("build child status = %s, want %s", got, before)
	}
	wantExtStep(t, runID, "build", "pending", 0)
	// Other fields stay open: the guard is about steering, not editing.
	planner.update(t1, build.IssueID, map[string]any{"priority": "high"}).Want(http.StatusOK)
}

func TestExtWorkflowGuardStepAgentCannotCancelOrDetachItsChild(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	_, planner, _, _, runID := e2eWorkflow(t)
	spec := extStep(t, runID, "spec")
	t1 := extTask(t, spec.IssueID, "step")

	wantWorkflowRefusal(t, planner.update(t1, spec.IssueID, map[string]any{"status": "cancelled"}), "cancel own")
	wantWorkflowRefusal(t, planner.update(t1, spec.IssueID, map[string]any{"assignee_type": "member", "assignee_id": testUserID}), "reassign own")
	wantWorkflowRefusal(t, planner.update(t1, spec.IssueID, map[string]any{"parent_issue_id": nil}), "detach own")
	wantWorkflowRefusal(t, planner.delete(t1, spec.IssueID), "delete own")
	wantExtStep(t, runID, "spec", "running", 1)

	// Once its step task has ended, the agent no longer works on the step.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, t1)
	wantWorkflowRefusal(t, planner.update(t1, spec.IssueID, map[string]any{"status": "done"}), "after its task")
}

func TestExtWorkflowGuardSupervisorCannotMoveAChild(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	supervisor, _, _, issue, runID := e2eWorkflow(t)
	spec := extStep(t, runID, "spec")
	turn := liveTask(t, supervisor.id, issue.ID, testutil.Cols{
		"ext_workflow_run_id": runID, "ext_workflow_role": "supervisor", "ext_workflow_kind": "conversation",
	})

	wantWorkflowRefusal(t, supervisor.update(turn, spec.IssueID, map[string]any{"status": "done"}), "supervisor status")
	wantExtStep(t, runID, "spec", "running", 1)
	// Deciding through comments stays open.
	supervisor.comment(turn, issue.ID, "Looking at it.", "")
}

func TestExtWorkflowGuardAgentCannotSteerTheParent(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	_, planner, _, issue, runID := e2eWorkflow(t)
	spec := extStep(t, runID, "spec")
	t1 := extTask(t, spec.IssueID, "step")
	other := dbfx.Issue(t, t.Name()+" other")

	wantWorkflowRefusal(t, planner.update(t1, issue.ID, map[string]any{"status": "cancelled"}), "parent status")
	wantWorkflowRefusal(t, planner.update(t1, issue.ID, map[string]any{"status": "in_review"}), "parent in_review")
	wantWorkflowRefusal(t, planner.update(t1, issue.ID, map[string]any{"assignee_type": "member", "assignee_id": testUserID}), "parent reassign")
	wantWorkflowRefusal(t, planner.update(t1, issue.ID, map[string]any{"parent_issue_id": other}), "parent parent")
	wantWorkflowRefusal(t, planner.delete(t1, issue.ID), "parent delete")

	if got := activeRunStatus(t, issue.ID); got != "running" {
		t.Fatalf("run status = %q, want running", got)
	}
	planner.comment(t1, issue.ID, "A note on the parent.", "")
}

func TestExtWorkflowGuardBatchRejectsTheWholeRequest(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	_, planner, _, issue, runID := e2eWorkflow(t)
	spec, build := extStep(t, runID, "spec"), extStep(t, runID, "build")
	t1 := extTask(t, spec.IssueID, "step")
	plain := dbfx.Issue(t, t.Name()+" plain")

	batch := func(path string, h http.HandlerFunc, body map[string]any) *testutil.Response {
		return testutil.Call(t, h, planner.request("POST", path, body, t1))
	}
	wantWorkflowRefusal(t, batch("/api/issues/batch-update", testHandler.BatchUpdateIssues,
		map[string]any{"issue_ids": []string{plain, build.IssueID}, "updates": map[string]any{"status": "done"}}), "batch status")
	wantWorkflowRefusal(t, batch("/api/issues/batch-update", testHandler.BatchUpdateIssues,
		map[string]any{"issue_ids": []string{plain, issue.ID}, "updates": map[string]any{"assignee_type": nil, "assignee_id": nil}}), "batch unassign parent")
	if got := issueStatus(t, plain); got != "todo" {
		t.Fatalf("plain issue status = %s, want todo (the batch is refused as a whole)", got)
	}
	wantWorkflowRefusal(t, batch("/api/issues/batch-delete", testHandler.BatchDeleteIssues,
		map[string]any{"issue_ids": []string{plain, build.IssueID}}), "batch delete")
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id = ANY($1::uuid[])`, []string{plain, build.IssueID}); n != 2 {
		t.Fatalf("%d of 2 issues left after a refused batch delete", n)
	}
	// A batch that only edits other fields, or only the agent's own step's
	// status, goes through.
	batch("/api/issues/batch-update", testHandler.BatchUpdateIssues,
		map[string]any{"issue_ids": []string{plain, build.IssueID, issue.ID}, "updates": map[string]any{"priority": "low"}}).Want(http.StatusOK)
	batch("/api/issues/batch-update", testHandler.BatchUpdateIssues,
		map[string]any{"issue_ids": []string{plain, spec.IssueID}, "updates": map[string]any{"status": "in_progress"}}).Want(http.StatusOK)
	if got := issueStatus(t, spec.IssueID); got != "in_progress" {
		t.Fatalf("spec child status = %s, want in_progress", got)
	}
}

func TestExtWorkflowGuardLetsPeopleIntervene(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	_, _, _, issue, runID := e2eWorkflow(t)
	spec, build := extStep(t, runID, "spec"), extStep(t, runID, "build")

	memberUpdate(t, build.IssueID, map[string]any{"assignee_type": "member", "assignee_id": testUserID}).Want(http.StatusOK)
	memberUpdate(t, spec.IssueID, map[string]any{"status": "cancelled"}).Want(http.StatusOK)
	// The engine reads the cancelled child as a skipped step, as before.
	wantExtStep(t, runID, "spec", "skipped", 1)
	memberUpdate(t, issue.ID, map[string]any{"status": "cancelled"}).Want(http.StatusOK)
	if got := activeRunStatus(t, issue.ID); got != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", got)
	}
}

func TestExtWorkflowGuardMemberDeletesAChild(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	_, _, _, _, runID := e2eWorkflow(t)
	build := extStep(t, runID, "build")
	testutil.Call(t, testHandler.DeleteIssue, withURLParam(newRequest("DELETE", "/api/issues/"+build.IssueID, nil), "id", build.IssueID)).Want(http.StatusNoContent)
}

func TestExtWorkflowGuardLeavesOtherIssuesAlone(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	agent := extWFAgent{t, createHandlerTestAgent(t, t.Name()+" worker", nil)}
	parent := dbfx.Issue(t, t.Name()+" parent")
	plain := dbfx.Issue(t, t.Name()+" plain", testutil.Cols{"parent_issue_id": parent})
	task := liveTask(t, agent.id, plain, nil)

	agent.update(task, plain, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
	agent.update(task, plain, map[string]any{"status": "done"}).Want(http.StatusOK)
	agent.update(task, plain, map[string]any{"assignee_type": "member", "assignee_id": testUserID}).Want(http.StatusOK)
	agent.update(task, plain, map[string]any{"parent_issue_id": nil}).Want(http.StatusOK)
	agent.update(task, parent, map[string]any{"status": "cancelled"}).Want(http.StatusOK)
	agent.delete(task, plain).Want(http.StatusNoContent)
}

func TestExtWorkflowGuardEndsWithTheRun(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	_, planner, _, issue, runID := e2eWorkflow(t)
	spec, build := extStep(t, runID, "spec"), extStep(t, runID, "build")
	t1 := extTask(t, spec.IssueID, "step")

	memberUpdate(t, issue.ID, map[string]any{"status": "cancelled"}).Want(http.StatusOK)
	if got := activeRunStatus(t, issue.ID); got != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", got)
	}
	planner.update(t1, build.IssueID, map[string]any{"status": "done"}).Want(http.StatusOK)
	planner.update(t1, issue.ID, map[string]any{"status": "backlog"}).Want(http.StatusOK)
	planner.delete(t1, build.IssueID).Want(http.StatusNoContent)
}
