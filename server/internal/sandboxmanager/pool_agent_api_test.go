package sandboxmanager_test

import (
	"context"
	"net/http"
	"testing"
)

// callWithToken sends one request the way an agent's multica CLI does: to the
// manager, with the task's own token instead of the daemon's placeholder.
func (p *simPod) callWithToken(ticket, method, path string) int {
	p.h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, p.h.podAPI.URL+path, nil)
	if err != nil {
		p.h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ticket)
	resp, err := p.client.Do(req)
	if err != nil {
		p.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func (f *fakeMultica) agentCallsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.agentCalls...)
}

// The agent's multica CLI talks to the same server address as the daemon, which
// in a pod is the manager, and authenticates with a task token minted by the
// server. The manager must relay that traffic unchanged (it never sees the
// workspace PAT on it), or no agent can read its issue or post a result.
func TestAgentCLITrafficWithATaskTokenReachesTheServerUnchanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	if status := pod.callWithToken("mat_task_token", http.MethodGet, "/api/issues/abc"); status != http.StatusOK {
		t.Fatalf("agent request: status %d", status)
	}
	got := h.upstream.agentCallsSeen()
	if len(got) != 1 || got[0] != "GET /api/issues/abc Bearer mat_task_token" {
		t.Fatalf("server saw %q, want the request with the agent's own token", got)
	}
}

// Anything else on those paths is still refused: the daemon's placeholder token
// proves nothing, and a caller that is not a registered pod is no agent.
func TestAgentCLITrafficWithoutATaskTokenOrFromAStrangerIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	stranger := h.newPod("127.0.0.3", "stranger") // never registered

	if status := pod.callWithToken(podPlaceholder, http.MethodGet, "/api/issues/abc"); status != http.StatusForbidden {
		t.Errorf("placeholder token on an agent path: status %d, want 403", status)
	}
	if status := stranger.callWithToken("mat_task_token", http.MethodGet, "/api/issues/abc"); status != http.StatusForbidden {
		t.Errorf("unregistered caller with a task token: status %d, want 403", status)
	}
	if got := h.upstream.agentCallsSeen(); len(got) != 0 {
		t.Errorf("refused requests reached the server: %q", got)
	}
}
