//go:build linux

package podsupervisor_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/podsupervisor"
)

// singlePod is one single-container pod: one supervisor, no runner, no shims.
type singlePod struct {
	t    *testing.T
	ctl  *controlServer
	out  string
	work string
	tmp  string
	shm  string
	home string
	tpl  string
	cmd  *exec.Cmd
}

const singleDaemonScript = `#!/bin/sh
echo $$ >> "$OUT/pids"
# What a task's agent could leave behind: a detached process, files in every
# writable area.
if [ ! -e "$OUT/spawned" ]; then
  : > "$OUT/spawned"
  setsid sleep "$MARK.1" &
  ( sleep "$MARK.2" & )
  echo task-secret > "$POD_WORK/leak"; echo task-secret > "$TMPDIR/leak"
  echo task-secret > "$HOME/leak"; echo task-secret > "$POD_SHM_DIR/leak"
  mkdir -p "$POD_WORK/ro/deep" && echo x > "$POD_WORK/ro/deep/f" && chmod 500 "$POD_WORK/ro/deep" "$POD_WORK/ro"
fi
env | sort > "$OUT/env.$$"
exec sleep 100000
`

func startSinglePod(t *testing.T, mark string) *singlePod {
	t.Helper()
	root := t.TempDir()
	p := &singlePod{t: t, ctl: newControlServer(t), out: filepath.Join(root, "out"), work: filepath.Join(root, "work"),
		tmp: filepath.Join(root, "tmp"), shm: filepath.Join(root, "shm"), home: filepath.Join(root, "home"), tpl: filepath.Join(root, "tpl")}
	for _, d := range []string{p.out, p.work, p.tmp, p.shm, p.home, p.tpl} {
		must(t, os.MkdirAll(d, 0o755))
	}
	must(t, os.WriteFile(filepath.Join(p.tpl, "config"), []byte("template"), 0o600))
	script := filepath.Join(p.out, "daemon.sh")
	must(t, os.WriteFile(script, []byte(singleDaemonScript), 0o755))
	p.cmd = exec.Command(supervisorBin, "-single",
		"-control-url", p.ctl.srv.URL, "-work-root", p.work, "-tmpdir", p.tmp,
		"-daemon-home", p.home, "-daemon-home-template", p.tpl, "-poll-interval", "50ms", "--", script)
	p.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "OUT=" + p.out, "MARK=" + mark, "POD_WORK=" + p.work,
		"POD_SHM_DIR=" + p.shm, "SECRET_FROM_IMAGE=no"}
	p.cmd.Stderr = io.Discard
	must(t, p.cmd.Start())
	t.Cleanup(func() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = p.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = p.cmd.Process.Kill()
			<-done
		}
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	return p
}

func (p *singlePod) daemonPids() []int {
	raw, _ := os.ReadFile(filepath.Join(p.out, "pids"))
	var pids []int
	for _, f := range strings.Fields(string(raw)) {
		n, _ := strconv.Atoi(f)
		pids = append(pids, n)
	}
	return pids
}

func (p *singlePod) waitDaemonStarts(n int) {
	p.t.Helper()
	eventually(p.t, 10*time.Second, fmt.Sprintf("%d daemon starts", n), func() bool { return len(p.daemonPids()) >= n })
}

func TestSingleResetKillsEverythingAndEmptiesEveryArea(t *testing.T) {
	mark := "9" + strconv.Itoa(os.Getpid()) + "7"
	pattern := "^sleep " + mark + "\\.[0-9]$"
	p := startSinglePod(t, mark)
	p.waitDaemonStarts(1)
	eventually(t, 5*time.Second, "the task's leftovers", func() bool {
		_, err := os.Stat(filepath.Join(p.work, "ro", "deep", "f"))
		return pgrepCount(t, pattern) == 2 && err == nil
	})

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	if n := pgrepCount(t, pattern); n != 0 {
		t.Errorf("%d orphan(s) survived the reset", n)
	}
	for _, dir := range []string{p.work, p.tmp, p.shm} {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s not empty after reset: %v", dir, entries)
		}
	}
	// HOME holds only the template again.
	entries, _ := os.ReadDir(p.home)
	if len(entries) != 1 || entries[0].Name() != "config" {
		t.Errorf("home after reset = %v, want only the template", entries)
	}
	p.waitDaemonStarts(2)
}

func TestSingleDaemonSharesHomeAndTmpdirWithTheAgents(t *testing.T) {
	p := startSinglePod(t, "9"+strconv.Itoa(os.Getpid())+"8")
	p.waitDaemonStarts(1)
	var env string
	eventually(t, 5*time.Second, "the daemon's environment", func() bool {
		raw, err := os.ReadFile(filepath.Join(p.out, "env."+strconv.Itoa(p.daemonPids()[0])))
		env = string(raw)
		return err == nil
	})
	for _, want := range []string{"HOME=" + p.home, "TMPDIR=" + p.tmp} {
		if !strings.Contains(env, want+"\n") {
			t.Errorf("daemon environment lacks %s:\n%s", want, env)
		}
	}
	if strings.Contains(env, "POD_AGENT_SOCKET") {
		t.Errorf("single pod sets a runner socket:\n%s", env)
	}
}

// A hostile task runs as the supervisor's own user, so the supervisor must not
// be readable or traceable by it, while the processes it starts stay ordinary.
func TestSingleSupervisorIsNotOpenToItsOwnUser(t *testing.T) {
	p := startSinglePod(t, "9"+strconv.Itoa(os.Getpid())+"9")
	p.waitDaemonStarts(1)
	sup := p.cmd.Process.Pid
	eventually(t, 5*time.Second, "the supervisor to be undumpable", func() bool {
		_, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", sup))
		return errors.Is(err, os.ErrPermission)
	})
	for _, f := range []string{"mem", "environ", "maps", "fd/0", "cwd", "root"} {
		path := fmt.Sprintf("/proc/%d/%s", sup, f)
		fd, err := os.Open(path)
		if err == nil {
			_ = fd.Close()
			t.Errorf("%s opened by the supervisor's own user", path)
		}
	}
	var perr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		perr = syscall.PtraceAttach(sup)
		if perr == nil {
			_, _ = syscall.Wait4(sup, nil, 0, nil)
			_ = syscall.PtraceDetach(sup)
		}
	}()
	<-done
	if perr == nil {
		t.Errorf("ptrace attach to the supervisor succeeded")
	}
	// The daemon it started is an ordinary, dumpable process (the flag does
	// not survive execve).
	if _, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", p.daemonPids()[0])); err != nil {
		t.Errorf("daemon environment unreadable: %v", err)
	}
}

// A task that signals everything it can reach, the supervisor included, must
// not stop the next task from getting a clean pod.
func TestSingleSupervisorStillResetsAfterHostileSignals(t *testing.T) {
	p := startSinglePod(t, "9"+strconv.Itoa(os.Getpid())+"6")
	p.waitDaemonStarts(1)
	sup := p.cmd.Process.Pid
	// SIGSTOP/SIGKILL from inside a pod are dropped by the kernel for PID 1;
	// here the supervisor is not PID 1, so only the signals it may ignore are
	// sent. SIGUSR1/SIGHUP must not end it.
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGPIPE} {
		_ = syscall.Kill(sup, sig)
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(sup, 0); err != nil {
		t.Fatalf("supervisor ended after a stray signal: %v", err)
	}
	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
}

func TestSingleConfigNeedsNoRunnerSettings(t *testing.T) {
	cfg := podsupervisor.Config{Single: true, DaemonCmd: []string{"/bin/true"}, WorkRoot: "/w", TmpDir: "/t",
		ControlURL: "http://x", DaemonHome: "/home/pod"}
	if _, err := podsupervisor.New(cfg); err != nil {
		t.Errorf("single config rejected: %v", err)
	}
	cfg.Single = false
	if _, err := podsupervisor.New(cfg); err == nil {
		t.Errorf("a pod config without a runner and shims is accepted unless Single is set")
	}
}
