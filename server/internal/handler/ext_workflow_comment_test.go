package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const extWFApproveBlock = "Approving.\n\n```ext-workflow\naction: approve\n```\n"

func TestIsNoteCommentIsPureAndIgnoresWorkflowBlocks(t *testing.T) {
	for content, want := range map[string]bool{
		extWFApproveBlock:                  false, // a block is a note only in workflow context: isNoteCommentOn
		"```ext-workflow\naction: approve": false,
		"/note remember this":              true,
		"please look at this":              false,
		"```yaml\naction: approve\n```\n":  false,
	} {
		if got := isNoteComment(content); got != want {
			t.Errorf("isNoteComment(%q) = %v, want %v", content, got, want)
		}
	}
}

// postComment creates a comment through the handler as userID ("" = owner).
func postComment(t *testing.T, userID, issueID string, body map[string]any) CommentResponse {
	t.Helper()
	var req *http.Request
	if userID == "" {
		req = newRequest("POST", "/api/issues/"+issueID+"/comments", body)
	} else {
		req = newRequestAs(userID, "POST", "/api/issues/"+issueID+"/comments", body)
	}
	var c CommentResponse
	testutil.Call(t, testHandler.CreateComment, withURLParam(req, "id", issueID)).Want(http.StatusCreated).JSON(&c)
	return c
}

func conversationTasks(t *testing.T, commentID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1 AND ext_workflow_kind = 'conversation'`, commentID)
}

func TestExtWorkflowMemberCommentWakesTheSupervisor(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	issue, _, _ := startExtRun(t)

	c := postComment(t, "", issue.ID, map[string]any{"content": "How is it going?"})
	if n := conversationTasks(t, c.ID); n != 1 {
		t.Fatalf("conversation tasks = %d, want 1", n)
	}
	var role, agentID string
	dbfx.QueryRow(t, `SELECT ext_workflow_role, agent_id FROM agent_task_queue WHERE trigger_comment_id = $1`, c.ID).Scan(&role, &agentID)
	var supervisor string
	dbfx.QueryRow(t, `SELECT definition->>'supervisor_agent_id' FROM ext_workflow_run WHERE issue_id = $1`, issue.ID).Scan(&supervisor)
	if role != "supervisor" || agentID != supervisor {
		t.Fatalf("conversation task role=%q agent=%s, want the supervisor %s", role, agentID, supervisor)
	}

	// A block from a person is a note; it wakes nobody.
	blocked := postComment(t, "", issue.ID, map[string]any{"content": extWFApproveBlock})
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, blocked.ID); n != 0 {
		t.Fatalf("a block comment enqueued %d tasks", n)
	}

	// Previewing an edit of an older comment must not wake the supervisor.
	older := dbfx.Comment(t, issue.ID, "Written before anyone listened")
	preview := withURLParam(newRequest("POST", "/api/issues/"+issue.ID+"/comment-trigger-preview", map[string]any{
		"content": "Edited text", "editing_comment_id": older,
	}), "id", issue.ID)
	testutil.Call(t, testHandler.PreviewCommentTriggers, preview).Want(http.StatusOK)
	if n := conversationTasks(t, older); n != 0 {
		t.Fatalf("preview woke the supervisor (%d tasks)", n)
	}

	setExtWorkflowEngine(t, nil)
	off := postComment(t, "", issue.ID, map[string]any{"content": "Anyone?"})
	if n := conversationTasks(t, off.ID); n != 0 {
		t.Fatalf("engine off: %d conversation tasks", n)
	}
	setExtWorkflowEngine(t, engine)
}

func TestExtWorkflowBlockOnAChildWakesNobody(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	_, runID, _ := startExtRun(t)
	child := getExtRun(t, runID).Steps[0].IssueID
	helper := createHandlerTestAgent(t, "ext-wf-mentioned-helper", nil)
	mention := "[@Helper](mention://agent/" + helper + ") "

	control := postComment(t, "", child, map[string]any{"content": mention + "please look"})
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, control.ID); n != 1 {
		t.Fatalf("control mention enqueued %d tasks, want 1", n)
	}
	blocked := postComment(t, "", child, map[string]any{"content": mention + extWFApproveBlock})
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, blocked.ID); n != 0 {
		t.Fatalf("block comment enqueued %d tasks, want 0", n)
	}
}

// A person's fenced block is inert: decisions from people go through the UI.
func TestExtWorkflowBlockFromAMemberIsInert(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	issue, runID, _ := startExtRun(t)
	failExtStep(t, engine, issue.ID) // the step now awaits the supervisor
	before := getExtRun(t, runID)
	if got := before.Steps[0].Status; got != "awaiting_supervisor" {
		t.Fatalf("step status = %q, want awaiting_supervisor", got)
	}
	child := before.Steps[0].IssueID
	childTasks := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, child)
	}
	tasksBefore := childTasks()
	protocolErrors := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'protocol_error'`, runID)
	}
	errorsBefore := protocolErrors()

	for _, issueID := range []string{child, issue.ID} {
		c := postComment(t, "", issueID, map[string]any{"content": extWFApproveBlock})
		// The listener never forwards a member comment; the engine also ignores one if it is.
		if err := engine.OnComment(context.Background(), util.MustParseUUID(c.ID)); err != nil {
			t.Fatalf("OnComment: %v", err)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, c.ID); n != 0 {
			t.Fatalf("member block on %s woke %d tasks", issueID, n)
		}
	}

	after := getExtRun(t, runID)
	if after.Steps[0].Status != before.Steps[0].Status || after.Steps[0].Attempts != before.Steps[0].Attempts || after.Status != before.Status {
		t.Fatalf("member block changed the run: step %q -> %q, run %q -> %q", before.Steps[0].Status, after.Steps[0].Status, before.Status, after.Status)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'decision'`, runID); n != 0 {
		t.Fatalf("decision events = %d, want 0", n)
	}
	if n := protocolErrors(); n != errorsBefore {
		t.Fatalf("protocol_error events %d -> %d", errorsBefore, n)
	}
	if n := childTasks(); n != tasksBefore {
		t.Fatalf("child tasks %d -> %d", tasksBefore, n)
	}
}

// A fenced block only means workflow protocol inside a workflow. Anywhere else
// it is ordinary text, and the comment routes like any other.
func TestExtWorkflowBlockOutsideAWorkflowBehavesLikeAComment(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	helper := createHandlerTestAgent(t, "ext-wf-plain-helper", nil)
	mention := "[@Helper](mention://agent/" + helper + ") "
	wakes := func(issueID string) int {
		c := postComment(t, "", issueID, map[string]any{"content": mention + extWFApproveBlock})
		return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, c.ID)
	}

	plain := createTestIssue(t, "Ordinary issue", "todo", "none")
	if n := wakes(plain); n != 1 {
		t.Fatalf("block on an ordinary issue enqueued %d tasks, want 1", n)
	}

	_, runID, _ := startExtRun(t)
	child := getExtRun(t, runID).Steps[0].IssueID
	setExtWorkflowEngine(t, nil)
	if n := wakes(child); n != 1 {
		t.Fatalf("engine off: block on a step child enqueued %d tasks, want 1", n)
	}
	if n := wakes(plain); n != 1 {
		t.Fatalf("engine off: block on an ordinary issue enqueued %d tasks, want 1", n)
	}
	setExtWorkflowEngine(t, engine)
	if n := wakes(child); n != 0 {
		t.Fatalf("engine on: block on a step child enqueued %d tasks, want 0", n)
	}
}

func TestIsNoteCommentOnDecidesByWorkflowContext(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	ctx := context.Background()
	issue, runID, _ := startExtRun(t)
	child := getExtRun(t, runID).Steps[0].IssueID
	load := func(id string) db.Issue {
		row, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(id))
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	plain := load(createTestIssue(t, "Ordinary issue for the note predicate", "todo", "none"))
	for name, tc := range map[string]struct {
		issue   db.Issue
		content string
		want    bool
	}{
		"parent block":        {load(issue.ID), extWFApproveBlock, true},
		"child block":         {load(child), extWFApproveBlock, true},
		"plain block":         {plain, extWFApproveBlock, false},
		"plain /note":         {plain, "/note x", true},
		"child ordinary text": {load(child), "hello", false},
	} {
		if got := testHandler.isNoteCommentOn(ctx, tc.issue, tc.content); got != tc.want {
			t.Errorf("%s: isNoteCommentOn = %v, want %v", name, got, tc.want)
		}
	}
}
