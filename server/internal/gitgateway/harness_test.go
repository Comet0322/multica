package gitgateway_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	recv     []recvReq
	// pushStatus, when set, is the HTTP status receive-pack answers.
	pushStatus int
	// rejectRef, when set, makes receive-pack report that ref as "ng".
	rejectRef string
	// project settings and merge requests created through the API.
	archived bool
	mrAccess string // merge_requests_access_level; default "enabled"
	mrs      []map[string]any
}

// recvReq is a receive-pack body as GitLab saw it. Only the head is kept so a
// large push is not buffered by the test either.
type recvReq struct {
	Size int64
	Head []byte
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

func (f *fakeGitLab) setPushOutcome(status int, rejectRef string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushStatus, f.rejectRef = status, rejectRef
}

func (f *fakeGitLab) setProject(archived bool, mrAccess string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archived, f.mrAccess = archived, mrAccess
}

func (f *fakeGitLab) createdMRs() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.mrs...)
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

func (f *fakeGitLab) pushes() []recvReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recvReq(nil), f.recv...)
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
	if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
		head := make([]byte, 4096)
		n, _ := io.ReadFull(r.Body, head)
		rest, _ := io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.recv = append(f.recv, recvReq{Size: int64(n) + rest, Head: head[:n]})
		f.mu.Unlock()
		f.mu.Lock()
		status, reject := f.pushStatus, f.rejectRef
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		if status != 0 {
			http.Error(w, "boom", status)
			return
		}
		report := pkt("unpack ok\n")
		for _, ref := range pushedRefs(head[:n]) {
			if ref == reject {
				report += pkt("ng " + ref + " pre-receive hook declined\n")
			} else {
				report += pkt("ok " + ref + "\n")
			}
		}
		io.WriteString(w, pkt("\x01"+report+"0000")+"0000")
		return
	}
	if r.URL.RawQuery == "service=git-receive-pack" {
		zero := strings.Repeat("0", 40)
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		io.WriteString(w, pkt("# service=git-receive-pack\n")+"0000"+pkt(zero+" capabilities^{}\x00report-status side-band-64k delete-refs\n")+"0000")
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
	case path == "/projects/group%2Fproj" && r.Method == http.MethodGet:
		access := f.mrAccess
		if access == "" {
			access = "enabled"
		}
		json.NewEncoder(w).Encode(map[string]any{"archived": f.archived, "default_branch": "main", "merge_requests_access_level": access})
	case path == "/projects/group%2Fproj/merge_requests" && r.Method == http.MethodPost:
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		in["iid"] = len(f.mrs) + 1
		in["state"] = "opened"
		in["web_url"] = "http://gitlab.example/group/proj/-/merge_requests/" + itoa(int64(len(f.mrs)+1))
		f.mrs = append(f.mrs, in)
		json.NewEncoder(w).Encode(in)
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
	gw      *gitgateway.Gateway
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
	h.gw = g
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

// pkt frames s as one pkt-line.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// pushedRefs lists the refs of the commands at the head of a receive-pack body.
func pushedRefs(head []byte) []string {
	var refs []string
	for len(head) >= 4 {
		n, err := strconv.ParseUint(string(head[:4]), 16, 16)
		if err != nil || n < 4 || int(n) > len(head) {
			break
		}
		line := string(head[4:n])
		head = head[n:]
		line, _, _ = strings.Cut(line, "\x00")
		if parts := strings.SplitN(strings.TrimSpace(line), " ", 3); len(parts) == 3 {
			refs = append(refs, parts[2])
		}
	}
	return refs
}
