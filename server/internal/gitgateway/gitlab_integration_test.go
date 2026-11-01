package gitgateway_test

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

// TestRealGitLabClone runs a real git client against a real GitLab through the
// gateway. It is skipped unless these variables are set:
//
//	GITGATEWAY_IT_GITLAB_URL        e.g. http://localhost:8929
//	GITGATEWAY_IT_ADMIN_TOKEN       administrator token
//	GITGATEWAY_IT_PROJECT           existing project path, e.g. group/proj
//	GITGATEWAY_IT_REPORTER_EMAIL    user with at least Reporter on it
//	GITGATEWAY_IT_OUTSIDER_EMAIL    active user without access to it
func TestRealGitLabClone(t *testing.T) {
	env := func(name string) string {
		v := os.Getenv("GITGATEWAY_IT_" + name)
		if v == "" {
			t.Skip("GITGATEWAY_IT_" + name + " not set")
		}
		return v
	}
	url, ticket, proj := env("GITLAB_URL"), env("ADMIN_TOKEN"), env("PROJECT")
	reporter, outsider := env("REPORTER_EMAIL"), env("OUTSIDER_EMAIL")
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

	clone := func(email string) error {
		tok, _ := reg.Issue(gitgateway.Identity{TaskID: "it-" + email, Initiator: email})
		remote := "http://task:" + tok + "@" + srv.Listener.Addr().String() + "/" + proj + ".git"
		cmd := exec.Command("git", "clone", "--quiet", remote, filepath.Join(t.TempDir(), "out"))
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		t.Logf("clone as %s: %v %s", email, err, out)
		return err
	}
	if err := clone(reporter); err != nil {
		t.Errorf("reporter clone failed: %v", err)
	}
	if err := clone(outsider); err == nil {
		t.Error("clone by a user without access succeeded")
	}
}
