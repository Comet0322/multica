// server/internal/workflow/e2e_test.go
package workflow

import (
	"context"
	"reflect"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// runToReview drives a fresh two-step flow until the approval step "build"
// is blocked waiting for review. The test plays the agents by moving the step
// issues' statuses itself.
func runToReview(t *testing.T) (*env, db.Issue) {
	t.Helper()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Ship feature", flowDoc("Planner", "Coder"))

	tick(t, e) // expand + dispatch plan
	if got := phaseOf(t, e, def, "plan"); got != PhaseRunning {
		t.Fatalf("plan phase = %s, want running", got)
	}
	if got := phaseOf(t, e, def, "build"); got != PhasePending {
		t.Fatalf("build phase = %s, want pending while plan runs", got)
	}
	plan := stepByNode(t, e, def, "plan")
	want := []string{"issue:" + uuidStr(plan)}
	if !reflect.DeepEqual(e.rec.enqueued, want) {
		t.Fatalf("enqueued = %v, want %v", e.rec.enqueued, want)
	}

	setStatus(t, e, plan, "done") // the planner agent finishes
	tick(t, e)
	tick(t, e) // build becomes ready, then dispatched
	if got := phaseOf(t, e, def, "plan"); got != PhaseDone {
		t.Fatalf("plan phase = %s, want done", got)
	}
	if got := phaseOf(t, e, def, "build"); got != PhaseRunning {
		t.Fatalf("build phase = %s, want running", got)
	}
	build := stepByNode(t, e, def, "build")
	want = append(want, "issue:"+uuidStr(build))
	if !reflect.DeepEqual(e.rec.enqueued, want) {
		t.Fatalf("enqueued = %v, want %v", e.rec.enqueued, want)
	}

	setStatus(t, e, build, "in_review") // the coder agent finishes, approval required
	tick(t, e)
	build = stepByNode(t, e, def, "build")
	if m, _ := readStepMeta(build); m.Phase != PhaseBlocked || build.Status != "blocked" {
		t.Fatalf("build phase=%s status=%s, want blocked/blocked", m.Phase, build.Status)
	}
	assertWorkspaceMetadataContract(t, e)
	return e, def
}

func assertDefinitionDone(t *testing.T, e *env, def db.Issue) {
	t.Helper()
	assertWorkspaceMetadataContract(t, e)
	got, err := e.q.GetIssue(context.Background(), def.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dm, _ := readDefMeta(got); dm.State != RunDone || got.Status != "done" {
		t.Fatalf("definition state=%s status=%s, want done/done", dm.State, got.Status)
	}
	for _, node := range []string{"plan", "build"} {
		s := stepByNode(t, e, def, node)
		if m, _ := readStepMeta(s); m.Phase != PhaseDone || s.Status != "done" {
			t.Fatalf("%s phase=%s status=%s, want done/done", node, m.Phase, s.Status)
		}
	}
}

// The whole story in one test: author moves a labelled issue to todo, steps
// run in order, the approval step waits, /accept finishes it, and the
// definition issue completes.
func TestWorkflowEndToEnd(t *testing.T) {
	ctx := context.Background()
	e, def := runToReview(t)

	build := stepByNode(t, e, def, "build")
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	assertDefinitionDone(t, e, def)
	if len(e.rec.enqueued) != 2 {
		t.Fatalf("enqueued = %v, want exactly the two initial dispatches", e.rec.enqueued)
	}
}

// A rejected review redoes the step with the reject comment as trigger; the
// second review is accepted and the workflow completes.
func TestWorkflowEndToEndRejectThenAccept(t *testing.T) {
	ctx := context.Background()
	e, def := runToReview(t)
	build := stepByNode(t, e, def, "build")

	cid := realComment(t, e, build, "/reject add tests")
	ev := cevent(e, uuidStr(build), "member", e.user, "/reject add tests")
	ev.ID = cid
	if err := e.engine.HandleComment(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if m := stepMetaOf(t, e, def, "build"); m.Phase != PhaseRunning || m.Attempts != 1 {
		t.Fatalf("after reject phase=%s attempts=%d, want running/1", m.Phase, m.Attempts)
	}
	if len(e.rec.enqueued) != 3 {
		t.Fatalf("enqueued = %v, want 3 entries", e.rec.enqueued)
	}
	if want, got := "mention:"+uuidStr(build)+":"+cid, e.rec.enqueued[2]; got != want {
		t.Fatalf("redo enqueue = %q, want %q", got, want)
	}

	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review") // the agent redoes the work
	tick(t, e)
	build = stepByNode(t, e, def, "build")
	if m, _ := readStepMeta(build); m.Phase != PhaseBlocked || build.Status != "blocked" {
		t.Fatalf("build phase=%s status=%s, want blocked/blocked again", m.Phase, build.Status)
	}

	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	assertDefinitionDone(t, e, def)
	if len(e.rec.enqueued) != 3 {
		t.Fatalf("enqueued = %v, want no dispatch after accept", e.rec.enqueued)
	}
}
