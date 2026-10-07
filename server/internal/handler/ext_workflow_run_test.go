package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// extRunReq is extWFReq plus the step URL param of the decision route.
func extRunReq(userID, method, path string, body any, runID, stepID string) *http.Request {
	req := extWFReq(userID, method, path, body, runID)
	if stepID != "" {
		chi.RouteContext(req.Context()).URLParams.Add("stepId", stepID)
	}
	return req
}

// startExtRun assigns a fresh issue to a one-step workflow and returns the
// parent issue and its run id.
func startExtRun(t *testing.T) (IssueResponse, string, string) {
	t.Helper()
	wf := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wf, "todo").Want(http.StatusCreated).JSON(&issue)
	dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1 OR issue_id IN (SELECT id FROM issue WHERE parent_issue_id = $1)`, issue.ID)
	var runID string
	dbfx.QueryRow(t, `SELECT id FROM ext_workflow_run WHERE issue_id = $1`, issue.ID).Scan(&runID)
	return issue, runID, wf
}

// failExtStep ends the step task without the child issue finishing, which
// sends the step to the supervisor with pending_reason=failure.
func failExtStep(t *testing.T, engine *extworkflow.Engine, parentID string) {
	t.Helper()
	var taskID string
	dbfx.QueryRow(t, `
		SELECT t.id FROM agent_task_queue t JOIN issue c ON c.id = t.issue_id
		WHERE c.parent_issue_id = $1 AND t.ext_workflow_role = 'step'
		ORDER BY t.created_at DESC LIMIT 1`, parentID).Scan(&taskID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', started_at = now(), completed_at = now() WHERE id = $1`, taskID)
	if err := engine.OnTaskTerminal(context.Background(), util.MustParseUUID(taskID)); err != nil {
		t.Fatalf("OnTaskTerminal: %v", err)
	}
}

func getExtRun(t *testing.T, runID string) ExtWorkflowRunResponse {
	t.Helper()
	var run ExtWorkflowRunResponse
	testutil.Call(t, testHandler.GetExtWorkflowRun, extRunReq("", "GET", "/api/ext/workflow-runs/"+runID, nil, runID, "")).
		Want(http.StatusOK).JSON(&run)
	return run
}

func TestExtWorkflowRunReads(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	issue, runID, wf := startExtRun(t)
	setExtWorkflowEngine(t, nil) // reads do not need the engine

	var byIssue struct {
		Runs   []ExtWorkflowRunSummaryResponse `json:"runs"`
		StepOf *ExtWorkflowStepOfResponse      `json:"step_of"`
	}
	testutil.Call(t, testHandler.ListExtWorkflowRunsForIssue, extRunReq("", "GET", "/api/ext/workflow-runs?issue_id="+issue.ID, nil, "", "")).
		Want(http.StatusOK).JSON(&byIssue)
	if len(byIssue.Runs) != 1 || byIssue.StepOf != nil {
		t.Fatalf("parent runs = %+v step_of = %+v", byIssue.Runs, byIssue.StepOf)
	}
	sum := byIssue.Runs[0]
	if sum.ID != runID || sum.WorkflowID != wf || !strings.HasPrefix(sum.WorkflowName, t.Name()+"-") || sum.IssueIdentifier != issue.Identifier ||
		sum.IssueTitle != issue.Title || sum.Status != "running" || sum.MaxRewinds != 3 || sum.TriggeredByType != "member" ||
		sum.TriggeredByID != testUserID || sum.FinishedAt != nil {
		t.Fatalf("summary = %+v (issue %s)", sum, issue.Identifier)
	}

	run := getExtRun(t, runID)
	if run.ID != runID || len(run.Steps) != 1 || len(run.Events) == 0 {
		t.Fatalf("run = %+v", run)
	}
	step := run.Steps[0]
	if step.NodeKey != "build" || step.Title != "Title build" || step.Status != "running" || step.Attempts != 1 ||
		step.MaxAttempts != 3 || step.RequiresReview || len(step.DependsOn) != 0 || step.PendingReason != nil || step.StartedAt == nil {
		t.Fatalf("step = %+v", step)
	}
	if run.Events[0].Kind != "run_started" || run.Events[0].ActorType != "member" || string(run.Events[0].Payload) == "" {
		t.Fatalf("first event = %+v", run.Events[0])
	}

	testutil.Call(t, testHandler.ListExtWorkflowRunsForIssue, extRunReq("", "GET", "/api/ext/workflow-runs?issue_id="+step.IssueID, nil, "", "")).
		Want(http.StatusOK).JSON(&byIssue)
	if len(byIssue.Runs) != 0 || byIssue.StepOf == nil {
		t.Fatalf("child runs = %+v step_of = %+v", byIssue.Runs, byIssue.StepOf)
	}
	if got := *byIssue.StepOf; got.RunID != runID || got.StepID != step.ID || got.NodeKey != "build" || got.Index != 1 || got.Total != 1 || got.ParentIssueID != issue.ID {
		t.Fatalf("step_of = %+v", got)
	}

	var page struct {
		Runs  []ExtWorkflowRunSummaryResponse `json:"runs"`
		Total int                             `json:"total"`
	}
	testutil.Call(t, testHandler.ListExtWorkflowRunsForWorkflow, extWFReq("", "GET", "/api/ext/workflows/"+wf+"/runs?limit=10", nil, wf)).
		Want(http.StatusOK).JSON(&page)
	if page.Total != 1 || len(page.Runs) != 1 || page.Runs[0].ID != runID {
		t.Fatalf("workflow runs = %+v", page)
	}

	testutil.Call(t, testHandler.ListExtWorkflowRunsForIssue, extRunReq("", "GET", "/api/ext/workflow-runs", nil, "", "")).Want(http.StatusBadRequest)
	testutil.Call(t, testHandler.GetExtWorkflowRun, extRunReq("", "GET", "/api/ext/workflow-runs/"+issue.ID, nil, issue.ID, "")).Want(http.StatusNotFound)
	testutil.Call(t, testHandler.ListExtWorkflowRunsForWorkflow, extWFReq("", "GET", "/api/ext/workflows/"+wf+"/runs?limit=x", nil, wf)).Want(http.StatusBadRequest)
}

func TestExtWorkflowRunDecision(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	issue, runID, _ := startExtRun(t)
	failExtStep(t, engine, issue.ID)
	stepID := getExtRun(t, runID).Steps[0].ID
	decide := func(userID string, body map[string]any) *testutil.Response {
		path := "/api/ext/workflow-runs/" + runID + "/steps/" + stepID + "/decision"
		return testutil.Call(t, testHandler.DecideExtWorkflowStep, extRunReq(userID, "POST", path, body, runID, stepID))
	}

	plain := createPlainMember(t, "ext-wf-decide-plain@multica.test")
	decide(plain, map[string]any{"action": "retry", "expected_status": "awaiting_supervisor"}).Want(http.StatusForbidden)
	decide("", map[string]any{"action": "retry", "expected_status": "awaiting_human"}).Want(http.StatusConflict)
	decide("", map[string]any{"action": "redo", "expected_status": "awaiting_supervisor"}).Want(http.StatusUnprocessableEntity)
	decide("", map[string]any{"action": "escalate", "reason": "x", "expected_status": "awaiting_supervisor"}).Want(http.StatusUnprocessableEntity)
	testutil.Call(t, testHandler.DecideExtWorkflowStep, extRunReq("", "POST", "/x", map[string]any{"action": "retry"}, runID, runID)).Want(http.StatusNotFound)

	setExtWorkflowEngine(t, nil)
	resp := decide("", map[string]any{"action": "retry", "expected_status": "awaiting_supervisor"}).Want(http.StatusConflict)
	if !strings.Contains(resp.Text(), "workflow_engine_disabled") {
		t.Fatalf("engine off body = %s", resp.Text())
	}
	setExtWorkflowEngine(t, engine)

	var run ExtWorkflowRunResponse
	decide("", map[string]any{"action": "retry", "expected_status": "awaiting_supervisor"}).Want(http.StatusOK).JSON(&run)
	if run.ID != runID || run.Steps[0].Status != "running" || run.Steps[0].Attempts != 2 || run.Steps[0].PendingReason != nil {
		t.Fatalf("after retry: %+v", run.Steps[0])
	}
	last := run.Events[len(run.Events)-1]
	found := false
	for _, ev := range run.Events {
		found = found || (ev.Kind == "decision" && ev.ActorType == "member" && ev.ActorID != nil && *ev.ActorID == testUserID)
	}
	if !found {
		t.Fatalf("no member decision event; last = %+v", last)
	}
	decide("", map[string]any{"action": "retry", "expected_status": "awaiting_supervisor"}).Want(http.StatusConflict)
}

func TestExtWorkflowRunCancel(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	issue, runID, _ := startExtRun(t)
	cancel := func(userID string) *testutil.Response {
		return testutil.Call(t, testHandler.CancelExtWorkflowRun, extRunReq(userID, "POST", "/api/ext/workflow-runs/"+runID+"/cancel", nil, runID, ""))
	}

	plain := createPlainMember(t, "ext-wf-cancel-plain@multica.test")
	cancel(plain).Want(http.StatusForbidden)
	setExtWorkflowEngine(t, nil)
	cancel("").Want(http.StatusConflict)
	setExtWorkflowEngine(t, engine)

	cancel("").Want(http.StatusNoContent)
	if got := activeRunStatus(t, issue.ID); got != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", got)
	}
	cancel("").Want(http.StatusConflict)
	testutil.Call(t, testHandler.CancelExtWorkflowRun, extRunReq("", "POST", "/x", nil, issue.ID, "")).Want(http.StatusNotFound)
}

// TestExtWorkflowRunMutationsRejectAgentActor pins I1 at the handler: a task
// token authenticates as the runtime owner (here the test user, who could
// otherwise decide and cancel), so the handlers refuse machine actors and
// leave the run untouched.
func TestExtWorkflowRunMutationsRejectAgentActor(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	issue, runID, _ := startExtRun(t)
	failExtStep(t, engine, issue.ID)
	stepID := getExtRun(t, runID).Steps[0].ID
	asAgent := func(req *http.Request) *http.Request {
		req.Header.Set("X-Actor-Source", "task_token")
		return req
	}

	path := "/api/ext/workflow-runs/" + runID + "/steps/" + stepID + "/decision"
	body := map[string]any{"action": "retry", "expected_status": "awaiting_supervisor"}
	testutil.Call(t, testHandler.DecideExtWorkflowStep, asAgent(extRunReq("", "POST", path, body, runID, stepID))).Want(http.StatusForbidden)
	testutil.Call(t, testHandler.CancelExtWorkflowRun, asAgent(extRunReq("", "POST", "/api/ext/workflow-runs/"+runID+"/cancel", nil, runID, ""))).Want(http.StatusForbidden)

	run := getExtRun(t, runID)
	if run.Status != "running" || run.Steps[0].Status != "awaiting_supervisor" || run.Steps[0].Attempts != 1 {
		t.Fatalf("agent request changed the run: status=%s step=%+v", run.Status, run.Steps[0])
	}
}

// otherWorkspaceReq rewrites a request's workspace param to a second
// workspace the test user also belongs to, so membership passes and only the
// run's workspace scoping can reject it.
func otherWorkspaceReq(t *testing.T, req *http.Request) *http.Request {
	t.Helper()
	var wsID string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO workspace (name, slug, issue_prefix) VALUES ('Ext Other WS', $1, 'EXO') RETURNING id`,
		fmt.Sprintf("ext-other-ws-%d", time.Now().UnixNano())).Scan(&wsID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1`, wsID)
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, wsID)
	})
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, wsID, testUserID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	chi.RouteContext(req.Context()).URLParams.Add("workspaceId", wsID)
	return req
}

func TestExtWorkflowRunCrossWorkspaceIs404(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	issue, runID, _ := startExtRun(t)
	failExtStep(t, engine, issue.ID)
	stepID := getExtRun(t, runID).Steps[0].ID

	testutil.Call(t, testHandler.GetExtWorkflowRun,
		otherWorkspaceReq(t, extRunReq("", "GET", "/x", nil, runID, ""))).Want(http.StatusNotFound)
	testutil.Call(t, testHandler.CancelExtWorkflowRun,
		otherWorkspaceReq(t, extRunReq("", "POST", "/x", nil, runID, ""))).Want(http.StatusNotFound)
	testutil.Call(t, testHandler.DecideExtWorkflowStep,
		otherWorkspaceReq(t, extRunReq("", "POST", "/x", map[string]any{"action": "retry", "expected_status": "awaiting_supervisor"}, runID, stepID))).
		Want(http.StatusNotFound)

	if got := activeRunStatus(t, issue.ID); got != "running" {
		t.Fatalf("run status after cross-workspace attempts = %q, want running", got)
	}
}
