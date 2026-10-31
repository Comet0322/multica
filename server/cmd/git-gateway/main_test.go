package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

func lookup(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func baseEnv() map[string]string {
	return map[string]string{
		"GIT_GATEWAY_GITLAB_URL":   "http://gitlab.internal",
		"GIT_GATEWAY_GITLAB_TOKEN": "glpat-secret-token",
		"GIT_GATEWAY_VERIFY_URL":   "http://manager:8791/v1/git/verify-token",
	}
}

func TestLoadReadsPolicySettings(t *testing.T) {
	env := baseEnv()
	env["GIT_GATEWAY_BRANCH_PREFIX"] = "bot/"
	env["GIT_GATEWAY_MIN_READ_LEVEL"] = "guest"
	env["GIT_GATEWAY_MIN_PUSH_LEVEL"] = "40"
	env["GIT_GATEWAY_MR_TARGETS"] = " main, release ,,"
	env["GIT_GATEWAY_MR_LABEL"] = "from-agent"
	env["GIT_GATEWAY_TASK_URL"] = "https://multica.example/{workspace_id}/tasks/{task_id}"
	cfg, _, err := load(lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BranchPrefix != "bot/" || cfg.MinReadLevel != 10 || cfg.MinPushLevel != 40 ||
		!reflect.DeepEqual(cfg.MRTargets, []string{"main", "release"}) || cfg.MRLabel != "from-agent" ||
		cfg.TaskURL != "https://multica.example/{workspace_id}/tasks/{task_id}" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadLeavesPolicyDefaultsUnset(t *testing.T) {
	cfg, _, err := load(lookup(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BranchPrefix != "" || cfg.MinReadLevel != 0 || cfg.MinPushLevel != 0 || len(cfg.MRTargets) != 0 || cfg.MRLabel != "" || cfg.TaskURL != "" {
		t.Fatalf("defaults must stay with the gateway: %+v", cfg)
	}
}

func TestLoadRefusesBadPolicySettings(t *testing.T) {
	cases := map[string]struct {
		set  map[string]string
		want string
	}{
		"level word":       {map[string]string{"GIT_GATEWAY_MIN_READ_LEVEL": "admin"}, "GIT_GATEWAY_MIN_READ_LEVEL"},
		"level number":     {map[string]string{"GIT_GATEWAY_MIN_PUSH_LEVEL": "35"}, "GIT_GATEWAY_MIN_PUSH_LEVEL"},
		"push below read":  {map[string]string{"GIT_GATEWAY_MIN_READ_LEVEL": "40", "GIT_GATEWAY_MIN_PUSH_LEVEL": "30"}, "must not be lower"},
		"prefix slash":     {map[string]string{"GIT_GATEWAY_BRANCH_PREFIX": "/agent/"}, "GIT_GATEWAY_BRANCH_PREFIX"},
		"prefix dots":      {map[string]string{"GIT_GATEWAY_BRANCH_PREFIX": "a..b/"}, "GIT_GATEWAY_BRANCH_PREFIX"},
		"label commas":     {map[string]string{"GIT_GATEWAY_MR_LABEL": "a,b"}, "GIT_GATEWAY_MR_LABEL"},
		"task url scheme":  {map[string]string{"GIT_GATEWAY_TASK_URL": "ftp://x/{task_id}"}, "GIT_GATEWAY_TASK_URL"},
		"task url unknown": {map[string]string{"GIT_GATEWAY_TASK_URL": "https://x/{issue_id}"}, "GIT_GATEWAY_TASK_URL"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := baseEnv()
			for k, v := range tc.set {
				env[k] = v
			}
			_, _, err := load(lookup(env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "glpat-secret-token") {
				t.Fatalf("error leaks the admin token: %v", err)
			}
		})
	}
}

func TestHandlerServesMCPAtMCPPathAndGitElsewhere(t *testing.T) {
	cfg, _, err := load(lookup(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	g, err := gitgateway.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := handler(g)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("/mcp should be the MCP handler (POST only), got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("/mcp without a token: got %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/group/project.git/info/refs?service=git-upload-pack", nil))
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("git path should reach the git handler, got %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}
