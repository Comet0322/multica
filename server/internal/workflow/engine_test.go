// server/internal/workflow/engine_test.go
package workflow

import (
	"context"
	"strings"
	"testing"

	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// expanded returns an env with an expanded two-step flow: plan -> build(approval).
func expanded(t *testing.T) (*env, db.Issue) {
	t.Helper()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Ship", flowDoc("Planner", "Coder"))
	if err := e.engine.Expand(context.Background(), def); err != nil {
		t.Fatal(err)
	}
	return e, def
}

func stepByNode(t *testing.T, e *env, def db.Issue, node string) db.Issue {
	t.Helper()
	kids, err := e.q.ListWorkflowChildren(context.Background(), dbListChildren(def))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kids {
		if m, ok := readStepMeta(k); ok && m.Node == node {
			return k
		}
	}
	t.Fatalf("no step %q", node)
	return db.Issue{}
}

func phaseOf(t *testing.T, e *env, def db.Issue, node string) Phase {
	t.Helper()
	m, _ := readStepMeta(stepByNode(t, e, def, node))
	return m.Phase
}

func setStatus(t *testing.T, e *env, i db.Issue, status string) {
	t.Helper()
	e.fx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, uuidStr(i), status)
}

func TestTickRunsStepsInDependencyOrder(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)

	if err := e.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "plan") != PhaseRunning || phaseOf(t, e, def, "build") != PhasePending {
		t.Fatal("tick 1: only plan should be running")
	}
	plan := stepByNode(t, e, def, "plan")
	if plan.Status != "in_progress" || len(e.rec.enqueued) != 1 {
		t.Fatalf("plan status=%s enqueued=%v", plan.Status, e.rec.enqueued)
	}

	if err := e.engine.Tick(ctx); err != nil { // nothing changed: idempotent
		t.Fatal(err)
	}
	if len(e.rec.enqueued) != 1 {
		t.Fatalf("idle tick re-dispatched: %v", e.rec.enqueued)
	}

	setStatus(t, e, plan, "done") // the agent finishes
	if err := e.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "plan") != PhaseDone {
		t.Fatal("plan should be done")
	}
	if err := e.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "build") != PhaseRunning || len(e.rec.enqueued) != 2 {
		t.Fatalf("build should be dispatched after plan: %v", e.rec.enqueued)
	}
}

func TestApprovalGateAcceptCompletesWorkflow(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	_ = e.engine.Tick(ctx)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	_ = e.engine.Tick(ctx)
	_ = e.engine.Tick(ctx)
	build := stepByNode(t, e, def, "build")
	setStatus(t, e, build, "in_review") // agent finished, approval required
	_ = e.engine.Tick(ctx)

	build = stepByNode(t, e, def, "build")
	if m, _ := readStepMeta(build); m.Phase != PhaseBlocked || build.Status != "blocked" {
		t.Fatalf("build phase=%s status=%s, want blocked/blocked", m.Phase, build.Status)
	}
	if ok, err := e.engine.ApplyEvent(ctx, build, EventAccept, pgtypeUUIDZero()); err != nil || !ok {
		t.Fatalf("accept = %v, %v", ok, err)
	}
	_ = e.engine.Tick(ctx)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunDone || got.Status != "done" {
		t.Fatalf("definition state=%s status=%s, want done/done", dm.State, got.Status)
	}
}

func TestRejectRedispatchesWithFeedbackThenFails(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	_ = e.engine.Tick(ctx)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	_ = e.engine.Tick(ctx)
	_ = e.engine.Tick(ctx)
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	_ = e.engine.Tick(ctx)

	comment := commentID(t, e, def)
	build := stepByNode(t, e, def, "build")
	if ok, _ := e.engine.ApplyEvent(ctx, build, EventReject, comment); !ok {
		t.Fatal("first reject should redo")
	}
	last := e.rec.enqueued[len(e.rec.enqueued)-1]
	if !strings.HasPrefix(last, "mention:") {
		t.Fatalf("redo must carry the reject comment as trigger, got %q", last)
	}
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	_ = e.engine.Tick(ctx)
	build = stepByNode(t, e, def, "build")
	if ok, _ := e.engine.ApplyEvent(ctx, build, EventReject, comment); !ok {
		t.Fatal("second reject should be applied")
	}
	if phaseOf(t, e, def, "build") != PhaseFailed {
		t.Fatalf("with max_retries=1 the second reject must fail the step, phase=%s", phaseOf(t, e, def, "build"))
	}
	_ = e.engine.Tick(ctx)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunBlocked || got.Status != "blocked" {
		t.Fatalf("definition state=%s status=%s, want blocked", dm.State, got.Status)
	}
	build = stepByNode(t, e, def, "build")
	if ok, _ := e.engine.ApplyEvent(ctx, build, EventRetry, pgtypeUUIDZero()); !ok {
		t.Fatal("retry should restart a failed step")
	}
	if m, _ := readStepMeta(stepByNode(t, e, def, "build")); m.Phase != PhaseRunning || m.Attempts != 0 {
		t.Fatalf("after retry phase=%s attempts=%d", m.Phase, m.Attempts)
	}
}

func TestDispatchErrorFailsStepWithComment(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	e.rec.failNext = errString("agent is archived")
	_ = e.engine.Tick(ctx)
	if phaseOf(t, e, def, "plan") != PhaseFailed {
		t.Fatalf("plan phase = %s, want failed", phaseOf(t, e, def, "plan"))
	}
	found := false
	for _, c := range e.rec.comments {
		if strings.Contains(c.Content, "agent is archived") {
			found = true
		}
	}
	if !found {
		t.Fatal("the failure reason must be commented on the step")
	}
	_ = e.engine.Tick(ctx)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunBlocked {
		t.Fatalf("workflow state = %s, want blocked", dm.State)
	}
}

func TestDeletedStepBlocksInsteadOfCompleting(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	e.fx.Exec(t, `DELETE FROM issue WHERE id = $1`, uuidStr(stepByNode(t, e, def, "build")))
	_ = e.engine.Tick(ctx)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	_ = e.engine.Tick(ctx)
	got, _ := e.q.GetIssue(ctx, def.ID)
	dm, _ := readDefMeta(got)
	if dm.State != RunBlocked || got.Status == "done" {
		t.Fatalf("a missing step must block the workflow, state=%s status=%s", dm.State, got.Status)
	}
}

func TestStepWithoutParentIsStillTracked(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	plan := stepByNode(t, e, def, "plan")
	e.fx.Exec(t, `UPDATE issue SET parent_issue_id = NULL WHERE id = $1`, uuidStr(plan))

	_ = e.engine.Tick(ctx)
	if phaseOf2(t, e, def, "plan") != PhaseRunning {
		t.Fatal("a step that lost its parent must still be dispatched")
	}
	setStatus(t, e, plan, "done")
	_ = e.engine.Tick(ctx)
	_ = e.engine.Tick(ctx)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State == RunBlocked {
		t.Fatal("a parentless step must not make the run look broken")
	}
	if phaseOf2(t, e, def, "build") != PhaseRunning {
		t.Fatal("the dependent step should run after the parentless one finishes")
	}
}

// phaseOf2 finds the step by run id, so it works when the parent link is gone.
func phaseOf2(t *testing.T, e *env, def db.Issue, node string) Phase {
	t.Helper()
	steps, err := e.q.ListWorkflowSteps(context.Background(), db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidStr(def)})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if m, ok := readStepMeta(s); ok && m.Node == node {
			return m.Phase
		}
	}
	t.Fatalf("no step %q", node)
	return ""
}

func TestAgentTaskFailureRetriesThenFails(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	_ = e.engine.Tick(ctx)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	for i := 0; i < 2; i++ {
		e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "failed", "runtime_id": e.runtime})
		_ = e.engine.Tick(ctx)
	}
	if phaseOf(t, e, def, "plan") != PhaseFailed {
		t.Fatalf("plan phase = %s, want failed after retries are exhausted", phaseOf(t, e, def, "plan"))
	}
	if len(e.rec.enqueued) != 2 {
		t.Fatalf("expected the initial dispatch plus one retry, got %v", e.rec.enqueued)
	}
}

func TestStrandedDefinitionIsMovedToInProgress(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	e.fx.Exec(t, `UPDATE issue SET status = 'todo' WHERE id = $1`, uuidStr(def))
	if err := e.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if got.Status != "in_progress" {
		t.Fatalf("definition status = %s, want in_progress", got.Status)
	}
}
