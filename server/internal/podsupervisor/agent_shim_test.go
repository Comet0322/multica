//go:build linux

package podsupervisor_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/podsupervisor"
)

// The daemon cannot be changed and starts agents as itself, so the pod puts a
// shim named like each agent CLI first on the daemon's PATH. The shim asks the
// supervisor, which holds the privileges, to run the real CLI as the agent
// user. Tests run unprivileged, so both users are the test user; what they
// check is the protocol, the environment and the process lifetime. The uid
// switch itself is checked against a real container by pool/verify.sh.

type splitPod struct {
	*pod
	shimDir, realDir, daemonHome string
	taskDir                      string
}

// startSplitPod starts the supervisor in split mode around a stub daemon
// script and a stub "fakeagent" CLI. Further shim names can be added with
// names; they have no real CLI behind them.
func startSplitPod(t *testing.T, daemon, agent string, names ...string) *splitPod {
	t.Helper()
	var sp *splitPod
	startPod(t, "", func(p *pod) {
		sp = &splitPod{pod: p}
		sp.shimDir = filepath.Join(p.out, "shims")
		sp.realDir = filepath.Join(p.out, "real")
		sp.daemonHome = filepath.Join(p.tmp, "daemon-home")
		sp.taskDir = filepath.Join(p.work, "task")
		dtpl := filepath.Join(p.out, "daemon-template")
		for _, d := range []string{sp.shimDir, sp.realDir, filepath.Join(dtpl, ".multica")} {
			must(t, os.MkdirAll(d, 0o755))
		}
		must(t, os.WriteFile(filepath.Join(dtpl, ".multica", "config.json"), []byte(`{"token":"daemon-only"}`), 0o600))
		must(t, os.WriteFile(filepath.Join(sp.realDir, "fakeagent"), []byte(agent), 0o755))
		for _, n := range append([]string{"fakeagent"}, names...) {
			if err := os.Link(supervisorBin, filepath.Join(sp.shimDir, n)); err != nil {
				t.Fatal(err)
			}
		}
		must(t, os.WriteFile(filepath.Join(p.out, "daemon.sh"), []byte(daemon), 0o755))
		p.args = []string{"-shim-dir", sp.shimDir, "-daemon-home", sp.daemonHome, "-daemon-home-template", dtpl}
		p.env = []string{"PATH=" + os.Getenv("PATH") + ":" + sp.realDir, "TASKDIR=" + sp.taskDir, "SHIMDIR=" + sp.shimDir}
	})
	return sp
}

func (p *pod) outFile(name string) string {
	b, _ := os.ReadFile(filepath.Join(p.out, name))
	return strings.TrimSpace(string(b))
}

func (p *pod) waitOutFile(name string) string {
	p.t.Helper()
	eventually(p.t, 10*time.Second, name, func() bool { return p.outFile(name) != "" })
	return p.outFile(name)
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("not a number: %q", s)
	}
	return n
}

const runOnceDaemon = `#!/bin/sh
echo $$ >> "$OUT/pids"
if [ ! -e "$OUT/ran" ]; then
  : > "$OUT/ran"
  echo "$HOME" > "$OUT/daemon.home"
  echo "$TMPDIR" > "$OUT/daemon.tmp"
  echo "$PATH" > "$OUT/daemon.path"
  umask > "$OUT/daemon.umask"
  mkdir -m 755 "$TASKDIR"
  : > "$TASKDIR/file"
  mkdir -m 700 "$TASKDIR/sub"
  cd "$TASKDIR"
  export KEEP=kept
  echo input | fakeagent one "two words" > "$OUT/agent.out" 2> "$OUT/agent.err"
  echo $? > "$OUT/agent.status"
fi
exec sleep 100000
`

const echoAgent = `#!/bin/sh
echo "args:$# $1|$2"
echo "pwd:$(pwd)"
echo "home:$HOME"
echo "tmpdir:$TMPDIR"
echo "path:$PATH"
echo "keep:$KEEP"
echo "stdin:$(cat)"
echo oops >&2
exit 7
`

func TestAgentRunsThroughShimWithTheAgentsEnvironment(t *testing.T) {
	p := startSplitPod(t, runOnceDaemon, echoAgent)
	status := p.waitOutFile("agent.status")
	out := p.outFile("agent.out") + "\n"

	if status != "7" {
		t.Errorf("agent exit status = %s, want the agent's own 7", status)
	}
	for _, want := range []string{
		"args:2 one|two words",
		"pwd:" + p.taskDir,
		"home:" + p.home,
		"tmpdir:" + p.tmp,
		"keep:kept",
		"stdin:input",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("agent output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, p.shimDir) {
		t.Errorf("the agent's PATH still contains the shim directory, so a process it starts would loop back into the launcher:\n%s", out)
	}
	if !strings.Contains(p.outFile("agent.err"), "oops") {
		t.Errorf("agent stderr not forwarded: %q", p.outFile("agent.err"))
	}
	if got := p.outFile("daemon.home"); got != p.daemonHome {
		t.Errorf("daemon HOME = %q, want its own home %q", got, p.daemonHome)
	}
	if got := p.outFile("daemon.tmp"); got != filepath.Join(p.daemonHome, "tmp") {
		t.Errorf("daemon TMPDIR = %q, want a directory inside its home", got)
	}
	if got := p.outFile("daemon.path"); !strings.HasPrefix(got, p.shimDir+":") {
		t.Errorf("daemon PATH = %q, want the shim directory first", got)
	}
	if got := p.outFile("daemon.umask"); got != "0002" {
		t.Errorf("daemon umask = %s, want 0002 so the agent user can share its files", got)
	}
}

func TestTaskDirectoryIsOpenedToTheAgentGroup(t *testing.T) {
	p := startSplitPod(t, runOnceDaemon, echoAgent)
	p.waitOutFile("agent.status")
	for path, want := range map[string]os.FileMode{
		p.taskDir:                        0o070,
		filepath.Join(p.taskDir, "file"): 0o060,
		filepath.Join(p.taskDir, "sub"):  0o070,
	} {
		fi, err := os.Lstat(path)
		must(t, err)
		if fi.Mode().Perm()&want != want {
			t.Errorf("%s mode %v lacks group bits %o", path, fi.Mode().Perm(), want)
		}
	}
}

const longAgent = `#!/bin/sh
case "$1" in
hang)
  echo $$ > "$OUT/agent.pid"
  sleep 100000 &
  echo $! > "$OUT/agent.child"
  wait
  ;;
term)
  trap 'echo term > "$OUT/term"; exit 3' TERM
  echo $$ > "$OUT/agent.pid"
  while :; do sleep 0.05; done
  ;;
*)
  echo ok
  ;;
esac
`

func daemonRunning(cmd string) string {
	return `#!/bin/sh
echo $$ >> "$OUT/pids"
if [ ! -e "$OUT/ran" ]; then
  : > "$OUT/ran"
  ` + cmd + `
fi
exec sleep 100000
`
}

func TestAgentDiesWhenTheShimIsKilled(t *testing.T) {
	p := startSplitPod(t, daemonRunning(`fakeagent hang & echo $! > "$OUT/shim.pid"`), longAgent)
	agent := atoi(t, p.waitOutFile("agent.pid"))
	child := atoi(t, p.waitOutFile("agent.child"))
	shim := atoi(t, p.waitOutFile("shim.pid"))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL); _ = syscall.Kill(agent, syscall.SIGKILL) })

	// The daemon kills the process group it created; SIGKILL cannot be forwarded.
	must(t, syscall.Kill(shim, syscall.SIGKILL))
	eventually(t, 10*time.Second, "agent and its child to die", func() bool { return !alive(agent) && !alive(child) })
}

func TestSignalsToTheShimReachTheAgent(t *testing.T) {
	p := startSplitPod(t, daemonRunning(`fakeagent term & echo $! > "$OUT/shim.pid"; wait $!; echo $? > "$OUT/agent.status"`), longAgent)
	p.waitOutFile("agent.pid")
	shim := atoi(t, p.waitOutFile("shim.pid"))

	must(t, syscall.Kill(shim, syscall.SIGTERM))
	if got := p.waitOutFile("agent.status"); got != "3" {
		t.Errorf("shim exit status = %s, want the agent's 3 after it handled SIGTERM", got)
	}
	if p.outFile("term") != "term" {
		t.Errorf("the agent never saw SIGTERM")
	}
}

func TestShimRefusesAnAgentWithNoRealCLI(t *testing.T) {
	p := startSplitPod(t, daemonRunning(`nosuch > "$OUT/out" 2> "$OUT/err"; echo $? > "$OUT/status"`), longAgent, "nosuch")
	if got := p.waitOutFile("status"); got != "127" {
		t.Errorf("exit status = %s, want 127", got)
	}
	if p.outFile("err") == "" {
		t.Errorf("no explanation on stderr")
	}
}

func TestResetStopsARunningAgentAndTheLauncherWorksAfterwards(t *testing.T) {
	daemon := `#!/bin/sh
echo $$ >> "$OUT/pids"
if [ ! -e "$OUT/ran" ]; then
  : > "$OUT/ran"
  fakeagent hang &
else
  fakeagent ok > "$OUT/second.out"
fi
exec sleep 100000
`
	p := startSplitPod(t, daemon, longAgent)
	agent := atoi(t, p.waitOutFile("agent.pid"))
	child := atoi(t, p.waitOutFile("agent.child"))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL); _ = syscall.Kill(agent, syscall.SIGKILL) })

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	if alive(agent) || alive(child) {
		t.Errorf("agent %d / child %d survived the reset", agent, child)
	}
	if got := p.waitOutFile("second.out"); got != "ok" {
		t.Errorf("agent launched after the reset printed %q", got)
	}
}

func TestDaemonHomeIsPrivateAndRebuiltOnReset(t *testing.T) {
	p := startSplitPod(t, daemonRunning(`:`), echoAgent)
	p.waitDaemonStarts(1)
	cfg := filepath.Join(p.daemonHome, ".multica", "config.json")

	fi, err := os.Stat(p.daemonHome)
	must(t, err)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("daemon home mode = %v, want 0700", fi.Mode().Perm())
	}
	if got := readFile(t, cfg); !strings.Contains(got, "daemon-only") {
		t.Errorf("daemon config not restored from its template: %q", got)
	}
	if _, err := os.Stat(filepath.Join(p.home, ".multica")); err == nil {
		t.Errorf("the daemon's CLI config is inside the agent's HOME")
	}
	tmp, err := os.Stat(p.tmp)
	must(t, err)
	if tmp.Mode()&os.ModeSticky == 0 || tmp.Mode().Perm() != 0o777 {
		t.Errorf("tmpdir mode = %v, want sticky and world-writable so the agent user cannot rename the daemon's directory", tmp.Mode())
	}

	must(t, os.WriteFile(cfg, []byte("tampered"), 0o600))
	must(t, os.WriteFile(filepath.Join(p.daemonHome, "leftover"), nil, 0o600))
	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	if got := readFile(t, cfg); !strings.Contains(got, "daemon-only") {
		t.Errorf("daemon config after reset = %q", got)
	}
	if _, err := os.Stat(filepath.Join(p.daemonHome, "leftover")); err == nil {
		t.Errorf("daemon home kept a file across the reset")
	}
	if _, err := os.Stat(filepath.Join(p.daemonHome, "tmp")); err != nil {
		t.Errorf("daemon TMPDIR not recreated: %v", err)
	}
}

func TestSplitConfigIsValidated(t *testing.T) {
	base := podsupervisor.Config{
		DaemonCmd: []string{"/bin/true"}, WorkRoot: "/w", Home: "/h", TmpDir: "/t", ControlURL: "http://x",
		ShimDir: "/shims",
	}
	if _, err := podsupervisor.New(base); err == nil || !strings.Contains(err.Error(), "daemon home") {
		t.Errorf("shim dir without a daemon home: err = %v", err)
	}
	base.DaemonHome = "/elsewhere/daemon"
	if _, err := podsupervisor.New(base); err == nil || !strings.Contains(err.Error(), "below the tmpdir") {
		t.Errorf("daemon home outside the tmpdir: err = %v", err)
	}
	base.DaemonHome = "/t/daemon"
	if _, err := podsupervisor.New(base); err != nil {
		t.Errorf("valid split config rejected: %v", err)
	}
}
