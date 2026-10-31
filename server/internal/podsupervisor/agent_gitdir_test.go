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

// The agent asks the daemon for its checkout (multica repo checkout) after it
// has started, so the worktree appears while the shim is already running.
const lateCheckoutDaemon = `#!/bin/sh
echo $$ >> "$OUT/pids"
if [ ! -e "$OUT/ran" ]; then
  : > "$OUT/ran"
  mkdir -m 755 "$TASKDIR"
  cd "$TASKDIR"
  fakeagent run > "$OUT/agent.out" 2>&1
  echo $? > "$OUT/agent.status"
fi
exec sleep 100000
`

const lateCheckoutAgent = `#!/bin/sh
wt="$(pwd)/../cache/repo.git/worktrees/app"
mkdir -p -m 755 "$wt" app
printf '[core]\n' > "$wt/config.worktree"
chmod 600 "$wt/config.worktree"
printf 'gitdir: %s\n' "$wt" > app/.git
sleep 2
stat -c %a "$wt/config.worktree" > "$OUT/late.mode"
`

func TestCheckoutCreatedAfterTheAgentStartedIsOpenedToTheAgentGroup(t *testing.T) {
	p := startSplitPod(t, lateCheckoutDaemon, lateCheckoutAgent)
	p.waitOutFile("agent.status")
	mode := p.outFile("late.mode")
	if len(mode) < 2 || mode[len(mode)-2] < '6' {
		t.Errorf("config.worktree created while the agent runs has mode %s: the agent's group cannot use it", mode)
	}
}

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
