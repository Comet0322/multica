// server/internal/workflow/store_test.go
package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCandidatesAndAtomicClaim(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	def := e.flowIssue(t, "Ship", "```yaml\nnodes: []\n```")
	plain := e.fx.Issue(t, "no label", dbfxCols("todo"))
	_ = plain

	cands, err := e.q.ListWorkflowDefinitionCandidates(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cands {
		if c.ID == def.ID {
			found = true
		}
		if c.Title == "no label" {
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
