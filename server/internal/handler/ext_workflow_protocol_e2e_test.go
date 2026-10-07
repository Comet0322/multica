package handler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// End-to-end agent protocol through the HTTP handlers: agents comment and
// move issues with their task headers, people comment and decide, and the
// test only plays the daemon (a task starts running, a task ends).

var extWFCommentForwardOnce sync.Once

// forwardExtWorkflowComments subscribes the comment protocol listener to the
// shared test bus, as cmd/server does, delivering to whichever engine the
// current test wired.
func forwardExtWorkflowComments() {
	extWFCommentForwardOnce.Do(func() {
		testHandler.Bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
			commentID, authorType, ok := ExtCommentFromEvent(e)
			if !ok || authorType != "agent" || testHandler.ExtWorkflow == nil {
				return
			}
			_ = testHandler.ExtWorkflow.OnComment(context.Background(), commentID)
		})
	})
}

type extWFAgent struct {
	t  *testing.T
	id string
}

func (a extWFAgent) request(method, path string, body any, taskID string) *http.Request {
	req := newRequest(method, path, body)
	req.Header.Set("X-Agent-ID", a.id)
	req.Header.Set("X-Task-ID", taskID)
	return req
}

// comment posts as the agent from inside taskID; parentID may be "".
func (a extWFAgent) comment(taskID, issueID, content, parentID string) CommentResponse {
	a.t.Helper()
	body := map[string]any{"content": content}
	if parentID != "" {
		body["parent_id"] = parentID
	}
	var c CommentResponse
	testutil.Call(a.t, testHandler.CreateComment, withURLParam(a.request("POST", "/api/issues/"+issueID+"/comments", body, taskID), "id", issueID)).
		Want(http.StatusCreated).JSON(&c)
	return c
}

// moves sets an issue's status as the agent, like `multica issue status`.
func (a extWFAgent) moves(taskID, issueID, status string) {
	a.t.Helper()
	testutil.Call(a.t, testHandler.UpdateIssue, withURLParam(a.request("PUT", "/api/issues/"+issueID, map[string]any{"status": status}, taskID), "id", issueID)).
		Want(http.StatusOK)
}

// extTask is the newest workflow task of a kind on an issue, marked running
// as if a daemon had claimed it.
func extTask(t *testing.T, issueID, kind string) string {
	t.Helper()
	var id string
	dbfx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE issue_id = $1 AND ext_workflow_kind = $2 ORDER BY created_at DESC, id DESC LIMIT 1`, issueID, kind).Scan(&id)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, id)
	return id
}

func mustExtTask(t *testing.T, taskID string) db.AgentTaskQueue {
	t.Helper()
	task, err := testHandler.Queries.GetAgentTask(context.Background(), util.MustParseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func endExtTask(t *testing.T, taskID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, taskID)
	if err := testHandler.ExtWorkflow.OnTaskTerminal(context.Background(), util.MustParseUUID(taskID)); err != nil {
		t.Fatalf("OnTaskTerminal: %v", err)
	}
}

func extStep(t *testing.T, runID, key string) ExtWorkflowStepResponse {
	t.Helper()
	for _, s := range getExtRun(t, runID).Steps {
		if s.NodeKey == key {
			return s
		}
	}
	t.Fatalf("no step %q", key)
	return ExtWorkflowStepResponse{}
}

func wantExtStep(t *testing.T, runID, key, status string, attempts int) ExtWorkflowStepResponse {
	t.Helper()
	s := extStep(t, runID, key)
	if s.Status != status || s.Attempts != attempts {
		t.Fatalf("step %s = %s/%d, want %s/%d", key, s.Status, s.Attempts, status, attempts)
	}
	return s
}

func protocolReplies(t *testing.T, commentID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM comment WHERE parent_id = $1 AND author_type = 'system'`, commentID)
}

const extWFBlockFence = "```ext-workflow\n"

// e2eWorkflow builds supervisor → spec → build (build needs review) and
// assigns a fresh issue to it.
func e2eWorkflow(t *testing.T) (supervisor, planner, coder extWFAgent, issue IssueResponse, runID string) {
	t.Helper()
	supervisor = extWFAgent{t, createHandlerTestAgent(t, t.Name()+" supervisor", nil)}
	planner = extWFAgent{t, createHandlerTestAgent(t, t.Name()+" planner", nil)}
	coder = extWFAgent{t, createHandlerTestAgent(t, t.Name()+" coder", nil)}
	wf := createExtWorkflowAs(t, "", t.Name(), supervisor.id)
	build := extWFNode("build", coder.id, "spec")
	build["requires_review"] = true
	updateExtWorkflow(t, "", wf.ID, map[string]any{"nodes": []any{extWFNode("spec", planner.id), build}}).Want(http.StatusOK)
	createWorkflowIssue(t, wf.ID, "todo").Want(http.StatusCreated).JSON(&issue)
	dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1 OR issue_id IN (SELECT id FROM issue WHERE parent_issue_id = $1)`, issue.ID)
	dbfx.QueryRow(t, `SELECT id FROM ext_workflow_run WHERE issue_id = $1`, issue.ID).Scan(&runID)
	return
}

func TestExtWorkflowAgentProtocolThroughComments(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	forwardExtWorkflowComments()
	supervisor, planner, coder, issue, runID := e2eWorkflow(t)
	spec, build := extStep(t, runID, "spec"), extStep(t, runID, "build")

	// spec runs and finishes; build starts.
	t1 := extTask(t, spec.IssueID, "step")
	planner.comment(t1, spec.IssueID, "Spec v1: offset pagination.", "")
	planner.moves(t1, spec.IssueID, "done")
	endExtTask(t, t1)
	wantExtStep(t, runID, "spec", "done", 1)
	wantExtStep(t, runID, "build", "running", 1)

	// The build agent finds the spec wrong and asks for a rewind.
	t2 := extTask(t, build.IssueID, "step")
	coder.comment(t2, build.IssueID, "The API has no offsets.\n\n"+extWFBlockFence+"action: request-rewind\nto: spec\nreason: The spec assumes offset pagination; the API only has cursors.\n```\n", "")
	if s := wantExtStep(t, runID, "build", "awaiting_supervisor", 1); s.PendingReason == nil || *s.PendingReason != "rewind_request" {
		t.Fatalf("build pending_reason = %v, want rewind_request", s.PendingReason)
	}
	endExtTask(t, t2)

	// The supervisor's first answer misses feedback: a protocol error and a
	// reply, nothing else. The second rewinds spec.
	rr := extTask(t, issue.ID, "rewind_request")
	bad := supervisor.comment(rr, build.IssueID, extWFBlockFence+"action: rewind\nto: spec\n```\n", "")
	if n := protocolReplies(t, bad.ID); n != 1 {
		t.Fatalf("protocol error replies = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'protocol_error'`, runID); n != 1 {
		t.Fatalf("protocol_error events = %d, want 1", n)
	}
	wantExtStep(t, runID, "build", "awaiting_supervisor", 1)
	supervisor.comment(rr, build.IssueID, extWFBlockFence+"action: rewind\nto: spec\nfeedback: |\n  Switch to cursor pagination.\n```\n", "")
	if s := wantExtStep(t, runID, "spec", "running", 1); s.LastFeedback == nil || *s.LastFeedback != "Switch to cursor pagination." {
		t.Fatalf("spec last_feedback = %v", s.LastFeedback)
	}
	wantExtStep(t, runID, "build", "pending", 0)
	if run := getExtRun(t, runID); run.RewindsUsed != 1 {
		t.Fatalf("rewinds_used = %d, want 1", run.RewindsUsed)
	}
	endExtTask(t, rr)

	// spec re-runs; build re-runs on the new spec, which its briefing carries.
	t3 := extTask(t, spec.IssueID, "step")
	planner.comment(t3, spec.IssueID, "Spec v2: cursor pagination.", "")
	planner.moves(t3, spec.IssueID, "done")
	endExtTask(t, t3)
	wantExtStep(t, runID, "build", "running", 1)
	t4 := extTask(t, build.IssueID, "step")
	if text, ok, err := engine.BuildBriefing(context.Background(), mustExtTask(t, t4)); err != nil || !ok || !strings.Contains(text, "> Spec v2: cursor pagination.") {
		t.Fatalf("build briefing (ok=%v err=%v) lacks the new spec:\n%s", ok, err, text)
	}
	coder.comment(t4, build.IssueID, "Build v2 done.", "")
	coder.moves(t4, build.IssueID, "in_review")
	endExtTask(t, t4)
	if s := wantExtStep(t, runID, "build", "awaiting_supervisor", 1); *s.PendingReason != "review" {
		t.Fatalf("build pending_reason = %s, want review", *s.PendingReason)
	}

	// The supervisor reviews on the child issue and approves.
	review := extTask(t, issue.ID, "review")
	supervisor.comment(review, build.IssueID, "Looks right.\n\n"+extWFBlockFence+"action: approve\n```\n", "")
	wantExtStep(t, runID, "build", "done", 1)
	var childStatus string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, build.IssueID).Scan(&childStatus)
	if childStatus != "done" {
		t.Fatalf("build child = %s, want done", childStatus)
	}
	// The block on the child woke nobody: the only tasks there are the step's.
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND ext_workflow_run_id IS NULL`, build.IssueID); n != 0 {
		t.Fatalf("%d platform tasks on the build child", n)
	}
	// The summary waits for the deciding review turn to end.
	endExtTask(t, review)
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND ext_workflow_kind = 'summary'`, issue.ID); n != 1 {
		t.Fatalf("summary tasks = %d, want 1", n)
	}
}

func TestExtWorkflowConversationThroughComments(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	forwardExtWorkflowComments()
	supervisor, planner, coder, issue, runID := e2eWorkflow(t)
	spec, build := extStep(t, runID, "spec"), extStep(t, runID, "build")
	t1 := extTask(t, spec.IssueID, "step")
	planner.moves(t1, spec.IssueID, "done")
	endExtTask(t, t1)
	t2 := extTask(t, build.IssueID, "step")
	coder.moves(t2, build.IssueID, "in_review")
	endExtTask(t, t2)
	wantExtStep(t, runID, "build", "awaiting_supervisor", 1)
	approve := extWFBlockFence + "action: approve\nstep: build\n```\n"

	// A member who may not decide asks; the supervisor's block for them is refused.
	plain := createPlainMember(t, "ext-wf-conversation-plain@multica.test")
	asked := postComment(t, plain, issue.ID, map[string]any{"content": "Can we just approve the build?"})
	c1 := extTask(t, issue.ID, "conversation")
	refused := supervisor.comment(c1, issue.ID, "On your behalf:\n\n"+approve, asked.ID)
	if n := protocolReplies(t, refused.ID); n != 1 {
		t.Fatalf("refused decision replies = %d, want 1", n)
	}
	wantExtStep(t, runID, "build", "awaiting_supervisor", 1)
	endExtTask(t, c1)

	// The member who started the run asks; the same block applies for them.
	asked = postComment(t, "", issue.ID, map[string]any{"content": "Approve the build, please."})
	c2 := extTask(t, issue.ID, "conversation")
	supervisor.comment(c2, issue.ID, "Approving for you:\n\n"+approve, asked.ID)
	wantExtStep(t, runID, "build", "done", 1)
	var onBehalf, actor string
	dbfx.QueryRow(t, `SELECT on_behalf_of::text, actor_id::text FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'decision'`, runID).Scan(&onBehalf, &actor)
	if onBehalf != testUserID || actor != supervisor.id {
		t.Fatalf("decision by %s on behalf of %s, want the supervisor for %s", actor, onBehalf, testUserID)
	}
}
