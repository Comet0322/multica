package gitgateway_test

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

// TestRealGitLabPush pushes with a real git client through the gateway to a
// real GitLab. It is skipped unless these variables are set:
//
//	GITGATEWAY_IT_GITLAB_URL        e.g. http://localhost:8929
//	GITGATEWAY_IT_ADMIN_TOKEN       administrator token
//	GITGATEWAY_IT_PROJECT           existing project path with at least one commit
//	GITGATEWAY_IT_DEVELOPER_EMAIL   user with at least Developer on it
//
// The branch the task creates is deleted again at the end.
func TestRealGitLabPush(t *testing.T) {
	env := func(name string) string {
		v := os.Getenv("GITGATEWAY_IT_" + name)
		if v == "" {
			t.Skip("GITGATEWAY_IT_" + name + " not set")
		}
		return v
	}
	url, ticket, proj, dev := env("GITLAB_URL"), env("ADMIN_TOKEN"), env("PROJECT"), env("DEVELOPER_EMAIL")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	reg := gitgateway.NewTokenRegistry()
	g, err := gitgateway.New(gitgateway.Config{GitLabURL: url, AdminTicket: ticket, Resolver: reg})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	defer srv.Close()

	const taskID = "it-push"
	tok, _ := reg.Issue(gitgateway.Identity{TaskID: taskID, Initiator: dev})
	remote := "http://task:" + tok + "@" + srv.Listener.Addr().String() + "/" + proj + ".git"
	dir := t.TempDir()
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "HOME="+dir,
			"GIT_AUTHOR_NAME=it", "GIT_AUTHOR_EMAIL=it@example.com", "GIT_COMMITTER_NAME=it", "GIT_COMMITTER_EMAIL=it@example.com")
		out, err := cmd.CombinedOutput()
		t.Logf("git %s: %v\n%s", strings.Join(args[:1], " "), err, out)
		return string(out), err
	}
	mustGit := func(args ...string) string {
		out, err := git(args...)
		if err != nil {
			t.Fatalf("git %v failed", args)
		}
		return out
	}

	mustGit("clone", "--quiet", remote, "work")
	dir += "/work"
	mainBefore := mustGit("ls-remote", remote, "HEAD")
	mustGit("commit", "--allow-empty", "-q", "-m", "gateway push test")

	// Default branch: stopped by the gateway, and GitLab's HEAD does not move.
	out, err := git("push", remote, "HEAD:refs/heads/main")
	if err == nil || !strings.Contains(out, "outside the task branch namespace") {
		t.Fatalf("push to the default branch was not refused by the gateway: %v", err)
	}
	if after := mustGit("ls-remote", remote, "HEAD"); after != mainBefore {
		t.Fatalf("default branch moved: %q -> %q", mainBefore, after)
	}

	// A branch inside the task namespace goes through.
	branch := "refs/heads/agent/" + taskID + "/gateway-push-test"
	mustGit("push", remote, "HEAD:"+branch)
	if out := mustGit("ls-remote", remote, branch); !strings.Contains(out, branch) {
		t.Fatalf("pushed branch not visible: %q", out)
	}
	mustGit("push", remote, ":"+branch)
}
