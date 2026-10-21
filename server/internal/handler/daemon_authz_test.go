package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
)

// authzFixture is one runtime owned by testUserID and bound to daemon
// "authz-own-daemon", with a queued task, plus a second workspace member who
// owns nothing.
type authzFixture struct {
	runtimeID string
	taskID    string
	memberID  string
}

const (
	authzOwnDaemon   = "authz-own-daemon"
	authzOtherDaemon = "authz-other-daemon"
)

func newAuthzFixture(t *testing.T) authzFixture {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	rt := dbfx.Runtime(t, "authz-runtime", map[string]any{
		"daemon_id":    authzOwnDaemon,
		"runtime_mode": "local",
		"visibility":   "public",
	})
	agent := dbfx.Agent(t, "authz-agent", rt)
	issue := dbfx.Issue(t, "authz issue")
	task := dbfx.Task(t, agent, map[string]any{"issue_id": issue, "runtime_id": rt})
	member := dbfx.User(t, "Authz Member", "authz-member@example.com")
	dbfx.Member(t, testWorkspaceID, member, "member")
	return authzFixture{runtimeID: rt, taskID: task, memberID: member}
}

func withAuthzMode(t *testing.T, mode daemonAuthzMode) {
	t.Helper()
	prev := testHandler.DaemonAuthzMode
	testHandler.DaemonAuthzMode = mode
	t.Cleanup(func() { testHandler.DaemonAuthzMode = prev })
}

type authzCaller struct {
	name  string
	build func(r *http.Request) *http.Request
	allow bool
}

func (f authzFixture) callers() []authzCaller {
	asUser := func(id string) func(*http.Request) *http.Request {
		return func(r *http.Request) *http.Request {
			r.Header.Set("X-User-ID", id)
			r.Header.Set("X-Workspace-ID", testWorkspaceID)
			return r
		}
	}
	asDaemon := func(ws, daemon string) func(*http.Request) *http.Request {
		return func(r *http.Request) *http.Request {
			return r.WithContext(middleware.WithDaemonContext(r.Context(), ws, daemon))
		}
	}
	return []authzCaller{
		{"mdt own daemon", asDaemon(testWorkspaceID, authzOwnDaemon), true},
		{"mdt other daemon same workspace", asDaemon(testWorkspaceID, authzOtherDaemon), false},
		{"mdt empty daemon id", asDaemon(testWorkspaceID, ""), false},
		{"mdt other workspace", asDaemon("00000000-0000-0000-0000-000000000001", authzOwnDaemon), false},
		{"owner PAT/JWT", asUser(testUserID), true},
		{"non-owner member PAT/JWT", asUser(f.memberID), false},
	}
}

type authzRoute struct {
	name string
	call func(f authzFixture, r *http.Request) *httptest.ResponseRecorder
}

func authzRoutes() []authzRoute {
	return []authzRoute{
		{"claim", func(f authzFixture, r *http.Request) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			testHandler.ClaimTaskByRuntime(w, withURLParam(r, "runtimeId", f.runtimeID))
			return w
		}},
		{"heartbeat", func(f authzFixture, r *http.Request) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			testHandler.DaemonHeartbeat(w, r)
			return w
		}},
		{"task status", func(f authzFixture, r *http.Request) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			testHandler.GetTaskStatus(w, withURLParam(r, "taskId", f.taskID))
			return w
		}},
	}
}

func (f authzFixture) request(route string) *http.Request {
	switch route {
	case "claim":
		return newRequest(http.MethodPost, "/api/daemon/runtimes/"+f.runtimeID+"/tasks/claim", nil)
	case "heartbeat":
		return newRequest(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": f.runtimeID})
	default:
		return newRequest(http.MethodGet, "/api/daemon/tasks/"+f.taskID+"/status", nil)
	}
}

// strip removes the default test-user identity so the caller supplies its own.
func strip(r *http.Request) *http.Request {
	r.Header.Del("X-User-ID")
	r.Header.Del("X-Workspace-ID")
	return r
}

func TestDaemonAuthzEnforce(t *testing.T) {
	f := newAuthzFixture(t)
	withAuthzMode(t, daemonAuthzEnforce)

	for _, route := range authzRoutes() {
		for _, c := range f.callers() {
			t.Run(route.name+"/"+c.name, func(t *testing.T) {
				w := route.call(f, c.build(strip(f.request(route.name))))
				if c.allow && w.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
				}
				if !c.allow && w.Code != http.StatusNotFound {
					t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

// The PoC repro: a plain member must not claim a task queued on another
// member's runtime.
func TestDaemonAuthzEnforceMemberCannotClaimOthersRuntime(t *testing.T) {
	f := newAuthzFixture(t)
	withAuthzMode(t, daemonAuthzEnforce)

	r := strip(f.request("claim"))
	r.Header.Set("X-User-ID", f.memberID)
	r.Header.Set("X-Workspace-ID", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.ClaimTaskByRuntime(w, withURLParam(r, "runtimeId", f.runtimeID))
	if w.Code != http.StatusNotFound {
		t.Fatalf("member claim on another member's runtime: expected 404, got %d: %s", w.Code, w.Body.String())
	}
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&status)
	if status != "queued" {
		t.Fatalf("task must stay queued, got %q", status)
	}
}

func TestDaemonAuthzLogModeAllowsButDoesNotBreak(t *testing.T) {
	f := newAuthzFixture(t)
	withAuthzMode(t, daemonAuthzLog)

	for _, route := range authzRoutes() {
		for _, c := range f.callers() {
			if c.name == "mdt other workspace" {
				continue // workspace isolation is not behind the mode switch
			}
			t.Run(route.name+"/"+c.name, func(t *testing.T) {
				w := route.call(f, c.build(strip(f.request(route.name))))
				if w.Code != http.StatusOK {
					t.Fatalf("log mode must allow, got %d: %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestDaemonAuthzOffModeSkipsRule(t *testing.T) {
	f := newAuthzFixture(t)
	withAuthzMode(t, daemonAuthzOff)

	r := strip(f.request("task status"))
	r.Header.Set("X-User-ID", f.memberID)
	r.Header.Set("X-Workspace-ID", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.GetTaskStatus(w, withURLParam(r, "taskId", f.taskID))
	if w.Code != http.StatusOK {
		t.Fatalf("off mode must not check ownership, got %d", w.Code)
	}
}

func TestDaemonAuthzRuntimeWithoutOwnerDeniedForUsers(t *testing.T) {
	f := newAuthzFixture(t)
	withAuthzMode(t, daemonAuthzEnforce)
	dbfx.Exec(t, `UPDATE agent_runtime SET owner_id = NULL WHERE id = $1`, f.runtimeID)

	r := strip(f.request("heartbeat"))
	r.Header.Set("X-User-ID", testUserID)
	r.Header.Set("X-Workspace-ID", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.DaemonHeartbeat(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("runtime with NULL owner must be denied for PAT/JWT, got %d", w.Code)
	}

	// The daemon token bound to its daemon_id still works.
	r = strip(f.request("heartbeat"))
	r = r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, authzOwnDaemon))
	w = httptest.NewRecorder()
	testHandler.DaemonHeartbeat(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("own mdt_ must still work, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDaemonAuthzDeregisterOnlyOwnRuntime(t *testing.T) {
	f := newAuthzFixture(t)
	withAuthzMode(t, daemonAuthzEnforce)

	deregister := func(setup func(*http.Request) *http.Request) {
		r := strip(newRequest(http.MethodPost, "/api/daemon/deregister", map[string]any{"runtime_ids": []string{f.runtimeID}}))
		w := httptest.NewRecorder()
		testHandler.DaemonDeregister(w, setup(r))
		if w.Code >= 500 {
			t.Fatalf("deregister: %d %s", w.Code, w.Body.String())
		}
	}
	status := func() string {
		var s string
		dbfx.QueryRow(t, `SELECT status FROM agent_runtime WHERE id = $1`, f.runtimeID).Scan(&s)
		return s
	}

	deregister(func(r *http.Request) *http.Request {
		r.Header.Set("X-User-ID", f.memberID)
		return r
	})
	if s := status(); s != "online" {
		t.Fatalf("non-owner member must not take a runtime offline, status=%q", s)
	}
	deregister(func(r *http.Request) *http.Request {
		return r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, authzOtherDaemon))
	})
	if s := status(); s != "online" {
		t.Fatalf("other daemon's mdt_ must not take a runtime offline, status=%q", s)
	}
	deregister(func(r *http.Request) *http.Request {
		return r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, authzOwnDaemon))
	})
	if s := status(); s != "offline" {
		t.Fatalf("own daemon must be able to deregister, status=%q", s)
	}
}

func TestParseDaemonAuthzMode(t *testing.T) {
	for in, want := range map[string]daemonAuthzMode{
		"": daemonAuthzLog, "log": daemonAuthzLog, "LOG": daemonAuthzLog, "junk": daemonAuthzLog,
		"off": daemonAuthzOff, " Enforce ": daemonAuthzEnforce,
	} {
		if got := parseDaemonAuthzMode(in); got != want {
			t.Errorf("parseDaemonAuthzMode(%q)=%q want %q", in, got, want)
		}
	}
}
