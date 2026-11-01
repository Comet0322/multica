package gitgateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

// mcpFake is a GitLab that answers the user, membership, branch, merge
// request and note calls the MCP tools make.
type mcpFake struct {
	mu    sync.Mutex
	users map[string]int64 // email -> id
	level map[int64]int
	notes []string
}

func newMCPHarness(t *testing.T) (*mcpFake, *gitgateway.TokenRegistry, *httptest.Server) {
	t.Helper()
	f := &mcpFake{users: map[string]int64{}, level: map[int64]int{}}
	gl := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(gl.Close)
	tokens := gitgateway.NewTokenRegistry()
	g, err := gitgateway.New(gitgateway.Config{GitLabURL: gl.URL, AdminTicket: adminTicket, Resolver: tokens})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.MCPHandler())
	t.Cleanup(srv.Close)
	return f, tokens, srv
}

func (f *mcpFake) addUser(email string, id int64, level int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[email] = id
	f.level[id] = level
}

func (f *mcpFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("PRIVATE-TOKEN") != adminTicket {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4")
	switch {
	case path == "/users":
		out := []map[string]any{}
		for email, id := range f.users {
			if strings.Contains(email, r.URL.Query().Get("search")) {
				out = append(out, map[string]any{"id": id, "state": "active", "email": email})
			}
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(path, "/projects/group%2Fproj/members/all/"):
		id := strings.TrimPrefix(path, "/projects/group%2Fproj/members/all/")
		for _, uid := range f.users {
			if itoa(uid) == id && f.level[uid] > 0 {
				json.NewEncoder(w).Encode(map[string]any{"access_level": f.level[uid]})
				return
			}
		}
		http.Error(w, "{}", http.StatusNotFound)
	case path == "/projects/group%2Fproj/repository/branches":
		json.NewEncoder(w).Encode([]map[string]any{
			{"name": "main", "default": true, "commit": map[string]any{"id": "secret-ish"}},
			{"name": "feature/x", "default": false},
		})
	case path == "/projects/group%2Fproj/merge_requests":
		json.NewEncoder(w).Encode([]map[string]any{{"iid": 3, "title": "Add x", "state": r.URL.Query().Get("state"), "author": map[string]any{"username": "alice", "email": "hidden"}}})
	case path == "/projects/group%2Fproj/merge_requests/3":
		json.NewEncoder(w).Encode(map[string]any{"iid": 3, "title": "Add x", "state": "opened", "source_branch": "feature/x"})
	case path == "/projects/group%2Fproj/merge_requests/3/notes" && r.Method == http.MethodPost:
		var b struct{ Body string }
		json.NewDecoder(r.Body).Decode(&b)
		f.notes = append(f.notes, b.Body)
		json.NewEncoder(w).Encode(map[string]any{"id": 11, "body": b.Body})
	default:
		http.Error(w, "{}", http.StatusNotFound)
	}
}

// rpc posts one JSON-RPC message and returns the HTTP status and decoded reply.
func rpc(t *testing.T, srv *httptest.Server, ticket, method string, params any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(string(body)))
	if ticket != "" {
		req.Header.Set("Authorization", "Bearer "+ticket)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func call(t *testing.T, srv *httptest.Server, ticket, tool string, args map[string]any) (text string, isError bool) {
	t.Helper()
	code, out := rpc(t, srv, ticket, "tools/call", map[string]any{"name": tool, "arguments": args})
	if code != 200 {
		t.Fatalf("%s: status %d", tool, code)
	}
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no result in %v", tool, out)
	}
	content := res["content"].([]any)[0].(map[string]any)
	return content["text"].(string), res["isError"] == true
}

func TestMCPHandshakeAndToolList(t *testing.T) {
	f, tokens, srv := newMCPHarness(t)
	f.addUser("alice@example.com", 7, gitgateway.LevelReporter)
	tok, _ := tokens.Issue(gitgateway.Identity{TaskID: "t1", Initiator: "alice@example.com"})

	code, out := rpc(t, srv, tok, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	res := out["result"].(map[string]any)
	if code != 200 || res["protocolVersion"] != "2025-06-18" || res["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("initialize = %d %v", code, out)
	}
	_, out = rpc(t, srv, tok, "tools/list", nil)
	names := map[string]bool{}
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"list_branches", "get_merge_request", "list_merge_requests", "comment_on_merge_request"} {
		if !names[n] {
			t.Errorf("tool %s not listed: %v", n, names)
		}
	}

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	if resp, err := srv.Client().Do(req); err != nil || resp.StatusCode != 202 {
		t.Fatalf("notification = %v %v", resp, err)
	}
}

func TestMCPReporterReadsAndComments(t *testing.T) {
	f, tokens, srv := newMCPHarness(t)
	f.addUser("alice@example.com", 7, gitgateway.LevelReporter)
	tok, _ := tokens.Issue(gitgateway.Identity{TaskID: "t1", Initiator: "alice@example.com"})

	text, isErr := call(t, srv, tok, "list_branches", map[string]any{"project": "Group/Proj.git"})
	if isErr || !strings.Contains(text, `"main"`) || strings.Contains(text, "secret-ish") {
		t.Fatalf("list_branches = %v %q", isErr, text)
	}
	text, isErr = call(t, srv, tok, "get_merge_request", map[string]any{"project": "group/proj", "iid": 3})
	if isErr || !strings.Contains(text, "feature/x") {
		t.Fatalf("get_merge_request = %v %q", isErr, text)
	}
	text, isErr = call(t, srv, tok, "list_merge_requests", map[string]any{"project": "group/proj"})
	if isErr || !strings.Contains(text, `"opened"`) || strings.Contains(text, "hidden") || !strings.Contains(text, "alice") {
		t.Fatalf("list_merge_requests = %v %q", isErr, text)
	}
	if text, isErr = call(t, srv, tok, "get_merge_request", map[string]any{"project": "group/proj", "iid": 99}); !isErr {
		t.Fatalf("missing MR should be a tool error, got %q", text)
	}
	if _, isErr = call(t, srv, tok, "comment_on_merge_request", map[string]any{"project": "group/proj", "iid": 3, "body": "looks good"}); isErr {
		t.Fatal("reporter comment refused")
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "looks good") || !strings.Contains(f.notes[0], "alice@example.com") {
		t.Fatalf("note = %q; want the text attributed to the initiator", f.notes)
	}
}

func TestMCPNeedsReporterAccess(t *testing.T) {
	f, tokens, srv := newMCPHarness(t)
	f.addUser("guest@example.com", 2, 10)
	f.addUser("stranger@example.com", 3, 0)
	for _, email := range []string{"guest@example.com", "stranger@example.com", "nobody@example.com"} {
		tok, _ := tokens.Issue(gitgateway.Identity{TaskID: email, Initiator: email})
		for tool, args := range map[string]map[string]any{
			"list_branches":            {"project": "group/proj"},
			"comment_on_merge_request": {"project": "group/proj", "iid": 3, "body": "hi"},
		} {
			if text, isErr := call(t, srv, tok, tool, args); !isErr || !strings.HasPrefix(text, "gateway:") {
				t.Errorf("%s %s = %v %q; want a refusal", email, tool, isErr, text)
			}
		}
	}
	if len(f.notes) != 0 {
		t.Fatalf("refused comments were posted: %q", f.notes)
	}
}

func TestMCPRejectsMissingExpiredAndForeignTokens(t *testing.T) {
	f, tokens, srv := newMCPHarness(t)
	f.addUser("alice@example.com", 7, gitgateway.LevelReporter)
	tok, _ := tokens.Issue(gitgateway.Identity{TaskID: "t1", Initiator: "alice@example.com"})

	if code, _ := rpc(t, srv, "", "tools/list", nil); code != 401 {
		t.Errorf("no token = %d", code)
	}
	if code, _ := rpc(t, srv, "gwt_forged", "tools/list", nil); code != 401 {
		t.Errorf("forged token = %d", code)
	}
	tokens.Revoke("t1")
	if code, _ := rpc(t, srv, tok, "tools/list", nil); code != 401 {
		t.Errorf("revoked token = %d", code)
	}
	// A token issued to another task answers with that task's own identity,
	// so a stranger's token carries no access to alice's projects.
	other, _ := tokens.Issue(gitgateway.Identity{TaskID: "t2", Initiator: "mallory@example.com"})
	if _, isErr := call(t, srv, other, "list_branches", map[string]any{"project": "group/proj"}); !isErr {
		t.Error("another person's token read alice's project")
	}
}

func TestMCPBadArgumentsAreRefusedBeforeGitLab(t *testing.T) {
	f, tokens, srv := newMCPHarness(t)
	f.addUser("alice@example.com", 7, gitgateway.LevelReporter)
	tok, _ := tokens.Issue(gitgateway.Identity{TaskID: "t1", Initiator: "alice@example.com"})
	for _, args := range []map[string]any{
		{"project": "../etc"}, {"project": "group"}, {"project": "group/proj/../x"}, {"project": ""},
	} {
		if _, isErr := call(t, srv, tok, "list_branches", args); !isErr {
			t.Errorf("%v accepted", args)
		}
	}
	if _, isErr := call(t, srv, tok, "comment_on_merge_request", map[string]any{"project": "group/proj", "iid": 3, "body": " "}); !isErr {
		t.Error("empty comment accepted")
	}
	if _, isErr := call(t, srv, tok, "list_merge_requests", map[string]any{"project": "group/proj", "state": "x"}); !isErr {
		t.Error("bad state accepted")
	}
	if code, out := rpc(t, srv, tok, "tools/call", map[string]any{"name": "delete_branch", "arguments": map[string]any{}}); code != 200 || out["error"] == nil {
		t.Errorf("unknown tool = %d %v", code, out)
	}
	if code, out := rpc(t, srv, tok, "resources/list", nil); code != 200 || out["error"] == nil {
		t.Errorf("unknown method = %d %v", code, out)
	}
}

func TestInjectMCPConfigKeepsUserServersAndCannotBeShadowed(t *testing.T) {
	task := json.RawMessage(`{"id":"t1","agent":{"name":"a","mcp_config":{"mcpServers":{
		"mine":{"type":"http","url":"https://mine.example/mcp"},
		"GitLab-Gateway":{"type":"http","url":"https://evil.example/mcp"}},"other":1}}}`)
	out, err := gitgateway.InjectMCPConfig(task, "https://gw.example/mcp", "gwt_abc")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID    string
		Agent struct {
			Name      string
			McpConfig struct {
				McpServers map[string]struct {
					Type, URL string
					Headers   map[string]string
				}
				Other int
			} `json:"mcp_config"`
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	srvs := got.Agent.McpConfig.McpServers
	gw := srvs[gitgateway.MCPServerName]
	if got.ID != "t1" || got.Agent.Name != "a" || got.Agent.McpConfig.Other != 1 {
		t.Fatalf("other fields lost: %s", out)
	}
	if gw.Type != "http" || gw.URL != "https://gw.example/mcp" || gw.Headers["Authorization"] != "Bearer gwt_abc" {
		t.Fatalf("gateway entry = %+v", gw)
	}
	if srvs["mine"].URL != "https://mine.example/mcp" || len(srvs) != 2 {
		t.Fatalf("servers = %+v; want the user's own kept and the shadow dropped", srvs)
	}

	// No agent, no mcp_config.
	out, err = gitgateway.InjectMCPConfig(json.RawMessage(`{"id":"t2"}`), "https://gw.example/mcp", "gwt_abc")
	if err != nil || !strings.Contains(string(out), "gwt_abc") {
		t.Fatalf("bare task: %s %v", out, err)
	}
}
