package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
)

func controllerCall(t *testing.T, h http.HandlerFunc, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(method, path, body)
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func newManagedRuntime(t *testing.T, daemonID string) (runtimeID string) {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	w := controllerCall(t, testHandler.ControllerUpsertManagedRuntime, http.MethodPost, "/internal/v1/managed-runtimes", map[string]any{
		"workspace_id": testWorkspaceID,
		"daemon_id":    daemonID,
		"provider":     "claude",
		"display_name": "Company Claude",
		"owner_id":     testUserID,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("upsert managed runtime: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, out.ID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM daemon_token WHERE daemon_id = $1`, daemonID)
	})
	return out.ID
}

func TestControllerAuthMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	cases := []struct {
		name, secret, header string
		want                 int
	}{
		{"unset secret disables API", "", "Bearer anything", http.StatusNotFound},
		{"missing header", "s3cret", "", http.StatusUnauthorized},
		{"wrong secret", "s3cret", "Bearer nope", http.StatusUnauthorized},
		{"not bearer", "s3cret", "s3cret", http.StatusUnauthorized},
		{"correct", "s3cret", "Bearer s3cret", http.StatusNoContent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal/v1/managed-runtimes/state", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			w := httptest.NewRecorder()
			middleware.ControllerAuth(c.secret)(next).ServeHTTP(w, req)
			if w.Code != c.want {
				t.Fatalf("got %d want %d", w.Code, c.want)
			}
		})
	}
}

func TestManagedRuntimeUpsertAndRejectTakeover(t *testing.T) {
	id := newManagedRuntime(t, "managed-upsert-test")
	var managed bool
	var visibility, custom string
	dbfx.QueryRow(t, `SELECT managed, visibility, COALESCE(custom_name,'') FROM agent_runtime WHERE id = $1`, id).Scan(&managed, &visibility, &custom)
	if !managed || visibility != "public" || custom != "Company Claude" {
		t.Fatalf("managed=%v visibility=%q custom_name=%q", managed, visibility, custom)
	}

	// Same slot again: idempotent, same row.
	if again := newManagedRuntime(t, "managed-upsert-test"); again != id {
		t.Fatalf("second upsert created a new row: %s vs %s", again, id)
	}

	// An unmanaged runtime in the same slot must not be taken over.
	other := dbfx.Runtime(t, "local-claim", map[string]any{"daemon_id": "managed-takeover", "provider": "claude", "runtime_mode": "local"})
	_ = other
	w := controllerCall(t, testHandler.ControllerUpsertManagedRuntime, http.MethodPost, "/internal/v1/managed-runtimes", map[string]any{
		"workspace_id": testWorkspaceID, "daemon_id": "managed-takeover", "provider": "claude",
		"display_name": "x", "owner_id": testUserID,
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("takeover must be 409, got %d %s", w.Code, w.Body.String())
	}
}

func TestManagedRuntimeUpsertValidation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	bad := []map[string]any{
		{"workspace_id": "nope", "daemon_id": "d1", "provider": "claude", "display_name": "x", "owner_id": testUserID},
		{"workspace_id": testWorkspaceID, "daemon_id": "bad id!", "provider": "claude", "display_name": "x", "owner_id": testUserID},
		{"workspace_id": testWorkspaceID, "daemon_id": "d1", "provider": "", "display_name": "x", "owner_id": testUserID},
		{"workspace_id": testWorkspaceID, "daemon_id": "d1", "provider": "claude", "display_name": "", "owner_id": testUserID},
		// owner is not a member of the workspace
		{"workspace_id": testWorkspaceID, "daemon_id": "d1", "provider": "claude", "display_name": "x", "owner_id": "00000000-0000-0000-0000-0000000000aa"},
	}
	for i, body := range bad {
		w := controllerCall(t, testHandler.ControllerUpsertManagedRuntime, http.MethodPost, "/internal/v1/managed-runtimes", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("case %d: expected 400, got %d %s", i, w.Code, w.Body.String())
		}
	}
}

func TestManagedDaemonTokenLifecycle(t *testing.T) {
	newManagedRuntime(t, "managed-token-test")

	issue := func(daemonID string, ttl int) (int, string) {
		w := controllerCall(t, testHandler.ControllerIssueDaemonToken, http.MethodPost, "/internal/v1/daemon-tokens", map[string]any{
			"workspace_id": testWorkspaceID, "daemon_id": daemonID, "ttl_seconds": ttl,
		})
		var out struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Token
	}

	if code, _ := issue("not-a-managed-daemon", 60); code != http.StatusNotFound {
		t.Fatalf("token for unknown/unmanaged daemon must be 404, got %d", code)
	}
	if code, _ := issue("managed-token-test", 90000); code != http.StatusBadRequest {
		t.Fatalf("ttl over 24h must be 400, got %d", code)
	}
	code, raw := issue("managed-token-test", 600)
	if code != http.StatusOK || len(raw) < 8 || raw[:4] != "mdt_" {
		t.Fatalf("issue: code=%d token prefix ok=%v", code, len(raw) >= 4 && raw[:4] == "mdt_")
	}

	row, err := testHandler.Queries.GetDaemonTokenByHash(context.Background(), auth.HashToken(raw))
	if err != nil {
		t.Fatalf("issued token not found by hash: %v", err)
	}
	if uuidToString(row.WorkspaceID) != testWorkspaceID || row.DaemonID != "managed-token-test" {
		t.Fatalf("token bound to %s/%s", uuidToString(row.WorkspaceID), row.DaemonID)
	}

	w := controllerCall(t, testHandler.ControllerRevokeDaemonTokens, http.MethodDelete, "/internal/v1/daemon-tokens", map[string]any{
		"workspace_id": testWorkspaceID, "daemon_id": "managed-token-test",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	if _, err := testHandler.Queries.GetDaemonTokenByHash(context.Background(), auth.HashToken(raw)); err == nil {
		t.Fatal("token still valid after revoke")
	}
}

func TestManagedRuntimeStateCountsTasks(t *testing.T) {
	id := newManagedRuntime(t, "managed-state-test")
	agent := dbfx.Agent(t, "managed-state-agent", id)
	for i, status := range []string{"queued", "queued", "running", "completed"} {
		issue := dbfx.Issue(t, "managed state issue "+string(rune('a'+i)))
		dbfx.Task(t, agent, map[string]any{"issue_id": issue, "runtime_id": id, "status": status})
	}

	w := controllerCall(t, testHandler.ControllerManagedRuntimeState, http.MethodGet, "/internal/v1/managed-runtimes/state", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("state: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Runtimes []struct {
			ID          string `json:"id"`
			Status      string `json:"status"`
			QueuedTasks int    `json:"queued_tasks"`
			ActiveTasks int    `json:"active_tasks"`
		} `json:"runtimes"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	for _, rt := range out.Runtimes {
		if rt.ID == id {
			if rt.QueuedTasks != 2 || rt.ActiveTasks != 1 || rt.Status != "offline" {
				t.Fatalf("state = %+v", rt)
			}
			return
		}
	}
	t.Fatal("managed runtime missing from state")
}

// A daemon registering with its mdt_ lands on the pre-created row and must not
// clear managed, visibility or the display name.
func TestManagedRuntimeSurvivesDaemonRegister(t *testing.T) {
	id := newManagedRuntime(t, "managed-register-test")

	req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/register", map[string]any{
		"workspace_id": testWorkspaceID,
		"daemon_id":    "managed-register-test",
		"device_name":  "pod-xyz",
		"cli_version":  "0.0.0-test",
		"runtimes":     []map[string]any{{"name": "claude", "type": "claude", "version": "2.1.0", "status": "online"}},
	}, testWorkspaceID, "managed-register-test")
	w := httptest.NewRecorder()
	testHandler.DaemonRegister(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var managed bool
	var visibility, custom, status string
	var owner string
	dbfx.QueryRow(t, `SELECT managed, visibility, COALESCE(custom_name,''), status, COALESCE(owner_id::text,'') FROM agent_runtime WHERE id = $1`, id).
		Scan(&managed, &visibility, &custom, &status, &owner)
	if !managed || visibility != "public" || custom != "Company Claude" || status != "online" || owner != testUserID {
		t.Fatalf("after register: managed=%v visibility=%q custom=%q status=%q owner=%q", managed, visibility, custom, status, owner)
	}
}
