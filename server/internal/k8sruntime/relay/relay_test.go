package relay

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	daemonTok = "mul_daemon"
	podTok    = "mat_pod"
	mcpTok    = "mdt_task"
)

// newRelay returns a relay in front of an upstream that records the
// Authorization header and path of the last request it received.
func newRelay(t *testing.T) (*httptest.Server, *string, *string) {
	t.Helper()
	var gotAuth, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	r, err := New(u, daemonTok)
	if err != nil {
		t.Fatal(err)
	}
	r.Register(podTok, Task{TaskID: "T1", RuntimeID: "R1", WorkspaceID: "W1", RemoteMCPToken: mcpTok})
	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	return front, &gotAuth, &gotPath
}

func do(t *testing.T, base, method, path, token string) int {
	t.Helper()
	req, _ := http.NewRequest(method, base+path, strings.NewReader("{}"))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestDaemonCallsSwapInDaemonCredential(t *testing.T) {
	front, auth, path := newRelay(t)
	cases := []struct{ method, path string }{
		{"POST", "/api/daemon/tasks/T1/start"},
		{"POST", "/api/daemon/tasks/T1/messages"},
		{"POST", "/api/daemon/tasks/T1/complete"},
		{"POST", "/api/daemon/tasks/T1/fail"},
		{"POST", "/api/daemon/tasks/T1/cancel-ack"},
		{"GET", "/api/daemon/tasks/T1/status"},
		{"POST", "/api/daemon/tasks/T1/supplements/claim"},
		{"POST", "/api/daemon/tasks/T1/supplements/C9/ack"},
		{"POST", "/api/daemon/runtimes/R1/tasks/T1/prepare-lease"},
		{"POST", "/api/daemon/runtimes/R1/tasks/T1/skill-bundles/resolve"},
		{"GET", "/api/daemon/workspaces/W1/repos"},
	}
	for _, c := range cases {
		if got := do(t, front.URL, c.method, c.path, podTok); got != http.StatusNoContent {
			t.Fatalf("%s %s: status %d, want 204", c.method, c.path, got)
		}
		if *auth != "Bearer "+daemonTok || *path != c.path {
			t.Fatalf("%s %s: forwarded auth=%q path=%q", c.method, c.path, *auth, *path)
		}
	}
}

func TestMCPCallbacksUsePerClaimToken(t *testing.T) {
	front, auth, _ := newRelay(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/daemon/tasks/T1/plugin-hooks"},
		{"GET", "/api/daemon/tasks/T1/plugin-mcp/X/credential"},
	} {
		if got := do(t, front.URL, c.method, c.path, podTok); got != http.StatusNoContent {
			t.Fatalf("%s %s: status %d", c.method, c.path, got)
		}
		if *auth != "Bearer "+mcpTok {
			t.Fatalf("%s %s: forwarded auth=%q, want the per-claim token", c.method, c.path, *auth)
		}
	}
}

func TestDaemonCallsOutsideTaskScopeAreDenied(t *testing.T) {
	front, _, _ := newRelay(t)
	denied := []struct{ method, path string }{
		{"POST", "/api/daemon/tasks/OTHER/complete"},                  // other task
		{"POST", "/api/daemon/runtimes/R2/tasks/T1/prepare-lease"},    // other runtime
		{"POST", "/api/daemon/runtimes/R1/tasks/OTHER/prepare-lease"}, // other task
		{"GET", "/api/daemon/workspaces/W2/repos"},                    // other workspace
		{"POST", "/api/daemon/register"},                              // not on the allowlist
		{"POST", "/api/daemon/heartbeat"},                             // not on the allowlist
		{"POST", "/api/daemon/tasks/claim"},                           // would let a pod claim work
		{"POST", "/api/daemon/runtimes/R1/recover-orphans"},           // fails every task of the runtime
		{"POST", "/api/daemon/tasks/T1/wait-local-directory"},         // unsupported in a sandbox
		{"GET", "/api/daemon/tasks/T1/start"},                         // wrong method
		{"POST", "/api/daemon/tasks/T1/../OTHER/complete"},            // traversal
		{"POST", "/api/daemon/tasks/T1/plugin-hooks/../../../x"},      // traversal
	}
	for _, c := range denied {
		if got := do(t, front.URL, c.method, c.path, podTok); got != http.StatusForbidden {
			t.Errorf("%s %s: status %d, want 403", c.method, c.path, got)
		}
	}
}

func TestUserAPICallsForwardedWithTaskToken(t *testing.T) {
	front, auth, path := newRelay(t)
	if got := do(t, front.URL, "GET", "/api/issues/abc", podTok); got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	if *auth != "Bearer "+podTok || *path != "/api/issues/abc" {
		t.Fatalf("user API call must keep the task token: auth=%q path=%q", *auth, *path)
	}
}

func TestUnknownOrMissingTokenRejected(t *testing.T) {
	front, _, _ := newRelay(t)
	for _, tok := range []string{"", "mat_unknown", daemonTok} {
		if got := do(t, front.URL, "GET", "/api/issues/abc", tok); got != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", tok, got)
		}
	}
}

func TestUnregisterRevokes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	r, _ := New(u, daemonTok)
	r.Register(podTok, Task{TaskID: "T1"})
	front := httptest.NewServer(r)
	defer front.Close()
	if got := do(t, front.URL, "GET", "/api/x", podTok); got != http.StatusOK {
		t.Fatalf("before revoke: %d", got)
	}
	r.Unregister(podTok)
	if got := do(t, front.URL, "GET", "/api/x", podTok); got != http.StatusUnauthorized {
		t.Fatalf("after revoke: %d, want 401", got)
	}
}
