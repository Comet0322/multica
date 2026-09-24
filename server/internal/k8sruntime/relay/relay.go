// Package relay is the scoped reverse proxy that sits between sandbox Pods and
// the Multica server.
//
// A sandbox Pod runs the daemon's task pipeline unchanged, but it must never
// hold the daemon credential. The Pod authenticates to the relay with its task
// token (mat_). For the daemon-only endpoints the relay checks that the request
// targets the very task that token belongs to, then swaps in the daemon
// credential before forwarding. Everything else (the agent CLI's user-API
// calls) is forwarded untouched, so the agent has exactly the permissions it
// has under a native daemon.
package relay

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

const daemonPrefix = "/api/daemon/"

// Task is the scope a task token grants. RemoteMCPToken is the per-claim mdt_
// token; it stays in the relay and is swapped in for the two MCP callbacks.
type Task struct {
	TaskID         string
	RuntimeID      string
	WorkspaceID    string
	RemoteMCPToken string
}

// Relay forwards sandbox requests to one upstream server.
type Relay struct {
	daemonToken string
	proxy       *httputil.ReverseProxy

	mu    sync.RWMutex
	tasks map[string]Task // task token -> scope
}

// New returns a Relay that forwards to upstream and authenticates daemon-only
// calls with daemonToken.
func New(upstream *url.URL, daemonToken string) (*Relay, error) {
	if upstream == nil || upstream.Host == "" {
		return nil, errors.New("relay: upstream URL is required")
	}
	if daemonToken == "" {
		return nil, errors.New("relay: daemon token is required")
	}
	r := &Relay{daemonToken: daemonToken, tasks: make(map[string]Task)}
	r.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
		},
	}
	return r, nil
}

// Register grants token access to its task's scope.
func (r *Relay) Register(token string, t Task) {
	r.mu.Lock()
	r.tasks[token] = t
	r.mu.Unlock()
}

// Unregister revokes token.
func (r *Relay) Unregister(token string) {
	r.mu.Lock()
	delete(r.tasks, token)
	r.mu.Unlock()
}

func (r *Relay) lookup(token string) (Task, bool) {
	r.mu.RLock()
	t, ok := r.tasks[token]
	r.mu.RUnlock()
	return t, ok
}

func bearer(req *http.Request) string {
	h := strings.TrimSpace(req.Header.Get("Authorization"))
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}

// ServeHTTP implements http.Handler.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	token := bearer(req)
	task, ok := r.lookup(token)
	if !ok {
		http.Error(w, "unknown task credential", http.StatusUnauthorized)
		return
	}

	if strings.HasPrefix(req.URL.Path, daemonPrefix) {
		auth, allowed := r.authorizeDaemonCall(task, req.Method, strings.TrimPrefix(req.URL.Path, daemonPrefix))
		if !allowed {
			http.Error(w, "daemon endpoint not permitted for this task", http.StatusForbidden)
			return
		}
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	r.proxy.ServeHTTP(w, req)
}

// authorizeDaemonCall decides whether a task may call a daemon-only endpoint
// (path is relative to /api/daemon/) and returns the credential to forward.
// Unknown endpoints are denied: the allowlist is the security boundary.
func (r *Relay) authorizeDaemonCall(t Task, method, path string) (string, bool) {
	seg := strings.Split(strings.Trim(path, "/"), "/")
	for _, s := range seg {
		if s == "" || s == ".." || s == "." {
			return "", false
		}
	}

	switch {
	// tasks/{id}/...
	case len(seg) >= 3 && seg[0] == "tasks":
		if seg[1] != t.TaskID {
			return "", false
		}
		return r.authorizeTaskCall(t, method, seg[2:])

	// runtimes/{rt}/tasks/{id}/prepare-lease | skill-bundles/resolve
	case len(seg) >= 5 && seg[0] == "runtimes" && seg[2] == "tasks":
		if seg[1] != t.RuntimeID || seg[3] != t.TaskID {
			return "", false
		}
		rest := strings.Join(seg[4:], "/")
		if method == http.MethodPost && (rest == "prepare-lease" || rest == "skill-bundles/resolve") {
			return r.daemonToken, true
		}

	// workspaces/{ws}/repos, refreshed by /repo/checkout
	case len(seg) == 3 && seg[0] == "workspaces" && seg[2] == "repos":
		if seg[1] == t.WorkspaceID && method == http.MethodGet {
			return r.daemonToken, true
		}
	}
	return "", false
}

func (r *Relay) authorizeTaskCall(t Task, method string, rest []string) (string, bool) {
	switch strings.Join(rest, "/") {
	case "start", "progress", "usage", "messages", "complete", "fail",
		"cancel-ack", "session", "supplements/claim":
		if method == http.MethodPost {
			return r.daemonToken, true
		}
	case "status":
		if method == http.MethodGet {
			return r.daemonToken, true
		}
	case "plugin-hooks":
		if method == http.MethodPost && t.RemoteMCPToken != "" {
			return t.RemoteMCPToken, true
		}
	}

	// supplements/{commentId}/ack and plugin-mcp/{contributionId}/credential
	if len(rest) == 3 && rest[0] == "supplements" && rest[2] == "ack" && method == http.MethodPost {
		return r.daemonToken, true
	}
	if len(rest) == 3 && rest[0] == "plugin-mcp" && rest[2] == "credential" &&
		method == http.MethodGet && t.RemoteMCPToken != "" {
		return t.RemoteMCPToken, true
	}
	return "", false
}
