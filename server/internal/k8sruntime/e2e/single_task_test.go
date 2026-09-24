// Package e2e drives the sandbox runner through the real relay against a fake
// Multica server, with a fake claude CLI, to prove the Pod-side pipeline works
// without the daemon credential.
package e2e

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/k8sruntime/relay"
)

// TestMain lets the test binary serve the private execution-environment helper
// protocol, exactly as the runner binary does, because the daemon re-executes
// its own executable to prepare each task's environment.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == execenv.PreparationHelperArg {
		if err := execenv.RunPreparationHelper(os.Stdin, os.Stdout, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

const (
	daemonToken = "mul_controller_credential"
	taskToken   = "mat_task_token"
)

type call struct{ method, path, auth string }

func TestRunnerCompletesTaskThroughRelayWithoutDaemonCredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	// Fake claude: reads the prompt, emits a successful stream-json result.
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo '2.1.0 (Claude Code)'; exit 0; fi\n" +
		"IFS= read -r _\n" +
		`echo '{"type":"system","subtype":"init","session_id":"sess-1"}'` + "\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","result":"all done"}'` + "\n"
	writeExec(t, filepath.Join(bin, "claude"), script)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Fake Multica server: records every call and its Authorization header.
	var mu sync.Mutex
	var calls []call
	var completed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path, r.Header.Get("Authorization")})
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/fail") {
			b, _ := io.ReadAll(r.Body)
			t.Logf("fail body: %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"status":"running"}`))
		case strings.HasSuffix(r.URL.Path, "/complete"):
			mu.Lock()
			completed = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/start"):
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()

	// The real relay in front of it, exactly as the controller runs it.
	up, _ := url.Parse(server.URL)
	rl, err := relay.New(up, daemonToken)
	if err != nil {
		t.Fatal(err)
	}
	rl.Register(taskToken, relay.Task{TaskID: "task-1", RuntimeID: "rt-1", WorkspaceID: "ws-1"})
	front := httptest.NewServer(rl)
	defer front.Close()

	// Pod-side config: everything points at the relay.
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("MULTICA_SERVER_URL", front.URL)
	t.Setenv("MULTICA_DAEMON_ID", "k8s-test")
	t.Setenv("MULTICA_WORKSPACES_ROOT", filepath.Join(root, "ws"))
	t.Setenv("MULTICA_DAEMON_AUTO_UPDATE", "false")
	t.Setenv("IS_SANDBOX", "1") // the test container runs as root; real Pods run as uid 1000
	cfg, err := daemon.LoadConfig(daemon.Overrides{HealthPort: 0})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.HealthPort = freePort(t)

	task := daemon.Task{
		ID: "task-1", WorkspaceID: "ws-1", RuntimeID: "rt-1", AgentID: "agent-1",
		AuthToken: taskToken, StartClaimSupported: true, DispatchedAt: "2026-01-01T00:00:00Z",
		Agent: &daemon.AgentData{ID: "agent-1", Name: "sandbox-agent"},
	}
	rt := daemon.Runtime{ID: "rt-1", Provider: "claude", Name: "claude", Status: "online"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := daemon.RunSingleTask(ctx, cfg, task, rt, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("RunSingleTask: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !completed {
		t.Fatalf("task was never completed; calls:\n%s", dump(calls))
	}
	var started bool
	for _, c := range calls {
		if strings.HasPrefix(c.path, "/api/daemon/") {
			if c.auth != "Bearer "+daemonToken {
				t.Errorf("daemon call %s %s reached the server with auth %q; the relay must swap in the daemon credential", c.method, c.path, c.auth)
			}
		}
		if strings.HasSuffix(c.path, "/start") {
			started = true
		}
		if strings.Contains(c.auth, taskToken) && strings.HasPrefix(c.path, "/api/daemon/") {
			t.Errorf("task token leaked to a daemon endpoint: %s", c.path)
		}
	}
	if !started {
		t.Errorf("start was never called; calls:\n%s", dump(calls))
	}
	// The runner must never have used the daemon credential itself: it only
	// ever knew the task token, so reaching the server with it required the relay.
	if os.Getenv("MULTICA_CONTROLLER_TOKEN") != "" {
		t.Error("test environment must not contain the controller token")
	}
}

func dump(calls []call) string {
	var b strings.Builder
	for _, c := range calls {
		b.WriteString("  " + c.method + " " + c.path + " [" + c.auth + "]\n")
	}
	return b.String()
}

func writeExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
