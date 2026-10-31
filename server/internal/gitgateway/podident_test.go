package gitgateway_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

// fakePods answers which tasks the pod at an address holds. The test client
// connects from loopback, so entries are keyed by 127.0.0.1.
type fakePods struct {
	mu  sync.Mutex
	ids map[string][]gitgateway.Identity
	err error
}

func (f *fakePods) set(ip string, ids ...gitgateway.Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ids == nil {
		f.ids = map[string][]gitgateway.Identity{}
	}
	f.ids[ip] = ids
}

func (f *fakePods) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakePods) ResolvePod(_ context.Context, ip string) ([]gitgateway.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ids[ip], f.err
}

func withPods(p gitgateway.PodResolver) func(*gitgateway.Config) {
	return func(c *gitgateway.Config) { c.PodResolver = p }
}

func ident(task, email string) gitgateway.Identity {
	return gitgateway.Identity{TaskID: task, WorkspaceID: "ws-1", Initiator: email}
}

func TestDaemonReadFromLeasedPodIsAllowedAndUnleasedIsRefused(t *testing.T) {
	pods := &fakePods{}
	h := newHarness(t, withPods(pods))
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)

	if code, _ := h.do("GET", refsPath, "", ""); code != 403 {
		t.Fatalf("no lease: status %d, want 403", code)
	}
	pods.set("127.0.0.1", ident("t1", "alice@example.com"))
	if code, body := h.do("GET", refsPath, "", ""); code != 200 || !strings.Contains(body, "git-upload-pack") {
		t.Fatalf("leased info/refs = %d %q", code, body)
	}
	if code, body := h.do("POST", packPath, "", "0032want abc\n"); code != 200 || body != "PACK:0032want abc\n" {
		t.Fatalf("leased upload-pack = %d %q", code, body)
	}
	pods.set("127.0.0.1") // lease ended
	if code, _ := h.do("GET", refsPath, "", ""); code != 403 {
		t.Fatalf("after the lease: status %d, want 403", code)
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
}

func TestWithoutPodResolverTokenlessRequestsStayUnauthorized(t *testing.T) {
	h := newHarness(t)
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)
	if code, _ := h.do("GET", refsPath, "", ""); code != 401 {
		t.Fatalf("status %d, want 401", code)
	}
}

func TestAddressIdentityNeverWrites(t *testing.T) {
	pods := &fakePods{}
	pods.set("127.0.0.1", ident("t1", "alice@example.com"))
	h := newHarness(t, withPods(pods))
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelDeveloper)

	for _, c := range []struct{ method, path string }{
		{"GET", "/group/proj.git/info/refs?service=git-receive-pack"},
		{"POST", "/group/proj.git/git-receive-pack"},
		{"POST", "/api/v4/projects/group%2Fproj/merge_requests"},
		{"DELETE", packPath},
		{"POST", refsPath},
		{"GET", packPath},
	} {
		if code, _ := h.do(c.method, c.path, "", "0000"); code != 403 {
			t.Errorf("%s %s = %d, want 403", c.method, c.path, code)
		}
	}
	if n := len(h.gitlab.gitRequests()); n != 0 || h.gitlab.calls() != 0 {
		t.Fatalf("a refused write reached GitLab (%d git, %d api)", n, h.gitlab.calls())
	}
	for _, r := range h.audit.records(t) {
		if r.Decision != gitgateway.AuditDeny {
			t.Fatalf("unexpected decision %+v", r)
		}
	}
}

func TestSharedPodAnyInitiatorWithAccessLetsTheReadThrough(t *testing.T) {
	pods := &fakePods{}
	h := newHarness(t, withPods(pods))
	h.gitlab.addUser("alice@example.com", 1, "active", gitgateway.LevelReporter)
	h.gitlab.addUser("bob@example.com", 2, "active", 10) // guest: below Reporter
	h.gitlab.addUser("carol@example.com", 3, "active", 0)

	pods.set("127.0.0.1", ident("t1", "bob@example.com"), ident("t2", "carol@example.com"), ident("t3", "alice@example.com"))
	if code, _ := h.do("GET", refsPath, "", ""); code != 200 {
		t.Fatalf("one initiator with access: status %d, want 200", code)
	}
	recs := h.audit.records(t)
	if last := recs[len(recs)-1]; last.Decision != gitgateway.AuditAllow || last.TaskID != "t3" || last.Initiator != "alice@example.com" {
		t.Fatalf("audit should name the initiator that granted it: %+v", last)
	}

	pods.set("127.0.0.1", ident("t1", "bob@example.com"), ident("t2", "carol@example.com"), ident("t4", "nobody@example.com"))
	if code, _ := h.do("GET", refsPath, "", ""); code != 403 {
		t.Fatalf("no initiator with access: status %d, want 403", code)
	}
}

func TestPodLookupFailuresRefuse(t *testing.T) {
	pods := &fakePods{}
	h := newHarness(t, withPods(pods))
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)
	pods.set("127.0.0.1", ident("t1", "alice@example.com"))

	pods.setErr(errors.New("manager down"))
	if code, _ := h.do("GET", refsPath, "", ""); code != 503 {
		t.Fatalf("manager down: status %d, want 503", code)
	}
	pods.setErr(nil)
	h.gitlab.setFail(500)
	if code, _ := h.do("GET", refsPath, "", ""); code != 503 {
		t.Fatalf("GitLab down: status %d, want 503", code)
	}
}

func TestBadTokenIsRefusedEvenFromALeasedAddress(t *testing.T) {
	pods := &fakePods{}
	pods.set("127.0.0.1", ident("t1", "alice@example.com"))
	h := newHarness(t, withPods(pods))
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelReporter)
	if code, _ := h.do("GET", refsPath, "gwt_not-a-live-token", ""); code != 401 {
		t.Fatalf("bad token from a leased pod: status %d, want 401", code)
	}
}

func TestPodVerifyEndpointRoundTrip(t *testing.T) {
	lookup := func(ip string) []gitgateway.Identity {
		if ip == "10.0.0.5" {
			return []gitgateway.Identity{ident("t1", "alice@example.com")}
		}
		return nil
	}
	srv := httptest.NewServer(gitgateway.PodVerifyHandler(lookup))
	defer srv.Close()
	r := gitgateway.HTTPPodResolver{URL: srv.URL}

	ids, err := r.ResolvePod(context.Background(), "10.0.0.5")
	if err != nil || len(ids) != 1 || ids[0].TaskID != "t1" {
		t.Fatalf("leased: %v %v", ids, err)
	}
	if ids, err = r.ResolvePod(context.Background(), "10.0.0.6"); err != nil || len(ids) != 0 {
		t.Fatalf("unleased: %v %v", ids, err)
	}
	resp, err := http.Get(srv.URL)
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET must be refused: %v %v", resp, err)
	}
	resp.Body.Close()

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) }))
	defer down.Close()
	if _, err := (gitgateway.HTTPPodResolver{URL: down.URL}).ResolvePod(context.Background(), "10.0.0.5"); err == nil {
		t.Fatal("a failing manager must be an error, not an empty answer")
	}
}
