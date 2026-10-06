package extworkflow

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// memberSays posts a person's comment on the parent and delivers it to the
// conversation hook, as the comment handler's assignee fallback does.
func (e *env) memberSays(t *testing.T, parent, member pgtype.UUID, content string) pgtype.UUID {
	t.Helper()
	id := util.MustParseUUID(e.fx.Comment(t, util.UUIDToString(parent), content, testutil.Cols{"author_id": member}))
	if err := e.engine.OnMemberParentComment(context.Background(), parent, id, member); err != nil {
		t.Fatalf("OnMemberParentComment: %v", err)
	}
	return id
}

func (e *env) conversationTask(t *testing.T, comment pgtype.UUID) (db.AgentTaskQueue, bool) {
	t.Helper()
	var id pgtype.UUID
	if e.fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1 AND ext_workflow_kind = 'conversation'`, comment) == 0 {
		return db.AgentTaskQueue{}, false
	}
	e.fx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE trigger_comment_id = $1 AND ext_workflow_kind = 'conversation'`, comment).Scan(&id)
	task, err := e.q.GetAgentTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task, true
}

func TestMemberCommentWakesTheSupervisorOnce(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)
	comment := e.memberSays(t, run.IssueID, e.user, "Ship it, the review comments are addressed.")

	task, ok := e.conversationTask(t, comment)
	if !ok {
		t.Fatal("no conversation task")
	}
	if task.AgentID != supervisor || task.IssueID != run.IssueID || task.ExtWorkflowRole.String != RoleSupervisor ||
		task.ExtWorkflowRunID != run.ID || task.ExtWorkflowStepID.Valid || task.Status != "queued" ||
		task.TriggerCommentID != comment {
		t.Fatalf("conversation task = %+v", task)
	}
	if err := e.engine.OnMemberParentComment(context.Background(), run.IssueID, comment, e.user); err != nil {
		t.Fatal(err)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, comment); n != 1 {
		t.Fatalf("conversation tasks = %d, want 1", n)
	}
	// The review the step waits for is untouched.
	if got := e.latestTask(t, build, RoleSupervisor); got.ExtWorkflowKind.String != KindReview || got.Status != "queued" {
		t.Fatalf("review task = %s/%s", got.ExtWorkflowKind.String, got.Status)
	}
}

func TestMemberCommentNeedsAccessToTheSupervisor(t *testing.T) {
	e := newEnv(t)
	run, _, supervisor := e.reviewRun(t, e.user)
	e.access.deny(supervisor)
	comment := e.memberSays(t, run.IssueID, e.user, "Anyone there?")
	if _, ok := e.conversationTask(t, comment); ok {
		t.Fatal("a member without access to the supervisor woke it")
	}
}

func TestConversationActsOnBehalfOfAPermittedMember(t *testing.T) {
	e := newEnv(t)
	run, _, supervisor := e.reviewRun(t, e.user)
	comment := e.memberSays(t, run.IssueID, e.user, "Approve the build step please.")
	task, _ := e.conversationTask(t, comment)
	task = e.running(t, task)

	missing := e.agentSays(t, run.IssueID, supervisor, &task, block("action: approve"))
	if events, replies := e.protocolErrors(t, run, missing); events != 1 || replies != 1 {
		t.Fatalf("block without step: events=%d replies=%d, want 1/1", events, replies)
	}

	e.agentSays(t, run.IssueID, supervisor, &task, block("action: approve", "step: build"))
	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	var onBehalf, actor pgtype.UUID
	e.fx.QueryRow(t, `SELECT on_behalf_of, actor_id FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'decision'`, run.ID).Scan(&onBehalf, &actor)
	if onBehalf != e.user || actor != supervisor {
		t.Fatalf("decision by %v on behalf of %v", actor, onBehalf)
	}
}

func TestConversationRefusesAnUnpermittedMember(t *testing.T) {
	e := newEnv(t)
	plain := e.member(t, "member")
	run, _, supervisor := e.reviewRun(t, e.user)
	comment := e.memberSays(t, run.IssueID, plain, "Just approve it.")
	task, ok := e.conversationTask(t, comment)
	if !ok {
		t.Fatal("a member who may invoke the supervisor gets a conversation turn")
	}
	task = e.running(t, task)

	refused := e.agentSays(t, run.IssueID, supervisor, &task, block("action: approve", "step: build"))
	if events, replies := e.protocolErrors(t, run, refused); events != 1 || replies != 1 {
		t.Fatalf("refused decision: events=%d replies=%d, want 1/1", events, replies)
	}
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)
}

func TestConversationTaskIsStampedWithItsCommenter(t *testing.T) {
	e := newEnv(t)
	run, _, _ := e.reviewRun(t, e.user)
	comment := e.memberSays(t, run.IssueID, e.user, "Looking good.")
	var actor pgtype.UUID
	e.fx.QueryRow(t, `SELECT originator_user_id FROM agent_task_queue WHERE trigger_comment_id = $1`, comment).Scan(&actor)
	if actor != e.user {
		t.Fatalf("actor_user_id = %v, want %v", actor, e.user)
	}
}

func TestTwoMembersGetTheirOwnConversationTasks(t *testing.T) {
	e := newEnv(t)
	other := e.member(t, "member")
	run, _, _ := e.reviewRun(t, e.user)
	a := e.memberSays(t, run.IssueID, e.user, "First thought.")
	b := e.memberSays(t, run.IssueID, other, "Second thought.")
	for _, c := range []struct {
		comment, member pgtype.UUID
	}{{a, e.user}, {b, other}} {
		task, ok := e.conversationTask(t, c.comment)
		if !ok {
			t.Fatalf("comment %v has no conversation task", c.comment)
		}
		var actor pgtype.UUID
		e.fx.QueryRow(t, `SELECT originator_user_id FROM agent_task_queue WHERE id = $1`, task.ID).Scan(&actor)
		if task.TriggerCommentID != c.comment || actor != c.member {
			t.Fatalf("task for %v: trigger=%v actor=%v want member %v", c.comment, task.TriggerCommentID, actor, c.member)
		}
	}
}

func TestMemberCommentWithoutActiveRunWakesNothing(t *testing.T) {
	e := newEnv(t)
	run, _, _ := e.reviewRun(t, e.user)
	if err := e.engine.CancelRun(context.Background(), run.ID, "member", e.user); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	comment := e.memberSays(t, run.IssueID, e.user, "Anyone?")
	if _, ok := e.conversationTask(t, comment); ok {
		t.Fatal("a conversation task was created without an active run")
	}
}

func TestMemberCommentWithEngineDisabledWakesNothing(t *testing.T) {
	e := newEnv(t)
	run, _, _ := e.reviewRun(t, e.user)
	off := NewEngine(Deps{Pool: e.pool, Queries: e.q, Issues: e.issues, Tasks: e.tasks, Access: e.access, Publisher: e.pub, Enabled: false})
	id := util.MustParseUUID(e.fx.Comment(t, util.UUIDToString(run.IssueID), "Hello", testutil.Cols{"author_id": e.user}))
	if err := off.OnMemberParentComment(context.Background(), run.IssueID, id, e.user); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.conversationTask(t, id); ok {
		t.Fatal("a disabled engine woke the supervisor")
	}
}

func TestConcurrentDeliveriesOfOneCommentWakeOnce(t *testing.T) {
	e := newEnv(t)
	run, _, _ := e.reviewRun(t, e.user)
	id := util.MustParseUUID(e.fx.Comment(t, util.UUIDToString(run.IssueID), "Race me.", testutil.Cols{"author_id": e.user}))
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.engine.OnMemberParentComment(context.Background(), run.IssueID, id, e.user)
		}()
	}
	wg.Wait()
	if n := e.fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, id); n != 1 {
		t.Fatalf("conversation tasks = %d, want 1", n)
	}
}
