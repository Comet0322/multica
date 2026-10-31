package gitgateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

// TestRealGitLabCreateMR pushes a branch through the gateway, opens a merge
// request with the MCP tool and reads it back from a real GitLab. It is
// skipped unless these variables are set:
//
//	GITGATEWAY_IT_GITLAB_URL        e.g. http://localhost:8929
//	GITGATEWAY_IT_ADMIN_TOKEN       administrator token
//	GITGATEWAY_IT_PROJECT           existing project path with at least one commit
//	GITGATEWAY_IT_DEVELOPER_EMAIL   user with at least Developer on it
//
// The merge request is closed and the branch deleted again at the end.
func TestRealGitLabCreateMR(t *testing.T) {
	env := func(name string) string {
		v := os.Getenv("GITGATEWAY_IT_" + name)
		if v == "" {
			t.Skip("GITGATEWAY_IT_" + name + " not set")
		}
		return v
	}
	base, token, proj, dev := strings.TrimRight(env("GITLAB_URL"), "/"), env("ADMIN_TOKEN"), env("PROJECT"), env("DEVELOPER_EMAIL")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	reg := gitgateway.NewTokenRegistry()
	g, err := gitgateway.New(gitgateway.Config{
		GitLabURL: base, AdminToken: token, Resolver: reg,
		TaskURL: "https://multica.example/tasks/{task_id}",
	})
	if err != nil {
		t.Fatal(err)
	}
	gitSrv := httptest.NewServer(g)
	defer gitSrv.Close()
	mcpSrv := httptest.NewServer(g.MCPHandler())
	defer mcpSrv.Close()

	const taskID = "it-mr"
	tok, _ := reg.Issue(gitgateway.Identity{TaskID: taskID, Initiator: dev})
	remote := "http://task:" + tok + "@" + gitSrv.Listener.Addr().String() + "/" + proj + ".git"
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "HOME="+dir,
			"GIT_AUTHOR_NAME=it", "GIT_AUTHOR_EMAIL=it@example.com", "GIT_COMMITTER_NAME=it", "GIT_COMMITTER_EMAIL=it@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("clone", "--quiet", remote, "work")
	dir += "/work"
	branch := "agent/" + taskID + "/gateway-mr-test"
	git("commit", "--allow-empty", "-q", "-m", "gateway merge request test")
	git("push", remote, "HEAD:refs/heads/"+branch)
	defer git("push", remote, ":refs/heads/"+branch)

	api := func(method, path string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(method, base+"/api/v4"+path, nil)
		req.Header.Set("PRIVATE-TOKEN", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	// Not the default branch, not another task's branch.
	if text, isErr := call(t, mcpSrv, tok, "create_merge_request", map[string]any{"project": proj, "source_branch": "agent/other/x", "title": "no"}); !isErr {
		t.Fatalf("unpushed branch accepted: %q", text)
	}

	text, isErr := call(t, mcpSrv, tok, "create_merge_request", map[string]any{
		"project": proj, "source_branch": branch, "title": "Gateway MR test", "description": "From the integration test.",
	})
	if isErr {
		t.Fatalf("create failed: %q", text)
	}
	var mr struct {
		IID int `json:"iid"`
	}
	if err := json.Unmarshal([]byte(text), &mr); err != nil || mr.IID == 0 {
		t.Fatalf("no iid in %q", text)
	}
	mrPath := "/projects/" + url.PathEscape(proj) + "/merge_requests/" + itoa(int64(mr.IID))
	defer api("PUT", mrPath+"?state_event=close")

	got := api("GET", mrPath)
	desc, _ := got["description"].(string)
	for _, want := range []string{"From the integration test.", dev, "https://multica.example/tasks/" + taskID} {
		if !strings.Contains(desc, want) {
			t.Errorf("description on GitLab lacks %q:\n%s", want, desc)
		}
	}
	labels, _ := got["labels"].([]any)
	if len(labels) != 1 || labels[0] != "created-by-agent" {
		t.Errorf("labels on GitLab = %v", got["labels"])
	}
}
