// server/internal/workflow/contract_test.go
package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Platform contract for issue.metadata, mirrored from
// handler.validateIssueMetadataValue / maxIssueMetadataKeys, the
// issue_metadata_size_limit CHECK and the frontend IssueMetadataSchema
// (z.record(z.string(), z.union([z.string(), z.number(), z.boolean()]))).
// The frontend drops the WHOLE issue list when one issue violates it, so the
// engine must never write anything else.
const (
	contractMaxKeys  = 50
	contractMaxBytes = 8192
)

var (
	stepKeys = []string{KeyRun, KeyNode, KeyDeps, KeyAgent, KeyAgentID, KeyApproval, KeyMaxRetries, KeyAttempts, KeyPhase, KeyDispatchedAt}
	defKeys  = []string{KeyState, KeyErrorHash, KeyClaimedAt, KeyTotal}
)

// metadataContractError reports why raw is not a flat JSON object whose
// values are all strings, numbers or booleans (no null, array or object), with
// at most 50 keys and at most 8192 bytes; nil when it complies.
func metadataContractError(raw []byte) error {
	if len(raw) > contractMaxBytes {
		return fmt.Errorf("metadata is %d bytes, contract max is %d", len(raw), contractMaxBytes)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("metadata must be a JSON object")
	}
	// Same rule as handler.validateIssueMetadataValue, applied per value.
	var bag map[string]json.RawMessage
	if err := json.Unmarshal(raw, &bag); err != nil {
		return fmt.Errorf("metadata must be a JSON object: %w", err)
	}
	if len(bag) > contractMaxKeys {
		return fmt.Errorf("metadata has %d keys, contract max is %d", len(bag), contractMaxKeys)
	}
	for k, v := range bag {
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			return fmt.Errorf("metadata[%q] is not valid JSON: %w", k, err)
		}
		switch val.(type) {
		case string, bool, float64:
		case nil:
			return fmt.Errorf("metadata[%q] is null; only string, number or bool are allowed", k)
		default:
			return fmt.Errorf("metadata[%q] = %s is not a primitive; only string, number or bool are allowed", k, v)
		}
	}
	// And the decode the API and the UI see: map[string]any, every value a
	// string, float64 or bool.
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("metadata decode: %w", err)
	}
	for k, v := range decoded {
		switch v.(type) {
		case string, float64, bool:
		default:
			return fmt.Errorf("metadata[%q] decodes to %T, want string, float64 or bool", k, v)
		}
	}
	// Workflow issues carry their full flat key set with the right types.
	if _, ok := decoded[KeyRun]; ok {
		if err := keyTypesError(decoded, stepKeys); err != nil {
			return err
		}
	}
	if _, ok := decoded[KeyState]; ok {
		if err := keyTypesError(decoded, defKeys); err != nil {
			return err
		}
	}
	return nil
}

func keyTypesError(bag map[string]any, keys []string) error {
	for _, k := range keys {
		v, ok := bag[k]
		if !ok {
			return fmt.Errorf("workflow metadata lacks %q", k)
		}
		var good bool
		switch k {
		case KeyApproval:
			_, good = v.(bool)
		case KeyMaxRetries, KeyAttempts, KeyTotal:
			_, good = v.(float64)
		default:
			_, good = v.(string)
		}
		if !good {
			return fmt.Errorf("workflow metadata %q has type %T", k, v)
		}
	}
	return nil
}

// assertMetadataContract fails the test unless raw meets the platform's
// issue.metadata contract.
func assertMetadataContract(t *testing.T, raw []byte) {
	t.Helper()
	if err := metadataContractError(raw); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
}

func issueWithMeta(raw []byte) db.Issue { return db.Issue{Metadata: raw} }

// assertWorkspaceMetadataContract loads every issue of the env's workspace,
// as the issue list endpoint would, and checks each one's metadata.
func assertWorkspaceMetadataContract(t *testing.T, e *env) {
	t.Helper()
	checkWorkspaceMetadata(t, e, true)
}

// checkWorkspaceMetadata is assertWorkspaceMetadataContract for shared
// helpers that may run before any issue exists.
func checkWorkspaceMetadata(t *testing.T, e *env, requireIssues bool) {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT title, metadata FROM issue WHERE workspace_id = $1`, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var title string
		var raw []byte
		if err := rows.Scan(&title, &raw); err != nil {
			t.Fatal(err)
		}
		n++
		if err := metadataContractError(raw); err != nil {
			t.Fatalf("issue %q breaks the metadata contract: %v: %s", title, err, raw)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 && requireIssues {
		t.Fatal("workspace has no issues; the contract check would be vacuous")
	}
}

func TestMetaKeysAreFlatPrimitives(t *testing.T) {
	for _, m := range []StepMeta{
		{},
		{Run: "r", Node: "build", Deps: []string{"plan", "lint"}, Agent: "Coder", AgentID: "a", Approval: true, MaxRetries: 2, Attempts: 1, Phase: PhaseRunning, DispatchedAt: "2026-01-01T00:00:00.123Z"},
	} {
		raw, err := stepMetaJSON(m)
		if err != nil {
			t.Fatal(err)
		}
		assertMetadataContract(t, raw)
		if m.Run == "" {
			continue
		}
		got, ok := readStepMeta(issueWithMeta(raw))
		if !ok || got.Node != m.Node || len(got.Deps) != 2 || got.Deps[1] != "lint" || !got.Approval ||
			got.MaxRetries != 2 || got.Attempts != 1 || got.Phase != PhaseRunning || got.DispatchedAt != m.DispatchedAt {
			t.Fatalf("round trip = %+v, %v", got, ok)
		}
	}
	raw, err := defMetaJSON(DefMeta{State: RunRunning, Total: 3})
	if err != nil {
		t.Fatal(err)
	}
	assertMetadataContract(t, raw)
	for _, bad := range []string{`{"wf_state":null}`, `{"workflow":{"state":"running"}}`, `{"wf_deps":["a"]}`, `[]`} {
		if metadataContractError([]byte(bad)) == nil {
			t.Fatalf("contract check accepted %s", bad)
		}
	}
	if dm, ok := readDefMeta(issueWithMeta(raw)); !ok || dm.State != RunRunning || dm.Total != 3 {
		t.Fatalf("def round trip = %+v, %v", dm, ok)
	}
	// A step with no dependencies stores an empty string and reads back as none.
	raw, _ = stepMetaJSON(StepMeta{Run: "r", Node: "a"})
	if got, _ := readStepMeta(issueWithMeta(raw)); len(got.Deps) != 0 {
		t.Fatalf("deps = %v, want none", got.Deps)
	}
}

// The guard against the bug that emptied the web UI's issue list: after every
// major step of every path (happy path, reject/retry, invalid definition,
// stopped run) every issue in the workspace still meets the metadata contract.
func TestWorkflowMetadataContractThroughFullRun(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	check := func() { t.Helper(); assertWorkspaceMetadataContract(t, e) }

	// An invalid definition in the same workspace.
	bad := e.flowIssue(t, "Bad", flowDoc("Ghost", "Coder"))
	tick(t, e)
	check()
	if got, _ := e.q.GetIssue(ctx, bad.ID); func() bool { dm, _ := readDefMeta(got); return dm.State != RunInvalid }() {
		t.Fatal("bad definition should be invalid")
	}

	def := e.flowIssue(t, "Ship", flowDoc("Planner", "Coder"))
	check()
	tick(t, e) // expand + dispatch plan
	check()
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	tick(t, e)
	tick(t, e)
	check()
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	tick(t, e)
	check()
	if phaseOf(t, e, def, "build") != PhaseBlocked {
		t.Fatal("build should wait for review")
	}

	// Reject until retries are exhausted, then /retry.
	build := stepByNode(t, e, def, "build")
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/reject once")); err != nil {
		t.Fatal(err)
	}
	check()
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	tick(t, e)
	check()
	build = stepByNode(t, e, def, "build")
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/reject twice")); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	check()
	if got, _ := e.q.GetIssue(ctx, def.ID); func() bool { dm, _ := readDefMeta(got); return dm.State != RunBlocked }() {
		t.Fatal("definition should be blocked after the step failed")
	}
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(stepByNode(t, e, def, "build")), "member", e.user, "/retry")); err != nil {
		t.Fatal(err)
	}
	check()
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	tick(t, e)
	check()

	// Accept and complete.
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(stepByNode(t, e, def, "build")), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	check()
	assertDefinitionDone(t, e, def)

	// A run stopped by closing its definition.
	stop := e.flowIssue(t, "Stop", flowDoc("Planner", "Coder"))
	tick(t, e)
	setStatus(t, e, stop, "cancelled")
	tick(t, e)
	check()
	if got, _ := e.q.GetIssue(ctx, stop.ID); func() bool { dm, _ := readDefMeta(got); return dm.State != RunStopped }() {
		t.Fatal("closed definition should be stopped")
	}
}
