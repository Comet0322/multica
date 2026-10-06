// server/internal/workflow/commands_test.go
package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
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
