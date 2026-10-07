package main

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
)

// TestExtWorkflowHumanOnlyRoutesRejectTaskToken pins I1: an agent's task
// token authenticates as its runtime owner, so the template writes, run
// cancel and step decision routes must refuse it outright. Reads stay open.
func TestExtWorkflowHumanOnlyRoutesRejectTaskToken(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}

	ctx := context.Background()
	var agentID string
	if err := testPool.QueryRow(ctx, `
		SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at LIMIT 1
	`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("load integration-test agent: %v", err)
	}
	taskID := ensureAgentTask(t, agentID)
	token, err := auth.GenerateAgentTaskToken()
	if err != nil {
		t.Fatalf("generate task token: %v", err)
	}
	var tokenID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO task_token (token_hash, task_id, agent_id, workspace_id, user_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text
	`, auth.HashToken(token), taskID, agentID, testWorkspaceID, testUserID, time.Now().Add(time.Hour)).Scan(&tokenID); err != nil {
		t.Fatalf("insert task token: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM task_token WHERE id = $1`, tokenID); err != nil {
			t.Logf("delete task token fixture: %v", err)
		}
	})

	do := func(t *testing.T, method, path string) int {
		t.Helper()
		body := bytes.NewReader([]byte(`{}`))
		req, err := http.NewRequest(method, testServer.URL+path, body)
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		// Spoofed actor source must be discarded by auth.
		req.Header.Set("X-Actor-Source", "member")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("perform request: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	const id = "00000000-0000-0000-0000-000000000097"
	for _, tc := range []struct {
		name, method, path string
	}{
		{"create workflow", http.MethodPost, "/api/ext/workflows/"},
		{"update workflow", http.MethodPut, "/api/ext/workflows/" + id + "/"},
		{"archive workflow", http.MethodDelete, "/api/ext/workflows/" + id + "/"},
		{"cancel run", http.MethodPost, "/api/ext/workflow-runs/" + id + "/cancel"},
		{"decide step", http.MethodPost, "/api/ext/workflow-runs/" + id + "/steps/" + id + "/decision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := do(t, tc.method, tc.path); got != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", got)
			}
		})
	}

	t.Run("reads stay open to agents", func(t *testing.T) {
		if got := do(t, http.MethodGet, "/api/ext/workflows/"); got != http.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
	})
}
