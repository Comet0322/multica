package gitgateway_test

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

// TestRealGitLabDaemonCloneByAddress clones through the gateway with no task
// token, the way the daemon does: a client rewritten by url.<gateway>.insteadOf
// whose source address holds a lease. It uses the same GITGATEWAY_IT_*
// variables as TestRealGitLabClone and is skipped without them.
func TestRealGitLabDaemonCloneByAddress(t *testing.T) {
	env := func(name string) string {
		v := os.Getenv("GITGATEWAY_IT_" + name)
		if v == "" {
			t.Skip("GITGATEWAY_IT_" + name + " not set")
		}
		return v
	}
	url, token, proj := env("GITLAB_URL"), env("ADMIN_TOKEN"), env("PROJECT")
	reporter, outsider := env("REPORTER_EMAIL"), env("OUTSIDER_EMAIL")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	pods := &fakePods{}
	g, err := gitgateway.New(gitgateway.Config{
		GitLabURL: url, AdminToken: token, Resolver: gitgateway.NewTokenRegistry(), PodResolver: pods,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	defer srv.Close()

	clone := func(email string) error {
		pods.set("127.0.0.1", gitgateway.Identity{TaskID: "it-" + email, Initiator: email})
		cmd := exec.CommandContext(context.Background(), "git",
			"-c", "url."+srv.URL+"/.insteadOf="+url+"/",
			"clone", "--quiet", url+"/"+proj+".git", filepath.Join(t.TempDir(), "out"))
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		t.Logf("clone as %s: %v %s", email, err, out)
		return err
	}
	if err := clone(reporter); err != nil {
		t.Errorf("daemon clone for a reporter failed: %v", err)
	}
	if err := clone(outsider); err == nil {
		t.Error("daemon clone for a user without access succeeded")
	}
}
