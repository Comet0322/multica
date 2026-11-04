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

// fakeAnthropic plays the Messages API: step n answers call n, and every
// request is kept so a test can check what the model was shown.
type fakeAnthropic struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
	keys     []string
}

func newFakeAnthropic(t *testing.T, steps []func(prev map[string]any) string) *fakeAnthropic {
	t.Helper()
	f := &fakeAnthropic{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.keys = append(f.keys, r.Header.Get("x-api-key"))
		n := len(f.requests)
		f.mu.Unlock()
		if n > len(steps) {
			http.Error(w, `{"error":{"type":"invalid_request_error","message":"script exhausted"}}`, 400)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, steps[n-1](req))
	}))
	t.Cleanup(f.Close)
	return f
}

func toolUse(id, name string, input map[string]any) func(map[string]any) string {
	return func(map[string]any) string {
		in, _ := json.Marshal(input)
		return `{"model":"claude-haiku-5-5","stop_reason":"tool_use","content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + string(in) + `}],"usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":40}}`
	}
}

func finalAnswer(text string) func(map[string]any) string {
	return func(map[string]any) string {
		return `{"model":"claude-haiku-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"` + text + `"}],"usage":{"input_tokens":60,"output_tokens":5}}`
	}
}

// lastToolResult returns the tool_result text the model was shown most recently.
func lastToolResult(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		return ""
	}
	content, _ := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	for _, c := range content {
		if b, _ := c.(map[string]any); b["type"] == "tool_result" {
			s, _ := b["content"].(string)
			return s
		}
	}
	return ""
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestHarnessBackendClonesEditsAndDeliversWithNoNetwork is the whole point in
// one run: a model, through the real Messages API wire format, gets code into a
// network-less container, changes it there, and the change reaches the remote
// and Multica, neither of which the container can see.
func TestHarnessBackendClonesEditsAndDeliversWithNoNetwork(t *testing.T) {
	image := os.Getenv("HARNESS_DOCKER_IMAGE")
	if image == "" {
		t.Skip("HARNESS_DOCKER_IMAGE not set")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))

	seed := t.TempDir()
	gitIn(t, seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "hello.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-q", "-m", "initial")
	remote := filepath.Join(t.TempDir(), "app.git")
	gitIn(t, filepath.Dir(remote), "clone", "-q", "--bare", seed, remote)

	api := newFakeAnthropic(t, []func(map[string]any) string{
		toolUse("t1", "git_clone", map[string]any{"repo": remote}),
		toolUse("t2", "bash", map[string]any{"command": "cd app && echo \"print('bye')\" >> hello.py && git add -A && git commit -q -m 'add bye' && git log --oneline | head -3"}),
		toolUse("t3", "bash", map[string]any{"command": "cd app && git push origin HEAD 2>&1; echo push-exit=$?"}),
		toolUse("t4", "deliver", map[string]any{"path": "app", "branch": "agent/task-1/bye"}),
		toolUse("t5", "comment_add", map[string]any{"content": "delivered agent/task-1/bye"}),
		finalAnswer("delivered"),
	})
	srv := newHarnessServer(t)
	cwd := t.TempDir()
	env := map[string]string{
		"HARNESS_MODEL": "anthropic", "HARNESS_ANTHROPIC_API_KEY": "sk-fake", "HARNESS_ANTHROPIC_BASE_URL": api.URL,
		"HARNESS_SANDBOX_IMAGE": image, "HARNESS_GIT_ALLOWED_REPOS": remote,
		"MULTICA_TOKEN": "mat_task_token", "MULTICA_SERVER_URL": srv.URL, "MULTICA_WORKSPACE_ID": "ws-1",
		"MULTICA_AGENT_ID": "agent-1", "MULTICA_TASK_ID": "task-1",
	}

	res, _ := runHarness(t, env, cwd)
	if res.Status != "completed" || res.Output != "delivered" {
		t.Fatalf("result = %+v", res)
	}

	if got := gitIn(t, remote, "log", "-1", "--format=%s", "agent/task-1/bye"); got != "add bye" {
		t.Errorf("remote branch head = %q", got)
	}
	if got := gitIn(t, remote, "log", "-1", "--format=%an", "agent/task-1/bye"); got != "Multica Agent" {
		t.Errorf("author = %q", got)
	}
	if seen := srv.seen(); len(seen) != 1 || !strings.Contains(seen[0], "delivered agent/task-1/bye") {
		t.Errorf("Multica saw %q", seen)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.requests) != 6 {
		t.Fatalf("model calls = %d, want 6", len(api.requests))
	}
	for _, k := range api.keys {
		if k != "sk-fake" {
			t.Errorf("api key sent = %q", k)
		}
	}
	first := api.requests[0]
	sys, _ := json.Marshal(first["system"])
	tl, _ := json.Marshal(first["tools"])
	for _, want := range []string{"no network", "deliver"} {
		if !strings.Contains(string(sys), want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	for _, name := range []string{"bash", "git_clone", "deliver", "comment_add", "issue_create"} {
		if !strings.Contains(string(tl), `"name":"`+name+`"`) {
			t.Errorf("tool %s not offered", name)
		}
	}
	if got := lastToolResult(api.requests[1]); !strings.Contains(got, "cloned") {
		t.Errorf("model was told %q after git_clone", got)
	}
	// The container has no route out, so a push from inside must fail, and the
	// model is shown that it failed.
	if got := lastToolResult(api.requests[3]); strings.Contains(got, "push-exit=0") {
		t.Errorf("a push from the sandbox succeeded: %q", got)
	}
	if got := lastToolResult(api.requests[4]); !strings.Contains(got, "pushed agent/task-1/bye") {
		t.Errorf("model was told %q after deliver", got)
	}
	if res.Usage["claude-haiku-5-5"].CacheReadTokens == 0 {
		t.Errorf("cache usage not reported: %+v", res.Usage)
	}
}

// TestHarnessBackendWithARealModel lets claude-haiku-5-5 do the task itself, in
// a network-less container, and checks only the outcome. It spends real tokens,
// so it runs only when told where a Messages API is:
//
//	HARNESS_LIVE_BASE_URL   e.g. a LiteLLM proxy
//	HARNESS_LIVE_KEY_FILE   a file holding the key (so it is not in the command line)
//	HARNESS_DOCKER_IMAGE    sandbox image with git and tar
func TestHarnessBackendWithARealModel(t *testing.T) {
	base, keyFile, image := os.Getenv("HARNESS_LIVE_BASE_URL"), os.Getenv("HARNESS_LIVE_KEY_FILE"), os.Getenv("HARNESS_DOCKER_IMAGE")
	if base == "" || keyFile == "" || image == "" {
		t.Skip("HARNESS_LIVE_BASE_URL, HARNESS_LIVE_KEY_FILE and HARNESS_DOCKER_IMAGE are needed")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))

	seed := t.TempDir()
	gitIn(t, seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "hello.py"), []byte("def greet(name):\n    return \"hello \" + name\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-q", "-m", "initial")
	remote := filepath.Join(t.TempDir(), "app.git")
	gitIn(t, filepath.Dir(remote), "clone", "-q", "--bare", seed, remote)

	srv := newHarnessServer(t)
	env := map[string]string{
		"HARNESS_MODEL": "anthropic", "HARNESS_ANTHROPIC_API_KEY": strings.TrimSpace(string(key)), "HARNESS_ANTHROPIC_BASE_URL": base,
		"HARNESS_MODEL_ID": "claude-haiku-5-5", "HARNESS_SANDBOX_IMAGE": image, "HARNESS_GIT_ALLOWED_REPOS": remote,
		"MULTICA_TOKEN": "mat_task_token", "MULTICA_SERVER_URL": srv.URL, "MULTICA_WORKSPACE_ID": "ws-1",
		"MULTICA_AGENT_ID": "agent-1", "MULTICA_TASK_ID": "task-live",
	}
	b, err := New("harness", Config{Env: env, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	prompt := "Your assigned issue ID is: " + harnessTestIssue + "\n\n" +
		"Task: the repository " + remote + " has hello.py with a greet(name) function. " +
		"Add a function farewell(name) that returns \"goodbye \" + name. Commit it in one commit, " +
		"deliver it to the branch agent/task-live/farewell, then post a comment on your issue that names the branch. Be brief."
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sess, err := b.Execute(ctx, prompt, ExecOptions{Cwd: t.TempDir(), MaxTurns: 25})
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for m := range sess.Messages {
		switch m.Type {
		case MessageToolUse:
			steps = append(steps, "-> "+m.Tool)
		case MessageToolResult:
			steps = append(steps, "   "+firstLine(m.Output, 100))
		case MessageText:
			steps = append(steps, "   say: "+firstLine(m.Content, 100))
		}
	}
	res := <-sess.Result
	t.Logf("status=%s turns/usage=%+v\n%s", res.Status, res.Usage, strings.Join(steps, "\n"))

	if res.Status != "completed" {
		t.Fatalf("result = %+v", res)
	}
	if got := gitIn(t, remote, "show", "agent/task-live/farewell:hello.py"); !strings.Contains(got, "def farewell") || !strings.Contains(got, "goodbye") || !strings.Contains(got, "def greet") {
		t.Errorf("delivered hello.py:\n%s", got)
	}
	if gitIn(t, remote, "rev-parse", "main") != gitIn(t, remote, "rev-parse", "main") || gitIn(t, remote, "rev-list", "--count", "main..agent/task-live/farewell") != "1" {
		t.Errorf("want exactly one new commit on the branch")
	}
	var commented bool
	for _, p := range srv.seen() {
		if strings.Contains(p, "/comments") && strings.Contains(p, "agent/task-live/farewell") {
			commented = true
		}
	}
	if !commented {
		t.Errorf("no comment naming the branch reached Multica: %q", srv.seen())
	}
}

func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

func TestHarnessBackendRefusesAnUnimplementedModel(t *testing.T) {
	b, _ := New("harness", Config{Env: map[string]string{"HARNESS_MODEL": "claude"}, Logger: slog.Default()})
	if _, err := b.Execute(context.Background(), "x", ExecOptions{}); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("err = %v", err)
	}
}
