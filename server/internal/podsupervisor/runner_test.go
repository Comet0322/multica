//go:build linux

package podsupervisor_test

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// control sends one operation to the runner's control socket, as the
// supervisor does, and returns the reply. ok is false when the runner closed
// the connection without answering.
func control(t *testing.T, socket, op string) (reply map[string]any, ok bool) {
	t.Helper()
	conn, err := net.DialTimeout("unix", "@"+socket+"-ctl", 5*time.Second)
	if err != nil {
		t.Fatalf("connect to the runner: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if err := json.NewEncoder(conn).Encode(map[string]string{"op": op}); err != nil {
		return nil, false
	}
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return nil, false
	}
	return reply, true
}

// runShim runs the shim of an agent CLI the way the daemon does and returns its
// exit status and stderr.
func runShim(t *testing.T, p *pod, name string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(p.shimDir, name), args...)
	cmd.Dir = p.work
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "POD_AGENT_SOCKET=" + p.socket}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run the shim: %v", err)
	}
	return code, strings.TrimSpace(out.String()), errb.String()
}

func TestRunnerOnlyAnswersTheDaemonsUser(t *testing.T) {
	root := t.TempDir()
	socket := "podsupervisor-uid-" + strconv.Itoa(os.Getpid())
	dirs := map[string]string{}
	for _, d := range []string{"work", "tmp", "home", "shims"} {
		dirs[d] = filepath.Join(root, d)
		must(t, os.Mkdir(dirs[d], 0o755))
	}
	// The runner is told another user is the daemon's: the test user is a
	// stranger to it, like an agent is to the real one.
	cmd := exec.Command(supervisorBin, "-role", "runner", "-agent-socket", socket, "-daemon-uid", strconv.Itoa(os.Getuid()+1),
		"-work-root", dirs["work"], "-tmpdir", dirs["tmp"], "-home", dirs["home"], "-shim-dir", dirs["shims"])
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	must(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	eventually(t, 10*time.Second, "the runner to listen", func() bool {
		c, err := net.Dial("unix", "@"+socket+"-ctl")
		if err != nil {
			return false
		}
		c.Close()
		return true
	})
	for _, op := range []string{"open", "close", "status"} {
		if reply, ok := control(t, socket, op); ok {
			t.Errorf("the runner answered %s for a user that is not the daemon's: %v", op, reply)
		}
	}
	conn, err := net.Dial("unix", "@"+socket)
	must(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte{0})
	if n, err := conn.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("the launch socket answered a user that is not the daemon's: %d bytes, %v", n, err)
	}
}

func TestClosedRunnerRefusesAgentsUntilOpened(t *testing.T) {
	p := startSplitPod(t, daemonRunning(`:`), echoAgent)
	p.waitDaemonStarts(1)
	eventually(t, 10*time.Second, "the runner to be open", func() bool {
		r, ok := control(t, p.socket, "status")
		return ok && r["open"] == true
	})

	if _, ok := control(t, p.socket, "close"); !ok {
		t.Fatalf("close was not answered")
	}
	code, _, stderr := runShim(t, p.pod, "fakeagent", "x")
	if code != 126 || !strings.Contains(stderr, "not accepting") {
		t.Errorf("a closed runner started an agent: status %d, stderr %q", code, stderr)
	}
	if _, ok := control(t, p.socket, "open"); !ok {
		t.Fatalf("open was not answered")
	}
	code, stdout, stderr := runShim(t, p.pod, "fakeagent", "x")
	if code != 7 || !strings.Contains(stdout, "args:1 x|") {
		t.Errorf("an open runner did not run the agent: status %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestSupervisorReopensARunnerThatWasClosedUnderTheDaemon(t *testing.T) {
	p := startSplitPod(t, daemonRunning(`:`), echoAgent)
	p.waitDaemonStarts(1)
	if _, ok := control(t, p.socket, "close"); !ok {
		t.Fatalf("close was not answered")
	}
	// A runner that restarts comes back closed; the daemon is still running.
	eventually(t, 15*time.Second, "the supervisor to open the runner again", func() bool {
		r, ok := control(t, p.socket, "status")
		return ok && r["open"] == true
	})
}

func TestCloseKillsEveryAgentProcessAndEmptiesTheAgentsHome(t *testing.T) {
	p := startSplitPod(t, daemonRunning(`fakeagent hang & echo $! > "$OUT/shim.pid"`), longAgent)
	agent := atoi(t, p.waitOutFile("agent.pid"))
	child := atoi(t, p.waitOutFile("agent.child"))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL); _ = syscall.Kill(agent, syscall.SIGKILL) })
	must(t, os.WriteFile(filepath.Join(p.home, "left-by-the-agent"), []byte("x"), 0o600))
	must(t, os.WriteFile(filepath.Join(p.work, "left-in-the-work-root"), []byte("x"), 0o600))

	if _, ok := control(t, p.socket, "close"); !ok {
		t.Fatalf("close was not answered")
	}
	if alive(agent) || alive(child) {
		t.Errorf("agent %d / child %d survived the close", agent, child)
	}
	for _, f := range []string{filepath.Join(p.home, "left-by-the-agent"), filepath.Join(p.work, "left-in-the-work-root")} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s survived the close", f)
		}
	}
}
