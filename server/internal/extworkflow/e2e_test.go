package extworkflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// End-to-end runs with simulated agents: the test plays every agent by
// moving child issues (through the real child-event hook) and ending tasks
// (through OnTaskTerminal, as the bus listener does). Decisions go straight
// to Advance, the call the decision API and the comment protocol make.

// childMoves sets a child issue's status the way an agent's CLI call does and
// lets the platform process the parent's sub-issue change.
func (e *env) childMoves(t *testing.T, parent, child pgtype.UUID, status string) {
	t.Helper()
	e.setStatus(t, child, status)
	if err := (&service.IssueWakeupService{Tasks: e.tasks}).ProcessChildEvents(context.Background(), parent); err != nil {
		t.Fatalf("ProcessChildEvents: %v", err)
	}
}

func (e *env) running(t *testing.T, task db.AgentTaskQueue) db.AgentTaskQueue {
	t.Helper()
	e.fx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, task.ID)
	task.Status = "running"
	return task
}

func (e *env) decide(t *testing.T, run db.ExtWorkflowRun, key string, d Decision, actor Actor) {
	t.Helper()
	if err := e.engine.Advance(context.Background(), run.ID, AdvanceInput{StepKey: key, Event: Event{Kind: EvDecision, Decision: d}, Actor: actor}); err != nil {
		t.Fatalf("decide %s on %s: %v", d.Action, key, err)
	}
}

func (e *env) summaryTask(t *testing.T, run db.ExtWorkflowRun) db.AgentTaskQueue {
	t.Helper()
	task, err := e.q.GetLatestExtWorkflowSummaryTask(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("summary task: %v", err)
	}
	return task
}

func (e *env) wantRunDone(t *testing.T, parent pgtype.UUID) {
	t.Helper()
	run := e.run(t, parent)
	if RunStatus(run.Status) != RunDone || !run.FinishedAt.Valid {
		t.Fatalf("run = %s (finished=%v), want done", run.Status, run.FinishedAt.Valid)
	}
	if got := e.issue(t, parent).Status; got != "in_review" {
		t.Fatalf("parent = %s, want in_review", got)
	}
}

func TestE2EHappyPath(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, review: true, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)

	spec := e.step(t, run, "spec")
	specTask := e.running(t, e.latestTask(t, spec, RoleStep))
	e.childMoves(t, parent, spec.IssueID, "done")
	e.endTask(t, specTask, "completed")
	wantStepRow(t, e.step(t, run, "spec"), StepDone, 1)

	build := e.step(t, run, "build")
	wantStepRow(t, build, StepRunning, 1)
	buildTask := e.running(t, e.latestTask(t, build, RoleStep))
	e.setStatus(t, build.IssueID, "in_review")
	e.endTask(t, buildTask, "completed")
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)

	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	if review.ExtWorkflowKind.String != KindReview || review.IssueID != parent {
		t.Fatalf("review task kind=%q issue=%v", review.ExtWorkflowKind.String, review.IssueID)
	}
	e.decide(t, run, "build", Decision{Action: ActionApprove}, Actor{Type: "agent", ID: supervisor, TaskID: review.ID})
	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	if got := e.issue(t, build.IssueID).Status; got != "done" {
		t.Fatalf("build child = %s, want done", got)
	}
	e.endTask(t, review, "completed")

	summary := e.summaryTask(t, run)
	if summary.AgentID != supervisor || summary.IssueID != parent || summary.Status != "queued" {
		t.Fatalf("summary task = %s for %v on %v", summary.Status, summary.AgentID, summary.IssueID)
	}
	e.endTask(t, summary, "completed")
	e.wantRunDone(t, parent)
	if n := e.fx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`, parent); n != 2 {
		t.Fatalf("milestone comments = %d, want started + finished", n)
	}
}

func TestE2EReviewRedoThenApprove(t *testing.T) {
	e := newEnv(t)
	supervisor, coder := e.agent(t, "Supervisor"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: coder, review: true})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	build := e.step(t, run, "build")

	first := e.running(t, e.latestTask(t, build, RoleStep))
	e.setStatus(t, build.IssueID, "in_review")
	e.endTask(t, first, "completed")
	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	e.decide(t, run, "build", Decision{Action: ActionRedo, Feedback: "add tests"}, Actor{Type: "agent", ID: supervisor, TaskID: review.ID})
	e.endTask(t, review, "completed")

	build = e.step(t, run, "build")
	wantStepRow(t, build, StepRunning, 2)
	if build.LastFeedback.String != "add tests" || e.issue(t, build.IssueID).Status != "in_progress" {
		t.Fatalf("feedback=%q child=%s", build.LastFeedback.String, e.issue(t, build.IssueID).Status)
	}
	second := e.running(t, e.latestTask(t, build, RoleStep))
	if second.ID == first.ID || !strings.Contains(second.HandoffNote.String, "attempt 2 of 3") {
		t.Fatalf("second attempt task %v note=%q", second.ID, second.HandoffNote.String)
	}

	e.setStatus(t, build.IssueID, "in_review")
	e.endTask(t, second, "completed")
	review = e.running(t, e.latestTask(t, e.step(t, run, "build"), RoleSupervisor))
	e.decide(t, run, "build", Decision{Action: ActionApprove}, Actor{Type: "agent", ID: supervisor, TaskID: review.ID})
	e.endTask(t, review, "completed")
	e.endTask(t, e.summaryTask(t, run), "completed")
	e.wantRunDone(t, parent)
	wantStepRow(t, e.step(t, run, "build"), StepDone, 2)
}

func TestE2ERequestRewindThenRewindRerunsDownstream(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	e.childMoves(t, parent, spec.IssueID, "done")
	e.endTask(t, e.latestTask(t, spec, RoleStep), "completed")

	build := e.step(t, run, "build")
	buildTask := e.running(t, e.latestTask(t, build, RoleStep))
	e.decide(t, run, "build", Decision{Action: ActionRequestRewind, To: "spec", Reason: "the spec assumes pagination"},
		Actor{Type: "agent", ID: coder, TaskID: buildTask.ID})
	e.endTask(t, buildTask, "completed")
	build = e.step(t, run, "build")
	wantStepRow(t, build, StepAwaitingSupervisor, 1)
	arbiter := e.running(t, e.latestTask(t, build, RoleSupervisor))
	if arbiter.ExtWorkflowKind.String != KindRewindRequest {
		t.Fatalf("supervisor kind = %q, want rewind_request", arbiter.ExtWorkflowKind.String)
	}

	e.decide(t, run, "build", Decision{Action: ActionRewind, To: "spec", Feedback: "use cursor pagination"},
		Actor{Type: "agent", ID: supervisor, TaskID: arbiter.ID})
	e.endTask(t, arbiter, "completed")
	if got := e.run(t, parent).RewindsUsed; got != 1 {
		t.Fatalf("rewinds used = %d", got)
	}
	spec = e.step(t, run, "spec")
	wantStepRow(t, spec, StepRunning, 1)
	if spec.LastFeedback.String != "use cursor pagination" {
		t.Fatalf("spec feedback = %q", spec.LastFeedback.String)
	}
	wantStepRow(t, e.step(t, run, "build"), StepPending, 0)
	if got := e.issue(t, build.IssueID).Status; got != "backlog" {
		t.Fatalf("build child = %s, want backlog", got)
	}

	e.childMoves(t, parent, spec.IssueID, "done")
	e.endTask(t, e.latestTask(t, spec, RoleStep), "completed")
	build = e.step(t, run, "build")
	wantStepRow(t, build, StepRunning, 1)
	if n := e.countTasks(t, build, RoleStep); n != 2 {
		t.Fatalf("build step tasks = %d, want the original and the rerun", n)
	}
	e.childMoves(t, parent, build.IssueID, "done")
	e.endTask(t, e.latestTask(t, build, RoleStep), "completed")
	e.endTask(t, e.summaryTask(t, run), "completed")
	e.wantRunDone(t, parent)
}

func TestE2EEscalateThenHumanDecision(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, coder := e.agent(t, "Supervisor"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: coder, review: true})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	build := e.step(t, run, "build")
	e.setStatus(t, build.IssueID, "in_review")
	e.endTask(t, e.latestTask(t, build, RoleStep), "completed")

	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	e.decide(t, run, "build", Decision{Action: ActionEscalate, Reason: "needs a product decision"}, Actor{Type: "agent", ID: supervisor, TaskID: review.ID})
	e.endTask(t, review, "completed")
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingHuman, 1)
	if RunStatus(e.run(t, parent).Status) != RunWaitingHuman {
		t.Fatalf("run = %s, want waiting_human", e.run(t, parent).Status)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND type = $2`, e.user, InboxTypeEscalation); n != 1 {
		t.Fatalf("escalation inbox items = %d", n)
	}

	approve := AdvanceInput{StepKey: "build", Event: Event{Kind: EvDecision, Decision: Decision{Action: ActionApprove}}, Actor: Actor{Type: "member", ID: e.user}}
	stale := approve
	stale.ExpectedStatus = StepAwaitingSupervisor
	if err := e.engine.Advance(ctx, run.ID, stale); !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("stale decision: %v", err)
	}
	approve.ExpectedStatus = StepAwaitingHuman
	if err := e.engine.Advance(ctx, run.ID, approve); err != nil {
		t.Fatalf("human approve: %v", err)
	}
	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	if RunStatus(e.run(t, parent).Status) != RunRunning {
		t.Fatalf("run = %s, want running", e.run(t, parent).Status)
	}
	e.endTask(t, e.summaryTask(t, run), "completed")
	e.wantRunDone(t, parent)
}

func TestE2EParentCancelCancelsTasks(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	task := e.running(t, e.latestTask(t, spec, RoleStep))

	e.setStatus(t, parent, "cancelled")
	if err := e.engine.OnParentChanged(context.Background(), parent); err != nil {
		t.Fatalf("OnParentChanged: %v", err)
	}
	if got := RunStatus(e.run(t, parent).Status); got != RunCancelled {
		t.Fatalf("run = %s, want cancelled", got)
	}
	if got := e.latestTask(t, spec, RoleStep); got.ID != task.ID || got.Status != "cancelled" || got.FailureReason.String != service.ExtWorkflowEngineCancelReason {
		t.Fatalf("spec task = %s (%q)", got.Status, got.FailureReason.String)
	}
	for _, key := range []string{"spec", "build"} {
		s := e.step(t, run, key)
		if StepStatus(s.Status) != StepCancelled || e.issue(t, s.IssueID).Status != "cancelled" {
			t.Fatalf("%s = %s, child %s", key, s.Status, e.issue(t, s.IssueID).Status)
		}
	}
	e.endTask(t, task, "cancelled") // the engine's own cancellation reports back harmlessly
}

func TestE2ERetryPendingIsIgnored(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	first := e.running(t, e.latestTask(t, spec, RoleStep))

	e.fx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', completed_at = now(), failure_reason = 'provider_network' WHERE id = $1`, first.ID)
	retry, err := e.q.CreateRetryTask(ctx, db.CreateRetryTaskParams{ID: first.ID})
	if err != nil {
		t.Fatalf("CreateRetryTask: %v", err)
	}
	if err := e.engine.OnTaskTerminal(ctx, first.ID); err != nil {
		t.Fatalf("OnTaskTerminal: %v", err)
	}
	wantStepRow(t, e.step(t, run, "spec"), StepRunning, 1)
	if e.countTasks(t, spec, RoleSupervisor) != 0 || retry.ExtWorkflowStepID != spec.ID {
		t.Fatalf("retry pending woke the supervisor or lost its stamp")
	}

	e.childMoves(t, parent, spec.IssueID, "done")
	e.endTask(t, e.running(t, retry), "completed")
	wantStepRow(t, e.step(t, run, "spec"), StepDone, 1)
}

func TestE2EReconcileRecoversALostTaskEvent(t *testing.T) {
	e := newEnv(t)
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	task := e.latestTask(t, spec, RoleStep)

	// The task fails, but the bus event never reaches the engine.
	e.fx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', started_at = now(), completed_at = now(), failure_reason = 'agent_error', error = 'crashed' WHERE id = $1`, task.ID)
	wantStepRow(t, e.step(t, run, "spec"), StepRunning, 1)

	if err := e.engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	spec = e.step(t, run, "spec")
	wantStepRow(t, spec, StepAwaitingSupervisor, 1)
	if sup := e.latestTask(t, spec, RoleSupervisor); sup.ExtWorkflowKind.String != KindFailure {
		t.Fatalf("supervisor kind = %q, want failure", sup.ExtWorkflowKind.String)
	}
}

func TestE2EEditingTheTemplateDoesNotChangeARunningRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "a", title: "A", agent: planner},
		wfNode{key: "b", title: "B", agent: coder, deps: []string{"a"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	a := e.step(t, run, "a")

	// Replace the template's nodes with a single node c while the run is active.
	if err := e.q.DeleteExtWorkflowNodes(ctx, wf); err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.CreateExtWorkflowNode(ctx, db.CreateExtWorkflowNodeParams{
		WorkflowID: wf, WorkspaceID: e.ws, Key: "c", Title: "C", AgentID: planner,
		Prompt: "Do C", MaxAttempts: 3, DependsOn: []string{}, Position: 0,
	}); err != nil {
		t.Fatal(err)
	}

	e.childMoves(t, parent, a.IssueID, "done")
	e.endTask(t, e.latestTask(t, a, RoleStep), "completed")

	b := e.step(t, run, "b")
	wantStepRow(t, b, StepRunning, 1)
	if n := e.countTasks(t, b, RoleStep); n != 1 {
		t.Fatalf("b step tasks = %d, want 1", n)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run_step WHERE run_id = $1 AND node_key = 'c'`, run.ID); n != 0 {
		t.Fatalf("step c exists in the running run (%d)", n)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run_step WHERE run_id = $1`, run.ID); n != 2 {
		t.Fatalf("run steps = %d, want the original 2", n)
	}
}
