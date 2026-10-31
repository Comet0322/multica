//go:build linux

package podsupervisor_test

import (
	"os"
	"path/filepath"
	"testing"
)

// A task checkout is a git worktree of a bare repository cache that sits outside
// the task directory (.repos/<repo>.git). Git creates the worktree's own
// config.worktree there with mode 0600 for its owner, the daemon, so the agent
// user could not run a single git command in its checkout ("permission denied"
// on config.worktree). The shim opens the worktree's git directory to the
// group together with the task directory.
const gitdirDaemon = `#!/bin/sh
echo $$ >> "$OUT/pids"
if [ ! -e "$OUT/ran" ]; then
  : > "$OUT/ran"
  mkdir -m 755 "$TASKDIR"
  mkdir -p -m 755 "$TASKDIR/../cache/repo.git/worktrees/app" "$TASKDIR/app"
  printf '[core]\n' > "$TASKDIR/../cache/repo.git/worktrees/app/config.worktree"
  chmod 600 "$TASKDIR/../cache/repo.git/worktrees/app/config.worktree"
  printf 'gitdir: %s\n' "$(cd "$TASKDIR/../cache/repo.git/worktrees/app" && pwd)" > "$TASKDIR/app/.git"
  cd "$TASKDIR"
  fakeagent run > "$OUT/agent.out" 2>&1
  echo $? > "$OUT/agent.status"
fi
exec sleep 100000
`

func TestWorktreeGitDirOfTheCheckoutIsOpenedToTheAgentGroup(t *testing.T) {
	p := startSplitPod(t, gitdirDaemon, echoAgent)
	p.waitOutFile("agent.status")
	cfg := filepath.Join(p.work, "cache", "repo.git", "worktrees", "app", "config.worktree")
	fi, err := os.Lstat(cfg)
	must(t, err)
	if fi.Mode().Perm()&0o060 != 0o060 {
		t.Errorf("%s mode %v: the agent's group cannot read and write it", cfg, fi.Mode().Perm())
	}
}
