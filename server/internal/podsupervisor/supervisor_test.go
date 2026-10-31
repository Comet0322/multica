//go:build linux

package podsupervisor_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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
}

func newControlServer(t *testing.T) *controlServer {
	t.Helper()
	c := &controlServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
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

type pod struct {
	t        *testing.T
	ctl      *controlServer
	out      string // test-owned directory, never touched by resets
	work     string
	home     string
	tmp      string
	template string
	args     []string // extra supervisor flags, set by the setup function
	env      []string // extra supervisor environment
	cmd      *exec.Cmd
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
		t:        t,
		ctl:      newControlServer(t),
		out:      filepath.Join(root, "out"),
		work:     filepath.Join(root, "work"),
		home:     filepath.Join(root, "home"),
		tmp:      filepath.Join(root, "tmp"),
		template: filepath.Join(root, "template"),
	}
	for _, d := range []string{p.out, p.work, p.home, p.tmp, p.template} {
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
	args := append([]string{
		"-control-url", p.ctl.srv.URL,
		"-work-root", p.work, "-home", p.home, "-tmpdir", p.tmp,
		"-home-template", p.template,
		"-poll-interval", "50ms",
	}, p.args...)
	p.cmd = exec.Command(supervisorBin, append(args, "--", script)...)
	p.cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "OUT=" + p.out, "MARK=" + mark, "SPAWN=" + map[bool]string{true: "1"}[mark != ""]}, p.env...)
	p.cmd.Stderr = io.Discard
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
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
	for _, want := range []string{"HOME=" + p.home, "TMPDIR=" + p.tmp} {
		if !strings.Contains(second, want+"\n") {
			t.Errorf("environment lacks %s:\n%s", want, second)
		}
	}
}

func TestFailedResetIsReportedAndDoesNotRestartDaemon(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)
	old := p.daemonPids()[0]

	// Make the TMPDIR impossible to clean: it is no longer a directory.
	must(t, os.RemoveAll(p.tmp))
	must(t, os.WriteFile(p.tmp, []byte("not a dir"), 0o644))

	rep := p.ctl.reset(t, "r1")
	if rep.Status != "failed" || rep.Error == "" {
		t.Fatalf("reset report = %+v, want failed with an error", rep)
	}
	time.Sleep(500 * time.Millisecond)
	if n := len(p.daemonPids()); n != 1 {
		t.Errorf("daemon restarted after failed reset: pids %v", p.daemonPids())
	}
	if alive(old) {
		t.Errorf("old daemon still running after failed reset")
	}

	// A later reset retries and recovers once the cause is gone.
	must(t, os.Remove(p.tmp))
	must(t, os.Mkdir(p.tmp, 0o755))
	if rep := p.ctl.reset(t, "r2"); rep.Status != "done" {
		t.Fatalf("retry report = %+v", rep)
	}
	p.waitDaemonStarts(2)
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
