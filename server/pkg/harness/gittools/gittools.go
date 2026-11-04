// Package gittools is how code gets into and out of a sandbox that has no
// network and no credentials.
//
// Both tools run on the host. git_clone clones an allowed repository with the
// host's credentials and copies the working tree, .git included, into the
// sandbox. The agent then edits and commits inside the sandbox with plain git,
// which works offline. deliver brings the new commits back as a git bundle,
// checks them, and pushes them to a branch of the task's own. The sandbox never
// holds a credential, never speaks to a remote, and cannot push anywhere but
// the one place deliver sends it.
package gittools

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
	"github.com/multica-ai/multica/server/pkg/harness/tools"
)

// Config is the git access of one task.
type Config struct {
	SB sandbox.Sandbox
	// Workdir is the sandbox directory repositories are cloned under.
	Workdir string
	// HostDir is host scratch space for the clones; the caller removes it.
	HostDir string
	// Allowed lists the repositories the task may clone and push to. A URL
	// matches ignoring a trailing ".git" or "/" and letter case.
	Allowed []string
	// User and Token authenticate https remotes on the host. Token may be empty
	// for repositories that need none.
	User, Token string
	// TaskID gives the branch namespace: deliver pushes only agent/<TaskID>/*.
	TaskID string
	// AuthorName and AuthorEmail are the commit identity set in each clone.
	AuthorName, AuthorEmail string
	// Git is the git binary on the host; empty means "git".
	Git string
}

// Tools returns git_clone and deliver.
func Tools(c Config) []tools.Tool {
	if c.Git == "" {
		c.Git = "git"
	}
	if c.User == "" {
		c.User = "oauth2"
	}
	if c.AuthorName == "" {
		c.AuthorName = "Multica Agent"
	}
	if c.AuthorEmail == "" {
		c.AuthorEmail = "agent@multica.invalid"
	}
	s := &state{Config: c, clones: map[string]*clone{}}
	return []tools.Tool{gitClone{s}, deliver{s}}
}

type clone struct {
	url     string // as given by the model, matched against Allowed
	hostDir string
	base    string // commit the working tree started from
}

type state struct {
	Config
	mu     sync.Mutex
	clones map[string]*clone // by absolute sandbox path
	n      int
}

func (s *state) abs(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(s.Workdir, p)
}

func normalize(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	u = strings.TrimRight(u, "/")
	return strings.TrimSuffix(u, ".git")
}

func (s *state) allowed(u string) bool {
	n := normalize(u)
	if n == "" {
		return false
	}
	for _, a := range s.Allowed {
		if normalize(a) == n {
			return true
		}
	}
	return false
}

// git runs the host's git with the task's credentials for repo, and never
// prompts. The credential is scoped to the repository's URL so it is not sent
// to a host a redirect points at.
func (s *state) git(ctx context.Context, dir, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.Git, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https:ssh:file",
		"GIT_CONFIG_NOSYSTEM=1",
		"LC_ALL=C",
	)
	if s.Token != "" && strings.HasPrefix(repo, "https://") {
		cred := base64.StdEncoding.EncodeToString([]byte(s.User + ":" + s.Token))
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+repo+".extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+cred,
		)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, s.scrub(strings.TrimSpace(errb.String())))
	}
	return strings.TrimSpace(out.String()), nil
}

// scrub keeps the credential out of anything that is shown to the model.
func (s *state) scrub(msg string) string {
	if s.Token == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, s.Token, "***")
	return strings.ReplaceAll(msg, base64.StdEncoding.EncodeToString([]byte(s.User+":"+s.Token)), "***")
}

func fail(format string, a ...any) (tools.Result, error) {
	return tools.Result{Output: fmt.Sprintf(format, a...), IsError: true}, nil
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// git_clone

type gitClone struct{ *state }

func (gitClone) Spec() tools.Spec {
	return tools.Spec{
		Name: "git_clone",
		Description: "Clone an allowed repository into the sandbox, working tree and history included. " +
			"The sandbox has no network, so this is the only way to get code in; after it, use git in bash to edit and commit, then call deliver.",
		Schema: map[string]any{"type": "object", "required": []string{"repo"}, "properties": map[string]any{
			"repo": str("Repository URL, exactly as the task lists it."),
			"dest": str("Directory name under the working directory (default: the repository's name)."),
			"ref":  str("Branch or tag to check out (default: the repository's default branch)."),
		}},
	}
}

func (t gitClone) Run(ctx context.Context, in map[string]any) (tools.Result, error) {
	repo, _ := in["repo"].(string)
	if !t.allowed(repo) {
		return fail("repository %q is not available to this task; allowed: %s", repo, strings.Join(t.Allowed, ", "))
	}
	dest, _ := in["dest"].(string)
	if dest == "" {
		dest = strings.TrimSuffix(path.Base(strings.TrimRight(repo, "/")), ".git")
	}
	if dest == "" || dest == "." || dest == ".." || strings.ContainsAny(dest, "/\\") {
		return fail("dest must be a plain directory name, got %q", dest)
	}
	target := t.abs(dest)
	t.mu.Lock()
	if _, taken := t.clones[target]; taken {
		t.mu.Unlock()
		return fail("%s already holds a clone; pick another dest", dest)
	}
	t.n++
	n := t.n
	t.mu.Unlock()

	hostDir := filepath.Join(t.HostDir, fmt.Sprintf("clone-%d", n))
	args := []string{"clone", "--quiet", "--no-tags"}
	if ref, _ := in["ref"].(string); ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, "--", repo, hostDir)
	if _, err := t.git(ctx, t.HostDir, repo, args...); err != nil {
		return fail("clone failed: %v", err)
	}
	base, err := t.git(ctx, hostDir, repo, "rev-parse", "HEAD")
	if err != nil {
		return tools.Result{}, err
	}
	// The credential only ever lived in the environment of the commands above,
	// but the remote URL is the model's to read, so it must carry none.
	if _, err := t.git(ctx, hostDir, repo, "remote", "set-url", "origin", repo); err != nil {
		return tools.Result{}, err
	}

	archive, err := tarDir(hostDir)
	if err != nil {
		return tools.Result{}, fmt.Errorf("git_clone: pack: %w", err)
	}
	parent := path.Dir(target)
	tarName := fmt.Sprintf(".gt-%d.tar", n)
	if err := t.SB.Put(ctx, path.Join(parent, tarName), archive, 0o600); err != nil {
		return tools.Result{}, fmt.Errorf("git_clone: upload: %w", err)
	}
	// -o: do not restore the host's file owners, or git refuses the tree as
	// owned by someone else.
	script := fmt.Sprintf(`set -e
mkdir -p %[1]s && tar -x -o -f %[2]s -C %[1]s && rm -f %[2]s
git -C %[1]s config user.name %[3]s
git -C %[1]s config user.email %[4]s
git config --global --add safe.directory %[5]s 2>/dev/null || true`,
		q(dest), q(tarName), q(t.AuthorName), q(t.AuthorEmail), q(target))
	res, err := t.SB.Exec(ctx, sandbox.ExecRequest{Command: script, Cwd: parent, Timeout: 5 * time.Minute})
	if err != nil {
		return tools.Result{}, fmt.Errorf("git_clone: unpack: %w", err)
	}
	if res.ExitCode != 0 {
		return fail("could not unpack the clone in the sandbox: %s", strings.TrimSpace(res.Stderr))
	}

	t.mu.Lock()
	t.clones[target] = &clone{url: repo, hostDir: hostDir, base: base}
	t.mu.Unlock()
	return tools.Result{Output: fmt.Sprintf("cloned %s at %s into %s", repo, base[:min(12, len(base))], target)}, nil
}

// q quotes one word for the sandbox's POSIX shell.
func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// tarDir packs a directory tree, regular files, directories and symlinks only.
func tarDir(root string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		case !info.Mode().IsRegular() && !info.IsDir():
			return nil
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// deliver

type deliver struct{ *state }

func (t deliver) Spec() tools.Spec {
	return tools.Spec{
		Name: "deliver",
		Description: "Publish your committed work from a cloned repository to a branch of its remote. " +
			"Commit in the sandbox first; uncommitted changes are not delivered. The branch name must start with " +
			fmt.Sprintf("%q. Delivering again to the same branch adds the newer commits.", t.prefix()),
		Schema: map[string]any{"type": "object", "required": []string{"path", "branch"}, "properties": map[string]any{
			"path":   str("The cloned repository directory."),
			"branch": str("Branch to push to, starting with " + t.prefix()),
		}},
	}
}

func (s *state) prefix() string { return "agent/" + s.TaskID + "/" }

func (t deliver) Run(ctx context.Context, in map[string]any) (tools.Result, error) {
	p, _ := in["path"].(string)
	branch, _ := in["branch"].(string)
	t.mu.Lock()
	c := t.clones[t.abs(p)]
	t.mu.Unlock()
	if c == nil {
		return fail("%q is not a repository cloned with git_clone", p)
	}
	if !strings.HasPrefix(branch, t.prefix()) || len(branch) == len(t.prefix()) {
		return fail("branch must start with %q and name the change after it", t.prefix())
	}
	if _, err := t.git(ctx, c.hostDir, c.url, "check-ref-format", "--branch", branch); err != nil {
		return fail("%q is not a valid branch name", branch)
	}
	dir := t.abs(p)

	sh := func(cmd string) (sandbox.ExecResult, error) {
		return t.SB.Exec(ctx, sandbox.ExecRequest{Command: cmd, Cwd: dir, Timeout: 5 * time.Minute})
	}
	// Untracked files (build output, caches) are not part of any commit and are
	// not delivered, so they must not stop a delivery; edits to tracked files are
	// the ones an agent forgets to commit.
	if res, err := sh("git status --porcelain --untracked-files=no"); err != nil {
		return tools.Result{}, fmt.Errorf("deliver: %w", err)
	} else if strings.TrimSpace(res.Stdout) != "" {
		return fail("there are uncommitted changes to tracked files; commit them first (git add -A && git commit):\n%s", truncate(res.Stdout, 2000))
	}
	head, err := sh("git rev-parse HEAD")
	if err != nil || head.ExitCode != 0 {
		return fail("could not read HEAD in %s: %s", p, strings.TrimSpace(head.Stderr))
	}
	if strings.TrimSpace(head.Stdout) == c.base {
		return fail("nothing to deliver: %s has no commits beyond the clone", p)
	}
	const bundlePath = ".git/gt-deliver.bundle"
	res, err := sh(fmt.Sprintf("git bundle create %s %s..HEAD", bundlePath, c.base))
	if err != nil {
		return tools.Result{}, fmt.Errorf("deliver: %w", err)
	}
	if res.ExitCode != 0 {
		return fail("could not bundle the commits: %s", strings.TrimSpace(res.Stderr))
	}
	data, err := t.SB.Get(ctx, path.Join(dir, bundlePath))
	if err != nil {
		return tools.Result{}, fmt.Errorf("deliver: read bundle: %w", err)
	}
	_, _ = sh("rm -f " + bundlePath)

	local := filepath.Join(c.hostDir, ".git", "gt-in.bundle")
	if err := os.WriteFile(local, data, 0o600); err != nil {
		return tools.Result{}, err
	}
	defer os.Remove(local)
	// verify checks the bundle is well formed and its prerequisites are commits
	// the host clone already has; fetching then re-hashes every object.
	if _, err := t.git(ctx, c.hostDir, c.url, "bundle", "verify", local); err != nil {
		return fail("the bundle from the sandbox is not valid: %v", err)
	}
	if _, err := t.git(ctx, c.hostDir, c.url, "fetch", "--quiet", local, "HEAD:refs/heads/"+branch); err != nil {
		return fail("could not take the commits from the sandbox: %v", err)
	}
	// No force: a history that was rewritten after an earlier delivery is rejected.
	if _, err := t.git(ctx, c.hostDir, c.url, "push", "--quiet", "origin", "refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		return fail("push rejected: %v", err)
	}
	sha, _ := t.git(ctx, c.hostDir, c.url, "rev-parse", "refs/heads/"+branch)
	return tools.Result{Output: fmt.Sprintf("pushed %s to %s (%s)", branch, redact(c.url), sha[:min(12, len(sha))])}, nil
}

// redact drops any user info from a URL before it is shown.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
