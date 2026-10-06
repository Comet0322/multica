package extworkflow

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestValidateAssignment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, worker := e.agent(t, "Supervisor"), e.agent(t, "Worker")
	ok := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: worker})
	empty := e.workflow(t, supervisor, 3)
	archived := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: worker})
	e.fx.Exec(t, `UPDATE ext_workflow SET archived_at = now() WHERE id = $1`, archived)
	idle := util.MustParseUUID(e.fx.Agent(t, "No runtime", ""))
	noRuntime := e.workflow(t, idle, 3, wfNode{key: "build", title: "Build", agent: worker})

	assignErr := func(err error) *AssignError {
		t.Helper()
		var ae *AssignError
		if !errors.As(err, &ae) {
			t.Fatalf("err = %v, want *AssignError", err)
		}
		return ae
	}
	if err := e.engine.ValidateAssignment(ctx, e.ws, ok, "member", e.user); err != nil {
		t.Fatalf("valid assignment: %v", err)
	}
	for name, tc := range map[string]struct {
		workflow pgtype.UUID
		status   int
		sentinel error
	}{
		"unknown":    {dbid.NewV7(), http.StatusBadRequest, ErrWorkflowNotFound},
		"archived":   {archived, http.StatusBadRequest, ErrWorkflowArchived},
		"no nodes":   {empty, http.StatusBadRequest, ErrNoNodes},
		"supervisor": {noRuntime, http.StatusBadRequest, ErrSupervisorUnavailable},
	} {
		ae := assignErr(e.engine.ValidateAssignment(ctx, e.ws, tc.workflow, "member", e.user))
		if ae.Status != tc.status || !errors.Is(ae, tc.sentinel) {
			t.Fatalf("%s: status=%d err=%v", name, ae.Status, ae.Err)
		}
	}
	e.access.deny(worker)
	if ae := assignErr(e.engine.ValidateAssignment(ctx, e.ws, ok, "member", e.user)); ae.Status != http.StatusForbidden || !errors.Is(ae, ErrAgentNotInvokable) {
		t.Fatalf("denied: status=%d err=%v", ae.Status, ae.Err)
	}
	off := NewEngine(Deps{Pool: e.pool, Queries: e.q, Issues: e.issues, Tasks: e.tasks, Access: e.access, Publisher: e.pub})
	if ae := assignErr(off.ValidateAssignment(ctx, e.ws, ok, "member", e.user)); ae.Status != http.StatusConflict || ae.Code != "workflow_engine_disabled" {
		t.Fatalf("disabled: status=%d code=%s", ae.Status, ae.Code)
	}
}

func TestStartRunCreatesChildrenAndDispatchesRoots(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, review: true, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")

	run := e.start(t, parent)
	if RunStatus(run.Status) != RunRunning || run.TriggeredByType != "member" || run.TriggeredByID != e.user {
		t.Fatalf("run = %s by %s:%v", run.Status, run.TriggeredByType, run.TriggeredByID)
	}
	spec, build := e.step(t, run, "spec"), e.step(t, run, "build")
	wantStepRow(t, spec, StepRunning, 1)
	wantStepRow(t, build, StepPending, 0)
	specChild, buildChild := e.issue(t, spec.IssueID), e.issue(t, build.IssueID)
	if specChild.ParentIssueID != parent || specChild.Status != "in_progress" || specChild.AssigneeID != planner || specChild.Title != "Ship feature · Spec" {
		t.Fatalf("spec child = %q %s parent=%v assignee=%v", specChild.Title, specChild.Status, specChild.ParentIssueID, specChild.AssigneeID)
	}
	if buildChild.Status != "backlog" || e.countTasks(t, build, RoleStep) != 0 {
		t.Fatalf("build child = %s with %d tasks, want backlog and none", buildChild.Status, e.countTasks(t, build, RoleStep))
	}
	task := e.latestTask(t, spec, RoleStep)
	if task.Status != "queued" || task.AgentID != planner || task.IssueID != spec.IssueID ||
		task.ExtWorkflowRunID != run.ID || task.ExtWorkflowKind.String != KindStep || task.OriginatorUserID != e.user {
		t.Fatalf("spec task = %+v", task)
	}
	if got := e.issue(t, parent).Status; got != "in_progress" {
		t.Fatalf("parent status = %s, want in_progress", got)
	}
	if got, want := e.runEvents(t, run), []string{RunEventRunStarted, RunEventStepStarted}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`, parent); n != 1 {
		t.Fatalf("milestone comments = %d, want 1", n)
	}
	if !reflect.DeepEqual(e.pub.events, []string{protocol.EventExtWorkflowRunUpdated + ":" + util.UUIDToString(run.ID)}) {
		t.Fatalf("published = %v", e.pub.events)
	}

	if err := e.engine.StartRun(ctx, parent, "member", e.user); err != nil {
		t.Fatalf("second StartRun: %v", err)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run WHERE issue_id = $1`, parent); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

func TestStartRunIgnoresOtherAssignees(t *testing.T) {
	e := newEnv(t)
	agent := e.agent(t, "Solo")
	issue := util.MustParseUUID(e.fx.Issue(t, "Plain", testutil.Cols{"assignee_type": "agent", "assignee_id": agent}))
	if err := e.engine.StartRun(context.Background(), issue, "member", e.user); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run WHERE issue_id = $1`, issue); n != 0 {
		t.Fatalf("runs = %d, want 0", n)
	}
}

func TestStartRunDispatchFailureGoesToSupervisor(t *testing.T) {
	e := newEnv(t)
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	parent := e.parentIssue(t, wf, "todo")
	e.access.deny(planner) // the assigner lost access after assigning

	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	wantStepRow(t, spec, StepAwaitingSupervisor, 1)
	if spec.PendingReason.String != string(PendingFailure) || spec.SupervisorWakes != 1 || e.countTasks(t, spec, RoleStep) != 0 {
		t.Fatalf("spec reason=%q wakes=%d step tasks=%d", spec.PendingReason.String, spec.SupervisorWakes, e.countTasks(t, spec, RoleStep))
	}
	sup := e.latestTask(t, spec, RoleSupervisor)
	if sup.IssueID != parent || sup.AgentID != supervisor || sup.ExtWorkflowKind.String != KindFailure {
		t.Fatalf("supervisor task issue=%v agent=%v kind=%q", sup.IssueID, sup.AgentID, sup.ExtWorkflowKind.String)
	}
}

func TestAdvanceGuardsDecisions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	run := e.start(t, e.parentIssue(t, wf, "todo"))

	err := e.engine.Advance(ctx, run.ID, AdvanceInput{StepKey: "spec", Event: Event{Kind: EvDecision, Decision: Decision{Action: ActionApprove}}, Actor: Actor{Type: "member", ID: e.user}})
	if !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("approve while running: %v", err)
	}
	err = e.engine.Advance(ctx, run.ID, AdvanceInput{StepKey: "spec", Event: Event{Kind: EvTick}, ExpectedStatus: StepAwaitingHuman})
	if !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("expected-status guard: %v", err)
	}
	if err := e.engine.Advance(ctx, dbid.NewV7(), AdvanceInput{Event: Event{Kind: EvTick}}); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	wantStepRow(t, e.step(t, run, "spec"), StepRunning, 1)
}

func TestStartRunIsIdempotentWhileARunIsActive(t *testing.T) {
	e := newEnv(t)
	a := e.agent(t, "A")
	wf := e.workflow(t, e.agent(t, "Sup"), 3, wfNode{key: "a", title: "A", agent: a})
	parent := e.parentIssue(t, wf, "todo")
	e.start(t, parent)
	if err := e.engine.StartRun(context.Background(), parent, "member", e.user); err != nil {
		t.Fatalf("second StartRun: %v", err)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run WHERE issue_id=$1`, parent); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM issue WHERE parent_issue_id=$1`, parent); n != 1 {
		t.Fatalf("children = %d, want 1", n)
	}
}
