package sandboxbroker

import (
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxprovider"
)

func TestPlacementFollowsIsolation(t *testing.T) {
	issueA := claimedTaskRef{ID: "t1", AgentID: testAgent, IssueID: "issue-a"}
	issueA2 := claimedTaskRef{ID: "t2", AgentID: testAgent, IssueID: "issue-a"}
	issueB := claimedTaskRef{ID: "t3", AgentID: testAgent, IssueID: "issue-b"}
	otherAgent := claimedTaskRef{ID: "t4", AgentID: "6f1c2a51-0000-4000-8000-0000000000a2", IssueID: "issue-a"}
	chat := claimedTaskRef{ID: "t5", AgentID: testAgent, ChatSessionID: "chat-1"}
	noScope := claimedTaskRef{ID: "t6", AgentID: testAgent}
	key := func(iso Isolation, ref claimedTaskRef) placement {
		t.Helper()
		p, err := place(iso, ref)
		if err != nil {
			t.Fatal(err)
		}
		if !sandboxprovider.ValidKey(p.Key) {
			t.Fatalf("invalid key %q", p.Key)
		}
		return p
	}

	if a, b := key(IsolationIssue, issueA), key(IsolationIssue, issueA2); a.Key != b.Key || a.Kind != scopeIssue {
		t.Fatalf("tasks of one issue should share a sandbox: %+v %+v", a, b)
	}
	if key(IsolationIssue, issueA).Key == key(IsolationIssue, issueB).Key {
		t.Fatal("issues must not share a sandbox")
	}
	if key(IsolationIssue, issueA).Key == key(IsolationIssue, otherAgent).Key {
		t.Fatal("agents must not share a sandbox on the same issue")
	}
	if p := key(IsolationIssue, chat); p.Kind != scopeChat || p.ScopeID != "chat-1" {
		t.Fatalf("chat placement = %+v", p)
	}
	if p := key(IsolationIssue, noScope); p.Kind != scopeTask {
		t.Fatalf("a task without issue or chat gets its own sandbox, got %+v", p)
	}
	if key(IsolationTask, issueA).Key == key(IsolationTask, issueA2).Key {
		t.Fatal("task isolation must not share sandboxes")
	}
	if a, b := key(IsolationAgent, issueA), key(IsolationAgent, issueB); a.Key != b.Key || a.Key != "agent-"+testAgent {
		t.Fatalf("agent isolation = %+v %+v", a, b)
	}
	if _, err := place(IsolationIssue, claimedTaskRef{ID: "t7", IssueID: "issue-a"}); err == nil {
		t.Fatal("a task without agent cannot be placed")
	}
}

func complete(h *harness, key, task string) {
	h.t.Helper()
	token := h.token(key)
	h.do(http.MethodPost, "/api/daemon/tasks/claim", token, map[string]any{"max_tasks": 5})
	if rec := h.do(http.MethodPost, "/api/daemon/tasks/"+task+"/complete", token, map[string]any{}); rec.Code != http.StatusOK {
		h.t.Fatalf("complete %s: %d", task, rec.Code)
	}
}

func exists(h *harness, key string) bool {
	h.broker.mu.Lock()
	_, inJournal := h.broker.state.Sandboxes[key]
	h.broker.mu.Unlock()
	h.provider.mu.Lock()
	_, inProvider := h.provider.sandboxes[key]
	h.provider.mu.Unlock()
	if inJournal != inProvider {
		h.t.Fatalf("sandbox %s journal=%v provider=%v", key, inJournal, inProvider)
	}
	return inJournal
}

func TestIssueSandboxIsDeletedAfterIssueIsDone(t *testing.T) {
	h := newHarness(t)
	h.registered()
	const t2, issueB = "6f1c2a51-0000-4000-8000-0000000000t2", "6f1c2a51-0000-4000-8000-0000000000i2"
	h.server.issueOf[t2] = issueB
	h.server.claims = []string{testTask, t2}
	h.step()
	keyA, keyB := issueKey(testIssue), issueKey(issueB)
	if !exists(h, keyA) || !exists(h, keyB) || keyA == keyB {
		t.Fatal("each issue should get its own sandbox")
	}
	tokenA := h.token(keyA)
	complete(h, keyA, testTask)
	complete(h, keyB, t2)

	// Done, but within the grace period: kept in case it is reopened.
	h.server.issues[testIssue] = map[string]any{"status": "done", "updated_at": h.clock.Add(-time.Minute)}
	h.advance(cleanupCheckInterval)
	h.step()
	if !exists(h, keyA) {
		t.Fatal("recently finished issue should keep its sandbox")
	}

	h.server.issues[testIssue] = map[string]any{"status": "done", "updated_at": h.clock.Add(-2 * time.Hour)}
	h.server.issues[issueB] = map[string]any{"status": "in_review", "updated_at": h.clock.Add(-2 * time.Hour)}
	h.advance(cleanupCheckInterval)
	h.step()
	if exists(h, keyA) {
		t.Fatal("done issue's sandbox and volume should be deleted")
	}
	if !exists(h, keyB) {
		t.Fatal("open issue's sandbox must be kept")
	}
	if rec := h.do(http.MethodPost, "/api/daemon/heartbeat", tokenA, map[string]any{"runtime_id": testRuntime}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted sandbox credential should be revoked, got %d", rec.Code)
	}

	h.server.issues[issueB] = map[string]any{"status": "deleted"}
	h.advance(cleanupCheckInterval)
	h.step()
	if exists(h, keyB) {
		t.Fatal("deleted issue's sandbox should be deleted")
	}
}

func TestIssueSandboxIsKeptWhileItHasWork(t *testing.T) {
	h := newHarness(t)
	h.registered()
	h.server.claims = []string{testTask}
	h.step()
	h.server.issues[testIssue] = map[string]any{"status": "cancelled", "updated_at": h.clock.Add(-2 * time.Hour)}
	h.advance(cleanupCheckInterval)
	h.step()
	if !exists(h, issueKey(testIssue)) {
		t.Fatal("a sandbox with an unfinished task must never be deleted")
	}
}

func TestTaskIsolationDeletesSandboxWhenTaskEnds(t *testing.T) {
	h := newHarnessWith(t, IsolationTask)
	h.registered()
	const t2 = "6f1c2a51-0000-4000-8000-0000000000t2"
	h.server.claims = []string{testTask, t2}
	h.step()
	k1 := h.broker.state.Tasks[testTask].Key
	k2 := h.broker.state.Tasks[t2].Key
	if k1 == k2 {
		t.Fatal("tasks must get separate sandboxes")
	}
	complete(h, k1, testTask)
	h.step()
	if exists(h, k1) {
		t.Fatal("finished task's sandbox should be deleted immediately")
	}
	if !exists(h, k2) {
		t.Fatal("running task's sandbox must be kept")
	}
}

func TestAgentIsolationDeletesUnusedSandboxAfterTTL(t *testing.T) {
	h := newHarnessWith(t, IsolationAgent)
	h.registered()
	h.server.claims = []string{testTask}
	h.step()
	key := "agent-" + testAgent
	complete(h, key, testTask)
	h.server.issues[testIssue] = map[string]any{"status": "done", "updated_at": h.clock.Add(-48 * time.Hour)}

	h.advance(h.broker.cfg.VolumeTTL - time.Hour)
	h.step()
	if !exists(h, key) {
		t.Fatal("agent sandbox outlives its issues until the TTL")
	}
	h.advance(2 * time.Hour)
	h.step()
	if exists(h, key) {
		t.Fatal("agent sandbox unused past the TTL should be deleted")
	}
}
