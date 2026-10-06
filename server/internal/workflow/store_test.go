// server/internal/workflow/store_test.go
package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCandidatesAndAtomicClaim(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	def := e.flowIssue(t, "Ship", "```yaml\nnodes: []\n```")
	plain := e.fx.Issue(t, "no label", dbfxCols("todo"))

	cands, err := e.q.ListWorkflowDefinitionCandidates(ctx, 100000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cands {
		if c.ID == def.ID {
			found = true
		}
		if util.UUIDToString(c.ID) == plain {
			t.Fatal("unlabelled issue must not be a candidate")
		}
	}
	if !found {
		t.Fatal("labelled todo issue should be a candidate")
	}

	value, _ := json.Marshal(DefMeta{State: RunExpanding, ClaimedAt: time.Now().UTC().Format(time.RFC3339)})
	params := db.ClaimWorkflowDefinitionParams{
		Value: value, ID: def.ID, WorkspaceID: def.WorkspaceID,
		StaleBefore: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	}
	if _, err := e.q.ClaimWorkflowDefinition(ctx, params); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := e.q.ClaimWorkflowDefinition(ctx, params); err != pgx.ErrNoRows {
		t.Fatalf("second claim = %v, want ErrNoRows", err)
	}
	// A claim older than stale_before is reclaimable.
	params.StaleBefore = pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	if _, err := e.q.ClaimWorkflowDefinition(ctx, params); err != nil {
		t.Fatalf("stale reclaim: %v", err)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	issue := e.flowIssue(t, "Meta", "x")
	want := StepMeta{Run: "r1", Node: "build", Deps: []string{"plan"}, Agent: "Coder", AgentID: "a1", MaxRetries: 2, Phase: PhasePending}
	if err := e.engine.writeMeta(ctx, issue, want); err != nil {
		t.Fatal(err)
	}
	if err := e.engine.writeMeta(ctx, issue, want); err != nil {
		t.Fatalf("rewriting the same value must be a no-op, got %v", err)
	}
	got, err := e.q.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := readStepMeta(got)
	if !ok || m.Node != "build" || m.Deps[0] != "plan" || m.Phase != PhasePending {
		t.Fatalf("readStepMeta = %+v, %v", m, ok)
	}
	if _, ok := readDefMeta(got); ok {
		t.Fatal("step metadata must not parse as definition metadata")
	}
}

func claimParams(issue db.Issue, state string, stale time.Time) db.ClaimWorkflowDefinitionParams {
	value, _ := json.Marshal(DefMeta{State: RunExpanding, ClaimedAt: time.Now().UTC().Format(time.RFC3339)})
	return db.ClaimWorkflowDefinitionParams{
		Value: value, ID: issue.ID, WorkspaceID: issue.WorkspaceID,
		StaleBefore: pgtype.Timestamptz{Time: stale, Valid: true},
	}
}

func setWorkflowMeta(t *testing.T, e *env, issue db.Issue, raw string) {
	t.Helper()
	e.fx.Exec(t, `UPDATE issue SET metadata = jsonb_build_object('workflow', $2::jsonb) WHERE id = $1`, util.UUIDToString(issue.ID), raw)
}

func TestClaimEligibility(t *testing.T) {
	ctx := context.Background()
	hourAgo := time.Now().Add(-time.Hour)

	t.Run("invalid row is claimable", func(t *testing.T) {
		e := newEnv(t)
		i := e.flowIssue(t, "inv", "x")
		setWorkflowMeta(t, e, i, `{"state":"invalid"}`)
		if _, err := e.q.ClaimWorkflowDefinition(ctx, claimParams(i, "", hourAgo)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("expanding without claimed_at is reclaimable", func(t *testing.T) {
		e := newEnv(t)
		i := e.flowIssue(t, "noclaim", "x")
		setWorkflowMeta(t, e, i, `{"state":"expanding"}`)
		if _, err := e.q.ClaimWorkflowDefinition(ctx, claimParams(i, "", hourAgo)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("expanding with malformed claimed_at is reclaimable", func(t *testing.T) {
		e := newEnv(t)
		i := e.flowIssue(t, "bad", "x")
		setWorkflowMeta(t, e, i, `{"state":"expanding","claimed_at":"not-a-date"}`)
		if _, err := e.q.ClaimWorkflowDefinition(ctx, claimParams(i, "", hourAgo)); err != nil {
			t.Fatalf("malformed claimed_at must count as stale, got %v", err)
		}
	})
	t.Run("wrong workspace", func(t *testing.T) {
		e := newEnv(t)
		i := e.flowIssue(t, "ws", "x")
		p := claimParams(i, "", hourAgo)
		p.WorkspaceID = e.flowIssue(t, "other", "y").ID
		if _, err := e.q.ClaimWorkflowDefinition(ctx, p); err != pgx.ErrNoRows {
			t.Fatalf("got %v, want ErrNoRows", err)
		}
	})
	t.Run("non-todo is not claimable", func(t *testing.T) {
		e := newEnv(t)
		i := e.flowIssue(t, "done", "x")
		e.fx.Exec(t, `UPDATE issue SET status = 'done' WHERE id = $1`, util.UUIDToString(i.ID))
		if _, err := e.q.ClaimWorkflowDefinition(ctx, claimParams(i, "", hourAgo)); err != pgx.ErrNoRows {
			t.Fatalf("got %v, want ErrNoRows", err)
		}
	})
}

func TestListWorkflowStepsByRunIgnoresParent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	def := e.flowIssue(t, "def", "x")
	step := e.fx.Issue(t, "step", dbfx.Cols{"parent_issue_id": nil})
	e.fx.Exec(t, `UPDATE issue SET metadata = jsonb_build_object('workflow', jsonb_build_object('run','run-xyz','node','a')) WHERE id = $1`, step)
	steps, err := e.q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: e.wsUUID, Run: "run-xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || util.UUIDToString(steps[0].ID) != step {
		t.Fatalf("steps = %d, want exactly the orphaned step", len(steps))
	}
	if steps[0].ParentIssueID.Valid {
		t.Fatal("step must have no parent")
	}
	if steps[0].ID == def.ID {
		t.Fatal("definition issue must not be returned")
	}
}

func TestLatestWorkflowTaskStatus(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	issue := e.flowIssue(t, "tasks", "x")
	agent := e.agent(t, "A")
	agentUUID, _ := util.ParseUUID(agent)
	params := db.LatestWorkflowTaskStatusParams{IssueID: issue.ID, AgentID: agentUUID}
	if _, err := e.q.LatestWorkflowTaskStatus(ctx, params); err != pgx.ErrNoRows {
		t.Fatalf("got %v, want ErrNoRows", err)
	}
	iid := util.UUIDToString(issue.ID)
	e.fx.Task(t, agent, dbfx.Cols{"issue_id": iid, "status": "failed", "runtime_id": e.runtime, "created_at": dbfx.Raw("now() - interval '1 hour'")})
	e.fx.Task(t, agent, dbfx.Cols{"issue_id": iid, "status": "completed", "runtime_id": e.runtime, "created_at": dbfx.Raw("now()")})
	got, err := e.q.LatestWorkflowTaskStatus(ctx, params)
	if err != nil || got != "completed" {
		t.Fatalf("got %q, %v; want completed", got, err)
	}
}
