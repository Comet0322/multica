package gitgateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

func TestReporterCanCloneAndAdminTokenStaysInGateway(t *testing.T) {
	h := newHarness(t)
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)
	tok := h.issue("t1", "Alice@Example.com")

	if code, body := h.do("GET", refsPath, tok, ""); code != 200 || !strings.Contains(body, "git-upload-pack") {
		t.Fatalf("info/refs = %d %q", code, body)
	}
	if code, body := h.do("POST", packPath, tok, "0032want abc\n"); code != 200 || body != "PACK:0032want abc\n" {
		t.Fatalf("upload-pack = %d %q", code, body)
	}

	reqs := h.gitlab.gitRequests()
	if len(reqs) != 2 {
		t.Fatalf("GitLab saw %d git requests, want 2", len(reqs))
	}
	for _, r := range reqs {
		if !strings.HasPrefix(r.Auth, "Basic ") || r.Cookie != "" {
			t.Fatalf("upstream auth=%q cookie=%q; want only the admin credential", r.Auth, r.Cookie)
		}
	}
	if reqs[0].Path != "/group/proj.git/info/refs" || reqs[0].RawQuery != "service=git-upload-pack" {
		t.Fatalf("upstream request = %+v", reqs[0])
	}
}

func TestAccessLevelsAndUserState(t *testing.T) {
	h := newHarness(t)
	h.gitlab.addUser("dev@example.com", 1, "active", gitgateway.LevelDeveloper)
	h.gitlab.addUser("guest@example.com", 2, "active", 10)
	h.gitlab.addUser("stranger@example.com", 3, "active", 0) // not a member
	h.gitlab.addUser("blocked@example.com", 4, "blocked", 40)

	cases := []struct {
		email string
		want  int
	}{
		{"dev@example.com", 200},
		{"guest@example.com", 403},
		{"stranger@example.com", 403},
		{"blocked@example.com", 403},
		{"nobody@example.com", 403},
		{"", 403},            // no human initiator
		{"dev@example", 403}, // GitLab search matches substrings; only an exact email counts
	}
	for _, c := range cases {
		tok := h.issue("task-"+c.email, c.email)
		if code, _ := h.do("GET", refsPath, tok, ""); code != c.want {
			t.Errorf("%q: status %d, want %d", c.email, code, c.want)
		}
	}
}

func TestTokenIsRefusedWhenMissingUnknownOrAfterTaskEnds(t *testing.T) {
	h := newHarness(t)
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)
	tok := h.issue("t1", "alice@example.com")

	if code, _ := h.do("GET", refsPath, "", ""); code != 401 {
		t.Fatalf("no credentials = %d, want 401 so git asks for them", code)
	}
	if code, _ := h.do("GET", refsPath, "gwt_forged", ""); code != 401 {
		t.Fatalf("forged token = %d", code)
	}
	if code, _ := h.do("GET", refsPath, tok, ""); code != 200 {
		t.Fatalf("live token = %d", code)
	}
	h.tokens.Revoke("t1")
	if code, _ := h.do("GET", refsPath, tok, ""); code != 401 {
		t.Fatalf("token after task end = %d, want 401", code)
	}
}

func TestManagerOutageRefuses(t *testing.T) {
	manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer manager.Close()
	h := newHarness(t, func(c *gitgateway.Config) { c.Resolver = gitgateway.HTTPResolver{URL: manager.URL} })
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)
	if code, _ := h.do("GET", refsPath, "gwt_any", ""); code != 503 {
		t.Fatalf("manager 500 = %d, want refusal", code)
	}
	if len(h.gitlab.gitRequests()) != 0 {
		t.Fatal("request reached GitLab without a verified task")
	}
}

func TestHTTPResolverAgainstVerifyHandler(t *testing.T) {
	reg := gitgateway.NewTokenRegistry()
	srv := httptest.NewServer(reg.VerifyHandler())
	defer srv.Close()
	tok, _ := reg.Issue(gitgateway.Identity{TaskID: "t9", WorkspaceID: "ws-2", Initiator: "Bob@Example.com"})
	res := gitgateway.HTTPResolver{URL: srv.URL}

	id, err := res.Resolve(context.Background(), tok)
	if err != nil || id.TaskID != "t9" || id.WorkspaceID != "ws-2" || id.Initiator != "bob@example.com" {
		t.Fatalf("resolve = %+v, %v", id, err)
	}
	reg.Revoke("t9")
	if _, err := res.Resolve(context.Background(), tok); err != gitgateway.ErrUnknownToken {
		t.Fatalf("revoked token error = %v", err)
	}
}

func TestAccessAnswersAreCachedForTenMinutesThenRefetched(t *testing.T) {
	h := newHarness(t)
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)
	tok := h.issue("t1", "alice@example.com")

	h.do("GET", refsPath, tok, "")
	first := h.gitlab.calls()
	if first == 0 {
		t.Fatal("first request made no GitLab lookups")
	}
	h.clock.Advance(9*time.Minute + 59*time.Second)
	if code, _ := h.do("POST", packPath, tok, "x"); code != 200 || h.gitlab.calls() != first {
		t.Fatalf("within TTL: status %d, lookups %d -> %d; want a cache hit", code, first, h.gitlab.calls())
	}

	// Access is revoked in GitLab; the cache keeps serving until it expires.
	h.gitlab.setLevel(7, 10)
	if code, _ := h.do("GET", refsPath, tok, ""); code != 200 {
		t.Fatalf("cached answer should still allow, got %d", code)
	}
	h.clock.Advance(2 * time.Second)
	if code, _ := h.do("GET", refsPath, tok, ""); code != 403 {
		t.Fatalf("after TTL status %d, want 403 from a fresh lookup", code)
	}
	if h.gitlab.calls() <= first {
		t.Fatal("expired cache did not trigger a new lookup")
	}
}

func TestGitLabFailureRefusesAndNeverUsesStaleCache(t *testing.T) {
	h := newHarness(t)
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)
	tok := h.issue("t1", "alice@example.com")
	if code, _ := h.do("GET", refsPath, tok, ""); code != 200 {
		t.Fatalf("warm-up = %d", code)
	}

	h.clock.Advance(11 * time.Minute) // cache expired
	h.gitlab.setFail(http.StatusBadGateway)
	before := len(h.gitlab.gitRequests())
	if code, _ := h.do("GET", refsPath, tok, ""); code != 503 {
		t.Fatalf("GitLab 5xx with expired cache = %d, want refusal", code)
	}
	if len(h.gitlab.gitRequests()) != before {
		t.Fatal("request was forwarded although GitLab could not be asked")
	}

	// Failures are not cached either: once GitLab recovers, access works again.
	h.gitlab.setFail(0)
	if code, _ := h.do("GET", refsPath, tok, ""); code != 200 {
		t.Fatalf("after recovery = %d", code)
	}
}

func TestGitLabTimeoutRefuses(t *testing.T) {
	h := newHarness(t, func(c *gitgateway.Config) { c.LookupTimeout = 100 * time.Millisecond })
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)
	h.gitlab.mu.Lock()
	h.gitlab.hang = make(chan struct{})
	release := h.gitlab.hang
	h.gitlab.mu.Unlock()
	defer close(release)

	tok := h.issue("t1", "alice@example.com")
	if code, _ := h.do("GET", refsPath, tok, ""); code != 503 {
		t.Fatalf("GitLab timeout = %d, want refusal", code)
	}
}

func TestOnlyAllowListedRequestsPass(t *testing.T) {
	h := newHarness(t)
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelDeveloper)
	tok := h.issue("t1", "alice@example.com")

	denied := []struct{ method, path string }{
		{"POST", refsPath},
		{"PUT", refsPath},
		{"GET", packPath},
		{"DELETE", packPath},
		{"GET", "/group/proj.git/info/refs"},
		{"GET", "/group/proj.git/info/refs?service=git-upload-pack&x=1"},
		{"GET", "/group/proj.git/info/refs?service=git-receive-pack&x=1"},
		{"POST", "/group/proj.git/git-receive-pack?service=x"},
		{"GET", "/group/proj.git/git-receive-pack"},
		{"POST", "/group/proj.git/git-upload-pack?service=x"},
		{"GET", "/group/proj.git/HEAD"},
		{"GET", "/group/proj.git/objects/info/packs"},
		{"GET", "/api/v4/projects"},
		{"GET", "/api/v4/projects/group%2Fproj/repository/archive"},
		{"POST", "/api/v4/projects/group%2Fproj/merge_requests"},
		{"GET", "/group/proj/-/raw/main/README.md"},
		{"GET", "/"},
		{"GET", "/proj.git/info/refs?service=git-upload-pack"}, // no namespace
	}
	for _, c := range denied {
		if code, _ := h.do(c.method, c.path, tok, ""); code != 403 {
			t.Errorf("%s %s = %d, want 403", c.method, c.path, code)
		}
	}
	if n := len(h.gitlab.gitRequests()); n != 0 {
		t.Fatalf("%d refused requests reached GitLab", n)
	}
}

// Every string must be refused, or be reduced to the canonical project path:
// GitLab then sees exactly the path that was authorized.
func TestPathBypassAttemptsAreRefusedOrNormalized(t *testing.T) {
	h := newHarness(t)
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)
	tok := h.issue("t1", "alice@example.com")
	const q = "?service=git-upload-pack"

	cases := []struct {
		name, path string
		allowed    bool
	}{
		{"upper case", "/Group/PROJ.git/info/refs" + q, true},
		{"upper case suffix", "/group/proj.GIT/info/refs" + q, true},
		{"encoded slash", "/group%2Fproj.git/info/refs" + q, true},
		{"encoded slash upper", "/group%2fproj.git/info/refs" + q, true},
		{"duplicate slashes", "/group//proj.git/info/refs" + q, true},
		{"leading slashes", "//group/proj.git/info/refs" + q, true},
		{"trailing slash", "/group/proj.git/info/refs/" + q, true},
		{"encoded endpoint slash", "/group/proj.git%2Finfo%2Frefs" + q, true},
		{"dot dot to other project", "/group/../secret/proj.git/info/refs" + q, false},
		{"dot dot after project", "/group/proj.git/../other.git/info/refs" + q, false},
		{"single dot", "/group/./proj.git/info/refs" + q, false},
		{"encoded dot dot", "/group/%2e%2e/secret/proj.git/info/refs" + q, false},
		{"encoded dot dot slash", "/group/..%2fsecret/proj.git/info/refs" + q, false},
		{"double encoded dot dot", "/group/%252e%252e/secret/proj.git/info/refs" + q, false},
		{"double encoded slash", "/group%252Fproj.git/info/refs" + q, false},
		{"backslash", "/group%5Cproj.git/info/refs" + q, false},
		{"null byte", "/group/proj%00.git/info/refs" + q, false},
		{"newline", "/group/proj%0a.git/info/refs" + q, false},
		{"semicolon", "/group/proj.git/info/refs;x" + q, false},
		{"missing .git", "/group/proj/info/refs" + q, false},
		{"lookalike unicode", "/gr%D0%BEup/proj.git/info/refs" + q, false},
		{"space", "/group/pro%20j.git/info/refs" + q, false},
		{"leading dash segment", "/-/proj.git/info/refs" + q, false},
	}
	for _, c := range cases {
		before := len(h.gitlab.gitRequests())
		code, _ := h.do("GET", c.path, tok, "")
		reqs := h.gitlab.gitRequests()
		if c.allowed {
			if code != 200 || len(reqs) != before+1 || reqs[before].Path != "/group/proj.git/info/refs" {
				t.Errorf("%s: status %d, upstream %+v; want canonical /group/proj.git/info/refs", c.name, code, reqs[before:])
			}
			continue
		}
		if code == 200 || len(reqs) != before {
			t.Errorf("%s: status %d with %d new upstream requests; want refusal", c.name, code, len(reqs)-before)
		}
	}
}

func TestAuditRecordsDecisionsWithoutCredentials(t *testing.T) {
	h := newHarness(t)
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)
	h.gitlab.addUser("guest@example.com", 8, "active", 10)
	tok := h.issue("t1", "alice@example.com")
	guest := h.issue("t2", "guest@example.com")

	h.do("GET", refsPath, tok, "")
	h.do("GET", refsPath, guest, "")
	h.do("GET", "/group/proj.git/HEAD", tok, "")
	h.do("GET", refsPath, "gwt_forged", "")

	recs := h.audit.records(t)
	if len(recs) != 4 {
		t.Fatalf("got %d audit records, want 4: %s", len(recs), h.audit.String())
	}
	allow := recs[0]
	if allow.Decision != gitgateway.AuditAllow || allow.TaskID != "t1" || allow.WorkspaceID != "ws-1" ||
		allow.Initiator != "alice@example.com" || allow.Project != project ||
		allow.Action != gitgateway.ActionInfoRefs || allow.Reason == "" || allow.Time.IsZero() {
		t.Fatalf("allow record = %+v", allow)
	}
	if g := recs[1]; g.Decision != gitgateway.AuditDeny || g.TaskID != "t2" || !strings.Contains(g.Reason, "access level 10") {
		t.Fatalf("guest record = %+v", g)
	}
	if r := recs[2]; r.Decision != gitgateway.AuditDeny || r.TaskID != "t1" || r.Reason != "path not allowed" {
		t.Fatalf("receive-pack record = %+v", r)
	}
	if r := recs[3]; r.Decision != gitgateway.AuditDeny || r.TaskID != "" {
		t.Fatalf("forged token record = %+v", r)
	}
	for _, secret := range []string{tok, guest, "gwt_forged", adminToken} {
		if strings.Contains(h.audit.String(), secret) {
			t.Fatalf("audit log contains a credential: %q", secret)
		}
	}
}
