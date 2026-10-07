package extworkflow

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// escalatedRun starts a one-step review run and escalates its step: the
// person has one open escalation item. It returns the run and its parent.
func (e *env) escalatedRun(t *testing.T) (db.ExtWorkflowRun, pgtype.UUID) {
	t.Helper()
	supervisor, coder := e.agent(t, "Supervisor"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: coder, review: true})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	build := e.step(t, run, "build")
	e.setStatus(t, build.IssueID, "in_review")
	e.endTask(t, e.latestTask(t, build, RoleStep), "completed")
	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	e.decide(t, run, "build", Decision{Action: ActionEscalate, Reason: "needs a product decision"}, Actor{Type: "agent", ID: supervisor, TaskID: review.ID})
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingHuman, 1)
	if n := e.openEscalations(t, parent); n != 1 {
		t.Fatalf("open escalation items = %d, want 1", n)
	}
	return run, parent
}

func (e *env) openEscalations(t *testing.T, parent pgtype.UUID) int {
	t.Helper()
	return e.fx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND type = $2 AND issue_id = $3 AND archived = false`,
		e.user, InboxTypeEscalation, parent)
}

// wantEscalationResolved checks the escalation item left the inbox (archived
// and read) and the person's clients were told.
func (e *env) wantEscalationResolved(t *testing.T, parent pgtype.UUID, archived *archivedEvents) {
	t.Helper()
	if n := e.openEscalations(t, parent); n != 0 {
		t.Fatalf("open escalation items = %d, want 0", n)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND type = $2 AND issue_id = $3 AND read = false`,
		e.user, InboxTypeEscalation, parent); n != 0 {
		t.Fatalf("unread escalation items = %d, want 0", n)
	}
	got := archived.all()
	if len(got) != 1 {
		t.Fatalf("inbox:batch-archived events = %v, want 1", got)
	}
	p := got[0]
	if p["recipient_id"] != util.UUIDToString(e.user) || p["issue_id"] != util.UUIDToString(parent) || p["count"] != int64(1) {
		t.Fatalf("inbox:batch-archived payload = %v", p)
	}
}

type archivedEvents struct {
	mu       sync.Mutex
	payloads []map[string]any
}

func (a *archivedEvents) all() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]any(nil), a.payloads...)
}

func (e *env) watchBatchArchived() *archivedEvents {
	a := &archivedEvents{}
	e.bus.Subscribe(protocol.EventInboxBatchArchived, func(ev events.Event) {
		if p, ok := ev.Payload.(map[string]any); ok {
			a.mu.Lock()
			a.payloads = append(a.payloads, p)
			a.mu.Unlock()
		}
	})
	return a
}

func TestEscalationInboxResolvesOnHumanDecision(t *testing.T) {
	e := newEnv(t)
	run, parent := e.escalatedRun(t)
	archived := e.watchBatchArchived()

	e.decide(t, run, "build", Decision{Action: ActionApprove}, Actor{Type: "member", ID: e.user})
	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	e.wantEscalationResolved(t, parent, archived)
}

func TestEscalationInboxResolvesWhenTheRunIsCancelled(t *testing.T) {
	e := newEnv(t)
	run, parent := e.escalatedRun(t)
	archived := e.watchBatchArchived()

	if err := e.engine.Advance(context.Background(), run.ID, AdvanceInput{Event: Event{Kind: EvCancelRun, Reason: "not needed"}}); err != nil {
		t.Fatalf("cancel run: %v", err)
	}
	if got := RunStatus(e.run(t, parent).Status); got != RunCancelled {
		t.Fatalf("run = %s, want cancelled", got)
	}
	e.wantEscalationResolved(t, parent, archived)
}

func TestEscalationInboxStaysWhileTheStepWaits(t *testing.T) {
	e := newEnv(t)
	run, parent := e.escalatedRun(t)
	archived := e.watchBatchArchived()

	// A tick that leaves the step awaiting a person resolves nothing.
	if err := e.engine.Advance(context.Background(), run.ID, AdvanceInput{Event: Event{Kind: EvTick}}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n := e.openEscalations(t, parent); n != 1 || len(archived.all()) != 0 {
		t.Fatalf("open escalation items = %d, archived events = %v", n, archived.all())
	}
}
