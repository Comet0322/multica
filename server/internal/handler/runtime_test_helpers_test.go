package handler

import (
	"context"
	"testing"
)

func setRuntimeTestMemberRole(t *testing.T, userID, role string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		UPDATE member
		SET role = $3
		WHERE workspace_id = $1 AND user_id = $2
	`, testWorkspaceID, userID, role); err != nil {
		t.Fatalf("set runtime test member role to %s: %v", role, err)
	}
	if testHandler.MembershipCache != nil {
		testHandler.MembershipCache.Invalidate(context.Background(), userID, testWorkspaceID)
	}
}

func promoteRuntimeTestMemberToAdmin(t *testing.T, userID string) {
	t.Helper()
	setRuntimeTestMemberRole(t, userID, "admin")
}
