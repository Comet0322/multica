package extworkflow

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockProbeAccess answers like fakeAccess but, on every call, tries to take
// the parent issue and run row locks on a fresh pool connection with NOWAIT.
// A failure means the caller still holds them: the permission check would
// have needed a second connection while the run was locked.
type lockProbeAccess struct {
	*fakeAccess
	pool   *pgxpool.Pool
	parent pgtype.UUID

	mu     sync.Mutex
	calls  int
	locked int
}

func (p *lockProbeAccess) CanInvokeAgent(ctx context.Context, ws pgtype.UUID, actorType string, actorID pgtype.UUID, agentID pgtype.UUID) (bool, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.parent.Valid && p.heldElsewhere(ctx) {
		p.mu.Lock()
		p.locked++
		p.mu.Unlock()
	}
	return p.fakeAccess.CanInvokeAgent(ctx, ws, actorType, actorID, agentID)
}

func (p *lockProbeAccess) heldElsewhere(ctx context.Context) bool {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false
	}
	defer tx.Rollback(ctx)
	for _, stmt := range []string{
		`SELECT 1 FROM ext_workflow_run WHERE issue_id = $1 FOR UPDATE NOWAIT`,
		`SELECT 1 FROM issue WHERE id = $1 FOR UPDATE NOWAIT`,
	} {
		rows, err := tx.Query(ctx, stmt, p.parent)
		if err != nil {
			return true
		}
		rows.Close()
		if rows.Err() != nil {
			return true
		}
	}
	return false
}

func (p *lockProbeAccess) snapshot() (calls, locked int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.locked
}

func TestPermissionCheckRunsOutsideTheRunLock(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	probe := &lockProbeAccess{fakeAccess: e.access, pool: e.pool, parent: parent}
	e.engine.access = probe

	run := e.start(t, parent) // StartRun dispatches the root
	if calls, locked := probe.snapshot(); calls == 0 || locked != 0 {
		t.Fatalf("StartRun: access calls=%d, made while locked=%d", calls, locked)
	}

	before, _ := probe.snapshot()
	spec := e.step(t, run, "spec")
	specTask := e.running(t, e.latestTask(t, spec, RoleStep))
	e.childMoves(t, parent, spec.IssueID, "done") // advance dispatches build
	e.endTask(t, specTask, "completed")
	wantStepRow(t, e.step(t, run, "build"), StepRunning, 1)
	if calls, locked := probe.snapshot(); calls == before || locked != 0 {
		t.Fatalf("advance: access calls=%d (was %d), made while locked=%d", calls, before, locked)
	}
}

func TestDeniedAgentStillTakesTheDispatchFailurePathOnAdvance(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)

	e.access.deny(coder) // revoked after the run started
	spec := e.step(t, run, "spec")
	specTask := e.running(t, e.latestTask(t, spec, RoleStep))
	e.childMoves(t, parent, spec.IssueID, "done")
	e.endTask(t, specTask, "completed")

	build := e.step(t, run, "build")
	wantStepRow(t, build, StepAwaitingSupervisor, 1)
	if build.PendingReason.String != string(PendingFailure) || e.countTasks(t, build, RoleStep) != 0 {
		t.Fatalf("build reason=%q step tasks=%d", build.PendingReason.String, e.countTasks(t, build, RoleStep))
	}
}
