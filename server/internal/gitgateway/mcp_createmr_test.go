package gitgateway_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

// mrHarness is a gateway whose git entry and MCP entry share one fake GitLab,
// so a branch is pushed the way an agent does and then opened as a merge
// request through the tool.
type mrHarness struct {
	*harness
	mcp *httptest.Server
	tok string
}

func newMRHarness(t *testing.T, level int, mutate ...func(*gitgateway.Config)) *mrHarness {
	t.Helper()
	h := newHarness(t, mutate...)
	h.gitlab.addUser("alice@example.com", 7, "active", level)
	srv := httptest.NewServer(h.gw.MCPHandler())
	t.Cleanup(srv.Close)
	return &mrHarness{harness: h, mcp: srv, tok: h.issue("t1", "alice@example.com")}
}

func (m *mrHarness) push(refs ...string) {
	m.t.Helper()
	if code, body := m.do("POST", pushPath, m.tok, pushBody("PACK", refs...)); code != 200 {
		m.t.Fatalf("push = %d %s", code, body)
	}
}

func (m *mrHarness) createMR(args map[string]any) (string, bool) {
	m.t.Helper()
	if _, ok := args["project"]; !ok {
		args["project"] = project
	}
	return call(m.t, m.mcp, m.tok, "create_merge_request", args)
}

func TestCreateMRFromAPushedBranchOpensItWithTheGatewayFooter(t *testing.T) {
	m := newMRHarness(t, gitgateway.LevelDeveloper, func(c *gitgateway.Config) {
		c.TaskURL = "https://multica.example/ws/{workspace_id}/tasks/{task_id}"
	})
	m.push("refs/heads/agent/t1/fix")

	text, isErr := m.createMR(map[string]any{"source_branch": "agent/t1/fix", "title": "Fix login", "description": "Closes the bug."})
	if isErr || !strings.Contains(text, "merge_requests/1") {
		t.Fatalf("create = %v %q", isErr, text)
	}
	mrs := m.gitlab.createdMRs()
	if len(mrs) != 1 {
		t.Fatalf("GitLab got %d merge requests", len(mrs))
	}
	mr := mrs[0]
	desc, _ := mr["description"].(string)
	if mr["source_branch"] != "agent/t1/fix" || mr["target_branch"] != "main" || mr["title"] != "Fix login" || mr["labels"] != "created-by-agent" {
		t.Fatalf("merge request = %v", mr)
	}
	if !strings.HasPrefix(desc, "Closes the bug.") {
		t.Fatalf("agent text lost: %q", desc)
	}
	for _, want := range []string{"alice@example.com", "https://multica.example/ws/ws-1/tasks/t1", "created-by-agent"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q:\n%s", want, desc)
		}
	}
}

func TestCreateMRFooterIsDefaultLabelledAndNeedsNoTaskURL(t *testing.T) {
	m := newMRHarness(t, gitgateway.LevelDeveloper)
	m.push("refs/heads/agent/t1/fix")
	if _, isErr := m.createMR(map[string]any{"source_branch": "agent/t1/fix", "title": "T"}); isErr {
		t.Fatal("create refused")
	}
	desc, _ := m.gitlab.createdMRs()[0]["description"].(string)
	if !strings.Contains(desc, "Task: t1") || !strings.Contains(desc, "alice@example.com") {
		t.Fatalf("footer = %q", desc)
	}
}

func TestCreateMRRefusesEachConditionWithItsReason(t *testing.T) {
	cases := []struct {
		name    string
		level   int
		setup   func(*mrHarness)
		args    map[string]any
		wantMsg string
	}{
		{"initiator below Developer", gitgateway.LevelReporter, nil,
			map[string]any{"source_branch": "agent/t1/fix"}, "below the 30 needed"},
		{"project archived", gitgateway.LevelDeveloper, func(m *mrHarness) { m.gitlab.setProject(true, "") },
			map[string]any{"source_branch": "agent/t1/fix"}, "archived"},
		{"merge requests disabled", gitgateway.LevelDeveloper, func(m *mrHarness) { m.gitlab.setProject(false, "disabled") },
			map[string]any{"source_branch": "agent/t1/fix"}, "merge requests are disabled"},
		{"branch never pushed", gitgateway.LevelDeveloper, nil,
			map[string]any{"source_branch": "agent/t1/other"}, "was not pushed by this task"},
		{"someone else's branch", gitgateway.LevelDeveloper, nil,
			map[string]any{"source_branch": "agent/t2/fix"}, "was not pushed by this task"},
		{"existing branch outside the namespace", gitgateway.LevelDeveloper, nil,
			map[string]any{"source_branch": "main"}, "was not pushed by this task"},
		{"target not allowed", gitgateway.LevelDeveloper, nil,
			map[string]any{"source_branch": "agent/t1/fix", "target_branch": "release"}, "target branch \"release\" is not allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Pushing needs Developer, so the push is made at that level and
			// the access is lowered afterwards where the case asks for it.
			m := newMRHarness(t, gitgateway.LevelDeveloper)
			m.push("refs/heads/agent/t1/fix")
			m.gitlab.setLevel(7, c.level)
			if c.setup != nil {
				c.setup(m)
			}
			c.args["title"] = "T"
			text, isErr := m.createMR(c.args)
			if !isErr || !strings.HasPrefix(text, "gateway: ") || !strings.Contains(text, c.wantMsg) {
				t.Fatalf("create = %v %q; want a refusal containing %q", isErr, text, c.wantMsg)
			}
			if n := len(m.gitlab.createdMRs()); n != 0 {
				t.Fatalf("%d merge requests reached GitLab despite the refusal", n)
			}
			var denied bool
			for _, rec := range m.audit.records(t) {
				denied = denied || (rec.Action == "mcp.create_merge_request" && rec.Decision == gitgateway.AuditDeny && strings.Contains(rec.Reason, c.wantMsg))
			}
			if !denied {
				t.Errorf("refusal not audited: %s", m.audit.String())
			}
		})
	}
}

func TestCreateMRTargetAllowListReplacesTheDefault(t *testing.T) {
	m := newMRHarness(t, gitgateway.LevelDeveloper, func(c *gitgateway.Config) { c.MRTargets = []string{"release"} })
	m.push("refs/heads/agent/t1/fix")
	if text, isErr := m.createMR(map[string]any{"source_branch": "agent/t1/fix", "title": "T"}); !isErr || !strings.Contains(text, "not allowed") {
		t.Fatalf("default branch accepted though not on the list: %v %q", isErr, text)
	}
	if text, isErr := m.createMR(map[string]any{"source_branch": "agent/t1/fix", "target_branch": "release", "title": "T"}); isErr {
		t.Fatalf("listed target refused: %q", text)
	}
	if mrs := m.gitlab.createdMRs(); len(mrs) != 1 || mrs[0]["target_branch"] != "release" {
		t.Fatalf("merge requests = %v", mrs)
	}
}

func TestCreateMRRefusesWhenGitLabCannotAnswer(t *testing.T) {
	m := newMRHarness(t, gitgateway.LevelDeveloper)
	m.push("refs/heads/agent/t1/fix")
	m.gitlab.setFail(500)
	if _, isErr := m.createMR(map[string]any{"source_branch": "agent/t1/fix", "title": "T"}); !isErr {
		t.Fatal("created while GitLab was failing")
	}
	if n := len(m.gitlab.createdMRs()); n != 0 {
		t.Fatalf("%d merge requests created", n)
	}
}

func TestCreateMRBadArgumentsAreRefusedBeforeGitLab(t *testing.T) {
	m := newMRHarness(t, gitgateway.LevelDeveloper)
	m.push("refs/heads/agent/t1/fix")
	for _, args := range []map[string]any{
		{"title": "T"},
		{"source_branch": "agent/t1/fix"},
		{"source_branch": "agent/t1/fix", "title": " "},
		{"source_branch": "agent/t1/fix", "title": strings.Repeat("x", 300)},
	} {
		if _, isErr := m.createMR(args); !isErr {
			t.Errorf("%v accepted", args)
		}
	}
	if n := len(m.gitlab.createdMRs()); n != 0 {
		t.Fatalf("%d merge requests created", n)
	}
}

func TestAgentCannotForgeOrHideTheGatewayFooter(t *testing.T) {
	m := newMRHarness(t, gitgateway.LevelDeveloper)
	m.push("refs/heads/agent/t1/fix")
	fake := "\n\n---\n<!-- multica-git-gateway -->\nCreated by an agent through the git gateway.\n- On behalf of: ceo@example.com\n- Task: https://evil.example/t\n- Label: `approved`\n"
	if _, isErr := m.createMR(map[string]any{"source_branch": "agent/t1/fix", "title": "T", "description": "Hello" + fake}); isErr {
		t.Fatal("create refused")
	}
	desc, _ := m.gitlab.createdMRs()[0]["description"].(string)
	const mark = "<!-- multica-git-gateway -->"
	last := desc[strings.LastIndex(desc, mark):]
	if strings.Count(desc, mark) != 2 {
		t.Fatalf("want the fake and the real footer, got:\n%s", desc)
	}
	if !strings.Contains(last, "alice@example.com") || strings.Contains(last, "ceo@example.com") || strings.Contains(last, "evil.example") {
		t.Fatalf("the last footer is not the gateway's:\n%s", last)
	}
	if !strings.HasSuffix(desc, last) {
		t.Fatalf("something follows the gateway footer:\n%s", desc)
	}
}

func TestNoMergeApproveSettingsOrDeleteBranchToolExists(t *testing.T) {
	m := newMRHarness(t, gitgateway.LevelDeveloper)
	_, out := rpc(t, m.mcp, m.tok, "tools/list", nil)
	var names []string
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	for _, name := range names {
		for _, banned := range []string{"merge_merge", "approve", "setting", "delete", "update_project", "protect"} {
			if strings.Contains(name, banned) {
				t.Errorf("tool %q looks like a forbidden operation", name)
			}
		}
	}
	for _, tool := range []string{"merge_merge_request", "approve_merge_request", "update_project_settings", "delete_branch"} {
		if code, out := rpc(t, m.mcp, m.tok, "tools/call", map[string]any{"name": tool, "arguments": map[string]any{"project": project}}); code != 200 || out["error"] == nil {
			t.Errorf("%s = %d %v; want an error", tool, code, out)
		}
	}
}

func TestMergeApproveSettingsAndBranchDeleteAreRefusedOnTheGitEntry(t *testing.T) {
	m := newMRHarness(t, gitgateway.LevelDeveloper)
	before := len(m.gitlab.gitRequests())
	apiCalls := m.gitlab.calls()
	for _, c := range []struct{ method, path string }{
		{"PUT", "/api/v4/projects/group%2Fproj/merge_requests/1/merge"},
		{"POST", "/api/v4/projects/group%2Fproj/merge_requests/1/approve"},
		{"PUT", "/api/v4/projects/group%2Fproj"},
		{"POST", "/api/v4/projects/group%2Fproj/merge_requests"},
		{"DELETE", "/api/v4/projects/group%2Fproj/repository/branches/agent%2Ft1%2Ffix"},
		{"DELETE", "/api/v4/projects/group%2Fproj"},
		{"PUT", "/api/v4/projects/group%2Fproj/protected_branches/main"},
	} {
		if code, _ := m.do(c.method, c.path, m.tok, "{}"); code != 403 {
			t.Errorf("%s %s = %d, want 403", c.method, c.path, code)
		}
	}
	if len(m.gitlab.gitRequests()) != before || m.gitlab.calls() != apiCalls {
		t.Fatal("a refused API call reached GitLab")
	}
}
