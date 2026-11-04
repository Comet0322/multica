package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const harnessTestIssue = "11111111-1111-1111-1111-111111111111"

type harnessServer struct {
	*httptest.Server
	mu    sync.Mutex
	posts []string // "METHOD path auth task content"
}

// newHarnessServer stands in for the Multica server and records what the host
// side tools send it.
func newHarnessServer(t *testing.T) *harnessServer {
	t.Helper()
	s := &harnessServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.posts = append(s.posts, strings.Join([]string{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Task-ID"), asString(body["content"]) + asString(body["title"])}, " "))
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "33333333-3333-3333-3333-333333333333"})
	}))
	t.Cleanup(s.Close)
	return s
}

func asString(v any) string { s, _ := v.(string); return s }

func (s *harnessServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.posts...)
}

func writeScript(t *testing.T, steps []map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(steps)
	p := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

var harnessScript = []map[string]any{
	{"tool": "write_file", "input": map[string]any{"path": "hello.txt", "content": "from the sandbox\n"}},
	{"tool": "bash", "input": map[string]any{"command": "cat hello.txt && pwd"}},
	{"tool": "issue_create", "input": map[string]any{"title": "follow-up", "description": "found while working"}},
	{"tool": "comment_add", "input": map[string]any{"content": "work finished"}},
	{"say": "all done"},
}

func runHarness(t *testing.T, env map[string]string, cwd string) (Result, []Message) {
	t.Helper()
	b, err := New("harness", Config{Env: env, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sess, err := b.Execute(ctx, "Your assigned issue ID is: "+harnessTestIssue+"\nDo the work.", ExecOptions{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	var msgs []Message
	for m := range sess.Messages {
		msgs = append(msgs, m)
	}
	return <-sess.Result, msgs
}

func harnessEnv(srv *harnessServer, script string) map[string]string {
	return map[string]string{
		"HARNESS_MODEL": "mock", "HARNESS_MOCK_SCRIPT": script,
		"MULTICA_TOKEN": "mat_task_token", "MULTICA_SERVER_URL": srv.URL, "MULTICA_WORKSPACE_ID": "ws-1",
		"MULTICA_AGENT_ID": "agent-1", "MULTICA_TASK_ID": "task-1",
	}
}

func checkHarnessRun(t *testing.T, res Result, msgs []Message, srv *harnessServer) {
	t.Helper()
	if res.Status != "completed" || res.Output != "all done" || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	var types []MessageType
	var sawFile bool
	for _, m := range msgs {
		types = append(types, m.Type)
		if m.Type == MessageToolResult && m.Tool == "bash" && strings.Contains(m.Output, "from the sandbox") {
			sawFile = true
		}
	}
	if !sawFile {
		t.Errorf("bash did not read back the file the model wrote: %+v", msgs)
	}
	if types[0] != MessageToolUse || types[len(types)-1] != MessageText {
		t.Errorf("message stream = %v", types)
	}
	if u := res.Usage["mock"]; u.InputTokens == 0 {
		t.Errorf("usage not reported: %+v", res.Usage)
	}

	got := srv.seen()
	want := []string{
		"POST /api/issues Bearer mat_task_token task-1 follow-up",
		"POST /api/issues/" + harnessTestIssue + "/comments Bearer mat_task_token task-1 work finished",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("server saw\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestHarnessBackendRunsATaskThroughTheBackendInterface(t *testing.T) {
	srv := newHarnessServer(t)
	cwd := t.TempDir()
	env := harnessEnv(srv, writeScript(t, harnessScript))
	env["HARNESS_SANDBOX"] = "local"

	res, msgs := runHarness(t, env, cwd)
	checkHarnessRun(t, res, msgs, srv)

	if b, err := os.ReadFile(filepath.Join(cwd, "hello.txt")); err != nil || string(b) != "from the sandbox\n" {
		t.Errorf("file in the sandbox directory: %q, %v", b, err)
	}
}

// TestHarnessBackendInANetworkLessContainer is the point of the architecture:
// the sandbox has no network at all, yet the task reads its issue and posts to
// Multica, because those calls are made by the host side tools with the task
// token, which the container never receives.
func TestHarnessBackendInANetworkLessContainer(t *testing.T) {
	image := os.Getenv("HARNESS_DOCKER_IMAGE")
	if image == "" {
		t.Skip("HARNESS_DOCKER_IMAGE not set")
	}
	before := containerCount(t, image)

	srv := newHarnessServer(t)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte("# brief\nuse the tools"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := append([]map[string]any{{"tool": "bash", "input": map[string]any{"command": "cat CLAUDE.md > /dev/null && test -f /work/CLAUDE.md"}}}, harnessScript...)
	env := harnessEnv(srv, writeScript(t, script))
	env["HARNESS_SANDBOX_IMAGE"] = image

	res, msgs := runHarness(t, env, cwd)
	checkHarnessRun(t, res, msgs, srv)

	for _, m := range msgs {
		if m.Type == MessageToolResult && m.Tool == "bash" && strings.Contains(m.Output, "exit code") {
			t.Errorf("the seeded brief was not in the sandbox: %+v", m)
		}
		if strings.Contains(m.Output, "mat_task_token") || strings.Contains(m.Content, "mat_task_token") {
			t.Errorf("the task token leaked into the conversation: %+v", m)
		}
	}
	if after := containerCount(t, image); after != before {
		t.Errorf("sandbox container was not removed: %d running before, %d after", before, after)
	}
}

func containerCount(t *testing.T, image string) int {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-q", "--filter", "ancestor="+image).Output()
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(out)))
}

func TestHarnessBackendRefusesAnUnimplementedModel(t *testing.T) {
	b, _ := New("harness", Config{Env: map[string]string{"HARNESS_MODEL": "claude"}, Logger: slog.Default()})
	if _, err := b.Execute(context.Background(), "x", ExecOptions{}); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("err = %v", err)
	}
}
