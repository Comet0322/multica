package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

func envLookup(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func baseEnv() map[string]string {
	return map[string]string{
		"SANDBOX_POOL_TOKEN": "mul_pat_pool",
		"MULTICA_SERVER_URL": "http://multica:8080",
	}
}

func TestLoadPoolReadsPlacementLifetimeAndAudit(t *testing.T) {
	env := baseEnv()
	env["SANDBOX_POOL_HEARTBEAT_TIMEOUT"] = "30s"
	env["SANDBOX_POOL_RESET_TIMEOUT"] = "3m"
	env["SANDBOX_POOL_LOST_RETENTION"] = "7m"
	env["SANDBOX_PLACEMENT"] = "issue"
	env["SANDBOX_POD_CONCURRENCY"] = "8"
	env["SANDBOX_PLACEMENT_WAIT"] = "20s"
	env["SANDBOX_POD_IDLE_TIMEOUT"] = "5m"
	env["SANDBOX_POD_MAX_LIFETIME"] = "1h"
	env["SANDBOX_POD_DRAIN_GRACE"] = "2m"
	env["SANDBOX_AUDIT_FILE"] = "/var/log/audit.jsonl"
	s, err := loadPool(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	c := s.config
	if c.HeartbeatTimeout != 30*time.Second || c.ResetTimeout != 3*time.Minute || c.LostRetention != 7*time.Minute || c.Placement != sandboxmanager.PlacementIssue ||
		c.PodConcurrency != 8 || c.PlacementWait != 20*time.Second || c.PodIdleTimeout != 5*time.Minute ||
		c.PodMaxLifetime != time.Hour || c.PodDrainGrace != 2*time.Minute || s.auditFile != "/var/log/audit.jsonl" {
		t.Fatalf("unexpected settings: %+v", s)
	}
	if c.LiteLLM != nil || c.Git != nil {
		t.Fatal("LiteLLM and git gateway stay off unless their URL is set")
	}
}

func TestLoadPoolReadsLiteLLM(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("sk-master\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := baseEnv()
	env["SANDBOX_LITELLM_URL"] = "http://litellm:4000"
	env["SANDBOX_LITELLM_POD_URL"] = "http://litellm.pods:4000"
	env["SANDBOX_LITELLM_MASTER_KEY_FILE"] = keyFile
	env["SANDBOX_LITELLM_MODELS"] = "sonnet, haiku"
	env["SANDBOX_LITELLM_MAX_BUDGET"] = "2.5"
	env["SANDBOX_LITELLM_RPM_LIMIT"] = "60"
	env["SANDBOX_LITELLM_TASK_TIMEOUT"] = "1h"
	env["SANDBOX_LITELLM_MARGIN"] = "5m"
	env["SANDBOX_LITELLM_ENV"] = "ANTHROPIC_BASE_URL=${LITELLM_URL}, ANTHROPIC_AUTH_TOKEN=${LITELLM_KEY}"
	s, err := loadPool(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	l := s.config.LiteLLM
	if l == nil || l.URL != "http://litellm:4000" || l.PodURL != "http://litellm.pods:4000" || l.MasterKey != "sk-master" ||
		len(l.Models) != 2 || l.MaxBudget != 2.5 || l.RPMLimit != 60 || l.TaskTimeout != time.Hour || l.Margin != 5*time.Minute ||
		l.Env["ANTHROPIC_AUTH_TOKEN"] != "${LITELLM_KEY}" || len(l.Env) != 2 {
		t.Fatalf("unexpected LiteLLM settings: %+v", l)
	}
}

func TestLoadPoolReadsGitGateway(t *testing.T) {
	env := baseEnv()
	env["SANDBOX_GIT_GATEWAY_URL"] = "http://git-gateway:8081"
	env["SANDBOX_GIT_GATEWAY_ENV"] = "GIT_GATEWAY_TOKEN=${GIT_TOKEN},GIT_BASE=${GIT_GATEWAY_URL}"
	s, err := loadPool(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	g := s.config.Git
	if g == nil || g.URL != "http://git-gateway:8081" || g.Env["GIT_BASE"] != "${GIT_GATEWAY_URL}" || len(g.Env) != 2 {
		t.Fatalf("unexpected git gateway settings: %+v", g)
	}
}

func TestLoadPoolReadsGitGatewayMCPURL(t *testing.T) {
	env := baseEnv()
	env["SANDBOX_GIT_GATEWAY_URL"] = "http://git-gateway:8081"
	env["SANDBOX_GIT_GATEWAY_MCP_URL"] = "http://git-gateway:8081/mcp"
	s, err := loadPool(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if g := s.config.Git; g == nil || g.MCPURL != "http://git-gateway:8081/mcp" {
		t.Fatalf("unexpected git gateway settings: %+v", g)
	}
}

func TestLoadPoolRefusesBadGitGatewayMCPURL(t *testing.T) {
	for name, set := range map[string]map[string]string{
		"without gateway URL": {"SANDBOX_GIT_GATEWAY_MCP_URL": "http://g:8081/mcp"},
		"not http":            {"SANDBOX_GIT_GATEWAY_URL": "http://g:8081", "SANDBOX_GIT_GATEWAY_MCP_URL": "ftp://g/mcp"},
		"no host":             {"SANDBOX_GIT_GATEWAY_URL": "http://g:8081", "SANDBOX_GIT_GATEWAY_MCP_URL": "/mcp"},
		"credentials":         {"SANDBOX_GIT_GATEWAY_URL": "http://g:8081", "SANDBOX_GIT_GATEWAY_MCP_URL": "http://u:p@g/mcp"},
	} {
		t.Run(name, func(t *testing.T) {
			env := baseEnv()
			for k, v := range set {
				env[k] = v
			}
			_, err := loadPool(envLookup(env))
			if err == nil || !strings.Contains(err.Error(), "SANDBOX_GIT_GATEWAY_MCP_URL") || strings.Contains(err.Error(), "u:p") {
				t.Fatalf("want an error naming SANDBOX_GIT_GATEWAY_MCP_URL without the value, got %v", err)
			}
		})
	}
}

func TestLoadPoolRefusesBadSettings(t *testing.T) {
	cases := map[string]struct {
		set  map[string]string
		want string
	}{
		"bad duration":        {map[string]string{"SANDBOX_POD_IDLE_TIMEOUT": "soon"}, "SANDBOX_POD_IDLE_TIMEOUT"},
		"negative duration":   {map[string]string{"SANDBOX_POD_DRAIN_GRACE": "-1m"}, "SANDBOX_POD_DRAIN_GRACE"},
		"bad concurrency":     {map[string]string{"SANDBOX_POD_CONCURRENCY": "-2"}, "SANDBOX_POD_CONCURRENCY"},
		"bad placement":       {map[string]string{"SANDBOX_PLACEMENT": "pod"}, "SANDBOX_PLACEMENT"},
		"litellm orphan":      {map[string]string{"SANDBOX_LITELLM_MODELS": "x"}, "need SANDBOX_LITELLM_URL"},
		"litellm no key":      {map[string]string{"SANDBOX_LITELLM_URL": "http://l:4000", "SANDBOX_LITELLM_MODELS": "x"}, "SANDBOX_LITELLM_MASTER_KEY"},
		"litellm no models":   {map[string]string{"SANDBOX_LITELLM_URL": "http://l:4000", "SANDBOX_LITELLM_MASTER_KEY": "k"}, "SANDBOX_LITELLM_MODELS"},
		"litellm bad budget":  {map[string]string{"SANDBOX_LITELLM_MAX_BUDGET": "lots"}, "SANDBOX_LITELLM_MAX_BUDGET"},
		"litellm key twice":   {map[string]string{"SANDBOX_LITELLM_MASTER_KEY": "k", "SANDBOX_LITELLM_MASTER_KEY_FILE": "/x"}, "only one"},
		"litellm key missing": {map[string]string{"SANDBOX_LITELLM_MASTER_KEY_FILE": "/nonexistent/key"}, "SANDBOX_LITELLM_MASTER_KEY_FILE"},
		"litellm bad env":     {map[string]string{"SANDBOX_LITELLM_ENV": "NOEQUALS"}, "SANDBOX_LITELLM_ENV"},
		"git env orphan":      {map[string]string{"SANDBOX_GIT_GATEWAY_ENV": "A=b"}, "SANDBOX_GIT_GATEWAY_URL"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := baseEnv()
			for k, v := range tc.set {
				env[k] = v
			}
			_, err := loadPool(envLookup(env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadPoolErrorsNeverShowSecrets(t *testing.T) {
	env := baseEnv()
	env["SANDBOX_LITELLM_MASTER_KEY"] = "sk-super-secret"
	env["SANDBOX_LITELLM_MASTER_KEY_FILE"] = "/x"
	_, err := loadPool(envLookup(env))
	if err == nil || strings.Contains(err.Error(), "sk-super-secret") {
		t.Fatalf("error must exist and hide the key: %v", err)
	}
}

func TestStartupRejectsSettingsThePoolRefuses(t *testing.T) {
	env := baseEnv()
	env["SANDBOX_GIT_GATEWAY_URL"] = "ftp://nope"
	s, err := loadPool(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	s.podListen, s.adminListen = "127.0.0.1:0", "127.0.0.1:0"
	err = runPool(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), s)
	if err == nil || !strings.Contains(err.Error(), "git gateway URL") {
		t.Fatalf("a bad git gateway URL must stop startup, got %v", err)
	}
}

func TestAdminListenerServesGitVerifyOnlyWhenConfigured(t *testing.T) {
	status := func(git bool) int {
		t.Helper()
		env := baseEnv()
		if git {
			env["SANDBOX_GIT_GATEWAY_URL"] = "http://git-gateway:8081"
		}
		s, err := loadPool(envLookup(env))
		if err != nil {
			t.Fatal(err)
		}
		up := newFakeMultica(t, testWorkspaceA)
		s.config.MulticaURL = up.URL
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		pool, err := sandboxmanager.NewPool(s.config, log)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		podLn, _ := net.Listen("tcp", "127.0.0.1:0")
		adminLn, _ := net.Listen("tcp", "127.0.0.1:0")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- servePool(ctx, log, pool, podLn, adminLn, nil, make(chan os.Signal)) }()
		defer func() {
			cancel()
			<-done
		}()
		// A wrong method tells a mounted endpoint (405) from a missing one (404).
		const p = "/v1/git/verify-token"
		resp, err := http.Get("http://" + adminLn.Addr().String() + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		// The pod listener must never carry the endpoint.
		podResp, err := http.Post("http://"+podLn.Addr().String()+p, "application/json", strings.NewReader(`{"token":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		podResp.Body.Close()
		if podResp.StatusCode == http.StatusOK {
			t.Fatal("the pod listener served a git verify endpoint")
		}
		return resp.StatusCode
	}
	if got := status(false); got != http.StatusNotFound {
		t.Fatalf("without a git gateway the endpoint must not exist, got %d", got)
	}
	if got := status(true); got != http.StatusMethodNotAllowed {
		t.Fatalf("with a git gateway the admin listener must serve verify-token, got %d", got)
	}
}
