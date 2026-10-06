// server/internal/workflow/engine_test.go
package workflow

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
	assertWorkspaceMetadataContract(t, e)
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
	tick(t, e)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	tick(t, e)
	tick(t, e)
	build := stepByNode(t, e, def, "build")
	setStatus(t, e, build, "in_review") // agent finished, approval required
	tick(t, e)
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
	tick(t, e)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunDone || got.Status != "done" {
		t.Fatalf("definition state=%s status=%s, want done/done", dm.State, got.Status)
	}
}

func TestRejectRedispatchesWithFeedbackThenFails(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	tick(t, e)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	tick(t, e)
	tick(t, e)
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	tick(t, e)

	comment := commentID(t, e, def)
	build := stepByNode(t, e, def, "build")
	if ok, err := e.engine.ApplyEvent(ctx, build, EventReject, comment); err != nil || !ok {
		t.Fatalf("first reject should redo: %v", err)
	}
	last := e.rec.enqueued[len(e.rec.enqueued)-1]
	if !strings.HasPrefix(last, "mention:") {
		t.Fatalf("redo must carry the reject comment as trigger, got %q", last)
	}
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	tick(t, e)
	build = stepByNode(t, e, def, "build")
	if ok, err := e.engine.ApplyEvent(ctx, build, EventReject, comment); err != nil || !ok {
		t.Fatalf("second reject should be applied: %v", err)
	}
	if phaseOf(t, e, def, "build") != PhaseFailed {
		t.Fatalf("with max_retries=1 the second reject must fail the step, phase=%s", phaseOf(t, e, def, "build"))
	}
	tick(t, e)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunBlocked || got.Status != "blocked" {
		t.Fatalf("definition state=%s status=%s, want blocked", dm.State, got.Status)
	}
	build = stepByNode(t, e, def, "build")
	if ok, err := e.engine.ApplyEvent(ctx, build, EventRetry, pgtypeUUIDZero()); err != nil || !ok {
		t.Fatalf("retry should restart a failed step: %v", err)
	}
	if m, _ := readStepMeta(stepByNode(t, e, def, "build")); m.Phase != PhaseRunning || m.Attempts != 0 {
		t.Fatalf("after retry phase=%s attempts=%d", m.Phase, m.Attempts)
	}
}

func TestDispatchErrorFailsStepWithComment(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	e.rec.failNext = errString("agent is archived")
	tick(t, e)
	if phaseOf(t, e, def, "plan") != PhaseFailed {
		t.Fatalf("plan phase = %s, want failed", phaseOf(t, e, def, "plan"))
	}
	if !hasComment(e, "agent is archived") {
		t.Fatal("the failure reason must be commented on the step")
	}
	if s := stepByNode(t, e, def, "plan"); s.Status != "blocked" {
		t.Fatalf("failed step issue status = %s, want blocked", s.Status)
	}
	tick(t, e)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunBlocked {
		t.Fatalf("workflow state = %s, want blocked", dm.State)
	}
}

func TestDeletedStepBlocksInsteadOfCompleting(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	e.fx.Exec(t, `DELETE FROM issue WHERE id = $1`, uuidStr(stepByNode(t, e, def, "build")))
	tick(t, e)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	tick(t, e)
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

	tick(t, e)
	if phaseOf2(t, e, def, "plan") != PhaseRunning {
		t.Fatal("a step that lost its parent must still be dispatched")
	}
	setStatus(t, e, plan, "done")
	tick(t, e)
	tick(t, e)
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
	e, def := expanded(t)
	tick(t, e)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	for i := 0; i < 2; i++ {
		e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "failed", "runtime_id": e.runtime})
		tick(t, e)
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

// setDispatchedAt rewrites a step's dispatch stamp ("" clears it).
func setDispatchedAt(t *testing.T, e *env, step db.Issue, at time.Time) db.Issue {
	t.Helper()
	got, _ := e.q.GetIssue(context.Background(), step.ID)
	m, _ := readStepMeta(got)
	m.DispatchedAt = ""
	if !at.IsZero() {
		m.DispatchedAt = at.UTC().Format(time.RFC3339Nano)
	}
	if err := e.engine.writeMeta(context.Background(), got, m); err != nil {
		t.Fatal(err)
	}
	got, _ = e.q.GetIssue(context.Background(), step.ID)
	return got
}

func TestStaleAgentFailedSnapshotAppliesOnce(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	tick(t, e)
	snap := stepByNode(t, e, def, "plan") // running, attempts 0, max_retries 1
	if ok, err := e.engine.ApplyEvent(ctx, snap, EventAgentFailed, pgtypeUUIDZero()); err != nil || !ok {
		t.Fatalf("first = %v, %v", ok, err)
	}
	if ok, err := e.engine.ApplyEvent(ctx, snap, EventAgentFailed, pgtypeUUIDZero()); err != nil || ok {
		t.Fatalf("second with the stale snapshot = %v, %v; want false", ok, err)
	}
	m, _ := readStepMeta(stepByNode(t, e, def, "plan"))
	if m.Attempts != 1 || m.Phase != PhaseRunning || len(e.rec.enqueued) != 2 {
		t.Fatalf("attempts=%d phase=%s enqueued=%v, want 1/running/2", m.Attempts, m.Phase, e.rec.enqueued)
	}
}

func TestStaleDispatchLostSnapshotDispatchesOnce(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	tick(t, e)
	snap := stepByNode(t, e, def, "plan")
	before := len(e.rec.enqueued)
	for i, want := range []bool{true, false} {
		if ok, err := e.engine.ApplyEvent(ctx, snap, EventDispatchLost, pgtypeUUIDZero()); err != nil || ok != want {
			t.Fatalf("call %d = %v, %v; want %v", i, ok, err, want)
		}
	}
	if len(e.rec.enqueued)-before != 1 {
		t.Fatalf("dispatches = %d, want 1", len(e.rec.enqueued)-before)
	}
}

func TestOldTerminalTaskBeforeDispatchIsIgnored(t *testing.T) {
	e, def := expanded(t)
	tick(t, e)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "failed", "runtime_id": e.runtime, "created_at": dbfx.Raw("now() - interval '1 hour'")})
	tick(t, e)
	if got, _ := readStepMeta(stepByNode(t, e, def, "plan")); got.Phase != PhaseRunning || got.Attempts != 0 || len(e.rec.enqueued) != 1 {
		t.Fatalf("old failed task misread: phase=%s attempts=%d enqueued=%v", got.Phase, got.Attempts, e.rec.enqueued)
	}
}

func TestDispatchLostAfterGrace(t *testing.T) {
	e, def := expanded(t)
	tick(t, e)
	plan := stepByNode(t, e, def, "plan")
	tick(t, e)
	if len(e.rec.enqueued) != 1 {
		t.Fatalf("inside the grace window nothing is re-dispatched: %v", e.rec.enqueued)
	}
	setDispatchedAt(t, e, plan, time.Now().Add(-10*time.Minute))
	tick(t, e)
	if len(e.rec.enqueued) != 2 {
		t.Fatalf("enqueued = %v, want one re-dispatch after the grace", e.rec.enqueued)
	}
	tick(t, e) // fresh stamp again: grace window
	if len(e.rec.enqueued) != 2 {
		t.Fatalf("re-dispatch loop: %v", e.rec.enqueued)
	}
}

func TestAgentFinishedDuringTickIsNotRerun(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	tick(t, e)
	plan := stepByNode(t, e, def, "plan")
	stale := plan // observed while still running
	m, _ := readStepMeta(plan)
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "completed", "runtime_id": e.runtime})
	setStatus(t, e, plan, "done")
	if ok, err := e.engine.ApplyEvent(ctx, stale, EventAgentFailed, pgtypeUUIDZero()); err != nil || !ok {
		t.Fatalf("apply = %v, %v", ok, err)
	}
	got := stepByNode(t, e, def, "plan")
	if gm, _ := readStepMeta(got); gm.Phase != PhaseDone || gm.Attempts != 0 || got.Status != "done" || len(e.rec.enqueued) != 1 {
		t.Fatalf("phase=%s attempts=%d status=%s enqueued=%v, want done/0/done/1", gm.Phase, gm.Attempts, got.Status, e.rec.enqueued)
	}
}

func TestAgentFinishedTickPath(t *testing.T) {
	e2, def2 := expanded(t)
	tick(t, e2)
	p2 := stepByNode(t, e2, def2, "plan")
	m2, _ := readStepMeta(p2)
	e2.fx.Task(t, m2.AgentID, dbfx.Cols{"issue_id": uuidStr(p2), "status": "completed", "runtime_id": e2.runtime})
	setStatus(t, e2, p2, "done")
	tick(t, e2)
	if phaseOf(t, e2, def2, "plan") != PhaseDone || len(e2.rec.enqueued) != 1 {
		t.Fatalf("tick path: phase=%s enqueued=%v", phaseOf(t, e2, def2, "plan"), e2.rec.enqueued)
	}
}

func TestReassignedStepDoesNotRunAway(t *testing.T) {
	e, def := expanded(t)
	tick(t, e)
	plan := stepByNode(t, e, def, "plan")
	other := e.agent(t, "Other")
	e.fx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, uuidStr(plan), other)
	setDispatchedAt(t, e, plan, time.Now().Add(-10*time.Minute))
	e.fx.Task(t, other, dbfx.Cols{"issue_id": uuidStr(plan), "status": "queued", "runtime_id": e.runtime})
	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	if len(e.rec.enqueued) != 1 {
		t.Fatalf("a queued task by the new assignee must stop re-dispatch: %v", e.rec.enqueued)
	}

	// With no task rows for either agent and a stale stamp: exactly one dispatch.
	e3, def3 := expanded(t)
	tick(t, e3)
	p3 := stepByNode(t, e3, def3, "plan")
	o3 := e3.agent(t, "Other")
	e3.fx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, uuidStr(p3), o3)
	setDispatchedAt(t, e3, p3, time.Now().Add(-10*time.Minute))
	tick(t, e3)
	tick(t, e3)
	if len(e3.rec.enqueued) != 2 {
		t.Fatalf("enqueued = %v, want the initial dispatch plus exactly one", e3.rec.enqueued)
	}
}

func TestLostDispatchIsRedispatchedOnce(t *testing.T) {
	e, def := expanded(t)
	plan := stepByNode(t, e, def, "plan")
	plan = setStep(t, e, plan, PhaseRunning, 0, "in_progress") // claimed, but no task row and no enqueue
	setDispatchedAt(t, e, plan, time.Now().Add(-10*time.Minute))
	tick(t, e)
	if len(e.rec.enqueued) != 1 {
		t.Fatalf("enqueued = %v, want one re-dispatch", e.rec.enqueued)
	}
	if m, _ := readStepMeta(stepByNode(t, e, def, "plan")); m.Attempts != 0 {
		t.Fatalf("lost dispatch must not cost an attempt, attempts=%d", m.Attempts)
	}
	m, _ := readStepMeta(plan)
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "queued", "runtime_id": e.runtime})
	tick(t, e)
	if len(e.rec.enqueued) != 1 {
		t.Fatalf("re-dispatched despite a queued task: %v", e.rec.enqueued)
	}
}

func TestCancelledTaskFailsStepWithComment(t *testing.T) {
	e, def := expanded(t)
	tick(t, e)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "cancelled", "runtime_id": e.runtime})
	tick(t, e)
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
	e, def := expanded(t)
	tick(t, e)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "completed", "runtime_id": e.runtime})
	tick(t, e)
	if got, _ := readStepMeta(stepByNode(t, e, def, "plan")); got.Phase != PhaseRunning || got.Attempts != 1 || len(e.rec.enqueued) != 2 {
		t.Fatalf("first completed-without-finish: phase=%s attempts=%d enqueued=%v", got.Phase, got.Attempts, e.rec.enqueued)
	}
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "completed", "runtime_id": e.runtime, "created_at": dbfx.Raw("now() + interval '1 minute'")})
	tick(t, e)
	if phaseOf(t, e, def, "plan") != PhaseFailed || !hasComment(e, "Reply `/retry`") {
		t.Fatalf("second completed-without-finish must fail visibly, phase=%s", phaseOf(t, e, def, "plan"))
	}
}

func TestStatusReconciledForNonRunningPhase(t *testing.T) {
	e, def := expanded(t)
	build := stepByNode(t, e, def, "build")
	setStep(t, e, build, PhaseBlocked, 0, "in_review")
	tick(t, e)
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

// A close that passes the pre-check but loses the fenced write must leave the
// status exactly as the competing writer set it.
func TestCloseLostFenceLeavesStatusAlone(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	cur, _ := e.q.GetIssue(ctx, def.ID)
	dm, _ := readDefMeta(cur)
	dm.State = RunBlocked
	raw, _ := defMetaJSON(dm)
	// Simulate the competing writer landing after the status write, before the fence.
	e.engine.beforeClose = func() {
		e.engine.beforeClose = nil
		if _, err := e.q.SetWorkflowDefinitionState(ctx, db.SetWorkflowDefinitionStateParams{Value: raw, ID: def.ID, WorkspaceID: def.WorkspaceID, ExpectedState: "running"}); err != nil {
			t.Fatal(err)
		}
		setStatus(t, e, def, "blocked")
	}
	if err := e.engine.closeDefinition(ctx, def, RunDone, "done", "x"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if m, _ := readDefMeta(got); m.State != RunBlocked || got.Status != "blocked" {
		t.Fatalf("state=%s status=%s, want the competing writer's blocked/blocked", m.State, got.Status)
	}
}

func tick(t *testing.T, e *env) {
	t.Helper()
	if err := e.engine.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	checkWorkspaceMetadata(t, e, false)
}

func TestClockSkewInFlightTaskIsNeverRedispatched(t *testing.T) {
	e, def := expanded(t)
	tick(t, e)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	// The stamp is an hour ahead of every task row (a DB clock behind the app's).
	setDispatchedAt(t, e, plan, time.Now().Add(time.Hour))
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "queued", "runtime_id": e.runtime})
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "running", "runtime_id": e.runtime})
	tick(t, e)
	// Discriminating variant: the stamp is past the grace window and the only
	// task is running but was created before the stamp; without the in-flight
	// check this would look like a lost dispatch.
	setDispatchedAt(t, e, plan, time.Now().Add(-10*time.Minute))
	e.fx.Exec(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, uuidStr(plan))
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "running", "runtime_id": e.runtime, "created_at": dbfx.Raw("now() - interval '1 hour'")})
	tick(t, e)
	if got, _ := readStepMeta(stepByNode(t, e, def, "plan")); got.Phase != PhaseRunning || got.Attempts != 0 || len(e.rec.enqueued) != 1 {
		t.Fatalf("in-flight tasks must count regardless of since: phase=%s attempts=%d enqueued=%v", got.Phase, got.Attempts, e.rec.enqueued)
	}
}

func TestTerminalTaskOlderThanStampIsIgnoredEvenWithSkew(t *testing.T) {
	e, def := expanded(t)
	tick(t, e)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	setDispatchedAt(t, e, plan, time.Now().Add(time.Hour))
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "failed", "runtime_id": e.runtime})
	tick(t, e)
	if got, _ := readStepMeta(stepByNode(t, e, def, "plan")); got.Phase != PhaseRunning || got.Attempts != 0 || len(e.rec.enqueued) != 1 {
		t.Fatalf("older failed task misread: phase=%s attempts=%d enqueued=%v", got.Phase, got.Attempts, e.rec.enqueued)
	}
}

func TestQueuedTaskOlderThanStampIsInFlight(t *testing.T) {
	e, def := expanded(t)
	tick(t, e)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	// A duplicate-pending dispatch leaves an older queued task; the stamp is past the grace.
	e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": "queued", "runtime_id": e.runtime, "created_at": dbfx.Raw("now() - interval '1 hour'")})
	setDispatchedAt(t, e, plan, time.Now().Add(-10*time.Minute))
	tick(t, e)
	tick(t, e)
	if len(e.rec.enqueued) != 1 {
		t.Fatalf("an older queued task is in flight, got %v", e.rec.enqueued)
	}
}

func TestRejectAfterReassignmentTargetsCurrentAssignee(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	build := stepByNode(t, e, def, "build")
	build = setStep(t, e, build, PhaseBlocked, 0, "blocked")
	other := e.agent(t, "Other")
	e.fx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, uuidStr(build), other)
	build, _ = e.q.GetIssue(ctx, build.ID)
	if ok, err := e.engine.ApplyEvent(ctx, build, EventReject, commentID(t, e, def)); err != nil || !ok {
		t.Fatalf("reject = %v, %v", ok, err)
	}
	if len(e.rec.mentionAgents) != 1 || e.rec.mentionAgents[0] != other {
		t.Fatalf("mention targets %v, want the current assignee %s", e.rec.mentionAgents, other)
	}
}

// Two closers with different outcomes: ours (done) writes its status first, a
// competitor closes the run as blocked and wins the fence. State and status
// must still agree.
func TestConcurrentClosersConvergeStatusWithState(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	cur, _ := e.q.GetIssue(ctx, def.ID)
	dm, _ := readDefMeta(cur)
	dm.State = RunBlocked
	raw, _ := defMetaJSON(dm)
	e.engine.beforeClose = func() {
		e.engine.beforeClose = nil
		if _, err := e.q.SetWorkflowDefinitionState(ctx, db.SetWorkflowDefinitionStateParams{Value: raw, ID: def.ID, WorkspaceID: def.WorkspaceID, ExpectedState: "running"}); err != nil {
			t.Fatal(err)
		}
		// The competitor's own status write lost to ours, so status is still done.
	}
	if err := e.engine.closeDefinition(ctx, def, RunDone, "done", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if m, _ := readDefMeta(got); m.State != RunBlocked || got.Status != "blocked" {
		t.Fatalf("state=%s status=%s, want blocked/blocked", m.State, got.Status)
	}
}

func TestParkedTaskOlderThanStampIsInFlight(t *testing.T) {
	for _, status := range []string{"waiting_local_directory", "deferred"} {
		t.Run(status, func(t *testing.T) {
			e, def := expanded(t)
			tick(t, e)
			plan := stepByNode(t, e, def, "plan")
			m, _ := readStepMeta(plan)
			e.fx.Task(t, m.AgentID, dbfx.Cols{"issue_id": uuidStr(plan), "status": status, "runtime_id": e.runtime, "created_at": dbfx.Raw("now() - interval '1 hour'")})
			setDispatchedAt(t, e, plan, time.Now().Add(-10*time.Minute))
			tick(t, e)
			if len(e.rec.enqueued) != 1 {
				t.Fatalf("a %s task is in flight, got %v", status, e.rec.enqueued)
			}
		})
	}
}

// A reopen that lands between the converge read and its write must not be
// overwritten by the stale closer.
func TestConvergeDoesNotOverwriteAReopen(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	cur, _ := e.q.GetIssue(ctx, def.ID)
	dm, _ := readDefMeta(cur)
	dm.State = RunBlocked
	raw, _ := defMetaJSON(dm)
	e.engine.beforeClose = func() { // competitor closes as blocked and wins the fence
		e.engine.beforeClose = nil
		if _, err := e.q.SetWorkflowDefinitionState(ctx, db.SetWorkflowDefinitionStateParams{Value: raw, ID: def.ID, WorkspaceID: def.WorkspaceID, ExpectedState: "running"}); err != nil {
			t.Fatal(err)
		}
	}
	e.engine.beforeConvergeWrite = func() { // a /retry reopens before the converge writes
		e.engine.beforeConvergeWrite = nil
		if err := e.engine.reopenDefinition(ctx, def); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.engine.closeDefinition(ctx, def, RunDone, "done", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if m, _ := readDefMeta(got); m.State != RunRunning || got.Status != "in_progress" {
		t.Fatalf("state=%s status=%s, want running/in_progress", m.State, got.Status)
	}
}

func indepDoc(a, b string) string {
	return "```yaml\nnodes:\n  - id: a\n    agent: " + a + "\n    prompt: do a\n  - id: b\n    agent: " + b + "\n    prompt: do b\n```"
}

func TestNilInvokeCheckerFailsClosed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	e.engine.Invoke = nil
	def := e.flowIssue(t, "Ship", flowDoc("Planner", "Coder"))
	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunInvalid {
		t.Fatalf("state = %s, want invalid", dm.State)
	}
	if !hasComment(e, `unknown agent "Planner"`) {
		t.Fatal("a denied agent must look like an unknown one")
	}
}

func TestExpandHidesAgentsTheCreatorCannotInvoke(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	other := e.fx.User(t, "Other Owner", fmt.Sprintf("other-%d@example.test", time.Now().UnixNano()))
	e.fx.Member(t, e.ws, other, "member")
	private := e.fx.Agent(t, "Secret", e.runtime, dbfx.Cols{"owner_id": other})
	e.agent(t, "Coder")
	e.rec.deny(private)
	def := e.flowIssue(t, "Ship", flowDoc("Secret", "Coder"))
	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunInvalid {
		t.Fatalf("state = %s, want invalid", dm.State)
	}
	if !hasComment(e, `unknown agent "Secret"`) {
		t.Fatal("expected the same message as an unknown agent")
	}
	kids, _ := e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	if len(kids) != 0 {
		t.Fatalf("no steps may be created, got %d", len(kids))
	}
}

func TestPermissionRevokedMidRunFailsStepAtDispatch(t *testing.T) {
	e, def := expanded(t)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	e.rec.deny(m.AgentID)
	tick(t, e)
	if phaseOf(t, e, def, "plan") != PhaseFailed {
		t.Fatalf("plan phase = %s, want failed", phaseOf(t, e, def, "plan"))
	}
	if !hasComment(e, "not available to the workflow creator") {
		t.Fatal("the denial must be visible on the step")
	}
	if len(e.rec.enqueued) != 0 {
		t.Fatalf("nothing may be enqueued: %v", e.rec.enqueued)
	}
}

func TestCancelledDefinitionStopsRunOnce(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"cancelled", "done"} {
		t.Run(status, func(t *testing.T) {
			e, def := expanded(t)
			tick(t, e)
			before := len(e.rec.enqueued)
			setStatus(t, e, def, status)
			tick(t, e)
			tick(t, e)
			got, _ := e.q.GetIssue(ctx, def.ID)
			if dm, _ := readDefMeta(got); dm.State != RunStopped {
				t.Fatalf("state = %s, want stopped", dm.State)
			}
			n := 0
			e.rec.mu.Lock()
			for _, c := range e.rec.comments {
				if strings.Contains(c.Content, "Workflow stopped because this issue was closed") {
					n++
				}
			}
			e.rec.mu.Unlock()
			if n != 1 || len(e.rec.enqueued) != before {
				t.Fatalf("stop comments=%d, extra dispatches=%d, want 1/0", n, len(e.rec.enqueued)-before)
			}
		})
	}
}

func TestEngineDrivenDoneIsNotStopped(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	tick(t, e)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	tick(t, e)
	tick(t, e)
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	tick(t, e)
	if ok, err := e.engine.ApplyEvent(ctx, stepByNode(t, e, def, "build"), EventAccept, pgtypeUUIDZero()); err != nil || !ok {
		t.Fatalf("accept = %v, %v", ok, err)
	}
	tick(t, e)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunDone || got.Status != "done" {
		t.Fatalf("state=%s status=%s, want done/done", dm.State, got.Status)
	}
	if hasComment(e, "stopped because") {
		t.Fatal("a normal completion must not be reported as stopped")
	}
}

func TestOneFailingStepDoesNotBlockItsSibling(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Ship", indepDoc("Planner", "Coder"))
	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	e.engine.beforeObserve = func(node string) error {
		if node == "a" {
			return errString("boom")
		}
		return nil
	}
	if err := e.engine.Tick(ctx); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("tick error = %v, want the step error", err)
	}
	if phaseOf2(t, e, def, "b") != PhaseRunning || phaseOf2(t, e, def, "a") != PhasePending {
		t.Fatal("the healthy sibling must be observed even though step a failed")
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunRunning {
		t.Fatalf("a run with an unobserved step must not be closed, state=%s", dm.State)
	}
}

func TestAgentCreatedDefinitionRejectsApprovalSteps(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	agent := e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Ship", flowDoc("Planner", "Coder"))
	e.fx.Exec(t, `UPDATE issue SET creator_type = 'agent', creator_id = $2 WHERE id = $1`, uuidStr(def), agent)
	def, _ = e.q.GetIssue(ctx, def.ID)
	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunInvalid {
		t.Fatalf("state = %s, want invalid", dm.State)
	}
	if !hasComment(e, "agents cannot /accept") {
		t.Fatal("expected the approval explanation")
	}
}
