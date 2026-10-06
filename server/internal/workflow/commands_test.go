// server/internal/workflow/commands_test.go
package workflow

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in   string
		want Command
		ok   bool
	}{
		{"/accept", CommandAccept, true},
		{"  /ACCEPT looks good", CommandAccept, true},
		{"/reject missing tests\nmore", CommandReject, true},
		{"/retry", CommandRetry, true},
		{"/accepted", "", false},
		{"see /accept", "", false},
		{"/ accept", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := ParseCommand(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseCommand(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if !IsCommandToken("/Reject") || IsCommandToken("/note") {
		t.Error("IsCommandToken mismatch")
	}
}

func cevent(e *env, issue string, authorType, authorID, content string) CommentEvent {
	return CommentEvent{ID: "00000000-0000-0000-0000-000000000001", IssueID: issue, AuthorType: authorType, AuthorID: authorID, Content: content}
}

// blockedBuild drives the flow to build waiting for review.
func blockedBuild(t *testing.T) (*env, db.Issue, db.Issue) {
	e, def := expanded(t)
	tick(t, e)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	tick(t, e)
	tick(t, e)
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	tick(t, e)
	return e, def, stepByNode(t, e, def, "build")
}

func TestAcceptByCreatorCompletesStep(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "build") != PhaseDone {
		t.Fatalf("build phase = %s, want done", phaseOf(t, e, def, "build"))
	}
}

func TestCommandFromNonCreatorIsRefused(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	before := len(e.rec.comments)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", "00000000-0000-0000-0000-0000000000aa", "/accept")); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "build") != PhaseBlocked {
		t.Fatal("a stranger's /accept must not apply")
	}
	if len(e.rec.comments) != before+1 || !strings.Contains(e.rec.comments[before].Content, "creator") {
		t.Fatal("expected a refusal comment mentioning the creator")
	}
}

func TestCommandsOnAgentCreatedDefinitionAreRefusedVisibly(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	agent := e.agent(t, "Creator Bot")
	e.fx.Exec(t, `UPDATE issue SET creator_type = 'agent', creator_id = $2 WHERE id = $1`, uuidStr(def), agent)
	before := len(e.rec.comments)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "build") != PhaseBlocked || len(e.rec.comments) != before+1 {
		t.Fatal("agent-created definitions must refuse commands with a visible comment")
	}
	if got := e.rec.comments[before].Content; !strings.Contains(got, "unavailable for agent-created workflows") {
		t.Fatalf("refusal must say commands are unavailable for agent-created workflows, got %q", got)
	}
}

func TestRetryOnDefinitionRestartsAllFailedSteps(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	e.rec.failNext = errString("runtime offline")
	_ = e.engine.Tick(ctx) // plan fails at dispatch
	_ = e.engine.Tick(ctx) // workflow blocks
	cur, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(cur); dm.State != RunBlocked {
		t.Fatalf("precondition: state = %s", dm.State)
	}
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(def), "member", e.user, "/retry")); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "plan") != PhaseRunning {
		t.Fatalf("plan phase = %s, want running after /retry", phaseOf(t, e, def, "plan"))
	}
	cur, _ = e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(cur); dm.State != RunRunning || cur.Status != "in_progress" {
		t.Fatalf("definition state=%s status=%s, want running/in_progress", dm.State, cur.Status)
	}
}

func TestCommandOnUnrelatedIssueIsSilent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	plain := e.flowIssue(t, "Just an issue", "no yaml")
	before := len(e.rec.comments)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(plain), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	if len(e.rec.comments) != before {
		t.Fatal("a command on a non-workflow issue must be ignored silently")
	}
}

func TestAcceptOnRunningStepExplainsWhy(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	_ = e.engine.Tick(ctx)
	plan := stepByNode(t, e, def, "plan")
	before := len(e.rec.comments)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(plan), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	if len(e.rec.comments) != before+1 || !strings.Contains(e.rec.comments[before].Content, "not waiting") {
		t.Fatal("expected a 'not waiting for review' comment")
	}
}

func TestDecodeCommentEvent(t *testing.T) {
	type wire struct {
		ID         string `json:"id"`
		IssueID    string `json:"issue_id"`
		AuthorType string `json:"author_type"`
		AuthorID   string `json:"author_id"`
		Content    string `json:"content"`
		Extra      int    `json:"reactions_count"`
	}
	ev := events.Event{Type: protocol.EventCommentCreated, Payload: map[string]any{
		"comment":     wire{ID: "c1", IssueID: "i1", AuthorType: "member", AuthorID: "m1", Content: "/accept", Extra: 3},
		"issue_title": "x",
	}}
	got, ok := decodeCommentEvent(ev)
	want := CommentEvent{ID: "c1", IssueID: "i1", AuthorType: "member", AuthorID: "m1", Content: "/accept"}
	if !ok || got != want {
		t.Fatalf("decodeCommentEvent = %+v, %v; want %+v", got, ok, want)
	}
	for name, p := range map[string]any{
		"not a map":      "text",
		"no comment":     map[string]any{"issue": 1},
		"no issue id":    map[string]any{"comment": map[string]any{"id": "c"}},
		"comment string": map[string]any{"comment": "oops"},
	} {
		if _, ok := decodeCommentEvent(events.Event{Payload: p}); ok {
			t.Errorf("%s: want ok=false", name)
		}
	}
}

func TestListenerAppliesAcceptCommand(t *testing.T) {
	e, def, build := blockedBuild(t)
	bus := events.New()
	RegisterListeners(bus, e.engine)
	// A non-command comment must not do anything.
	bus.Publish(events.Event{Type: protocol.EventCommentCreated, Payload: map[string]any{"comment": map[string]any{
		"id": "00000000-0000-0000-0000-000000000001", "issue_id": uuidStr(build), "author_type": "member", "author_id": e.user, "content": "looks fine"}}})
	time.Sleep(150 * time.Millisecond)
	if phaseOf(t, e, def, "build") != PhaseBlocked {
		t.Fatal("a non-command comment must not change the step")
	}
	bus.Publish(events.Event{Type: protocol.EventCommentCreated, Payload: map[string]any{"comment": map[string]any{
		"id": "00000000-0000-0000-0000-000000000001", "issue_id": uuidStr(build), "author_type": "member", "author_id": e.user, "content": "/accept"}}})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if phaseOf(t, e, def, "build") == PhaseDone {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("build phase = %s, want done after /accept via listener", phaseOf(t, e, def, "build"))
}

func realComment(t *testing.T, e *env, issue db.Issue, content string) string {
	t.Helper()
	return e.fx.Comment(t, uuidStr(issue), content)
}

func stepMetaOf(t *testing.T, e *env, def db.Issue, node string) StepMeta {
	t.Helper()
	m, _ := readStepMeta(stepByNode(t, e, def, node))
	return m
}

func TestRejectByCreatorRedispatchesWithRealCommentOnce(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	cid := realComment(t, e, build, "/reject add tests")
	ev := cevent(e, uuidStr(build), "member", e.user, "/reject add tests")
	ev.ID = cid
	if err := e.engine.HandleComment(ctx, ev); err != nil {
		t.Fatal(err)
	}
	m := stepMetaOf(t, e, def, "build")
	if m.Phase != PhaseRunning || m.Attempts != 1 {
		t.Fatalf("after reject phase=%s attempts=%d, want running/1", m.Phase, m.Attempts)
	}
	want := "mention:" + uuidStr(build) + ":" + cid
	if last := e.rec.enqueued[len(e.rec.enqueued)-1]; last != want {
		t.Fatalf("enqueue = %q, want %q", last, want)
	}
	before := len(e.rec.comments)
	enq := len(e.rec.enqueued)
	if err := e.engine.HandleComment(ctx, ev); err != nil { // double delivery
		t.Fatal(err)
	}
	if m := stepMetaOf(t, e, def, "build"); m.Attempts != 1 || m.Phase != PhaseRunning {
		t.Fatalf("double delivery changed the step: phase=%s attempts=%d", m.Phase, m.Attempts)
	}
	if len(e.rec.enqueued) != enq {
		t.Fatal("double delivery must not enqueue again")
	}
	if len(e.rec.comments) != before+1 || !strings.Contains(e.rec.comments[before].Content, "not waiting") {
		t.Fatal("second delivery should be explained")
	}
}

func TestRejectAfterRetriesExhaustedFailsStepAndBlocksDefinition(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/reject once")); err != nil {
		t.Fatal(err)
	}
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	tick(t, e)
	if phaseOf(t, e, def, "build") != PhaseBlocked {
		t.Fatal("precondition: build blocked again")
	}
	build = stepByNode(t, e, def, "build")
	before := len(e.rec.comments)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/reject twice")); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "build") != PhaseFailed {
		t.Fatalf("phase = %s, want failed", phaseOf(t, e, def, "build"))
	}
	found := false
	for _, c := range e.rec.comments[before:] {
		if strings.Contains(c.Content, "/retry") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the failure comment")
	}
	tick(t, e)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunBlocked {
		t.Fatalf("definition state = %s, want blocked", dm.State)
	}
}

func TestStepRetryRestartsFailedStepAndReopensDefinition(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	setStep(t, e, build, PhaseFailed, 1, "blocked")
	tick(t, e)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunBlocked {
		t.Fatalf("precondition: state = %s", dm.State)
	}
	build = stepByNode(t, e, def, "build")
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/retry")); err != nil {
		t.Fatal(err)
	}
	if m := stepMetaOf(t, e, def, "build"); m.Phase != PhaseRunning || m.Attempts != 0 {
		t.Fatalf("phase=%s attempts=%d, want running/0", m.Phase, m.Attempts)
	}
	got, _ = e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunRunning || got.Status != "in_progress" {
		t.Fatalf("definition state=%s status=%s, want running/in_progress", dm.State, got.Status)
	}
}

func TestDoubleAcceptReportsNotWaiting(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	ev := cevent(e, uuidStr(build), "member", e.user, "/accept")
	if err := e.engine.HandleComment(ctx, ev); err != nil {
		t.Fatal(err)
	}
	updated := e.rec.updated
	before := len(e.rec.comments)
	if err := e.engine.HandleComment(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "build") != PhaseDone {
		t.Fatal("step must stay done")
	}
	if len(e.rec.comments) != before+1 || !strings.Contains(e.rec.comments[before].Content, "not waiting for review") {
		t.Fatal("second /accept should say the step is not waiting for review")
	}
	if e.rec.updated != updated {
		t.Fatal("second /accept must not transition anything")
	}
}

// alwaysFailTasks fails every enqueue.
type alwaysFailTasks struct{ *recorder }

func (alwaysFailTasks) EnqueueTaskForIssue(context.Context, db.Issue, ...pgtype.UUID) (db.AgentTaskQueue, error) {
	return db.AgentTaskQueue{}, errors.New("runtime offline")
}

// panicTasks panics on a mention enqueue.
type panicTasks struct{ *recorder }

func (panicTasks) EnqueueTaskForMention(context.Context, db.Issue, pgtype.UUID, pgtype.UUID, service.RunOrigin) (db.AgentTaskQueue, error) {
	panic("boom")
}

// commentFailDB fails comment inserts while on is set.
type commentFailDB struct {
	db.DBTX
	on atomic.Bool
}

func (c *commentFailDB) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	if c.on.Load() && strings.Contains(sql, "INSERT INTO comment") {
		return errRow{errors.New("comment insert failed")}
	}
	return c.DBTX.QueryRow(ctx, sql, args...)
}

func (c *commentFailDB) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	return c.DBTX.Exec(ctx, sql, args...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func TestDefinitionRetryReopensEvenWhenAStepWriteFails(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	setStep(t, e, build, PhaseFailed, 1, "blocked")
	setStep(t, e, stepByNode(t, e, def, "plan"), PhaseFailed, 1, "blocked")
	tick(t, e)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunBlocked {
		t.Fatalf("precondition: state = %s", dm.State)
	}
	wrapped := &commentFailDB{DBTX: e.pool}
	e.engine.Q = db.New(wrapped)
	e.engine.Tasks = alwaysFailTasks{e.rec}
	wrapped.on.Store(true)
	err := e.engine.HandleComment(ctx, cevent(e, uuidStr(def), "member", e.user, "/retry"))
	wrapped.on.Store(false)
	if err == nil {
		t.Fatal("expected the post-claim failure to be returned")
	}
	got, _ = e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunRunning {
		t.Fatalf("definition state = %s, want running despite the error", dm.State)
	}
}

func TestDefinitionRetryWithNoFailedStepsReevaluatesBlockedWorkflow(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	// Force a blocked definition whose steps are not failed.
	got, _ := e.q.GetIssue(ctx, def.ID)
	dm, _ := readDefMeta(got)
	dm.State = RunBlocked
	if err := e.engine.writeMeta(ctx, got, dm); err != nil {
		t.Fatal(err)
	}
	_ = build
	before := len(e.rec.comments)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(def), "member", e.user, "/retry")); err != nil {
		t.Fatal(err)
	}
	got, _ = e.q.GetIssue(ctx, def.ID)
	if d, _ := readDefMeta(got); d.State != RunRunning {
		t.Fatalf("state = %s, want running", d.State)
	}
	if len(e.rec.comments) != before+1 || !strings.Contains(e.rec.comments[before].Content, "re-evaluating") {
		t.Fatal("expected the re-evaluating message")
	}
	// Running definition: plain message, no reopen.
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(def), "member", e.user, "/retry")); err != nil {
		t.Fatal(err)
	}
	if last := e.rec.comments[len(e.rec.comments)-1].Content; last != "No failed steps to retry." {
		t.Fatalf("last comment = %q", last)
	}
}

func TestRunCommandRecoversFromPanic(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	e.engine.Tasks = panicTasks{e.rec}
	// Must return normally: a panic here would fail the whole test binary.
	runCommand(e.engine, cevent(e, uuidStr(build), "member", e.user, "/reject fix it"))
	e.engine.Tasks = e.rec
	if err := e.engine.Tick(ctx); err != nil {
		t.Fatalf("engine unusable after panic: %v", err)
	}
	_ = def
}

func TestListenerIgnoresNonMemberCommands(t *testing.T) {
	e, def, build := blockedBuild(t)
	bus := events.New()
	RegisterListeners(bus, e.engine)
	for _, at := range []string{"system", "agent"} {
		bus.Publish(events.Event{Type: protocol.EventCommentCreated, Payload: map[string]any{"comment": map[string]any{
			"id": "00000000-0000-0000-0000-000000000001", "issue_id": uuidStr(build), "author_type": at, "author_id": e.user, "content": "/accept"}}})
	}
	before := len(e.rec.comments)
	time.Sleep(300 * time.Millisecond)
	if phaseOf(t, e, def, "build") != PhaseBlocked || len(e.rec.comments) != before {
		t.Fatal("system/agent authored commands must be ignored entirely")
	}
}
