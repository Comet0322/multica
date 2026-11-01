//go:build linux

package podsupervisor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/podsupervisor"
)

var supervisorBin string

func TestMain(m *testing.M) {
	if os.Geteuid() == 0 {
		fmt.Println("skipping: tests must run as a non-root user")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "podsupervisor-bin")
	if err != nil {
		panic(err)
	}
	supervisorBin = filepath.Join(dir, "pod-supervisor")
	out, err := exec.Command("go", "build", "-o", supervisorBin, "github.com/multica-ai/multica/server/cmd/pod-supervisor").CombinedOutput()
	if err != nil {
		fmt.Printf("build pod-supervisor: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// controlServer plays the manager: it hands out one command at a time and
// records the outcomes the supervisor reports.
type controlServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	pending *podsupervisor.Command
	reports []podsupervisor.Report

	// getStatus, when set, is what every poll answers.
	getStatus int
	// eventStatus, when set, answers the next eventRejects event posts (the
	// events are still recorded).
	eventStatus  int
	eventRejects int
	// onPost sees every post as it arrives.
	onPost func(rep podsupervisor.Report)
}

// serveRunner answers the control socket like a runner that does nothing, for
// tests that run the supervisor in-process.
func serveRunner(t *testing.T, socket string) func() {
	t.Helper()
	ln, err := net.Listen("unix", "@"+socket+"-ctl")
	must(t, err)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var req map[string]string
				_ = json.NewDecoder(c).Decode(&req)
				_ = json.NewEncoder(c).Encode(map[string]any{"open": req["op"] != "close"})
			}()
		}
	}()
	return func() { _ = ln.Close() }
}

func newControlServer(t *testing.T) *controlServer {
	t.Helper()
	c := &controlServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if c.getStatus != 0 {
				w.WriteHeader(c.getStatus)
				return
			}
			if c.pending == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(c.pending)
		case http.MethodPost:
			var rep podsupervisor.Report
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &rep); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			c.reports = append(c.reports, rep)
			if c.onPost != nil {
				c.onPost(rep)
			}
			if rep.Event != "" && c.eventRejects > 0 {
				c.eventRejects--
				w.WriteHeader(c.eventStatus)
				return
			}
			if c.pending != nil && c.pending.ID == rep.ID {
				c.pending = nil
			}
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// reset issues a reset command and waits for its reported outcome.
func (c *controlServer) reset(t *testing.T, id string) podsupervisor.Report {
	t.Helper()
	c.mu.Lock()
	c.pending = &podsupervisor.Command{ID: id, Command: "reset"}
	c.mu.Unlock()
	var out podsupervisor.Report
	eventually(t, 20*time.Second, "report for "+id, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, r := range c.reports {
			if r.ID == id {
				out = r
				return true
			}
		}
		return false
	})
	return out
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// pod is one pod: the supervisor and the runner as two processes of the test
// user, each with the flags the two containers get.
type pod struct {
	t          *testing.T
	ctl        *controlServer
	out        string // test-owned directory, never touched by resets
	work       string
	home       string // the agent's HOME, the runner's
	tmp        string
	template   string // the agent's HOME template
	daemonHome string // the daemon's HOME, the supervisor's
	daemonTpl  string
	shimDir    string
	socket     string
	args       []string // extra supervisor flags, set by the setup function
	runnerArgs []string // extra runner flags
	env        []string // extra environment of both
	cmd        *exec.Cmd
	runner     *exec.Cmd
}

const daemonScript = `#!/bin/sh
id -u >> "$OUT/uids"
env | sort > "$OUT/env.$$"
echo $$ >> "$OUT/pids"
if [ -n "$SPAWN" ] && [ ! -e "$OUT/spawned" ]; then
  : > "$OUT/spawned"
  export AGENT_SET=leaked
  setsid sleep "$MARK.1" &
  sh -c 'sleep "$0.2" & wait' "$MARK" &
  ( sleep "$MARK.3" & )
fi
exec sleep 100000
`

// startPod launches the real supervisor binary around the stub daemon.
func startPod(t *testing.T, mark string, setup func(p *pod)) *pod {
	t.Helper()
	root := t.TempDir()
	p := &pod{
		t:          t,
		ctl:        newControlServer(t),
		out:        filepath.Join(root, "out"),
		work:       filepath.Join(root, "work"),
		home:       filepath.Join(root, "home"),
		tmp:        filepath.Join(root, "tmp"),
		template:   filepath.Join(root, "template"),
		daemonHome: filepath.Join(root, "daemon-home"),
		daemonTpl:  filepath.Join(root, "daemon-template"),
		shimDir:    filepath.Join(root, "shims"),
		socket:     fmt.Sprintf("podsupervisor-test-%d-%d", os.Getpid(), time.Now().UnixNano()),
	}
	for _, d := range []string{p.out, p.work, p.home, p.tmp, p.template, p.daemonHome, p.daemonTpl, p.shimDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(p.out, "daemon.sh")
	if err := os.WriteFile(script, []byte(daemonScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(p)
	}
	env := append([]string{"PATH=" + os.Getenv("PATH"), "OUT=" + p.out, "MARK=" + mark, "SPAWN=" + map[bool]string{true: "1"}[mark != ""]}, p.env...)

	runnerArgs := append([]string{
		"-role", "runner", "-agent-socket", p.socket, "-daemon-uid", strconv.Itoa(os.Getuid()),
		"-work-root", p.work, "-tmpdir", p.tmp, "-shim-dir", p.shimDir,
		"-home", p.home, "-home-template", p.template,
	}, p.runnerArgs...)
	p.runner = exec.Command(supervisorBin, runnerArgs...)
	p.runner.Env = env
	p.runner.Stderr = io.Discard
	if err := p.runner.Start(); err != nil {
		t.Fatal(err)
	}

	args := append([]string{
		"-control-url", p.ctl.srv.URL, "-agent-socket", p.socket,
		"-work-root", p.work, "-tmpdir", p.tmp, "-shim-dir", p.shimDir,
		"-daemon-home", p.daemonHome, "-daemon-home-template", p.daemonTpl,
		"-poll-interval", "50ms",
	}, p.args...)
	p.cmd = exec.Command(supervisorBin, append(args, "--", script)...)
	p.cmd.Env = env
	p.cmd.Stderr = io.Discard
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopProc := func(cmd *exec.Cmd) {
		if cmd.ProcessState != nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	t.Cleanup(func() {
		stopProc(p.cmd)
		stopProc(p.runner)
		// Make read-only trees removable for t.TempDir cleanup.
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	return p
}

func (p *pod) daemonPids() []int {
	raw, _ := os.ReadFile(filepath.Join(p.out, "pids"))
	var pids []int
	for _, f := range strings.Fields(string(raw)) {
		n, _ := strconv.Atoi(f)
		pids = append(pids, n)
	}
	return pids
}

func (p *pod) waitDaemonStarts(n int) {
	p.t.Helper()
	eventually(p.t, 10*time.Second, fmt.Sprintf("%d daemon starts", n), func() bool { return len(p.daemonPids()) >= n })
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func pgrepCount(t *testing.T, pattern string) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return 0
		}
		t.Fatalf("pgrep: %v", err)
	}
	return len(strings.Fields(string(out)))
}

func TestDaemonRunsAsNonRootAndRestartsOnCrash(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)

	uids, _ := os.ReadFile(filepath.Join(p.out, "uids"))
	if strings.TrimSpace(strings.Fields(string(uids))[0]) == "0" {
		t.Fatalf("daemon ran as root")
	}

	first := p.daemonPids()[0]
	if err := syscall.Kill(first, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	p.waitDaemonStarts(2)
	if second := p.daemonPids()[1]; second == first || !alive(second) {
		t.Fatalf("daemon was not restarted: pids %v", p.daemonPids())
	}
}

func TestResetKillsAllDescendantsIncludingDetachedOrphans(t *testing.T) {
	mark := "9" + strconv.Itoa(os.Getpid()) + "1" // sleep needs a number
	pattern := "^sleep " + mark + "\\.[0-9]$"
	p := startPod(t, mark, nil)
	p.waitDaemonStarts(1)
	eventually(t, 10*time.Second, "spawned processes", func() bool { return pgrepCount(t, pattern) == 3 })
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", pattern).Run() })
	old := p.daemonPids()[0]

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	if n := pgrepCount(t, pattern); n != 0 {
		t.Fatalf("%d descendants survived the reset", n)
	}
	if alive(old) {
		t.Fatalf("old daemon %d survived", old)
	}
	p.waitDaemonStarts(2)
}

func TestResetEmptiesWorkRootHomeAndTmpdir(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{p.work, p.home, p.tmp} {
		must(t, os.WriteFile(filepath.Join(root, ".hidden"), []byte("h"), 0o600))
		must(t, os.WriteFile(filepath.Join(root, "file"), []byte("f"), 0o644))
		must(t, os.Symlink(outside, filepath.Join(root, "link-to-dir")))
		must(t, os.Symlink("/nonexistent", filepath.Join(root, "dangling")))
		must(t, os.MkdirAll(filepath.Join(root, "a", "b", ".c"), 0o755))
		must(t, os.WriteFile(filepath.Join(root, "a", "b", ".c", "deep"), []byte("d"), 0o644))
		// Read-only nested directories at two levels.
		must(t, os.Chmod(filepath.Join(root, "a", "b", ".c"), 0o500))
		must(t, os.Chmod(filepath.Join(root, "a", "b"), 0o500))
		must(t, os.Chmod(filepath.Join(root, "a"), 0o555))
	}

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	for _, root := range []string{p.work, p.home, p.tmp} {
		entries, err := os.ReadDir(root)
		must(t, err)
		if len(entries) != 0 {
			t.Errorf("%s not empty after reset: %v", root, entries)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("reset followed a symlink and deleted outside content: %v", err)
	}
}

func TestResetRebuildsHomeFromTemplate(t *testing.T) {
	p := startPod(t, "", func(p *pod) {
		tpl := p.template
		must(t, os.MkdirAll(filepath.Join(tpl, ".config", "multica"), 0o755))
		must(t, os.WriteFile(filepath.Join(tpl, ".config", "multica", "config.json"), []byte(`{"token":"placeholder"}`), 0o600))
		must(t, os.WriteFile(filepath.Join(tpl, ".bashrc"), []byte("export A=1\n"), 0o644))
		must(t, os.MkdirAll(filepath.Join(tpl, "bin"), 0o755))
		must(t, os.WriteFile(filepath.Join(tpl, "bin", "tool"), []byte("#!/bin/sh\n"), 0o755))
		must(t, os.Symlink(".bashrc", filepath.Join(tpl, ".profile")))
		must(t, os.WriteFile(filepath.Join(tpl, "readonly"), []byte("ro"), 0o400))
		must(t, os.Chmod(filepath.Join(tpl, "bin"), 0o555))
		must(t, os.Chmod(filepath.Join(tpl, ".config", "multica"), 0o500))
		t.Cleanup(func() {
			_ = filepath.WalkDir(tpl, func(path string, d fs.DirEntry, err error) error {
				if d != nil && d.IsDir() {
					_ = os.Chmod(path, 0o700)
				}
				return nil
			})
		})
	})
	p.waitDaemonStarts(1)
	assertSameTree(t, p.template, p.home)

	// Agent damage: edit, delete and add.
	must(t, os.Chmod(filepath.Join(p.home, ".config", "multica"), 0o700))
	must(t, os.WriteFile(filepath.Join(p.home, ".config", "multica", "config.json"), []byte("tampered"), 0o600))
	must(t, os.Remove(filepath.Join(p.home, ".bashrc")))
	must(t, os.WriteFile(filepath.Join(p.home, "extra"), []byte("e"), 0o644))

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	assertSameTree(t, p.template, p.home)
}

type entry struct {
	mode    fs.FileMode
	content string
}

func snapshot(t *testing.T, root string) map[string]entry {
	t.Helper()
	got := map[string]entry{}
	must(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := entry{mode: info.Mode()}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			e.content, err = os.Readlink(path)
		case info.Mode().IsRegular():
			var b []byte
			b, err = os.ReadFile(path)
			e.content = string(b)
		}
		got[rel] = e
		return err
	}))
	return got
}

func assertSameTree(t *testing.T, want, got string) {
	t.Helper()
	// Template directories may be read-only; open them up for reading only.
	w, g := snapshot(t, want), snapshot(t, got)
	if len(w) != len(g) {
		t.Errorf("entry count differs: template %d, home %d", len(w), len(g))
	}
	for rel, we := range w {
		ge, ok := g[rel]
		if !ok {
			t.Errorf("missing %s", rel)
			continue
		}
		if we != ge {
			t.Errorf("%s differs: template %+v, home %+v", rel, we, ge)
		}
	}
}

func TestRestartedDaemonGetsFreshEnvironment(t *testing.T) {
	mark := "9" + strconv.Itoa(os.Getpid()) + "2"
	pattern := "^sleep " + mark + "\\.[0-9]$"
	p := startPod(t, mark, nil)
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", pattern).Run() })
	p.waitDaemonStarts(1)
	// The stub exports AGENT_SET only for the processes it spawns.
	eventually(t, 10*time.Second, "agent processes", func() bool { return pgrepCount(t, pattern) == 3 })

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	p.waitDaemonStarts(2)
	pids := p.daemonPids()
	first := readFile(t, filepath.Join(p.out, fmt.Sprintf("env.%d", pids[0])))
	second := readFile(t, filepath.Join(p.out, fmt.Sprintf("env.%d", pids[1])))
	if first != second {
		t.Errorf("environment differs across reset:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if strings.Contains(second, "AGENT_SET") {
		t.Errorf("agent-set variable survived the reset:\n%s", second)
	}
	for _, want := range []string{"HOME=" + p.daemonHome, "TMPDIR=" + filepath.Join(p.daemonHome, "tmp")} {
		if !strings.Contains(second, want+"\n") {
			t.Errorf("environment lacks %s:\n%s", want, second)
		}
	}
}

func TestReportedCommandIsNotExecutedTwice(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)
	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	p.waitDaemonStarts(2)

	// A server that keeps offering the same id after the report must not
	// cause another reset.
	p.ctl.mu.Lock()
	p.ctl.pending = &podsupervisor.Command{ID: "r1", Command: "reset"}
	p.ctl.mu.Unlock()
	time.Sleep(500 * time.Millisecond)
	if n := len(p.daemonPids()); n != 2 {
		t.Errorf("duplicate command re-ran the reset: pids %v", p.daemonPids())
	}
	p.ctl.mu.Lock()
	var buf bytes.Buffer
	for _, r := range p.ctl.reports {
		fmt.Fprintf(&buf, "%s:%s ", r.ID, r.Status)
	}
	p.ctl.mu.Unlock()
	if !strings.Contains(buf.String(), "r1:done") {
		t.Errorf("reports = %s", buf.String())
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	return string(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// set changes the server's behaviour while it runs.
func (c *controlServer) set(f func(c *controlServer)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c)
}

// events returns the supervisor events received so far, in order.
func (c *controlServer) events() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.reports {
		if r.Event != "" {
			out = append(out, r.Event)
		}
	}
	return out
}

// waitExit waits for the supervisor process to end and returns its error.
func (p *pod) waitExit(d time.Duration) error {
	p.t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		p.t.Fatalf("the supervisor did not exit within %s", d)
		return nil
	}
}

// H-2: a reset that fails is retried, and only a reset that keeps failing
// ends the supervisor, so the kubelet restarts the container.
func TestResetRetriesAndSucceedsOnceTheCauseIsGone(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)

	must(t, os.RemoveAll(p.tmp))
	must(t, os.WriteFile(p.tmp, []byte("not a dir"), 0o644))
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.Remove(p.tmp)
		_ = os.Mkdir(p.tmp, 0o755)
	}()

	rep := p.ctl.reset(t, "r1")
	if rep.Status != "done" {
		t.Fatalf("a reset whose second try works must report done, got %+v", rep)
	}
	p.waitDaemonStarts(2)
	if p.cmd.ProcessState != nil {
		t.Fatalf("the supervisor exited after a reset that recovered")
	}
}

func TestResetThatKeepsFailingIsReportedAndEndsTheSupervisor(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)
	old := p.daemonPids()[0]

	// The TMPDIR can never be cleaned: it is no longer a directory.
	must(t, os.RemoveAll(p.tmp))
	must(t, os.WriteFile(p.tmp, []byte("not a dir"), 0o644))

	rep := p.ctl.reset(t, "r1")
	if rep.Status != "failed" || rep.Error == "" {
		t.Fatalf("reset report = %+v, want failed with an error", rep)
	}
	err := p.waitExit(15 * time.Second)
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() == 0 {
		t.Fatalf("the supervisor must exit with a failure so the container restarts, got %v", err)
	}
	if n := len(p.daemonPids()); n != 1 {
		t.Errorf("daemon restarted after a failed reset: pids %v", p.daemonPids())
	}
	if alive(old) {
		t.Errorf("old daemon still running after the supervisor gave up")
	}
}

func TestResetFollowedByADaemonThatCannotStartEndsTheSupervisor(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)
	// The reset kills the daemon, cleans and then cannot start it again.
	must(t, os.Chmod(filepath.Join(p.out, "daemon.sh"), 0o644))

	rep := p.ctl.reset(t, "r1")
	if rep.Status != "failed" {
		t.Fatalf("a daemon that cannot start after the reset must be reported failed, got %+v", rep)
	}
	if err := p.waitExit(15 * time.Second); err == nil {
		t.Fatalf("the supervisor must exit with an error")
	}
}

func TestStartedEventComesBeforeTheDaemonStarts(t *testing.T) {
	var daemonRanFirst bool
	p := startPod(t, "", func(p *pod) {
		p.ctl.set(func(c *controlServer) {
			c.onPost = func(rep podsupervisor.Report) { // runs under c.mu
				if rep.Event == "started" {
					if _, err := os.Stat(filepath.Join(p.out, "pids")); err == nil {
						daemonRanFirst = true
					}
				}
			}
		})
	})
	p.waitDaemonStarts(1)
	ev := p.ctl.events()
	if len(ev) == 0 || ev[0] != "started" {
		t.Fatalf("the first event must be started, got %v", ev)
	}
	p.ctl.mu.Lock()
	defer p.ctl.mu.Unlock()
	if daemonRanFirst {
		t.Fatalf("the daemon was already running when the started event arrived")
	}
	if first := p.ctl.reports[0]; first.Event != "started" || first.ID != "" || first.Status != "" {
		t.Fatalf("the started event carries no command id: %+v", first)
	}
}

func TestEventIsRetriedOnServerErrorsThenTheDaemonStartsAnyway(t *testing.T) {
	p := startPod(t, "", func(p *pod) {
		p.ctl.set(func(c *controlServer) { c.eventStatus, c.eventRejects = http.StatusInternalServerError, 100 })
	})
	p.waitDaemonStarts(1)
	started := 0
	for _, e := range p.ctl.events() {
		if e == "started" {
			started++
		}
	}
	if started != 3 {
		t.Fatalf("a 5xx event is sent 3 times and then given up, got %d", started)
	}
}

func TestEventIsNotRetriedOnClientErrors(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusBadRequest} {
		p := startPod(t, "", func(p *pod) {
			p.ctl.set(func(c *controlServer) { c.eventStatus, c.eventRejects = code, 100 })
		})
		p.waitDaemonStarts(1)
		time.Sleep(300 * time.Millisecond)
		if ev := p.ctl.events(); len(ev) != 1 {
			t.Fatalf("a %d answer means delivered (an older manager), got events %v", code, ev)
		}
	}
}

func TestDaemonExitSendsDaemonExitedButAResetDoesNot(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)

	// A reset stops the daemon on purpose.
	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	p.waitDaemonStarts(2)
	for _, e := range p.ctl.events() {
		if e == "daemon-exited" {
			t.Fatalf("a reset is not a daemon exit: events %v", p.ctl.events())
		}
	}

	must(t, syscall.Kill(p.daemonPids()[1], syscall.SIGKILL))
	eventually(t, 10*time.Second, "daemon-exited event", func() bool {
		for _, e := range p.ctl.events() {
			if e == "daemon-exited" {
				return true
			}
		}
		return false
	})
	p.waitDaemonStarts(3)
}

func TestShmDirIsEmptiedAtStartAndOnEveryReset(t *testing.T) {
	shm := filepath.Join(t.TempDir(), "shm")
	must(t, os.Mkdir(shm, 0o755))
	must(t, os.WriteFile(filepath.Join(shm, "left-by-an-earlier-task"), []byte("x"), 0o644))
	p := startPod(t, "", func(p *pod) { p.env = []string{"POD_SHM_DIR=" + shm} })
	p.waitDaemonStarts(1)

	entries, err := os.ReadDir(shm)
	must(t, err)
	if len(entries) != 0 {
		t.Fatalf("shm not emptied at start: %v", entries)
	}

	must(t, os.WriteFile(filepath.Join(shm, "probe"), []byte("secret"), 0o644))
	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(shm, "probe")); !os.IsNotExist(err) {
		t.Fatalf("a task's shared-memory file survived the reset: %v", err)
	}
}

func TestShmDirThatDoesNotExistStopsTheSupervisorAtStart(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	p := startPod(t, "", func(p *pod) { p.env = []string{"POD_SHM_DIR=" + missing} })
	if err := p.waitExit(10 * time.Second); err == nil {
		t.Fatalf("a configured shm dir that cannot be emptied must stop the supervisor")
	}
}

func TestShmDirUnsetLeavesSharedMemoryAlone(t *testing.T) {
	shm := filepath.Join(t.TempDir(), "shm")
	must(t, os.Mkdir(shm, 0o755))
	must(t, os.WriteFile(filepath.Join(shm, "keep"), []byte("x"), 0o644))
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)
	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(shm, "keep")); err != nil {
		t.Fatalf("a directory nobody named was touched: %v", err)
	}
}

// H-2: a manager that keeps saying it does not know the pod ends the
// supervisor after UnknownTimeout; any other answer restarts the clock. It
// runs the supervisor in-process because the binary has no flag for the
// timeout.
func TestUnknownPodTimeoutEndsTheSupervisor(t *testing.T) {
	run := func(t *testing.T, serve func(n int) int) (time.Duration, error) {
		t.Helper()
		var mu sync.Mutex
		polls := 0
		start := time.Now()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				return
			}
			mu.Lock()
			polls++
			n := polls
			mu.Unlock()
			w.WriteHeader(serve(n))
		}))
		defer srv.Close()
		root := t.TempDir()
		for _, d := range []string{"work", "daemon-home", "tmp", "shims"} {
			must(t, os.Mkdir(filepath.Join(root, d), 0o755))
		}
		// A runner that answers every call, which is all this test needs.
		socket := fmt.Sprintf("podsupervisor-unknown-%d-%d", os.Getpid(), time.Now().UnixNano())
		stopRunner := serveRunner(t, socket)
		defer stopRunner()
		script := filepath.Join(root, "daemon.sh")
		must(t, os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 100000\n"), 0o755))
		s, err := podsupervisor.New(podsupervisor.Config{
			DaemonCmd: []string{script}, Env: []string{"PATH=" + os.Getenv("PATH")},
			WorkRoot: filepath.Join(root, "work"), TmpDir: filepath.Join(root, "tmp"), DaemonHome: filepath.Join(root, "daemon-home"),
			ShimDir: filepath.Join(root, "shims"), AgentSocket: socket,
			ControlURL: srv.URL, PollInterval: 20 * time.Millisecond, UnknownTimeout: 300 * time.Millisecond,
		})
		must(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = s.Run(ctx)
		return time.Since(start), err
	}

	t.Run("404 for the whole timeout", func(t *testing.T) {
		took, err := run(t, func(int) int { return http.StatusNotFound })
		if err == nil || took > 4*time.Second {
			t.Fatalf("want an error soon after the timeout, got %v after %s", err, took)
		}
	})
	t.Run("one other answer restarts the clock", func(t *testing.T) {
		// 404s for ~200ms, a single 204, then 404s again: without the reset
		// the supervisor would end at ~300ms.
		took, err := run(t, func(n int) int {
			if n == 10 {
				return http.StatusNoContent
			}
			return http.StatusNotFound
		})
		if err == nil || took < 450*time.Millisecond {
			t.Fatalf("the 204 must restart the clock; ended after %s with %v", took, err)
		}
	})
}
