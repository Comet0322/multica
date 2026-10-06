package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// extWFReq builds a request as userID (empty = workspace owner) carrying the
// chi params the ext workflow handlers read (workspaceId plus optional id).
func extWFReq(userID, method, path string, body any, id string) *http.Request {
	var req *http.Request
	if userID == "" {
		req = newRequest(method, path, body)
	} else {
		req = newRequestAs(userID, method, path, body)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("workspaceId", testWorkspaceID)
	if id != "" {
		rctx.URLParams.Add("id", id)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

type extWFValidationBody struct {
	Error  string `json:"error"`
	Errors []struct {
		NodeKey string `json:"node_key"`
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"errors"`
}

func extWFNode(key, agentID string, deps ...string) map[string]any {
	if deps == nil {
		deps = []string{}
	}
	return map[string]any{
		"key": key, "title": "Title " + key, "agent_id": agentID, "prompt": "do " + key,
		"requires_review": false, "max_attempts": 3, "depends_on": deps,
	}
}

// createExtWorkflowAs creates a workflow through the handler and registers
// cleanup for it and its nodes (there are no cascading deletes).
func createExtWorkflowAs(t *testing.T, userID, name, supervisorID string) ExtWorkflowResponse {
	t.Helper()
	var resp ExtWorkflowResponse
	testutil.Call(t, testHandler.CreateExtWorkflow, extWFReq(userID, "POST", "/api/ext/workflows", map[string]any{
		"name": name, "description": "d", "supervisor_agent_id": supervisorID,
	}, "")).Want(http.StatusCreated).JSON(&resp)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM ext_workflow_node WHERE workflow_id = $1`, resp.ID)
		testPool.Exec(context.Background(), `DELETE FROM ext_workflow WHERE id = $1`, resp.ID)
	})
	return resp
}

func updateExtWorkflow(t *testing.T, userID, id string, body any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.UpdateExtWorkflow, extWFReq(userID, "PUT", "/api/ext/workflows/"+id, body, id))
}

var (
	extWFEventsOnce sync.Once
	extWFEventsMu   sync.Mutex
	extWFEvents     []events.Event
)

// extWorkflowEventTypes returns, in publish order, the ext_workflow:* event
// types recorded so far for one workflow id. subscribeExtWorkflowEvents must
// have been called first.
func extWorkflowEventTypes(t *testing.T, workflowID string) []string {
	t.Helper()
	extWFEventsMu.Lock()
	defer extWFEventsMu.Unlock()
	var out []string
	for _, e := range extWFEvents {
		if p, ok := e.Payload.(map[string]any); ok && p["workflow_id"] == workflowID {
			out = append(out, e.Type)
		}
	}
	return out
}

// subscribeExtWorkflowEvents registers the recorder once per process; the bus
// has no unsubscribe.
func subscribeExtWorkflowEvents() {
	extWFEventsOnce.Do(func() {
		for _, typ := range []string{protocol.EventExtWorkflowCreated, protocol.EventExtWorkflowUpdated, protocol.EventExtWorkflowDeleted} {
			testHandler.Bus.Subscribe(typ, func(e events.Event) {
				extWFEventsMu.Lock()
				extWFEvents = append(extWFEvents, e)
				extWFEventsMu.Unlock()
			})
		}
	})
}

func requireExtWorkflowDB(t *testing.T) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
}

func TestExtWorkflow_Lifecycle(t *testing.T) {
	requireExtWorkflowDB(t)
	subscribeExtWorkflowEvents()
	supervisor := createHandlerTestAgent(t, "ext-wf-supervisor", nil)
	worker := createHandlerTestAgent(t, "ext-wf-worker", nil)

	wf := createExtWorkflowAs(t, "", "Release train", supervisor)
	if wf.MaxRewinds != 3 || wf.NodeCount != 0 || wf.CreatorID != testUserID || wf.ArchivedAt != nil {
		t.Fatalf("unexpected created workflow: %+v", wf)
	}

	var upd ExtWorkflowResponse
	updateExtWorkflow(t, "", wf.ID, map[string]any{
		"name":        "Release train v2",
		"max_rewinds": 5,
		"nodes": []any{
			extWFNode("build", worker),
			extWFNode("test", worker, "build"),
		},
	}).Want(http.StatusOK).JSON(&upd)
	if upd.Name != "Release train v2" || upd.MaxRewinds != 5 || upd.NodeCount != 2 || len(upd.Nodes) != 2 {
		t.Fatalf("unexpected updated workflow: %+v", upd)
	}
	if upd.Nodes[0].Key != "build" || upd.Nodes[0].Position != 0 || upd.Nodes[1].Key != "test" || upd.Nodes[1].Position != 1 {
		t.Fatalf("nodes not stored in request order: %+v", upd.Nodes)
	}
	if got := upd.Nodes[1].DependsOn; len(got) != 1 || got[0] != "build" {
		t.Fatalf("depends_on not stored: %+v", upd.Nodes[1])
	}

	// A rename without nodes keeps the stored node set.
	updateExtWorkflow(t, "", wf.ID, map[string]any{"description": "only text"}).Want(http.StatusOK).JSON(&upd)
	if upd.Description != "only text" || len(upd.Nodes) != 2 {
		t.Fatalf("settings-only update changed nodes: %+v", upd)
	}

	// Replacing the node set replaces it as a whole.
	updateExtWorkflow(t, "", wf.ID, map[string]any{
		"nodes": []any{extWFNode("only", worker)},
	}).Want(http.StatusOK).JSON(&upd)
	if len(upd.Nodes) != 1 || upd.Nodes[0].Key != "only" {
		t.Fatalf("node set not replaced: %+v", upd.Nodes)
	}

	var got ExtWorkflowResponse
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq("", "GET", "/api/ext/workflows/"+wf.ID, nil, wf.ID)).
		Want(http.StatusOK).JSON(&got)
	if len(got.Nodes) != 1 || got.NodeCount != 1 || got.ActiveRunCount != 0 {
		t.Fatalf("unexpected detail: %+v", got)
	}

	var list struct {
		Workflows []ExtWorkflowResponse `json:"workflows"`
	}
	testutil.Call(t, testHandler.ListExtWorkflows, extWFReq("", "GET", "/api/ext/workflows", nil, "")).
		Want(http.StatusOK).JSON(&list)
	var listed *ExtWorkflowResponse
	for i := range list.Workflows {
		if list.Workflows[i].ID == wf.ID {
			listed = &list.Workflows[i]
		}
	}
	if listed == nil || listed.NodeCount != 1 || listed.LastRunAt != nil || len(listed.Nodes) != 0 {
		t.Fatalf("unexpected list entry: %+v", listed)
	}

	testutil.Call(t, testHandler.DeleteExtWorkflow, extWFReq("", "DELETE", "/api/ext/workflows/"+wf.ID, nil, wf.ID)).
		Want(http.StatusNoContent)
	testutil.Call(t, testHandler.DeleteExtWorkflow, extWFReq("", "DELETE", "/api/ext/workflows/"+wf.ID, nil, wf.ID)).
		Want(http.StatusBadRequest)
	testutil.Call(t, testHandler.ListExtWorkflows, extWFReq("", "GET", "/api/ext/workflows", nil, "")).
		Want(http.StatusOK).JSON(&list)
	for _, w := range list.Workflows {
		if w.ID == wf.ID {
			t.Fatal("archived workflow still listed")
		}
	}
	// Archived workflows stay readable (run history links to them) but are read-only.
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq("", "GET", "/api/ext/workflows/"+wf.ID, nil, wf.ID)).
		Want(http.StatusOK).JSON(&got)
	if got.ArchivedAt == nil {
		t.Fatal("archived_at not set on archived workflow")
	}
	updateExtWorkflow(t, "", wf.ID, map[string]any{"name": "nope"}).Want(http.StatusConflict)

	want := []string{protocol.EventExtWorkflowCreated, protocol.EventExtWorkflowUpdated, protocol.EventExtWorkflowUpdated,
		protocol.EventExtWorkflowUpdated, protocol.EventExtWorkflowDeleted}
	gotEvents := extWorkflowEventTypes(t, wf.ID)
	if len(gotEvents) != len(want) {
		t.Fatalf("events = %v, want %v", gotEvents, want)
	}
	for i := range want {
		if gotEvents[i] != want[i] {
			t.Fatalf("events = %v, want %v", gotEvents, want)
		}
	}
}

func TestExtWorkflow_CreateValidation(t *testing.T) {
	requireExtWorkflowDB(t)
	supervisor := createHandlerTestAgent(t, "ext-wf-create-sup", nil)

	post := func(body any) *testutil.Response {
		return testutil.Call(t, testHandler.CreateExtWorkflow, extWFReq("", "POST", "/api/ext/workflows", body, ""))
	}
	post("{not json").Want(http.StatusBadRequest)
	post(map[string]any{"name": "  ", "supervisor_agent_id": supervisor}).Want(http.StatusBadRequest)

	var body extWFValidationBody
	post(map[string]any{"name": "x"}).Want(http.StatusUnprocessableEntity).JSON(&body)
	if body.Error != "validation_failed" || len(body.Errors) != 1 || body.Errors[0].Field != "supervisor_agent_id" {
		t.Fatalf("missing supervisor: %+v", body)
	}
	post(map[string]any{"name": "x", "supervisor_agent_id": "00000000-0000-0000-0000-00000000dead"}).
		Want(http.StatusUnprocessableEntity).JSON(&body)
	if len(body.Errors) != 1 || body.Errors[0].Message != "agent not found in this workspace" {
		t.Fatalf("unknown supervisor: %+v", body)
	}
	post(map[string]any{"name": "x", "supervisor_agent_id": "not-a-uuid"}).
		Want(http.StatusUnprocessableEntity).JSON(&body)
	if len(body.Errors) != 1 || body.Errors[0].Message != "invalid agent id" {
		t.Fatalf("malformed supervisor id: %+v", body)
	}
}

func TestExtWorkflow_UpdateStructureValidation(t *testing.T) {
	requireExtWorkflowDB(t)
	supervisor := createHandlerTestAgent(t, "ext-wf-struct-sup", nil)
	worker := createHandlerTestAgent(t, "ext-wf-struct-worker", nil)
	wf := createExtWorkflowAs(t, "", "Structure", supervisor)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"nodes": []any{extWFNode("keep", worker)}}).Want(http.StatusOK)

	var body extWFValidationBody
	updateExtWorkflow(t, "", wf.ID, map[string]any{
		"nodes": []any{
			extWFNode("a", worker, "b"),
			extWFNode("b", worker, "a"),
			extWFNode("c", worker, "ghost"),
			extWFNode("Bad Key", worker),
		},
	}).Want(http.StatusUnprocessableEntity).JSON(&body)
	if body.Error != "validation_failed" {
		t.Fatalf("error = %q", body.Error)
	}
	seen := map[string]bool{}
	for _, e := range body.Errors {
		seen[e.NodeKey+"/"+e.Field] = true
	}
	for _, key := range []string{"a/depends_on", "b/depends_on", "c/depends_on", "Bad Key/key"} {
		if !seen[key] {
			t.Errorf("missing validation error %q in %+v", key, body.Errors)
		}
	}

	updateExtWorkflow(t, "", wf.ID, map[string]any{"nodes": []any{}}).Want(http.StatusUnprocessableEntity)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"max_rewinds": 11}).Want(http.StatusUnprocessableEntity)
	updateExtWorkflow(t, "", wf.ID, "{not json").Want(http.StatusBadRequest)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"name": " "}).Want(http.StatusBadRequest)

	// Every rejected save left the stored definition untouched.
	var got ExtWorkflowResponse
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq("", "GET", "/", nil, wf.ID)).Want(http.StatusOK).JSON(&got)
	if len(got.Nodes) != 1 || got.Nodes[0].Key != "keep" || got.MaxRewinds != 3 || got.Name != "Structure" {
		t.Fatalf("rejected update mutated the workflow: %+v", got)
	}
}

func TestExtWorkflow_AgentChecks(t *testing.T) {
	requireExtWorkflowDB(t)
	privateAgent, _, memberID := privateAgentTestFixture(t)
	publicSupervisor := createHandlerTestAgent(t, "ext-wf-agent-sup", nil)
	publicWorker := createHandlerTestAgent(t, "ext-wf-agent-worker", nil)
	archivedAgent := createHandlerTestAgent(t, "ext-wf-archived", nil)
	if _, err := testPool.Exec(context.Background(), `UPDATE agent SET archived_at = now() WHERE id = $1`, archivedAgent); err != nil {
		t.Fatalf("archive agent: %v", err)
	}

	// A plain member cannot make a private agent they cannot invoke the supervisor.
	var body extWFValidationBody
	testutil.Call(t, testHandler.CreateExtWorkflow, extWFReq(memberID, "POST", "/api/ext/workflows", map[string]any{
		"name": "Private sup", "supervisor_agent_id": privateAgent,
	}, "")).Want(http.StatusUnprocessableEntity).JSON(&body)
	if len(body.Errors) != 1 || body.Errors[0].Field != "supervisor_agent_id" || body.Errors[0].Message != "you do not have access to this agent" {
		t.Fatalf("private supervisor: %+v", body)
	}

	wf := createExtWorkflowAs(t, memberID, "Agent checks", publicSupervisor)
	updateExtWorkflow(t, memberID, wf.ID, map[string]any{
		"nodes": []any{
			extWFNode("ok", publicWorker),
			extWFNode("private", privateAgent, "ok"),
			extWFNode("gone", archivedAgent, "ok"),
			extWFNode("missing", "00000000-0000-0000-0000-00000000dead", "ok"),
		},
	}).Want(http.StatusUnprocessableEntity).JSON(&body)
	got := map[string]string{}
	for _, e := range body.Errors {
		if e.Field != "agent_id" {
			t.Errorf("unexpected field %q in %+v", e.Field, e)
		}
		got[e.NodeKey] = e.Message
	}
	want := map[string]string{
		"private": "you do not have access to this agent",
		"gone":    "agent is archived",
		"missing": "agent not found in this workspace",
	}
	if len(got) != len(want) {
		t.Fatalf("agent errors = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("agent errors = %v, want %v", got, want)
		}
	}

	// The workspace owner may wire the same private agent (admin wiring is unrestricted).
	updateExtWorkflow(t, "", wf.ID, map[string]any{
		"supervisor_agent_id": privateAgent,
		"nodes":               []any{extWFNode("private", privateAgent)},
	}).Want(http.StatusOK)

	// A rename by the creator is not blocked by the agents wired earlier.
	updateExtWorkflow(t, memberID, wf.ID, map[string]any{"name": "Renamed"}).Want(http.StatusOK)
}

func TestExtWorkflow_Permissions(t *testing.T) {
	requireExtWorkflowDB(t)
	creatorID := createPlainMember(t, "ext-wf-creator@multica.test")
	strangerID := createPlainMember(t, "ext-wf-stranger@multica.test")
	supervisor := createHandlerTestAgent(t, "ext-wf-perm-sup", nil)

	wf := createExtWorkflowAs(t, creatorID, "Perms", supervisor)
	if wf.CreatorID != creatorID {
		t.Fatalf("creator_id = %s, want %s", wf.CreatorID, creatorID)
	}

	updateExtWorkflow(t, strangerID, wf.ID, map[string]any{"name": "Hijack"}).Want(http.StatusForbidden)
	testutil.Call(t, testHandler.DeleteExtWorkflow, extWFReq(strangerID, "DELETE", "/", nil, wf.ID)).Want(http.StatusForbidden)

	// Visible workspace-wide.
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq(strangerID, "GET", "/", nil, wf.ID)).Want(http.StatusOK)

	updateExtWorkflow(t, creatorID, wf.ID, map[string]any{"name": "Mine"}).Want(http.StatusOK)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"name": "Admin edit"}).Want(http.StatusOK)
	testutil.Call(t, testHandler.DeleteExtWorkflow, extWFReq(creatorID, "DELETE", "/", nil, wf.ID)).Want(http.StatusNoContent)
}

func TestExtWorkflow_NotFoundAndBadID(t *testing.T) {
	requireExtWorkflowDB(t)
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq("", "GET", "/", nil, "00000000-0000-0000-0000-00000000dead")).
		Want(http.StatusNotFound)
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq("", "GET", "/", nil, "not-a-uuid")).
		Want(http.StatusBadRequest)
}

// TestExtWorkflow_WorkspaceTeardownSweepsEveryTable pins the workspace-delete
// hook: the ext tables carry no foreign keys, so DeleteExtWorkflowWorkspaceData
// is the only thing that removes their rows.
func TestExtWorkflow_WorkspaceTeardownSweepsEveryTable(t *testing.T) {
	requireExtWorkflowDB(t)
	ctx := context.Background()

	tables := []string{"ext_workflow", "ext_workflow_node", "ext_workflow_run", "ext_workflow_run_step", "ext_workflow_run_event"}
	seed := func(workspaceID string) {
		t.Helper()
		for _, stmt := range []string{
			`INSERT INTO ext_workflow (workspace_id, name, supervisor_agent_id, creator_id)
			 VALUES ($1, 'teardown', gen_random_uuid(), gen_random_uuid())`,
			`INSERT INTO ext_workflow_node (workflow_id, workspace_id, key, title, agent_id)
			 VALUES (gen_random_uuid(), $1, 'k', 't', gen_random_uuid())`,
			`INSERT INTO ext_workflow_run (workspace_id, workflow_id, issue_id, triggered_by_type, triggered_by_id, definition)
			 VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'member', gen_random_uuid(), '{}'::jsonb)`,
			`INSERT INTO ext_workflow_run_step (run_id, workspace_id, node_key, agent_id, issue_id)
			 VALUES (gen_random_uuid(), $1, 'k', gen_random_uuid(), gen_random_uuid())`,
			`INSERT INTO ext_workflow_run_event (run_id, workspace_id, kind, actor_type)
			 VALUES (gen_random_uuid(), $1, 'run_started', 'engine')`,
		} {
			if _, err := testPool.Exec(ctx, stmt, workspaceID); err != nil {
				t.Fatalf("seed %q: %v", stmt, err)
			}
		}
	}
	var doomed, survivor string
	dbfx.QueryRow(t, `SELECT gen_random_uuid()::text`).Scan(&doomed)
	dbfx.QueryRow(t, `SELECT gen_random_uuid()::text`).Scan(&survivor)
	t.Cleanup(func() {
		for _, table := range tables {
			testPool.Exec(ctx, `DELETE FROM `+table+` WHERE workspace_id = ANY($1::uuid[])`, []string{doomed, survivor})
		}
	})
	seed(doomed)
	seed(survivor)

	doomedUUID, err := util.ParseUUID(doomed)
	if err != nil {
		t.Fatal(err)
	}
	if err := testHandler.Queries.DeleteExtWorkflowWorkspaceData(ctx, doomedUUID); err != nil {
		t.Fatalf("DeleteExtWorkflowWorkspaceData: %v", err)
	}
	for _, table := range tables {
		if n := dbfx.Count(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, doomed); n != 0 {
			t.Errorf("%s: %d rows survived teardown", table, n)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, survivor); n != 1 {
			t.Errorf("%s: other workspace has %d rows, want 1", table, n)
		}
	}
}

func TestExtWorkflow_DefaultsMaxAttempts(t *testing.T) {
	requireExtWorkflowDB(t)
	supervisor := createHandlerTestAgent(t, "ext-wf-default-sup", nil)
	worker := createHandlerTestAgent(t, "ext-wf-default-worker", nil)
	wf := createExtWorkflowAs(t, "", "Defaults", supervisor)

	node := extWFNode("n", worker)
	delete(node, "max_attempts")
	var upd ExtWorkflowResponse
	updateExtWorkflow(t, "", wf.ID, map[string]any{"nodes": []any{node}}).Want(http.StatusOK).JSON(&upd)
	if len(upd.Nodes) != 1 || upd.Nodes[0].MaxAttempts != 3 {
		t.Fatalf("omitted max_attempts not defaulted to 3: %+v", upd.Nodes)
	}
	if upd.MaxRewinds != 3 {
		t.Fatalf("max_rewinds changed: %d", upd.MaxRewinds)
	}
}

// A workflow in another workspace must be invisible and immutable through this
// workspace's routes even for a user who belongs to both.
func TestExtWorkflow_CrossWorkspaceIs404(t *testing.T) {
	requireExtWorkflowDB(t)
	ctx := context.Background()
	otherWS := dbfx.Workspace(t, "Ext WF Other", "ext-wf-other")
	dbfx.Member(t, otherWS, testUserID, "owner")
	var otherWF string
	dbfx.QueryRow(t, `INSERT INTO ext_workflow (workspace_id, name, supervisor_agent_id, creator_id)
		VALUES ($1, 'foreign', gen_random_uuid(), $2) RETURNING id::text`, otherWS, testUserID).Scan(&otherWF)
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM ext_workflow WHERE id = $1`, otherWF) })

	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq("", "GET", "/", nil, otherWF)).Want(http.StatusNotFound)
	updateExtWorkflow(t, "", otherWF, map[string]any{"name": "hijack"}).Want(http.StatusNotFound)
	testutil.Call(t, testHandler.DeleteExtWorkflow, extWFReq("", "DELETE", "/", nil, otherWF)).Want(http.StatusNotFound)

	if n := dbfx.Count(t, `SELECT count(*) FROM ext_workflow WHERE id = $1 AND name = 'foreign' AND archived_at IS NULL`, otherWF); n != 1 {
		t.Fatalf("foreign workflow was modified (count=%d)", n)
	}
}
