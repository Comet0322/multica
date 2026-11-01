package gitgateway_test

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

// TestRealGitLabAccessMatrix checks the cases GitLab 19.5 answers in ways the
// gateway has to interpret: a Guest, a blocked user, a Developer who only has
// the role through the project's group, and a project that is archived or has
// merge requests turned off. It is skipped unless these variables are set:
//
//	GITGATEWAY_IT_GITLAB_URL            e.g. http://localhost:8929
//	GITGATEWAY_IT_ADMIN_TOKEN           administrator token
//	GITGATEWAY_IT_PROJECT               project the users below are members of
//	GITGATEWAY_IT_GUEST_EMAIL           Guest on it
//	GITGATEWAY_IT_BLOCKED_EMAIL         blocked user who is Developer on it
//	GITGATEWAY_IT_GROUP_DEV_EMAIL       Developer only through the project's group
//	GITGATEWAY_IT_ARCHIVED_PROJECT      archived project; GITGATEWAY_IT_DEVELOPER_EMAIL is Developer
//	GITGATEWAY_IT_NO_MR_PROJECT         project with merge requests disabled; same Developer
//	GITGATEWAY_IT_DEVELOPER_EMAIL       Developer on all three projects
func TestRealGitLabAccessMatrix(t *testing.T) {
	env := func(name string) string {
		v := os.Getenv("GITGATEWAY_IT_" + name)
		if v == "" {
			t.Skip("GITGATEWAY_IT_" + name + " not set")
		}
		return v
	}
	url, ticket, proj := env("GITLAB_URL"), env("ADMIN_TOKEN"), env("PROJECT")
	guest, blocked, groupDev := env("GUEST_EMAIL"), env("BLOCKED_EMAIL"), env("GROUP_DEV_EMAIL")
	dev, archived, noMR := env("DEVELOPER_EMAIL"), env("ARCHIVED_PROJECT"), env("NO_MR_PROJECT")
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
	mcp := httptest.NewServer(g.MCPHandler())
	defer mcp.Close()

	clone := func(email, project string) error {
		tok, _ := reg.Issue(gitgateway.Identity{TaskID: "it-matrix", Initiator: email})
		remote := "http://task:" + tok + "@" + srv.Listener.Addr().String() + "/" + project + ".git"
		cmd := exec.Command("git", "clone", "--quiet", remote, filepath.Join(t.TempDir(), "out"))
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		t.Logf("clone %s as %s: %v %s", project, email, err, out)
		return err
	}
	if err := clone(groupDev, proj); err != nil {
		t.Errorf("a Developer through the group cannot clone: %v", err)
	}
	if err := clone(guest, proj); err == nil {
		t.Error("a Guest cloned")
	}
	if err := clone(blocked, proj); err == nil {
		t.Error("a blocked user cloned")
	}

	tok, _ := reg.Issue(gitgateway.Identity{TaskID: "it-matrix", Initiator: dev})
	for _, p := range []string{archived, noMR} {
		text, isErr := call(t, mcp, tok, "create_merge_request", map[string]any{"project": p, "source_branch": "agent/it-matrix/x", "title": "no"})
		if !isErr {
			t.Errorf("merge request opened on %s: %q", p, text)
		}
		t.Logf("create_merge_request on %s: %s", p, text)
	}
}
