package sandboxmanager_test

import (
	"slices"
	"testing"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

// enqueueIssue adds a task to the workspace under a chosen issue.
func (m *multiMultica) enqueueIssue(ws *fakeWorkspace, id, issueID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws.queue = append(ws.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": ws.runtimeID,
		"workspace_id": ws.id, "issue_id": issueID, "initiator_email": "alice@example.com",
	})
}

func TestWorkspacePlacementDrivesSharingAcrossWorkspaces(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, pat: patA}, &fakeWorkspace{id: wsB, pat: patB}
	up := newMultiMultica(t, a, b)
	// A shares pods per issue; B keeps the pool default of one task per pod.
	h := newMultiHarness(t, up, twoWorkspaces(func(c *sandboxmanager.WorkspaceConfig) { c.Placement = sandboxmanager.PlacementIssue }, nil))
	up.enqueueIssue(a, "ta1", "issue-1")
	up.enqueueIssue(a, "ta2", "issue-1")
	p1, p2, p3 := h.newPod("127.0.0.2", "pool-0"), h.newPod("127.0.0.3", "pool-1"), h.newPod("127.0.0.4", "pool-2")
	var ids []string
	for _, p := range []*simPod{p1, p2, p3} {
		p.nonce = "boot-" + p.name
		ids = p.startAll()
	}

	got := slices.Concat(p1.claimFrom(ids), p1.claimFrom(ids))
	if !slices.Equal(got, []string{"ta1", "ta2"}) {
		t.Fatalf("workspace A's issue should share the first pod: %v", got)
	}

	// B's tasks never share, even under one issue: each needs its own pod.
	up.enqueueIssue(b, "tb1", "issue-9")
	up.enqueueIssue(b, "tb2", "issue-9")
	if got := p2.claimFrom(ids); !slices.Equal(got, []string{"tb1"}) {
		t.Fatalf("pod 2 should take one B task: %v", got)
	}
	if got := p2.claimFrom(ids); len(got) != 0 {
		t.Fatalf("pod 2 must not take a second B task: %v", got)
	}
	if got := p3.claimFrom(ids); !slices.Equal(got, []string{"tb2"}) {
		t.Fatalf("pod 3 should take the other B task: %v", got)
	}
}
