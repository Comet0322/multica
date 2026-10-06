// server/internal/workflow/expand_test.go
package workflow

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func flowDoc(agentA, agentB string) string {
	return "```yaml\nnodes:\n  - id: plan\n    agent: " + agentA + "\n    prompt: make a plan\n  - id: build\n    depends_on: [plan]\n    agent: " + agentB + "\n    prompt: build it\n    approval: true\n```"
}

func TestExpandCreatesBacklogStepsWithMetadata(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Ship it", flowDoc("Planner", "Coder"))

	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	kids, err := e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	if err != nil || len(kids) != 2 {
		t.Fatalf("children = %d, err %v", len(kids), err)
	}
	byNode := map[string]StepMeta{}
	for _, k := range kids {
		m, ok := readStepMeta(k)
		if !ok {
			t.Fatalf("child %q has no workflow metadata", k.Title)
		}
		if k.Status != "backlog" || m.Phase != PhasePending || !k.AssigneeID.Valid {
			t.Fatalf("child %q: status=%s phase=%s assignee valid=%v", k.Title, k.Status, m.Phase, k.AssigneeID.Valid)
		}
		if k.CreatorID != def.CreatorID || k.CreatorType != def.CreatorType {
			t.Fatalf("child creator must equal the definition creator")
		}
		// Every step, including the first one created, must hang off the definition.
		if k.ParentIssueID != def.ID {
			t.Fatalf("step %q: parent_issue_id = %v, want the definition issue", m.Node, k.ParentIssueID)
		}
		byNode[m.Node] = m
	}
	if byNode["build"].Deps[0] != "plan" || !byNode["build"].Approval || byNode["plan"].MaxRetries != 1 {
		t.Fatalf("metadata = %+v", byNode)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	dm, ok := readDefMeta(got)
	if !ok || dm.State != RunRunning || dm.Total != 2 || got.Status != "in_progress" {
		t.Fatalf("definition meta = %+v status=%s", dm, got.Status)
	}
	if len(e.rec.enqueued) != 0 {
		t.Fatal("expansion must not dispatch anything")
	}
	for _, k := range kids {
		assertMetadataContract(t, k.Metadata)
	}
	assertMetadataContract(t, got.Metadata)
	assertWorkspaceMetadataContract(t, e)
}

func TestExpandInvalidYAMLCommentsOnceAndRecoversOnEdit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Bad", flowDoc("Ghost", "Coder")) // Ghost does not exist

	for i := 0; i < 3; i++ {
		cur, _ := e.q.GetIssue(ctx, def.ID)
		if err := e.engine.Expand(ctx, cur); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.rec.comments) != 1 || !strings.Contains(e.rec.comments[0].Content, `unknown agent "Ghost"`) {
		t.Fatalf("want exactly one error comment naming the agent, got %d: %+v", len(e.rec.comments), e.rec.comments)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunInvalid {
		t.Fatalf("state = %s, want invalid", dm.State)
	}
	assertWorkspaceMetadataContract(t, e)

	e.agent(t, "Ghost")
	e.fx.Exec(t, `UPDATE issue SET description = $2 WHERE id = $1`, uuidStr(def), flowDoc("Ghost", "Coder")+" ")
	cur, _ := e.q.GetIssue(ctx, def.ID)
	if err := e.engine.Expand(ctx, cur); err != nil {
		t.Fatal(err)
	}
	got, _ = e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunRunning {
		t.Fatalf("after the edit state = %s, want running", dm.State)
	}
	assertWorkspaceMetadataContract(t, e)
}

func TestExpandConcurrentClaimsCreateOneSet(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Race", flowDoc("Planner", "Coder"))

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = e.engine.Expand(ctx, def)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("expander %d returned %v", i, err)
		}
	}
	steps, _ := e.q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidStr(def)})
	assertOneStepPerNode(t, steps, "plan", "build")
}

func assertOneStepPerNode(t *testing.T, steps []db.Issue, nodes ...string) {
	t.Helper()
	if len(steps) != len(nodes) {
		t.Fatalf("got %d step issues, want %d", len(steps), len(nodes))
	}
	count := map[string]int{}
	for _, s := range steps {
		m, ok := readStepMeta(s)
		if !ok {
			t.Fatalf("step %q is not stamped", s.Title)
		}
		count[m.Node]++
	}
	for _, n := range nodes {
		if count[n] != 1 {
			t.Fatalf("node %q appears %d times, want 1 (%v)", n, count[n], count)
		}
	}
}

func TestExpandCandidatesResumesACrashedExpansion(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Crashed", flowDoc("Planner", "Coder"))
	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	kids, _ := e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	// Crash after the first step: second step missing, claim old, status todo.
	e.fx.Exec(t, `DELETE FROM issue WHERE id = $1`, uuidStr(kids[1]))
	e.fx.Exec(t, `UPDATE issue SET status = 'todo', metadata = '{"wf_state":"expanding","wf_error_hash":"","wf_claimed_at":"2000-01-01T00:00:00Z","wf_total":0}'::jsonb WHERE id = $1`, uuidStr(def))

	if err := e.engine.ExpandCandidates(ctx); err != nil {
		t.Fatal(err)
	}
	steps, _ := e.q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidStr(def)})
	assertOneStepPerNode(t, steps, "plan", "build")
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunRunning || got.Status != "in_progress" {
		t.Fatalf("definition state=%s status=%s, want running/in_progress", dm.State, got.Status)
	}
}

func TestExpandStalledExpanderLosesItsClaim(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Slow", flowDoc("Planner", "Coder"))

	later := time.Now().Add(3 * time.Minute)
	b := &Engine{Q: e.engine.Q, Issues: e.engine.Issues, Tasks: e.rec, Events: e.rec, Invoke: e.rec, Now: func() time.Time { return later }}
	var bErr error
	a := &Engine{Q: e.engine.Q, Issues: e.engine.Issues, Tasks: e.rec, Events: e.rec, Invoke: e.rec}
	a.beforeStep = func(node string) {
		// A has created "plan" and stalls before "build"; B reclaims the stale
		// claim and runs the whole expansion.
		if node == "build" {
			a.beforeStep = nil
			bErr = b.Expand(ctx, def)
		}
	}
	if err := a.Expand(ctx, def); err != nil {
		t.Fatalf("stalled expander must stop quietly, got %v", err)
	}
	if bErr != nil {
		t.Fatal(bErr)
	}
	steps, _ := e.q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidStr(def)})
	assertOneStepPerNode(t, steps, "plan", "build")
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunRunning || got.Status != "in_progress" {
		t.Fatalf("definition state=%s status=%s, want running/in_progress", dm.State, got.Status)
	}
}

func TestExpandDoesNotDuplicateAStepThatLostItsParent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Parentless", flowDoc("Planner", "Coder"))
	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	kids, _ := e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	// The first step loses its parent link, then the claim goes stale so the
	// definition is expanded again.
	e.fx.Exec(t, `UPDATE issue SET parent_issue_id = NULL WHERE id = $1`, uuidStr(kids[0]))
	e.fx.Exec(t, `UPDATE issue SET status = 'todo', metadata = '{"wf_state":"expanding","wf_error_hash":"","wf_claimed_at":"2000-01-01T00:00:00Z","wf_total":0}'::jsonb WHERE id = $1`, uuidStr(def))
	before := len(e.rec.comments)

	cur, _ := e.q.GetIssue(ctx, def.ID)
	if err := e.engine.Expand(ctx, cur); err != nil {
		t.Fatal(err)
	}
	steps, _ := e.q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidStr(def)})
	if len(steps) != 2 {
		t.Fatalf("a parentless step must be recognised, not recreated: %d steps", len(steps))
	}
	if len(e.rec.comments) != before+1 || !strings.Contains(e.rec.comments[before].Content, "not linked") {
		t.Fatal("the lost parent link must be surfaced in a comment")
	}
}

func TestExpandResumesAfterCrashAndAdoptsOrphans(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Resume", flowDoc("Planner", "Coder"))
	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	kids, _ := e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	// Simulate a crash: first step lost its metadata, second step never created,
	// and the claim is stale.
	e.fx.Exec(t, `UPDATE issue SET metadata = '{}'::jsonb WHERE id = $1`, uuidStr(kids[0]))
	e.fx.Exec(t, `DELETE FROM issue WHERE id = $1`, uuidStr(kids[1]))
	e.fx.Exec(t, `UPDATE issue SET status = 'todo', metadata = '{"wf_state":"expanding","wf_error_hash":"","wf_claimed_at":"2000-01-01T00:00:00Z","wf_total":0}'::jsonb WHERE id = $1`, uuidStr(def))

	cur, _ := e.q.GetIssue(ctx, def.ID)
	if err := e.engine.Expand(ctx, cur); err != nil {
		t.Fatal(err)
	}
	kids, _ = e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	if len(kids) != 2 {
		t.Fatalf("after resume %d step issues, want 2 (orphan adopted, missing created)", len(kids))
	}
	for _, k := range kids {
		if _, ok := readStepMeta(k); !ok {
			t.Fatalf("step %q still lacks metadata", k.Title)
		}
	}
}

func TestExpandBlocksADefinitionWithDuplicateSteps(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Dupes", flowDoc("Planner", "Coder"))
	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	kids, _ := e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	// Two stamped steps for the same node, claim stale, status todo.
	e.fx.Exec(t, `UPDATE issue SET metadata = (SELECT metadata FROM issue WHERE id = $1) WHERE id = $2`, uuidStr(kids[0]), uuidStr(kids[1]))
	e.fx.Exec(t, `UPDATE issue SET status = 'todo', metadata = '{"wf_state":"expanding","wf_error_hash":"","wf_claimed_at":"2000-01-01T00:00:00Z","wf_total":0}'::jsonb WHERE id = $1`, uuidStr(def))
	before := len(e.rec.comments)

	for i := 0; i < 2; i++ {
		if err := e.engine.ExpandCandidates(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunBlocked || got.Status != "blocked" {
		t.Fatalf("state=%s status=%s, want blocked/blocked", dm.State, got.Status)
	}
	if n := len(e.rec.comments) - before; n != 1 {
		t.Fatalf("want exactly one explanatory comment, got %d", n)
	}
}

func TestExpandLosingTheClaimAtFinishWritesNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "LateLoss", flowDoc("Planner", "Coder"))
	a := &Engine{Q: e.q, Issues: e.engine.Issues, Tasks: e.rec, Events: e.rec, Invoke: e.rec}
	a.beforeStep = func(node string) {
		if node == "finish" {
			// Another expander takes over before A's final renewal.
			e.fx.Exec(t, `UPDATE issue SET metadata = metadata || '{"wf_claimed_at":"2001-01-01T00:00:00Z"}'::jsonb WHERE id = $1`, uuidStr(def))
		}
	}
	if err := a.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunExpanding || got.Status != "todo" {
		t.Fatalf("state=%s status=%s, want untouched expanding/todo", dm.State, got.Status)
	}
}
