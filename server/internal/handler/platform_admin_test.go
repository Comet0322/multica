package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlatformAdminEmailsParsing(t *testing.T) {
	got := platformAdminEmails(" Ops@Corp.com, ,second@corp.com ")
	if len(got) != 2 {
		t.Fatalf("want 2 emails, got %v", got)
	}
	if _, ok := got["ops@corp.com"]; !ok {
		t.Fatalf("emails must be lower-cased: %v", got)
	}
	if len(platformAdminEmails("")) != 0 {
		t.Fatal("empty allowlist must admit nobody")
	}
}

func adminCall(t *testing.T, mw func(http.Handler) http.Handler, h http.HandlerFunc, method, path string, body any, runtimeID string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(method, path, body)
	if runtimeID != "" {
		req = withURLParam(req, "runtimeId", runtimeID)
	}
	w := httptest.NewRecorder()
	mw(h).ServeHTTP(w, req)
	return w
}

func TestPlatformAdminGate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	var email string
	if err := testPool.QueryRow(context.Background(), `SELECT email FROM "user" WHERE id = $1`, testUserID).Scan(&email); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PLATFORM_ADMIN_EMAILS", "")
	w := adminCall(t, testHandler.RequirePlatformAdmin, testHandler.ListPlatformManagedRuntimes, http.MethodGet, "/api/admin/managed-runtimes", nil, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("empty allowlist must 404, got %d", w.Code)
	}
	t.Setenv("PLATFORM_ADMIN_EMAILS", "someone-else@corp.com")
	w = adminCall(t, testHandler.RequirePlatformAdmin, testHandler.ListPlatformManagedRuntimes, http.MethodGet, "/api/admin/managed-runtimes", nil, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-admin must 404, got %d", w.Code)
	}
	// status probe is open and reports false.
	w = httptest.NewRecorder()
	testHandler.GetPlatformAdminStatus(w, newRequest(http.MethodGet, "/api/admin/status", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"is_admin":false`) {
		t.Fatalf("status probe: %d %s", w.Code, w.Body.String())
	}

	t.Setenv("PLATFORM_ADMIN_EMAILS", "x@y.z, "+strings.ToUpper(email))
	w = adminCall(t, testHandler.RequirePlatformAdmin, testHandler.ListPlatformManagedRuntimes, http.MethodGet, "/api/admin/managed-runtimes", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin must pass (case-insensitive), got %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	testHandler.GetPlatformAdminStatus(w, newRequest(http.MethodGet, "/api/admin/status", nil))
	if !strings.Contains(w.Body.String(), `"is_admin":true`) {
		t.Fatalf("status probe should be true: %s", w.Body.String())
	}
}

func TestPlatformAdminManagedRuntimeLifecycle(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	var email string
	if err := testPool.QueryRow(context.Background(), `SELECT email FROM "user" WHERE id = $1`, testUserID).Scan(&email); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLATFORM_ADMIN_EMAILS", email)
	gate := testHandler.RequirePlatformAdmin

	// create
	w := adminCall(t, gate, testHandler.CreatePlatformManagedRuntime, http.MethodPost, "/api/admin/managed-runtimes", map[string]any{
		"workspace_id": testWorkspaceID, "daemon_id": "admin-life-1", "provider": "claude",
		"display_name": "Admin Created", "owner_id": testUserID,
	}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, created.ID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM daemon_token WHERE daemon_id = 'admin-life-1'`)
	})

	// list shows unredacted fields
	w = adminCall(t, gate, testHandler.ListPlatformManagedRuntimes, http.MethodGet, "/api/admin/managed-runtimes", nil, "")
	var list struct {
		Runtimes []adminManagedRuntimeResponse `json:"runtimes"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	found := false
	for _, r := range list.Runtimes {
		if r.ID == created.ID {
			found = true
			if r.DaemonID == nil || *r.DaemonID != "admin-life-1" || r.DisplayName != "Admin Created" || r.WorkspaceName == "" {
				t.Fatalf("admin list item incomplete: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("created runtime missing from admin list: %s", w.Body.String())
	}

	// revoke tokens: mint one first
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO daemon_token (token_hash, workspace_id, daemon_id, expires_at) VALUES ('h-admin-life', $1, 'admin-life-1', now() + interval '1 hour')`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	w = adminCall(t, gate, testHandler.RevokePlatformManagedRuntimeTokens, http.MethodPost, "/x", nil, created.ID)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"revoked":1`) {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}

	// delete
	w = adminCall(t, gate, testHandler.DeletePlatformManagedRuntime, http.MethodDelete, "/x", nil, created.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	var n int
	_ = testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_runtime WHERE id = $1`, created.ID).Scan(&n)
	if n != 0 {
		t.Fatal("runtime row should be gone")
	}
}

func TestPlatformAdminCannotTouchUnmanagedRuntime(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	var email, rtID string
	if err := testPool.QueryRow(context.Background(), `SELECT email FROM "user" WHERE id = $1`, testUserID).Scan(&email); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLATFORM_ADMIN_EMAILS", email)
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		 VALUES ($1, 'plain-local', 'plain', 'local', 'claude', 'offline', '', '{}', $2) RETURNING id`, testWorkspaceID, testUserID).Scan(&rtID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, rtID) })
	for name, h := range map[string]http.HandlerFunc{
		"delete": testHandler.DeletePlatformManagedRuntime,
		"revoke": testHandler.RevokePlatformManagedRuntimeTokens,
	} {
		w := adminCall(t, testHandler.RequirePlatformAdmin, h, http.MethodPost, "/x", nil, rtID)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s on unmanaged runtime must 404, got %d", name, w.Code)
		}
	}
}
