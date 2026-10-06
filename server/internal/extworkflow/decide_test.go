package extworkflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// member adds a workspace member with the given role.
func (e *env) member(t *testing.T, role string) pgtype.UUID {
	t.Helper()
	n := envSerial.Add(1)
	user := e.fx.User(t, "Member "+role, fmt.Sprintf("extwf-member-%d-%d@multica.test", os.Getpid(), n))
	e.fx.Member(t, e.fx.WorkspaceID, user, role)
	return util.MustParseUUID(user)
}

// reviewRun starts a one-step workflow whose step needs review and finishes
// its first attempt, so the step waits for the supervisor.
func (e *env) reviewRun(t *testing.T, starter pgtype.UUID) (db.ExtWorkflowRun, db.ExtWorkflowRunStep, pgtype.UUID) {
	t.Helper()
	supervisor, coder := e.agent(t, "Supervisor"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: coder, review: true})
	parent := e.parentIssue(t, wf, "todo")
	if err := e.engine.StartRun(context.Background(), parent, "member", starter); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	run := e.run(t, parent)
	build := e.step(t, run, "build")
	e.running(t, e.latestTask(t, build, RoleStep))
	e.childMoves(t, parent, build.IssueID, "done")
	build = e.step(t, run, "build")
	wantStepRow(t, build, StepAwaitingSupervisor, 1)
	return run, build, supervisor
}

func TestMemberCanDecide(t *testing.T) {
	e := newEnv(t)
	trigger, creator, admin, plain := e.member(t, "member"), e.member(t, "member"), e.member(t, "admin"), e.member(t, "member")
	run, _, _ := e.reviewRun(t, trigger)
	e.fx.Exec(t, `UPDATE ext_workflow SET creator_id = $1 WHERE id = $2`, creator, run.WorkflowID)
	for name, tc := range map[string]struct {
		user pgtype.UUID
		want bool
	}{
		"trigger": {trigger, true}, "creator": {creator, true}, "admin": {admin, true}, "plain": {plain, false}, "none": {pgtype.UUID{}, false},
	} {
		got, err := e.engine.MemberCanDecide(context.Background(), run, tc.user)
		if err != nil || got != tc.want {
			t.Errorf("%s: MemberCanDecide = %v, %v; want %v", name, got, err, tc.want)
		}
	}
}

func TestDecideChecksPermissionStatusAndAction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plain := e.member(t, "member")
	run, build, supervisor := e.reviewRun(t, e.user)
	approve := Decision{Action: ActionApprove}
	member := func(user pgtype.UUID, d Decision, expected StepStatus) error {
		return e.engine.Decide(ctx, DecideInput{RunID: run.ID, StepID: build.ID, Decision: d, ActorType: "member", ActorID: user, ExpectedStatus: expected})
	}

	if err := member(plain, approve, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("plain member: %v, want ErrForbidden", err)
	}
	if err := e.engine.Decide(ctx, DecideInput{RunID: run.ID, StepID: build.ID, Decision: approve, ActorType: "agent", ActorID: supervisor, OnBehalfOf: plain}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("supervisor for a plain member: %v, want ErrForbidden", err)
	}
	if err := member(e.user, Decision{Action: ActionEscalate, Reason: "x"}, ""); !errors.Is(err, ErrIllegalDecision) {
		t.Fatalf("member escalate: %v, want ErrIllegalDecision", err)
	}
	if err := member(e.user, Decision{Action: ActionRedo}, ""); !errors.Is(err, ErrIllegalDecision) {
		t.Fatalf("redo without feedback: %v, want ErrIllegalDecision", err)
	}
	if err := member(e.user, approve, StepAwaitingHuman); !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("stale expected status: %v, want ErrStatusMismatch", err)
	}
	if err := e.engine.Decide(ctx, DecideInput{RunID: run.ID, StepID: run.ID, Decision: approve, ActorType: "member", ActorID: e.user}); !errors.Is(err, ErrStepNotFound) {
		t.Fatalf("foreign step id: %v, want ErrStepNotFound", err)
	}
	if err := e.engine.Decide(ctx, DecideInput{RunID: build.ID, StepID: build.ID, Decision: approve, ActorType: "member", ActorID: e.user}); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("unknown run: %v, want ErrRunNotFound", err)
	}
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)

	if err := member(e.user, approve, StepAwaitingSupervisor); err != nil {
		t.Fatalf("approve: %v", err)
	}
	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	events, err := e.q.ListExtWorkflowRunEvents(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var decision db.ExtWorkflowRunEvent
	for _, ev := range events {
		if ev.Kind == RunEventDecision {
			decision = ev
		}
	}
	if decision.ActorType != "member" || decision.ActorID != e.user || decision.StepID != build.ID {
		t.Fatalf("decision event = %+v", decision)
	}
	if err := member(e.user, approve, ""); !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("second approve: %v, want ErrStatusMismatch", err)
	}
}

func TestCancelRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plain := e.member(t, "member")
	run, build, _ := e.reviewRun(t, e.user)

	if err := e.engine.CancelRun(ctx, run.ID, "member", plain); !errors.Is(err, ErrForbidden) {
		t.Fatalf("plain member cancel: %v, want ErrForbidden", err)
	}
	if err := e.engine.CancelRun(ctx, run.ID, "member", e.user); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := e.run(t, run.IssueID).Status; got != string(RunCancelled) {
		t.Fatalf("run = %s, want cancelled", got)
	}
	wantStepRow(t, e.step(t, run, "build"), StepCancelled, 1)
	if n := e.fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE ext_workflow_run_id = $1 AND status IN ('queued','dispatched','running')`, run.ID); n != 0 {
		t.Fatalf("%d tasks still active", n)
	}
	if got := e.issue(t, build.IssueID).Status; got != "cancelled" {
		t.Fatalf("child = %s, want cancelled", got)
	}
	if err := e.engine.CancelRun(ctx, run.ID, "member", e.user); !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("second cancel: %v, want ErrStatusMismatch", err)
	}
}

func TestDecideNeedsTheEngine(t *testing.T) {
	off := NewEngine(Deps{})
	if err := off.Decide(context.Background(), DecideInput{Decision: Decision{Action: ActionApprove}}); !errors.Is(err, ErrEngineDisabled) {
		t.Fatalf("Decide = %v, want ErrEngineDisabled", err)
	}
	if err := off.CancelRun(context.Background(), pgtype.UUID{}, "member", pgtype.UUID{}); !errors.Is(err, ErrEngineDisabled) {
		t.Fatalf("CancelRun = %v, want ErrEngineDisabled", err)
	}
}
