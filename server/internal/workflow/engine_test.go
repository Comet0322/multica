// server/internal/workflow/engine_test.go
package workflow

import (
	"context"
	"strings"
	"sync"
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

	// The recorder queues no task row; add the one a real enqueue would create.
	pm, _ := readStepMeta(plan)
	e.fx.Task(t, pm.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "queued", "runtime_id": e.runtime})
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
	if !hasComment(e, "Waiting for review") {
		t.Fatal("blocking for approval must post the review-request comment")
	}

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
	if !hasComment(e, "agent is archived") {
		t.Fatal("the failure reason must be commented on the step")
	}
	if s := stepByNode(t, e, def, "plan"); s.Status != "blocked" {
		t.Fatalf("failed step issue status = %s, want blocked", s.Status)
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
	if !hasComment(e, "a step issue is missing") {
		t.Fatal("a missing step must be explained in a comment")
	}
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

func hasComment(e *env, sub string) bool {
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	for _, c := range e.rec.comments {
		if strings.Contains(c.Content, sub) {
			return true
		}
	}
	return false
}

// setStep forces a step's phase/attempts and matching issue status.
func setStep(t *testing.T, e *env, step db.Issue, phase Phase, attempts int, status string) db.Issue {
	t.Helper()
	m, _ := readStepMeta(step)
	m.Phase, m.Attempts = phase, attempts
	if err := e.engine.writeMeta(context.Background(), step, m); err != nil {
		t.Fatal(err)
	}
	setStatus(t, e, step, status)
	got, err := e.q.GetIssue(context.Background(), step.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func race(fns ...func()) {
	var ready, wg sync.WaitGroup
	start := make(chan struct{})
	ready.Add(len(fns))
	for _, f := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			f()
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
}

func TestConcurrentAcceptAndRejectApplyExactlyOnce(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	build := stepByNode(t, e, def, "build")
	comment := commentID(t, e, def)
	for i := 0; i < 20; i++ {
		build = setStep(t, e, build, PhaseBlocked, 0, "blocked")
		var okA, okR bool
		var errA, errR error
		race(
			func() { okA, errA = e.engine.ApplyEvent(ctx, build, EventAccept, pgtypeUUIDZero()) },
			func() { okR, errR = e.engine.ApplyEvent(ctx, build, EventReject, comment) },
		)
		if errA != nil || errR != nil {
			t.Fatalf("iter %d errors: %v %v", i, errA, errR)
		}
		if okA == okR {
			t.Fatalf("iter %d: accept=%v reject=%v, want exactly one applied", i, okA, okR)
		}
		m, _ := readStepMeta(stepByNode(t, e, def, "build"))
		if okA && (m.Phase != PhaseDone || m.Attempts != 0) || okR && (m.Phase != PhaseRunning || m.Attempts != 1) {
			t.Fatalf("iter %d: inconsistent end state accept=%v phase=%s attempts=%d", i, okA, m.Phase, m.Attempts)
		}
	}
}

func TestConcurrentAgentFailedBumpsAttemptsOnce(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	plan := stepByNode(t, e, def, "plan")
	for i := 0; i < 20; i++ {
		plan = setStep(t, e, plan, PhaseRunning, 0, "in_progress")
		var a, b bool
		var ea, eb error
		race(
			func() { a, ea = e.engine.ApplyEvent(ctx, plan, EventAgentFailed, pgtypeUUIDZero()) },
			func() { b, eb = e.engine.ApplyEvent(ctx, plan, EventAgentFailed, pgtypeUUIDZero()) },
		)
		if ea != nil || eb != nil || a == b {
			t.Fatalf("iter %d: applied %v/%v errs %v/%v, want exactly one", i, a, b, ea, eb)
		}
		if m, _ := readStepMeta(stepByNode(t, e, def, "plan")); m.Attempts != 1 || m.Phase != PhaseRunning {
			t.Fatalf("iter %d: attempts=%d phase=%s, want 1/running", i, m.Attempts, m.Phase)
		}
	}
}

func TestLostDispatchIsRedispatchedOnce(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	plan := stepByNode(t, e, def, "plan")
	setStep(t, e, plan, PhaseRunning, 0, "in_progress") // claimed, but no task row and no enqueue
	_ = e.engine.Tick(ctx)
	if len(e.rec.enqueued) != 1 {
		t.Fatalf("enqueued = %v, want one re-dispatch", e.rec.enqueued)
	}
	if m, _ := readStepMeta(stepByNode(t, e, def, "plan")); m.Attempts != 0 {
		t.Fatalf("lost dispatch must not cost an attempt, attempts=%d", m.Attempts)
	}
	m, _ := readStepMeta(plan)
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "queued", "runtime_id": e.runtime})
	_ = e.engine.Tick(ctx)
	if len(e.rec.enqueued) != 1 {
		t.Fatalf("re-dispatched despite a queued task: %v", e.rec.enqueued)
	}
}

func TestCancelledTaskFailsStepWithComment(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	_ = e.engine.Tick(ctx)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "cancelled", "runtime_id": e.runtime})
	_ = e.engine.Tick(ctx)
	if phaseOf(t, e, def, "plan") != PhaseFailed {
		t.Fatalf("plan phase = %s, want failed", phaseOf(t, e, def, "plan"))
	}
	if !hasComment(e, "ended without finishing") {
		t.Fatal("a cancelled run must leave a visible comment")
	}
	if len(e.rec.enqueued) != 1 {
		t.Fatalf("a cancelled run must not be redispatched: %v", e.rec.enqueued)
	}
}

func TestCompletedTaskWithoutFinishingCountsAsFailedAttempt(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	_ = e.engine.Tick(ctx)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "completed", "runtime_id": e.runtime})
	_ = e.engine.Tick(ctx)
	if got, _ := readStepMeta(stepByNode(t, e, def, "plan")); got.Phase != PhaseRunning || got.Attempts != 1 || len(e.rec.enqueued) != 2 {
		t.Fatalf("first completed-without-finish: phase=%s attempts=%d enqueued=%v", got.Phase, got.Attempts, e.rec.enqueued)
	}
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "completed", "runtime_id": e.runtime, "created_at": dbfx.Raw("now() + interval '1 minute'")})
	_ = e.engine.Tick(ctx)
	if phaseOf(t, e, def, "plan") != PhaseFailed || !hasComment(e, "Reply `/retry`") {
		t.Fatalf("second completed-without-finish must fail visibly, phase=%s", phaseOf(t, e, def, "plan"))
	}
}

func TestStatusReconciledForNonRunningPhase(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	build := stepByNode(t, e, def, "build")
	setStep(t, e, build, PhaseBlocked, 0, "in_review")
	_ = e.engine.Tick(ctx)
	if got := stepByNode(t, e, def, "build"); got.Status != "blocked" {
		t.Fatalf("status = %s, want blocked", got.Status)
	}
}

func TestStaleCloseWritesNothing(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	stale := def // still looks running
	cur, _ := e.q.GetIssue(ctx, def.ID)
	dm, _ := readDefMeta(cur)
	dm.State = RunBlocked
	if err := e.engine.writeMeta(ctx, cur, dm); err != nil {
		t.Fatal(err)
	}
	before := len(e.rec.comments)
	if err := e.engine.closeDefinition(ctx, stale, RunDone, "done", "should not appear"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if m, _ := readDefMeta(got); m.State != RunBlocked || got.Status == "done" || len(e.rec.comments) != before {
		t.Fatalf("stale close wrote state=%s status=%s comments=%d", m.State, got.Status, len(e.rec.comments))
	}
}
