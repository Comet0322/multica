package sandboxmanager_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

func withGateway(mutate ...func(*sandboxmanager.GitGatewayConfig)) func(*sandboxmanager.PoolConfig) {
	return func(c *sandboxmanager.PoolConfig) {
		cfg := &sandboxmanager.GitGatewayConfig{URL: "http://git-gateway.pool.svc:8081"}
		for _, m := range mutate {
			m(cfg)
		}
		c.Git = cfg
	}
}

func (h *harness) adminPost(path string, body any) (int, []byte) {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(h.admin.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func (h *harness) verifyToken(ticket string) int {
	status, _ := h.adminPost("/v1/git/verify-token", map[string]string{"token": ticket})
	return status
}

func (h *harness) verifyPod(ip string) []gitgateway.Identity {
	h.t.Helper()
	status, raw := h.adminPost("/v1/git/verify-pod", map[string]string{"ip": ip})
	if status != http.StatusOK {
		h.t.Fatalf("verify-pod status %d", status)
	}
	var out struct {
		Identities []gitgateway.Identity `json:"identities"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatal(err)
	}
	return out.Identities
}

func TestGitTokenIsInjectedForcedAndVerifiableByTheGateway(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withGateway())
	h.upstream.enqueueTask("t1", "Alice@Example.com", map[string]string{"GIT_GATEWAY_TOKEN": "forged", "KEEP_ME": "yes"})
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	task := pod.claimTask()
	env := envOf(t, task)
	ticket := env["GIT_GATEWAY_TOKEN"]
	if !strings.HasPrefix(ticket, "gwt_") || env["KEEP_ME"] != "yes" {
		t.Fatalf("token not forced over the agent's env: %v", env)
	}
	for name := range env {
		if hasPrefixFold(name, "MULTICA_") {
			t.Fatalf("injected variable %s starts with MULTICA_", name)
		}
	}
	status, raw := h.adminPost("/v1/git/verify-token", map[string]string{"token": ticket})
	var id gitgateway.Identity
	if status != http.StatusOK || json.Unmarshal(raw, &id) != nil || id.TaskID != "t1" || id.Initiator != "alice@example.com" || id.WorkspaceID != harnessWorkspaceID {
		t.Fatalf("identity = %+v (%d)", id, status)
	}
	if strings.Contains(h.logs.String(), ticket) || strings.Contains(h.audit.String(), ticket) {
		t.Fatal("the git token leaked into logs or the audit trail")
	}
}

func TestGitTokenTemplateUsesPlaceholders(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withGateway(func(c *sandboxmanager.GitGatewayConfig) {
		c.Env = map[string]string{
			"GIT_GW_URL":  sandboxmanager.GitURLPlaceholder,
			"GIT_GW_AUTH": "task:" + sandboxmanager.GitAuthPlaceholder,
		}
	}))
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	env := envOf(t, pod.claimTask())
	if env["GIT_GW_URL"] != "http://git-gateway.pool.svc:8081" || !strings.HasPrefix(env["GIT_GW_AUTH"], "task:gwt_") {
		t.Fatalf("template not applied: %v", env)
	}
}

func TestGitTokenIsRevokedWhenTheTaskCompletesOrFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, withGateway())
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	h.upstream.enqueueTask("t2", "bob@example.com", nil)
	a := h.newPod("127.0.0.2", "pool-0")
	b := h.newPod("127.0.0.3", "pool-1")
	a.start()
	b.start()
	t1 := envOf(t, a.claimTask())["GIT_GATEWAY_TOKEN"]
	t2 := envOf(t, b.claimTask())["GIT_GATEWAY_TOKEN"]
	if t1 == t2 || h.verifyToken(t1) != http.StatusOK || h.verifyToken(t2) != http.StatusOK {
		t.Fatal("each task needs its own live token")
	}

	a.report("t1", "complete")
	b.report("t2", "fail")
	if h.verifyToken(t1) != http.StatusNotFound || h.verifyToken(t2) != http.StatusNotFound {
		t.Fatal("tokens must die with their leases")
	}
}

func TestGitTokenIsRevokedWhenTheLeasePodIsReplaced(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withGateway())
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	pod.start()
	ticket := envOf(t, pod.claimTask())["GIT_GATEWAY_TOKEN"]

	pod.nonce = "boot-2"
	pod.register()
	if h.verifyToken(ticket) != http.StatusNotFound {
		t.Fatal("token survived the pod being replaced")
	}
}

func TestPodAddressResolvesToTheLeasedInitiatorOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, withGateway())
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	a := h.newPod("127.0.0.2", "pool-0")
	idle := h.newPod("127.0.0.3", "pool-1")
	idle.start()
	if got := h.verifyPod("127.0.0.3"); len(got) != 0 {
		t.Fatalf("an idle pod resolved to %v", got)
	}
	a.start()
	a.claimTask()

	got := h.verifyPod("127.0.0.2")
	if len(got) != 1 || got[0].TaskID != "t1" || got[0].Initiator != "alice@example.com" {
		t.Fatalf("leased pod resolved to %v", got)
	}
	if got := h.verifyPod("127.0.0.3"); len(got) != 0 {
		t.Fatalf("the other pod resolved to %v", got)
	}
	if got := h.verifyPod("10.9.9.9"); len(got) != 0 {
		t.Fatalf("an unknown address resolved to %v", got)
	}

	a.report("t1", "complete")
	if got := h.verifyPod("127.0.0.2"); len(got) != 0 {
		t.Fatalf("a pod with no lease resolved to %v", got)
	}
}

func TestVerifyEndpointsAreNotReachableFromPods(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withGateway())
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	for _, path := range []string{"/v1/git/verify-token", "/v1/git/verify-pod"} {
		status, _ := pod.call(http.MethodPost, path, map[string]string{"token": "x", "ip": "127.0.0.2"})
		if status != http.StatusForbidden {
			t.Fatalf("%s from a pod = %d, want 403", path, status)
		}
	}
}

func TestWithoutGatewayConfiguredTasksAndAdminAreUntouched(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueueTask("t1", "alice@example.com", map[string]string{"KEEP_ME": "yes"})
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	env := envOf(t, pod.claimTask())
	if _, ok := env["GIT_GATEWAY_TOKEN"]; ok || env["KEEP_ME"] != "yes" {
		t.Fatalf("env = %v", env)
	}
	if status, _ := h.adminPost("/v1/git/verify-pod", map[string]string{"ip": "127.0.0.2"}); status == http.StatusOK {
		t.Fatal("verify endpoints must not exist without a gateway configuration")
	}
}

func TestMulticaPrefixedGitEnvNamesAreRejected(t *testing.T) {
	t.Parallel()
	_, err := sandboxmanager.NewPool(sandboxmanager.PoolConfig{
		MulticaURL: "http://127.0.0.1:1", MulticaTicket: harnessPAT, WorkspaceID: harnessWorkspaceID,
		Git: &sandboxmanager.GitGatewayConfig{Env: map[string]string{"MULTICA_TOKEN": sandboxmanager.GitAuthPlaceholder}},
	}, nil)
	if err == nil {
		t.Fatal("want an error for a MULTICA_ variable name")
	}
}

// gatewayMCP reads the gitlab-gateway server out of a claimed task's mcp_config.
func gatewayMCP(task map[string]any) (url, auth string, ok bool) {
	agent, _ := task["agent"].(map[string]any)
	cfg, _ := agent["mcp_config"].(map[string]any)
	servers, _ := cfg["mcpServers"].(map[string]any)
	entry, ok := servers[gitgateway.MCPServerName].(map[string]any)
	if !ok {
		return "", "", false
	}
	headers, _ := entry["headers"].(map[string]any)
	url, _ = entry["url"].(string)
	auth, _ = headers["Authorization"].(string)
	return url, auth, entry["type"] == "http"
}

func TestClaimInjectsTheGatewayMCPServerWithTheTaskToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withGateway(func(c *sandboxmanager.GitGatewayConfig) {
		c.MCPURL = "http://git-gateway.pool.svc:8081/mcp"
	}))
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	task := pod.claimTask()
	url, auth, ok := gatewayMCP(task)
	if !ok || url != "http://git-gateway.pool.svc:8081/mcp" {
		t.Fatalf("gateway MCP entry = %q ok=%v in %v", url, ok, task["agent"])
	}
	ticket := envOf(t, task)["GIT_GATEWAY_TOKEN"]
	if auth != "Bearer "+ticket {
		t.Fatalf("MCP authorization does not carry the task's git token")
	}
	var id gitgateway.Identity
	status, raw := h.adminPost("/v1/git/verify-token", map[string]string{"token": strings.TrimPrefix(auth, "Bearer ")})
	if status != http.StatusOK || json.Unmarshal(raw, &id) != nil || id.TaskID != "t1" {
		t.Fatalf("MCP token identity = %+v (%d)", id, status)
	}
	if strings.Contains(h.logs.String(), ticket) || strings.Contains(h.audit.String(), ticket) {
		t.Fatal("the git token leaked into logs or the audit trail")
	}
}

func TestClaimWithoutMCPURLLeavesMCPConfigAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withGateway())
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	task := pod.claimTask()
	if _, _, ok := gatewayMCP(task); ok {
		t.Fatalf("gateway MCP entry injected without an MCP URL: %v", task["agent"])
	}
	if agent, _ := task["agent"].(map[string]any); agent["mcp_config"] != nil {
		t.Fatalf("mcp_config touched: %v", agent["mcp_config"])
	}
}

func TestMCPURLMustBeAnHTTPURL(t *testing.T) {
	t.Parallel()
	_, err := sandboxmanager.NewPool(sandboxmanager.PoolConfig{
		MulticaURL: "http://127.0.0.1:1", MulticaTicket: harnessPAT, WorkspaceID: harnessWorkspaceID,
		Git: &sandboxmanager.GitGatewayConfig{MCPURL: "git-gateway/mcp"},
	}, nil)
	if err == nil {
		t.Fatal("a non-URL MCP address was accepted")
	}
}
