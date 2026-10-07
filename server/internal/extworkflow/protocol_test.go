package extworkflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func block(lines ...string) string {
	return "Decision:\n\n```ext-workflow\n" + strings.Join(lines, "\n") + "\n```\n"
}

// agentSays posts an agent comment (stamped with the authoring task, as the
// CLI does) and delivers it to the engine like the comment:created listener.
func (e *env) agentSays(t *testing.T, issueID, agentID pgtype.UUID, task *db.AgentTaskQueue, content string) pgtype.UUID {
	t.Helper()
	cols := testutil.Cols{"author_type": "agent", "author_id": agentID}
	if task != nil {
		cols["source_task_id"] = task.ID
	}
	id := util.MustParseUUID(e.fx.Comment(t, util.UUIDToString(issueID), content, cols))
	if err := e.engine.OnComment(context.Background(), id); err != nil {
		t.Fatalf("OnComment: %v", err)
	}
	return id
}

// protocolErrors counts protocol_error events and the system replies under
// the given comment.
func (e *env) protocolErrors(t *testing.T, run db.ExtWorkflowRun, comment pgtype.UUID) (events, replies int) {
	t.Helper()
	events = e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'protocol_error' AND payload->>'comment_id' = $2`, run.ID, util.UUIDToString(comment))
	replies = e.fx.Count(t, `SELECT count(*) FROM comment WHERE parent_id = $1 AND author_type = 'system' AND content LIKE '%did not apply%'`, comment)
	return events, replies
}

func TestOnCommentSupervisorApprovesOnTheChild(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)
	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	if review.ExtWorkflowKind.String != KindReview {
		t.Fatalf("supervisor task kind = %q", review.ExtWorkflowKind.String)
	}
	e.agentSays(t, build.IssueID, supervisor, &review, block("action: approve", "reason: looks right"))

	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	var actorType string
	var actorID, stepID pgtype.UUID
	e.fx.QueryRow(t, `SELECT actor_type, actor_id, step_id FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'decision'`, run.ID).Scan(&actorType, &actorID, &stepID)
	if actorType != "agent" || actorID != supervisor || stepID != build.ID {
		t.Fatalf("decision by %s %v on %v", actorType, actorID, stepID)
	}
	if got := e.latestTask(t, build, RoleSupervisor).Status; got != "running" {
		t.Fatalf("deciding task = %s, want it left running", got)
	}
}

func TestOnCommentStepAgentRequestsRewind(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	e.childMoves(t, parent, spec.IssueID, "done")
	build := e.step(t, run, "build")
	buildTask := e.running(t, e.latestTask(t, build, RoleStep))

	e.agentSays(t, build.IssueID, coder, &buildTask, block("action: request-rewind", "to: spec", "reason: The spec assumes a paginated API."))

	build = e.step(t, run, "build")
	if StepStatus(build.Status) != StepAwaitingSupervisor || build.PendingReason.String != string(PendingRewindRequest) {
		t.Fatalf("build = %s/%s, want awaiting_supervisor/rewind_request", build.Status, build.PendingReason.String)
	}
	sup := e.latestTask(t, build, RoleSupervisor)
	if sup.ExtWorkflowKind.String != KindRewindRequest || sup.IssueID != parent {
		t.Fatalf("supervisor task kind=%q issue=%v", sup.ExtWorkflowKind.String, sup.IssueID)
	}
}

func TestOnCommentRejectsInvalidBlocks(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)
	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	for name, tc := range map[string]struct {
		issue   pgtype.UUID
		content string
		want    string
	}{
		"request-rewind from the supervisor": {build.IssueID, block("action: request-rewind", "reason: x"), "for step agents"},
		"unknown field":                      {build.IssueID, block("action: approve", "colour: red"), "not valid"},
		"two blocks":                         {build.IssueID, block("action: approve") + block("action: skip"), "only one"},
		"missing feedback":                   {build.IssueID, block("action: redo"), "needs feedback"},
		"decision on the parent":             {run.IssueID, block("action: approve", "step: build"), "child issue of step"},
	} {
		comment := e.agentSays(t, tc.issue, supervisor, &review, tc.content)
		events, replies := e.protocolErrors(t, run, comment)
		if events != 1 || replies != 1 {
			t.Errorf("%s: protocol_error events=%d replies=%d, want 1/1", name, events, replies)
		}
		var reply string
		e.fx.QueryRow(t, `SELECT content FROM comment WHERE parent_id = $1`, comment).Scan(&reply)
		if !strings.Contains(reply, tc.want) {
			t.Errorf("%s: reply %q does not mention %q", name, reply, tc.want)
		}
	}
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)
}

func TestOnCommentStepAgentMayOnlyRequestRewind(t *testing.T) {
	e := newEnv(t)
	supervisor, coder := e.agent(t, "Supervisor"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: coder})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	build := e.step(t, run, "build")
	task := e.running(t, e.latestTask(t, build, RoleStep))

	comment := e.agentSays(t, build.IssueID, coder, &task, block("action: approve"))
	if events, replies := e.protocolErrors(t, run, comment); events != 1 || replies != 1 {
		t.Fatalf("protocol_error events=%d replies=%d, want 1/1", events, replies)
	}
	wantStepRow(t, e.step(t, run, "build"), StepRunning, 1)
}

func TestOnCommentIgnoresNonParticipants(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)
	stranger := e.agent(t, "Stranger")
	review := e.latestTask(t, build, RoleSupervisor)
	cases := map[string]pgtype.UUID{}

	// A person's comment, an agent without a task in the run, a comment with
	// no block, and a comment stamped with an unrelated task.
	cases["member"] = util.MustParseUUID(e.fx.Comment(t, util.UUIDToString(build.IssueID), block("action: approve")))
	cases["stranger"] = e.agentSays(t, build.IssueID, stranger, nil, block("action: approve"))
	cases["no block"] = e.agentSays(t, build.IssueID, supervisor, &review, "Looks fine to me.")
	other := review
	other.ID = util.MustParseUUID(e.fx.Task(t, util.UUIDToString(supervisor), testutil.Cols{"status": "completed", "runtime_id": e.runtime}))
	cases["foreign source task"] = e.agentSays(t, build.IssueID, supervisor, &other, block("action: approve"))
	if err := e.engine.OnComment(context.Background(), cases["member"]); err != nil {
		t.Fatalf("OnComment(member): %v", err)
	}

	for name, comment := range cases {
		if events, replies := e.protocolErrors(t, run, comment); events != 0 || replies != 0 {
			t.Errorf("%s: protocol_error events=%d replies=%d, want none", name, events, replies)
		}
	}
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)
}

// A step agent is bound to its own step: request-rewind on another step's
// child issue or on the parent is rejected, and the protocol_error carries the
// authoring task id (the no-decision guard keys on it).
func TestOnCommentStepAgentBoundToItsOwnStep(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec, build := e.step(t, run, "spec"), e.step(t, run, "build")
	task := e.running(t, e.latestTask(t, build, RoleStep))
	rewind := block("action: request-rewind", "to: spec", "reason: wrong place")

	for name, issue := range map[string]pgtype.UUID{"other step's child": spec.IssueID, "parent": parent} {
		comment := e.agentSays(t, issue, coder, &task, rewind)
		if events, replies := e.protocolErrors(t, run, comment); events != 1 || replies != 1 {
			t.Errorf("%s: protocol_error events=%d replies=%d, want 1/1", name, events, replies)
		}
		var taskID string
		e.fx.QueryRow(t, `SELECT payload->>'task_id' FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'protocol_error' AND payload->>'comment_id' = $2`, run.ID, util.UUIDToString(comment)).Scan(&taskID)
		if taskID != util.UUIDToString(task.ID) {
			t.Errorf("%s: protocol_error task_id = %q, want %v", name, taskID, task.ID)
		}
	}
	wantStepRow(t, e.step(t, run, "build"), StepRunning, 1)
}

// A foreign agent citing the supervisor's task, and the supervisor citing its
// OWN review task after that task was cancelled, decide nothing and get no
// reply.
func TestOnCommentIgnoresEndedAndForeignTasks(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)
	review := e.latestTask(t, build, RoleSupervisor)
	other := e.agent(t, "Other")

	foreign := e.agentSays(t, build.IssueID, other, &review, block("action: approve"))
	e.fx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled', completed_at = now() WHERE id = $1`, review.ID)
	ended := e.agentSays(t, build.IssueID, supervisor, &review, block("action: approve"))
	for name, c := range map[string]pgtype.UUID{"foreign agent with supervisor task": foreign, "supervisor citing its ended task": ended} {
		if events, replies := e.protocolErrors(t, run, c); events != 0 || replies != 0 {
			t.Errorf("%s: protocol_error events=%d replies=%d, want none", name, events, replies)
		}
	}
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)
}

// A supervisor focused on step S posting on another step's child issue is a
// participant error that points at S's child issue.
func TestOnCommentSupervisorOnAnotherStepsChild(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner, review: true},
		wfNode{key: "build", title: "Build", agent: coder})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	e.running(t, e.latestTask(t, spec, RoleStep))
	e.childMoves(t, parent, spec.IssueID, "done")
	review := e.running(t, e.latestTask(t, e.step(t, run, "spec"), RoleSupervisor))
	build := e.step(t, run, "build")

	comment := e.agentSays(t, build.IssueID, supervisor, &review, block("action: approve"))
	if events, replies := e.protocolErrors(t, run, comment); events != 1 || replies != 1 {
		t.Fatalf("protocol_error events=%d replies=%d, want 1/1", events, replies)
	}
	var reply, taskID string
	e.fx.QueryRow(t, `SELECT content FROM comment WHERE parent_id = $1`, comment).Scan(&reply)
	e.fx.QueryRow(t, `SELECT payload->>'task_id' FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'protocol_error' AND payload->>'comment_id' = $2`, run.ID, util.UUIDToString(comment)).Scan(&taskID)
	if !strings.Contains(reply, "child issue of step \"Spec\"") || taskID != util.UUIDToString(review.ID) {
		t.Errorf("reply %q, task_id %q", reply, taskID)
	}
	wantStepRow(t, e.step(t, run, "spec"), StepAwaitingSupervisor, 1)
}

// The deciding task is re-checked under the run lock: a decision from a task
// that ended after authorization is refused and applies nothing.
func TestDecideRefusesTaskThatEndedAfterAuthorization(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)
	review := e.latestTask(t, build, RoleSupervisor)
	in := DecideInput{
		RunID: run.ID, StepID: build.ID, Decision: Decision{Action: ActionApprove},
		ActorType: "agent", ActorID: supervisor, TaskID: review.ID, ExpectedStatus: StepAwaitingSupervisor,
	}
	e.fx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled', completed_at = now() WHERE id = $1`, review.ID)
	if err := e.engine.Decide(context.Background(), in); !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("Decide with an ended task = %v, want ErrStatusMismatch", err)
	}
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)
}

func TestTwoInvalidBlockTurnsEscalate(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)

	first := e.running(t, e.latestTask(t, build, RoleSupervisor))
	e.agentSays(t, build.IssueID, supervisor, &first, block("action: maybe"))
	e.endTask(t, first, "completed")
	second := e.latestTask(t, e.step(t, run, "build"), RoleSupervisor)
	if second.ID == first.ID || e.step(t, run, "build").SupervisorWakes != 2 {
		t.Fatalf("supervisor was not re-woken after an invalid-block turn (wakes=%d)", e.step(t, run, "build").SupervisorWakes)
	}

	second = e.running(t, second)
	e.agentSays(t, build.IssueID, supervisor, &second, block("action: maybe"))
	e.endTask(t, second, "completed")
	build = e.step(t, run, "build")
	wantStepRow(t, build, StepAwaitingHuman, 1)
	if RunStatus(e.run(t, run.IssueID).Status) != RunWaitingHuman || !build.EscalationReason.Valid {
		t.Fatalf("run=%s escalation=%q", e.run(t, run.IssueID).Status, build.EscalationReason.String)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'escalated' AND payload->>'auto' = 'true'`, run.ID); n != 1 {
		t.Fatalf("auto escalations = %d, want 1", n)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND type = $2 AND issue_id = $3`, e.user, InboxTypeEscalation, run.IssueID); n != 1 {
		t.Fatalf("escalation inbox items = %d, want 1", n)
	}
	if n := e.countTasks(t, build, RoleSupervisor); n != 2 {
		t.Fatalf("supervisor tasks = %d, want 2", n)
	}
	// Reconciling again changes nothing: each turn's no-decision applies once.
	events := len(e.runEvents(t, run))
	if err := e.engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := len(e.runEvents(t, run)); got != events {
		t.Fatalf("run events %d -> %d after reconcile", events, got)
	}
}

func TestInvalidThenValidBlockIsADecision(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)

	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	e.agentSays(t, build.IssueID, supervisor, &review, block("action: maybe"))
	e.agentSays(t, build.IssueID, supervisor, &review, block("action: approve"))
	e.endTask(t, review, "completed")

	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	if n := e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'protocol_error' AND payload->>'reason' = 'no_decision'`, run.ID); n != 0 {
		t.Fatalf("no_decision events = %d, want 0", n)
	}
	if n := e.countTasks(t, e.step(t, run, "build"), RoleSupervisor); n != 1 {
		t.Fatalf("supervisor tasks = %d, want 1", n)
	}
}
