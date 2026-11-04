package gittools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
	"github.com/multica-ai/multica/server/pkg/harness/tools"
)

const taskID = "task-42"

func gitHost(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func branches(t *testing.T, repo string) []string {
	t.Helper()
	return strings.Fields(gitHost(t, repo, "for-each-ref", "--format=%(refname:short)", "refs/heads"))
}

type env struct {
	remote string // bare repository standing in for the hosted remote
	reg    *tools.Registry
	sbRoot string
	cfg    Config
}

// newEnv builds a remote with one commit and the tools over a local sandbox.
func newEnv(t *testing.T) *env {
	t.Helper()
	// The tools write git config; keep that out of the real home.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))

	remote := filepath.Join(t.TempDir(), "app.git")
	seed := t.TempDir()
	gitHost(t, seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "hello.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitHost(t, seed, "add", ".")
	gitHost(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-q", "-m", "initial")
	gitHost(t, filepath.Dir(remote), "clone", "-q", "--bare", seed, remote)

	root := t.TempDir()
	sb, err := sandbox.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{SB: sb, Workdir: "/work", HostDir: t.TempDir(), Allowed: []string{remote}, TaskID: taskID}
	return &env{remote: remote, reg: tools.NewRegistry(Tools(cfg)...), sbRoot: root, cfg: cfg}
}

func (e *env) run(t *testing.T, name string, in map[string]any) tools.Result {
	t.Helper()
	res, err := e.reg.Run(context.Background(), name, in)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

// inSandbox runs a command in the sandbox's clone, like the agent's bash tool.
func (e *env) inSandbox(t *testing.T, script string) {
	t.Helper()
	res, err := e.cfg.SB.Exec(context.Background(), sandbox.ExecRequest{Command: script, Cwd: "/work/app"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%s: %v %+v", script, err, res)
	}
}

func TestCloneEditDeliverRoundTrip(t *testing.T) {
	e := newEnv(t)

	if res := e.run(t, "git_clone", map[string]any{"repo": e.remote}); res.IsError {
		t.Fatalf("clone: %+v", res)
	}
	if b, err := os.ReadFile(filepath.Join(e.sbRoot, "work/app/hello.py")); err != nil || string(b) != "print('hi')\n" {
		t.Fatalf("the working tree is not in the sandbox: %q %v", b, err)
	}
	e.inSandbox(t, `echo "print('bye')" >> hello.py && git add -A && git commit -q -m "add bye"`)

	res := e.run(t, "deliver", map[string]any{"path": "app", "branch": "agent/" + taskID + "/bye"})
	if res.IsError || !strings.Contains(res.Output, "agent/task-42/bye") {
		t.Fatalf("deliver: %+v", res)
	}
	if got := gitHost(t, e.remote, "log", "-1", "--format=%s", "agent/task-42/bye"); got != "add bye" {
		t.Errorf("remote branch head = %q", got)
	}
	if got := gitHost(t, e.remote, "log", "-1", "--format=%an", "agent/task-42/bye"); got != "Multica Agent" {
		t.Errorf("commit author = %q, want the identity the clone was configured with", got)
	}
	if gitHost(t, e.remote, "rev-parse", "main") == gitHost(t, e.remote, "rev-parse", "agent/task-42/bye") {
		t.Error("main moved")
	}

	// Later work goes to the same branch.
	e.inSandbox(t, `echo "# more" >> hello.py && git commit -q -am "more"`)
	if res := e.run(t, "deliver", map[string]any{"path": "app", "branch": "agent/" + taskID + "/bye"}); res.IsError {
		t.Fatalf("second deliver: %+v", res)
	}
	if got := gitHost(t, e.remote, "rev-list", "--count", "main..agent/task-42/bye"); got != "2" {
		t.Errorf("commits on the branch = %s, want 2", got)
	}
}

func TestOnlyAllowedRepositoriesAreCloned(t *testing.T) {
	e := newEnv(t)
	for _, repo := range []string{"https://example.com/other/repo.git", "", "file:///etc", "ext::sh -c 'touch /tmp/pwned'"} {
		if res := e.run(t, "git_clone", map[string]any{"repo": repo}); !res.IsError || !strings.Contains(res.Output, "not available") {
			t.Errorf("repo %q: %+v", repo, res)
		}
	}
	for _, dest := range []string{"../x", "a/b", "..", "."} {
		if res := e.run(t, "git_clone", map[string]any{"repo": e.remote, "dest": dest}); !res.IsError {
			t.Errorf("dest %q accepted: %+v", dest, res)
		}
	}
}

func TestAllowListIgnoresSuffixAndCase(t *testing.T) {
	s := &state{Config: Config{Allowed: []string{"https://Git.Example.com/Org/Repo.git"}}}
	for _, u := range []string{"https://git.example.com/org/repo", "https://git.example.com/org/repo/", "HTTPS://GIT.EXAMPLE.COM/ORG/REPO.GIT"} {
		if !s.allowed(u) {
			t.Errorf("%q should match", u)
		}
	}
	for _, u := range []string{"https://git.example.com/org/repo2", "https://git.example.com/org", "http://git.example.com/org/repo"} {
		if s.allowed(u) {
			t.Errorf("%q should not match", u)
		}
	}
}

func TestDeliverRefusesWhatItMustNotPush(t *testing.T) {
	e := newEnv(t)
	e.run(t, "git_clone", map[string]any{"repo": e.remote})
	e.inSandbox(t, `echo x > new.txt && git add -A && git commit -q -m c1`)

	for name, in := range map[string]map[string]any{
		"main":           {"path": "app", "branch": "main"},
		"someone else's": {"path": "app", "branch": "agent/task-99/x"},
		"bare prefix":    {"path": "app", "branch": "agent/" + taskID + "/"},
		"bad ref":        {"path": "app", "branch": "agent/" + taskID + "/a..b"},
		"not a clone":    {"path": "nowhere", "branch": "agent/" + taskID + "/x"},
		"path traversal": {"path": "../app", "branch": "agent/" + taskID + "/x"},
	} {
		if res := e.run(t, "deliver", in); !res.IsError {
			t.Errorf("%s: %+v", name, res)
		}
	}
	if got := branches(t, e.remote); len(got) != 1 || got[0] != "main" {
		t.Errorf("the remote gained branches from refused deliveries: %v", got)
	}
}

func TestDeliverNeedsCommittedChanges(t *testing.T) {
	e := newEnv(t)
	e.run(t, "git_clone", map[string]any{"repo": e.remote})
	branch := "agent/" + taskID + "/x"

	if res := e.run(t, "deliver", map[string]any{"path": "app", "branch": branch}); !res.IsError || !strings.Contains(res.Output, "nothing to deliver") {
		t.Errorf("no commits: %+v", res)
	}
	e.inSandbox(t, `echo x >> hello.py`)
	if res := e.run(t, "deliver", map[string]any{"path": "app", "branch": branch}); !res.IsError || !strings.Contains(res.Output, "uncommitted") {
		t.Errorf("edited tracked file: %+v", res)
	}
}

func TestUntrackedFilesDoNotBlockADelivery(t *testing.T) {
	e := newEnv(t)
	e.run(t, "git_clone", map[string]any{"repo": e.remote})
	e.inSandbox(t, `echo 1 > f && git add f && git commit -q -m c && mkdir __pycache__ && echo junk > __pycache__/x.pyc`)

	branch := "agent/" + taskID + "/x"
	if res := e.run(t, "deliver", map[string]any{"path": "app", "branch": branch}); res.IsError {
		t.Fatalf("untracked cache blocked the delivery: %+v", res)
	}
	if got := gitHost(t, e.remote, "ls-tree", "-r", "--name-only", branch); strings.Contains(got, "__pycache__") {
		t.Errorf("an untracked file was delivered: %q", got)
	}
}

func TestRewrittenHistoryIsNotForcedThrough(t *testing.T) {
	e := newEnv(t)
	e.run(t, "git_clone", map[string]any{"repo": e.remote})
	branch := "agent/" + taskID + "/x"
	e.inSandbox(t, `echo 1 > f && git add -A && git commit -q -m first`)
	if res := e.run(t, "deliver", map[string]any{"path": "app", "branch": branch}); res.IsError {
		t.Fatal(res.Output)
	}
	tip := gitHost(t, e.remote, "rev-parse", branch)

	e.inSandbox(t, `git commit -q --amend -m rewritten`)
	if res := e.run(t, "deliver", map[string]any{"path": "app", "branch": branch}); !res.IsError {
		t.Errorf("a rewritten history was delivered: %+v", res)
	}
	if got := gitHost(t, e.remote, "rev-parse", branch); got != tip {
		t.Errorf("remote branch moved from %s to %s", tip, got)
	}
}

// The agent controls the sandbox, so it controls the bundle deliver reads back.
func TestACorruptBundleFromTheSandboxIsRejected(t *testing.T) {
	e := newEnv(t)
	e.cfg.SB = &tamperingSandbox{Sandbox: e.cfg.SB}
	reg := tools.NewRegistry(Tools(e.cfg)...)
	if res, _ := reg.Run(context.Background(), "git_clone", map[string]any{"repo": e.remote}); res.IsError {
		t.Fatal(res.Output)
	}
	if _, err := e.cfg.SB.Exec(context.Background(), sandbox.ExecRequest{Command: `echo 1 > f && git add -A && git commit -q -m c`, Cwd: "/work/app"}); err != nil {
		t.Fatal(err)
	}
	res, _ := reg.Run(context.Background(), "deliver", map[string]any{"path": "app", "branch": "agent/" + taskID + "/x"})
	if !res.IsError {
		t.Errorf("a tampered bundle was accepted: %+v", res)
	}
	if got := branches(t, e.remote); len(got) != 1 {
		t.Errorf("remote branches = %v", got)
	}
}

type tamperingSandbox struct{ sandbox.Sandbox }

func (s *tamperingSandbox) Get(ctx context.Context, p string) ([]byte, error) {
	if strings.HasSuffix(p, "gt-deliver.bundle") {
		return []byte("not a bundle"), nil
	}
	return s.Sandbox.Get(ctx, p)
}

func TestCredentialsNeverReachTheModel(t *testing.T) {
	s := &state{Config: Config{Token: "s3cret-token", User: "oauth2"}}
	cred := "b2F1dGgyOnMzY3JldC10b2tlbg=="
	msg := s.scrub("fatal: unable to access: header Authorization: Basic " + cred + " token s3cret-token")
	if strings.Contains(msg, "s3cret") || strings.Contains(msg, cred) {
		t.Errorf("scrub left a secret: %q", msg)
	}
	if got := redact("https://user:pw@host.example/org/repo.git"); strings.Contains(got, "pw") {
		t.Errorf("redact: %q", got)
	}
}
