package gitgateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

const (
	adminToken = "glpat-ADMIN-SECRET"
	project    = "group/proj"
)

// fakeGitLab answers the API calls the gateway makes and serves git endpoints
// for the projects it knows. It records what the gateway sent.
type fakeGitLab struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	users    map[string]fakeUser // by email
	levels   map[string]int      // "userID|project" -> access level
	fail     int                 // when set, API calls answer this status
	hang     chan struct{}       // when set, API calls block until closed
	apiCalls int
	gitReqs  []gitReq
}

type fakeUser struct {
	ID    int64
	State string
}

type gitReq struct {
	Method, Path, RawQuery, Auth, Cookie string
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	f := &fakeGitLab{t: t, users: map[string]fakeUser{}, levels: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitLab) addUser(email string, id int64, state string, level int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[email] = fakeUser{ID: id, State: state}
	if level > 0 {
		f.levels[levelKey(id, project)] = level
	}
}

func levelKey(id int64, project string) string { return strings.Join([]string{itoa(id), project}, "|") }

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (f *fakeGitLab) setLevel(id int64, level int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.levels[levelKey(id, project)] = level
}

func (f *fakeGitLab) setFail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = status
}

func (f *fakeGitLab) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apiCalls
}

func (f *fakeGitLab) gitRequests() []gitReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gitReq(nil), f.gitReqs...)
}

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if strings.HasPrefix(r.URL.Path, "/api/v4/") {
		f.apiCalls++
		fail, hang := f.fail, f.hang
		f.mu.Unlock()
		if hang != nil {
			select {
			case <-hang:
			case <-r.Context().Done():
				return
			}
		}
		if r.Header.Get("PRIVATE-TOKEN") != adminToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if fail != 0 {
			http.Error(w, "boom", fail)
			return
		}
		f.api(w, r)
		return
	}
	f.gitReqs = append(f.gitReqs, gitReq{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		Auth: r.Header.Get("Authorization"), Cookie: r.Header.Get("Cookie"),
	})
	f.mu.Unlock()
	if _, pw, _ := r.BasicAuth(); pw != adminToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/info/refs") {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		io.WriteString(w, "001e# service=git-upload-pack\n0000")
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Write(append([]byte("PACK:"), body...))
}

func (f *fakeGitLab) api(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4")
	switch {
	case path == "/users":
		search := r.URL.Query().Get("search")
		out := []map[string]any{}
		for email, u := range f.users {
			// GitLab's search also matches substrings.
			if strings.Contains(email, search) {
				out = append(out, map[string]any{"id": u.ID, "state": u.State, "email": email})
			}
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(path, "/projects/"):
		rest := strings.TrimPrefix(path, "/projects/")
		enc, id, ok := strings.Cut(rest, "/members/all/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		proj := strings.ReplaceAll(enc, "%2F", "/")
		level, found := f.levels[id+"|"+proj]
		if !found {
			http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_level": level})
	default:
		http.NotFound(w, r)
	}
}

// fakeClock is the gateway's time source, advanced by tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) records(t *testing.T) []gitgateway.AuditRecord {
	var out []gitgateway.AuditRecord
	for _, line := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if line == "" {
			continue
		}
		var r gitgateway.AuditRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("audit line is not JSON: %q", line)
		}
		out = append(out, r)
	}
	return out
}

type harness struct {
	t       *testing.T
	gitlab  *fakeGitLab
	tokens  *gitgateway.TokenRegistry
	clock   *fakeClock
	audit   *syncBuffer
	gateway *httptest.Server
}

func newHarness(t *testing.T, mutate ...func(*gitgateway.Config)) *harness {
	t.Helper()
	h := &harness{
		t: t, gitlab: newFakeGitLab(t), tokens: gitgateway.NewTokenRegistry(),
		clock: &fakeClock{t: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}, audit: &syncBuffer{},
	}
	cfg := gitgateway.Config{
		GitLabURL: h.gitlab.srv.URL, AdminToken: adminToken, Resolver: h.tokens,
		Audit: h.audit, Now: h.clock.Now,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	g, err := gitgateway.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.gateway = httptest.NewServer(g)
	t.Cleanup(h.gateway.Close)
	return h
}

func (h *harness) issue(taskID, email string) string {
	h.t.Helper()
	tok, err := h.tokens.Issue(gitgateway.Identity{TaskID: taskID, WorkspaceID: "ws-1", Initiator: email})
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

// get sends a request the way git does: the token is the Basic password.
func (h *harness) do(method, rawPath, token, body string) (int, string) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.gateway.URL+rawPath, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.SetBasicAuth("task", token)
	}
	req.Header.Set("Cookie", "session=client-cookie")
	resp, err := h.gateway.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const (
	refsPath = "/group/proj.git/info/refs?service=git-upload-pack"
	packPath = "/group/proj.git/git-upload-pack"
)
