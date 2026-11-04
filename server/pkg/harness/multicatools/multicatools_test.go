package multicatools_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/pkg/harness/multicatools"
	"github.com/multica-ai/multica/server/pkg/harness/tools"
)

const taskIssue = "11111111-1111-1111-1111-111111111111"

type call struct {
	Method, Path, Auth, Workspace, Agent, Task string
	Body                                       map[string]any
}

// fakeServer records what the tools send and answers from a table.
type fakeServer struct {
	*httptest.Server
	mu    sync.Mutex
	calls []call
	reply func(r *http.Request) (int, any)
}

func newFake(t *testing.T, reply func(r *http.Request) (int, any)) *fakeServer {
	t.Helper()
	f := &fakeServer{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := call{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"),
			Workspace: r.Header.Get("X-Workspace-ID"), Agent: r.Header.Get("X-Agent-ID"), Task: r.Header.Get("X-Task-ID")}
		_ = json.Unmarshal(raw, &c.Body)
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		status, body := reply(r)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) seen() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func registry(f *fakeServer) *tools.Registry {
	c := &multicatools.Client{BaseURL: f.URL, Token: "mat_task", WorkspaceID: "ws-1", AgentID: "agent-1", TaskID: "task-1"}
	return tools.NewRegistry(multicatools.Tools(c, multicatools.NewScope(taskIssue))...)
}

func run(t *testing.T, r *tools.Registry, name string, in map[string]any) tools.Result {
	t.Helper()
	res, err := r.Run(context.Background(), name, in)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func TestEveryCallCarriesTheTaskIdentity(t *testing.T) {
	f := newFake(t, func(*http.Request) (int, any) { return 200, map[string]any{"id": taskIssue} })
	r := registry(f)

	run(t, r, "issue_get", nil)
	run(t, r, "comment_add", map[string]any{"content": "done"})

	for _, c := range f.seen() {
		if c.Auth != "Bearer mat_task" || c.Workspace != "ws-1" || c.Agent != "agent-1" || c.Task != "task-1" {
			t.Errorf("%s %s sent the wrong identity: %+v", c.Method, c.Path, c)
		}
	}
	got := f.seen()
	if got[0].Method != "GET" || got[0].Path != "/api/issues/"+taskIssue {
		t.Errorf("issue_get hit %s %s", got[0].Method, got[0].Path)
	}
	if got[1].Method != "POST" || got[1].Path != "/api/issues/"+taskIssue+"/comments" || got[1].Body["content"] != "done" {
		t.Errorf("comment_add sent %+v", got[1])
	}
}

func TestOtherIssuesAreRefusedWithoutAskingTheServer(t *testing.T) {
	f := newFake(t, func(*http.Request) (int, any) { return 200, map[string]any{} })
	r := registry(f)
	other := "22222222-2222-2222-2222-222222222222"

	for name, in := range map[string]map[string]any{
		"issue_get":    {"issue_id": other},
		"issue_update": {"issue_id": other, "status": "done"},
		"comment_list": {"issue_id": other},
		"comment_add":  {"issue_id": other, "content": "x"},
		"issue_create": {"title": "x", "parent_id": other},
	} {
		res := run(t, r, name, in)
		if !res.IsError || !strings.Contains(res.Output, "outside this task") {
			t.Errorf("%s on another issue: %+v", name, res)
		}
	}
	// A path-shaped id must not be able to address something else either.
	if res := run(t, r, "issue_get", map[string]any{"issue_id": taskIssue + "/../" + other}); !res.IsError {
		t.Errorf("a traversal id was allowed: %+v", res)
	}
	if n := len(f.seen()); n != 0 {
		t.Errorf("refused calls reached the server %d times", n)
	}
}

func TestASubIssueTheTaskCreatedBecomesPartOfItsScope(t *testing.T) {
	child := "33333333-3333-3333-3333-333333333333"
	f := newFake(t, func(r *http.Request) (int, any) {
		if r.Method == "POST" && r.URL.Path == "/api/issues" {
			return 201, map[string]any{"id": child}
		}
		return 200, map[string]any{"id": child}
	})
	r := registry(f)

	if res := run(t, r, "comment_add", map[string]any{"issue_id": child, "content": "early"}); !res.IsError {
		t.Fatalf("the child was usable before it existed: %+v", res)
	}
	if res := run(t, r, "issue_create", map[string]any{"title": "child", "description": "d", "priority": "high"}); res.IsError {
		t.Fatalf("issue_create: %+v", res)
	}
	create := f.seen()[0]
	if create.Body["parent_issue_id"] != taskIssue || create.Body["title"] != "child" || create.Body["priority"] != "high" {
		t.Errorf("create body = %+v", create.Body)
	}
	for _, name := range []string{"comment_add", "issue_update"} {
		in := map[string]any{"issue_id": child, "content": "now", "status": "in_progress"}
		if res := run(t, r, name, in); res.IsError {
			t.Errorf("%s on the created child: %+v", name, res)
		}
	}
	if res := run(t, r, "issue_update", map[string]any{"issue_id": child, "status": "done"}); res.IsError {
		t.Errorf("update child: %+v", res)
	} else if last := f.seen()[len(f.seen())-1]; last.Method != "PUT" || last.Body["status"] != "done" {
		t.Errorf("update sent %+v", last)
	}
}

func TestServerRefusalsReachTheModelButOutagesEndTheTask(t *testing.T) {
	status := 403
	f := newFake(t, func(*http.Request) (int, any) { return status, map[string]any{"error": "no"} })
	r := registry(f)

	if res := run(t, r, "issue_get", nil); !res.IsError || !strings.Contains(res.Output, "403") {
		t.Errorf("a 4xx should come back as an error result: %+v", res)
	}
	status = 503
	if _, err := r.Run(context.Background(), "issue_get", nil); err == nil {
		t.Error("a 5xx should be a tool failure, not an answer")
	}
}

func TestUpdateWithNothingToChangeIsRejected(t *testing.T) {
	f := newFake(t, func(*http.Request) (int, any) { return 200, map[string]any{} })
	if res := run(t, registry(f), "issue_update", map[string]any{}); !res.IsError || len(f.seen()) != 0 {
		t.Errorf("empty update: %+v, %d calls", res, len(f.seen()))
	}
}
