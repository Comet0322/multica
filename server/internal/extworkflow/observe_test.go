package extworkflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
)

func TestOnTaskTerminalCompletedWithoutFinishingIsAFailure(t *testing.T) {
	e := newEnv(t)
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)

	e.endTask(t, e.latestTask(t, e.step(t, run, "spec"), RoleStep), "completed")
	spec := e.step(t, run, "spec")
	wantStepRow(t, spec, StepAwaitingSupervisor, 1)
	if spec.PendingReason.String != string(PendingFailure) {
		t.Fatalf("reason = %q", spec.PendingReason.String)
	}
	if sup := e.latestTask(t, spec, RoleSupervisor); sup.ExtWorkflowKind.String != KindFailure || sup.IssueID != parent {
		t.Fatalf("supervisor task kind=%q issue=%v", sup.ExtWorkflowKind.String, sup.IssueID)
	}
}

func TestOnChildEventsDefersNotificationsToCommit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	e.setStatus(t, spec.IssueID, "done")
	published := e.pub.count()

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	hctx, after := service.WithExtAfterCommit(ctx)
	if err := e.engine.OnChildEvents(hctx, tx, parent, []service.ExtChildEvent{{ChildID: spec.IssueID, Kind: "closed"}}); err != nil {
		t.Fatalf("OnChildEvents: %v", err)
	}
	if e.pub.count() != published {
		t.Fatalf("published before commit")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after.Run()
	if e.pub.count() != published+1 {
		t.Fatalf("published %d events after commit, want 1", e.pub.count()-published)
	}
	wantStepRow(t, e.step(t, run, "spec"), StepDone, 1)
	build := e.step(t, run, "build")
	wantStepRow(t, build, StepRunning, 1)
	if task := e.latestTask(t, build, RoleStep); task.Status != "queued" || task.AgentID != coder {
		t.Fatalf("build task = %s for %v", task.Status, task.AgentID)
	}
}

func TestSilentSupervisorIsWokenOnceThenEscalated(t *testing.T) {
	e := newEnv(t)
	supervisor, coder := e.agent(t, "Supervisor"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: coder, review: true})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	build := e.step(t, run, "build")

	e.setStatus(t, build.IssueID, "in_review")
	e.endTask(t, e.latestTask(t, build, RoleStep), "completed")
	build = e.step(t, run, "build")
	wantStepRow(t, build, StepAwaitingSupervisor, 1)
	first := e.latestTask(t, build, RoleSupervisor)
	if first.ExtWorkflowKind.String != KindReview {
		t.Fatalf("first supervisor kind = %q", first.ExtWorkflowKind.String)
	}

	e.endTask(t, first, "completed")
	second := e.latestTask(t, e.step(t, run, "build"), RoleSupervisor)
	if second.ID == first.ID || e.step(t, run, "build").SupervisorWakes != 2 {
		t.Fatalf("supervisor was not re-woken (wakes=%d)", e.step(t, run, "build").SupervisorWakes)
	}

	e.endTask(t, second, "completed")
	build = e.step(t, run, "build")
	wantStepRow(t, build, StepAwaitingHuman, 1)
	if RunStatus(e.run(t, parent).Status) != RunWaitingHuman || !build.EscalationReason.Valid {
		t.Fatalf("run=%s escalation=%q", e.run(t, parent).Status, build.EscalationReason.String)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND type = $2 AND issue_id = $3`, e.user, InboxTypeEscalation, parent); n != 1 {
		t.Fatalf("escalation inbox items = %d, want 1", n)
	}
	if n := e.countTasks(t, build, RoleSupervisor); n != 2 {
		t.Fatalf("supervisor tasks = %d, want 2", n)
	}
}

func TestOnParentDeletedCancelsTheRun(t *testing.T) {
	e := newEnv(t)
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	task := e.latestTask(t, e.step(t, run, "spec"), RoleStep)

	if err := e.engine.OnParentDeleted(context.Background(), parent); err != nil {
		t.Fatalf("OnParentDeleted: %v", err)
	}
	if got := RunStatus(e.run(t, parent).Status); got != RunCancelled {
		t.Fatalf("run = %s, want cancelled", got)
	}
	spec := e.step(t, run, "spec")
	wantStepRow(t, spec, StepCancelled, 1)
	if got := e.latestTask(t, spec, RoleStep); got.ID != task.ID || got.Status != "cancelled" || got.FailureReason.String != service.ExtWorkflowEngineCancelReason {
		t.Fatalf("task = %s (%q)", got.Status, got.FailureReason.String)
	}
	if got := e.issue(t, spec.IssueID).Status; got != "cancelled" {
		t.Fatalf("child = %s, want cancelled", got)
	}
}

// childEvents delivers child events through a real transaction, as the
// issue_child_event processor does.
func (e *env) childEvents(t *testing.T, parent pgtype.UUID, children ...pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	hctx, after := service.WithExtAfterCommit(ctx)
	evs := make([]service.ExtChildEvent, 0, len(children))
	for _, c := range children {
		evs = append(evs, service.ExtChildEvent{ChildID: c, Kind: "closed"})
	}
	if err := e.engine.OnChildEvents(hctx, tx, parent, evs); err != nil {
		t.Fatalf("OnChildEvents: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after.Run()
}

func TestEngineOwnChildWritesAreHarmlessTicks(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	e.setStatus(t, spec.IssueID, "done")
	e.childEvents(t, parent, spec.IssueID)
	build := e.step(t, run, "build")
	wantStepRow(t, build, StepRunning, 1)

	// The engine's own status writes on the children come back as events.
	events, tasks := len(e.runEvents(t, run)), e.countTasks(t, build, RoleStep)
	e.childEvents(t, parent, spec.IssueID, build.IssueID)
	e.childEvents(t, parent, spec.IssueID, build.IssueID)
	if got := len(e.runEvents(t, run)); got != events {
		t.Fatalf("run events %d -> %d, want no change", events, got)
	}
	if got := e.countTasks(t, build, RoleStep); got != tasks {
		t.Fatalf("build tasks %d -> %d, want no change", tasks, got)
	}
	wantStepRow(t, e.step(t, run, "build"), StepRunning, 1)
	wantStepRow(t, e.step(t, run, "spec"), StepDone, 1)
}

func TestOnTaskTerminalIgnoresEngineCancelledTask(t *testing.T) {
	e := newEnv(t)
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	task := e.latestTask(t, e.step(t, run, "spec"), RoleStep)
	e.fx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled', failure_reason = $2, completed_at = now() WHERE id = $1`, task.ID, service.ExtWorkflowEngineCancelReason)
	events := len(e.runEvents(t, run))
	if err := e.engine.OnTaskTerminal(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	wantStepRow(t, e.step(t, run, "spec"), StepRunning, 1)
	if got := len(e.runEvents(t, run)); got != events {
		t.Fatalf("run events %d -> %d, want no change", events, got)
	}
}
