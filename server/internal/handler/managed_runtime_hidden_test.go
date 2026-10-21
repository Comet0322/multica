package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestManagedRuntimeServiceable(t *testing.T) {
	now := time.Now()
	fresh := pgtype.Timestamptz{Time: now.Add(-10 * time.Second), Valid: true}
	stale := pgtype.Timestamptz{Time: now.Add(-10 * time.Minute), Valid: true}
	cases := []struct {
		name string
		rt   db.AgentRuntime
		want bool
	}{
		{"online daemon", db.AgentRuntime{Status: "online"}, true},
		{"suspended fresh", db.AgentRuntime{Status: "offline", ManagedState: "suspended", ManagedStateAt: fresh}, true},
		{"waking fresh", db.AgentRuntime{Status: "offline", ManagedState: "waking", ManagedStateAt: fresh}, true},
		{"running fresh but daemon not yet registered", db.AgentRuntime{Status: "offline", ManagedState: "running", ManagedStateAt: fresh}, true},
		{"unavailable fresh", db.AgentRuntime{Status: "offline", ManagedState: "unavailable", ManagedStateAt: fresh}, false},
		{"suspended but report is stale: controller is dead", db.AgentRuntime{Status: "offline", ManagedState: "suspended", ManagedStateAt: stale}, false},
		{"never reported", db.AgentRuntime{Status: "offline"}, false},
		{"unknown state", db.AgentRuntime{Status: "offline", ManagedState: "weird", ManagedStateAt: fresh}, false},
	}
	for _, c := range cases {
		if got := managedRuntimeServiceable(c.rt, now); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestRuntimeToUserResponseRedactsManaged(t *testing.T) {
	now := time.Now()
	rt := db.AgentRuntime{
		ID:             parseUUID(testWorkspaceID),
		WorkspaceID:    parseUUID(testWorkspaceID),
		DaemonID:       strToText("pod-secret-daemon-id"),
		Name:           "claude (sandbox-7f9c4)",
		CustomName:     strToText("Company Claude"),
		Provider:       "claude",
		Status:         "offline",
		DeviceInfo:     "kind-worker/pod sandbox-7f9c4",
		Metadata:       []byte(`{"cli_version":"0.6.1","launched_by":"x"}`),
		OwnerID:        parseUUID(testUserID),
		Managed:        true,
		ManagedState:   "suspended",
		ManagedStateAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
	body, _ := json.Marshal(runtimeToUserResponse(rt, now))
	for _, leak := range []string{"pod-secret-daemon-id", "sandbox-7f9c4", "kind-worker", "cli_version", "launched_by", testUserID} {
		if strings.Contains(string(body), leak) {
			t.Errorf("response leaks %q: %s", leak, body)
		}
	}
	var got AgentRuntimeResponse
	_ = json.Unmarshal(body, &got)
	if got.Name != "Company Claude" || got.Status != "online" || !got.Managed || got.DaemonID != nil || got.OwnerID != nil {
		t.Fatalf("unexpected projection: %+v", got)
	}

	// Unmanaged runtimes are untouched.
	rt.Managed = false
	full := runtimeToUserResponse(rt, now)
	if full.DeviceInfo == "" || full.DaemonID == nil || full.Status != "offline" {
		t.Fatalf("unmanaged runtime must be returned as-is: %+v", full)
	}
}

func TestManagedRuntimeHiddenInMemberListAndShownOnlyPublicly(t *testing.T) {
	id := newManagedRuntime(t, "managed-list-test")
	member := dbfx.User(t, "List Member", "list-member@example.com")
	dbfx.Member(t, testWorkspaceID, member, "member")

	report := func(state string) {
		t.Helper()
		w := controllerCall(t, testHandler.ControllerReportManagedState, http.MethodPut, "/internal/v1/managed-runtimes/state", map[string]any{
			"runtimes": []map[string]any{{"workspace_id": testWorkspaceID, "daemon_id": "managed-list-test", "state": state}},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("report state: %d %s", w.Code, w.Body.String())
		}
	}
	list := func() (AgentRuntimeResponse, string) {
		req := newRequest(http.MethodGet, "/api/runtimes", nil)
		req.Header.Set("X-User-ID", member)
		w := httptest.NewRecorder()
		testHandler.ListAgentRuntimes(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		var rows []AgentRuntimeResponse
		_ = json.Unmarshal(w.Body.Bytes(), &rows)
		for _, r := range rows {
			if r.ID == id {
				return r, w.Body.String()
			}
		}
		t.Fatalf("managed runtime missing from member list: %s", w.Body.String())
		return AgentRuntimeResponse{}, ""
	}

	report("suspended")
	got, raw := list()
	if got.Status != "online" || !got.Managed || got.DaemonID != nil || got.DeviceInfo != "" || got.OwnerID != nil {
		t.Fatalf("member view of suspended managed runtime: %+v", got)
	}
	if strings.Contains(raw, "managed-list-test") {
		t.Fatalf("daemon id leaked to member: %s", raw)
	}

	report("unavailable")
	if got, _ := list(); got.Status != "offline" {
		t.Fatalf("unavailable state must show offline, got %q", got.Status)
	}

	// A stale report fails closed.
	report("suspended")
	dbfx.Exec(t, `UPDATE agent_runtime SET managed_state_at = now() - interval '1 hour' WHERE id = $1`, id)
	if got, _ := list(); got.Status != "offline" {
		t.Fatalf("stale controller report must show offline, got %q", got.Status)
	}
}

func TestManagedRuntimeUserMutationsAreBlocked(t *testing.T) {
	id := newManagedRuntime(t, "managed-mutation-test")
	unmanaged := dbfx.Runtime(t, "plain", map[string]any{"daemon_id": "plain-daemon", "runtime_mode": "local"})

	reached := false
	h := testHandler.RejectManagedRuntime(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, route := range []string{"PATCH", "DELETE", "POST"} {
		reached = false
		req := withURLParam(newRequest(route, "/api/runtimes/"+id, nil), "runtimeId", id)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound || reached {
			t.Errorf("%s on managed runtime: code=%d reached=%v", route, w.Code, reached)
		}
	}
	reached = false
	req := withURLParam(newRequest("PATCH", "/api/runtimes/"+unmanaged, nil), "runtimeId", unmanaged)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !reached {
		t.Error("unmanaged runtime must pass through")
	}
}

func TestRedactManagedTaskPaths(t *testing.T) {
	id := newManagedRuntime(t, "managed-paths-test")
	plain := dbfx.Runtime(t, "plain-paths", map[string]any{"daemon_id": "plain-paths-daemon", "runtime_mode": "local"})
	agentManaged := dbfx.Agent(t, "managed-paths-agent", id)
	agentPlain := dbfx.Agent(t, "plain-paths-agent", plain)
	taskM := dbfx.Task(t, agentManaged, map[string]any{"issue_id": dbfx.Issue(t, "m"), "runtime_id": id, "work_dir": "/data/workspaces/ws/task1/workdir"})
	taskP := dbfx.Task(t, agentPlain, map[string]any{"issue_id": dbfx.Issue(t, "p"), "runtime_id": plain, "work_dir": "/home/me/ws/task2/workdir"})

	var rows []db.AgentTaskQueue
	for _, tid := range []string{taskM, taskP} {
		row, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(tid))
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	resp := []AgentTaskResponse{taskToResponse(rows[0], testWorkspaceID), taskToResponse(rows[1], testWorkspaceID)}
	if resp[0].WorkDir == "" {
		t.Fatal("fixture: work_dir not set")
	}
	testHandler.redactManagedTaskPaths(context.Background(), rows, resp)
	if resp[0].WorkDir != "" || resp[0].RelativeWorkDir != "" || resp[0].DurableWorkDir != "" {
		t.Fatalf("managed task paths not cleared: %+v", resp[0])
	}
	if resp[1].WorkDir == "" {
		t.Fatalf("unmanaged task path must remain: %+v", resp[1])
	}
}

func TestDaemonAuthzManagedRuntime(t *testing.T) {
	id := newManagedRuntime(t, "managed-authz-test")
	for _, mode := range []daemonAuthzMode{daemonAuthzLog, daemonAuthzEnforce} {
		withAuthzMode(t, mode)
		member := dbfx.User(t, "Managed Member "+string(mode), "managed-authz-"+string(mode)+"@example.com")
		dbfx.Member(t, testWorkspaceID, member, "member")

		hb := func(setup func(*http.Request) *http.Request) int {
			req := newRequest(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": id})
			req.Header.Del("X-User-ID")
			w := httptest.NewRecorder()
			testHandler.DaemonHeartbeat(w, setup(req))
			return w.Code
		}
		// The runtime's recorded owner (testUserID) and any other member are all
		// refused, even in log mode: managed runtimes enforce unconditionally.
		for name, user := range map[string]string{"owner": testUserID, "member": member} {
			code := hb(func(r *http.Request) *http.Request { r.Header.Set("X-User-ID", user); return r })
			if code != http.StatusNotFound {
				t.Errorf("mode=%s PAT/JWT %s on managed runtime: %d, want 404", mode, name, code)
			}
		}
		if code := hb(func(r *http.Request) *http.Request {
			return r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, "managed-authz-test"))
		}); code != http.StatusOK {
			t.Errorf("mode=%s own mdt_: %d, want 200", mode, code)
		}
		if code := hb(func(r *http.Request) *http.Request {
			return r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, "someone-else"))
		}); code != http.StatusNotFound {
			t.Errorf("mode=%s other daemon's mdt_: %d, want 404", mode, code)
		}
	}
}

func TestRegisterCannotTakeOverManagedDaemonID(t *testing.T) {
	newManagedRuntime(t, "managed-takeover-reg")
	register := func(daemonID string, legacy []string, ctxSetup func(*http.Request) *http.Request) int {
		req := newRequest(http.MethodPost, "/api/daemon/register", map[string]any{
			"workspace_id":      testWorkspaceID,
			"daemon_id":         daemonID,
			"legacy_daemon_ids": legacy,
			"device_name":       "attacker",
			"runtimes":          []map[string]any{{"name": "claude", "type": "claude", "version": "2.1.0", "status": "online"}},
		})
		w := httptest.NewRecorder()
		testHandler.DaemonRegister(w, ctxSetup(req))
		return w.Code
	}
	same := func(r *http.Request) *http.Request { return r }
	if code := register("managed-takeover-reg", nil, same); code != http.StatusNotFound {
		t.Errorf("PAT register over a managed daemon_id: %d, want 404", code)
	}
	if code := register("attacker-daemon", []string{"managed-takeover-reg"}, same); code != http.StatusNotFound {
		t.Errorf("PAT register merging a managed legacy id: %d, want 404", code)
	}
	otherToken := func(r *http.Request) *http.Request {
		r.Header.Del("X-User-ID")
		return r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, "some-other-daemon"))
	}
	if code := register("managed-takeover-reg", nil, otherToken); code != http.StatusNotFound {
		t.Errorf("another daemon's mdt_ registering a managed daemon_id: %d, want 404", code)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE daemon_id = 'attacker-daemon'`)
	})
}
