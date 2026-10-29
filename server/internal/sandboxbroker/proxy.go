package sandboxbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
)

type proxyContextKey struct{}

// proxyRoute carries per-request forwarding decisions into the reverse proxy.
type proxyRoute struct {
	// brokerAuth replaces the sandbox credential with the broker's token.
	brokerAuth bool
	// after runs with the upstream status code of a forwarded call.
	after func(status int)
}

// Handler serves the sandbox-facing API. See the package comment for the
// routing model; unknown daemon endpoints are refused and logged so a newer
// daemon's protocol additions are noticed instead of silently misrouted.
func (b *Broker) Handler() http.Handler {
	target, _ := url.Parse(b.cfg.MulticaURL)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			if route, _ := pr.In.Context().Value(proxyContextKey{}).(*proxyRoute); route != nil && route.brokerAuth {
				pr.Out.Header.Set("Authorization", "Bearer "+b.cfg.MulticaToken)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if route, _ := resp.Request.Context().Value(proxyContextKey{}).(*proxyRoute); route != nil && route.after != nil {
				route.after(resp.StatusCode)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			b.log.Warn("forward sandbox request", "path", r.URL.Path, "error", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
	forward := func(w http.ResponseWriter, r *http.Request, route *proxyRoute) {
		rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), proxyContextKey{}, route)))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			writeJSON(w, http.StatusOK, okBody)
			return
		}
		key, isSandbox := b.sandboxFor(r)
		if !strings.HasPrefix(r.URL.Path, "/api/daemon/") && r.URL.Path != "/api/workspaces" && r.URL.Path != "/api/tokens/current/renew" {
			// Agent traffic authenticated with its own task token.
			if isSandbox {
				b.refuse(w, r, key, "sandbox credential is only valid for daemon endpoints")
				return
			}
			forward(w, r, &proxyRoute{})
			return
		}
		if !isSandbox {
			// Task-scoped daemon credentials minted by the server (remote MCP,
			// plugin hooks) authenticate themselves upstream.
			if bearer(r) != "" && taskTokenPath(r.URL.Path) {
				forward(w, r, &proxyRoute{})
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		b.serveDaemon(w, r, key, forward)
	})
}

func bearer(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(token)
}

func (b *Broker) sandboxFor(r *http.Request) (string, bool) {
	token := bearer(r)
	if token == "" {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key, ok := b.tokens[token]
	return key, ok
}

// taskTokenPath matches daemon endpoints the server authorizes with a
// task-scoped token carried in the claim (RemoteMCPDaemonToken).
func taskTokenPath(path string) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/api/daemon/"), "/")
	if len(parts) < 3 || parts[0] != "tasks" {
		return false
	}
	return parts[2] == "plugin-hooks" || (len(parts) == 5 && parts[4] == "credential")
}

func (b *Broker) refuse(w http.ResponseWriter, r *http.Request, key, why string) {
	b.log.Warn("refused sandbox request", "sandbox", key, "method", r.Method, "path", r.URL.Path, "reason", why)
	http.Error(w, why, http.StatusForbidden)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var okBody = map[string]string{"status": "ok"}

func (b *Broker) serveDaemon(w http.ResponseWriter, r *http.Request, key string, forward func(http.ResponseWriter, *http.Request, *proxyRoute)) {
	path := r.URL.Path
	if path == "/api/workspaces" || path == "/api/daemon/workspaces" {
		writeJSON(w, http.StatusOK, []map[string]string{{"id": b.cfg.WorkspaceID, "name": b.cfg.Name}})
		return
	}
	if path == "/api/tokens/current/renew" {
		writeJSON(w, http.StatusOK, map[string]any{"expires_at": "", "renewed": false})
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/daemon/"), "/")
	brokerForward := func(after func(int)) {
		forward(w, r, &proxyRoute{brokerAuth: true, after: after})
	}
	switch {
	case path == "/api/daemon/register":
		b.serveRegister(w, r, key)
	case path == "/api/daemon/heartbeat":
		b.serveHeartbeat(w, r, key)
	case path == "/api/daemon/deregister":
		// The broker owns the upstream runtime's lifetime.
		writeJSON(w, http.StatusOK, okBody)
	case path == "/api/daemon/ws":
		// No wakeup socket: the daemon keeps polling claims over HTTP.
		http.NotFound(w, r)
	case path == "/api/daemon/tasks/claim":
		var req struct {
			MaxTasks int `json:"max_tasks"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
		writeJSON(w, http.StatusOK, map[string]any{"tasks": b.deliver(key, max(req.MaxTasks, 1))})
	case strings.HasSuffix(path, "/gc-check"):
		// Read-only questions about terminal state for local cleanup.
		brokerForward(nil)
	case len(parts) == 3 && parts[0] == "workspaces" && (parts[2] == "repos" || parts[2] == "runtime-profiles"):
		if !strings.EqualFold(parts[1], b.cfg.WorkspaceID) {
			b.refuse(w, r, key, "workspace is not served by this broker")
			return
		}
		if parts[2] == "runtime-profiles" {
			// Custom profiles launch arbitrary commands; sandboxes only run
			// the image's built-in providers.
			writeJSON(w, http.StatusOK, map[string]any{"workspace_id": b.cfg.WorkspaceID, "runtime_profiles": []any{}})
			return
		}
		brokerForward(nil)
	case len(parts) >= 2 && parts[0] == "runtimes":
		b.serveRuntimeScoped(w, r, key, parts, brokerForward)
	case len(parts) >= 3 && parts[0] == "tasks":
		if !b.ownsTask(key, parts[1]) {
			b.refuse(w, r, key, "task is not assigned to this sandbox")
			return
		}
		brokerForward(b.afterTaskCall(parts[1], parts[2]))
	default:
		b.refuse(w, r, key, "daemon endpoint not supported by the sandbox broker")
	}
}

func (b *Broker) serveRuntimeScoped(w http.ResponseWriter, r *http.Request, key string, parts []string, brokerForward func(func(int))) {
	runtimeID := parts[1]
	if !b.knowsRuntime(runtimeID) {
		http.Error(w, "runtime not found", http.StatusNotFound)
		return
	}
	rest := parts[2:]
	switch {
	case len(rest) == 2 && rest[0] == "tasks" && rest[1] == "claim":
		tasks := b.deliver(key, 1)
		var task any
		if len(tasks) > 0 {
			task = tasks[0]
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": task})
	case len(rest) == 1 && rest[0] == "recover-orphans":
		// Upstream this would fail every other sandbox's tasks on the shared
		// runtime; the broker recovers lost sandboxes itself.
		writeJSON(w, http.StatusOK, okBody)
	case len(rest) >= 3 && rest[0] == "tasks":
		if !b.ownsTask(key, rest[1]) {
			b.refuse(w, r, key, "task is not assigned to this sandbox")
			return
		}
		brokerForward(nil)
	case len(rest) == 3 && rest[0] == "models" && rest[2] == "result":
		if !b.takeModelRequest(key, rest[1]) {
			b.refuse(w, r, key, "model list request is not assigned to this sandbox")
			return
		}
		brokerForward(nil)
	case len(rest) >= 3 && (rest[0] == "update" || rest[0] == "local-skills"):
		// The broker answers these itself and never relays them.
		writeJSON(w, http.StatusOK, okBody)
	default:
		b.refuse(w, r, key, "daemon endpoint not supported by the sandbox broker")
	}
}

func (b *Broker) knowsRuntime(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.ContainsFunc(b.state.Runtimes, func(rt runtimeRef) bool { return rt.ID == id })
}

func (b *Broker) ownsTask(key, id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.state.Tasks[id]
	return ok && t.Key == key
}

func (b *Broker) afterTaskCall(id, action string) func(int) {
	return func(status int) {
		if status < 200 || status > 299 {
			return
		}
		switch action {
		case "start":
			b.mu.Lock()
			if t, ok := b.state.Tasks[id]; ok {
				t.State = taskStarted
				b.persistLocked()
			}
			b.mu.Unlock()
		case "complete", "fail":
			b.dropTask(id, "sandbox reported "+action)
		}
	}
}

func (b *Broker) deliver(key string, limit int) []json.RawMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []json.RawMessage
	for _, t := range b.state.Tasks {
		if len(out) >= limit {
			break
		}
		if t.Key == key && t.State == taskQueued {
			t.State = taskDelivered
			out = append(out, t.Claim)
		}
	}
	if len(out) > 0 {
		b.persistLocked()
	}
	if out == nil {
		out = []json.RawMessage{}
	}
	return out
}

func (b *Broker) takeModelRequest(key, id string) bool {
	if key != controlKey {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if req, ok := b.modelRequests[id]; ok && req.Delivered {
		delete(b.modelRequests, id)
		return true
	}
	return false
}

// serveRegister answers from the upstream registration. A registration from
// a sandbox running the configured image also refreshes the replayed
// identity, which is how the broker learns it the first time.
func (b *Broker) serveRegister(w http.ResponseWriter, r *http.Request, key string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req struct {
		Runtimes []struct {
			Type string `json:"type"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Per-sandbox fields would otherwise make every new sandbox look like a
	// new identity and re-register upstream.
	raw, err = machineIndependent(raw)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	headers := map[string]string{}
	for _, h := range []string{"X-Client-Platform", "X-Client-Version", "X-Client-OS", "X-Client-Capabilities"} {
		if v := r.Header.Get(h); v != "" {
			headers[h] = v
		}
	}

	b.mu.Lock()
	sb := b.state.Sandboxes[key]
	current := sb != nil && sb.Image == b.cfg.Image
	changed := current && (b.state.Identity == nil || b.state.Identity.Image != b.cfg.Image ||
		!bytes.Equal(b.state.Identity.Register, raw) || !maps.Equal(b.state.Identity.Headers, headers))
	if changed {
		b.state.Identity = &identity{Image: b.cfg.Image, Register: raw, Headers: headers}
		b.persistLocked()
	}
	b.mu.Unlock()
	if changed {
		if err := b.registerUpstream(r.Context()); err != nil {
			b.log.Warn("register runtime from sandbox identity", "sandbox", key, "error", err)
			http.Error(w, "runtime registration unavailable", http.StatusServiceUnavailable)
			return
		}
	}

	b.mu.Lock()
	registration := b.state.Registration
	b.mu.Unlock()
	if len(registration) == 0 {
		http.Error(w, "runtime registration unavailable", http.StatusServiceUnavailable)
		return
	}
	var resp map[string]json.RawMessage
	var runtimes []map[string]any
	if json.Unmarshal(registration, &resp) != nil || json.Unmarshal(resp["runtimes"], &runtimes) != nil {
		http.Error(w, "runtime registration unavailable", http.StatusServiceUnavailable)
		return
	}
	offered := map[string]bool{}
	for _, rt := range req.Runtimes {
		offered[rt.Type] = true
	}
	kept := runtimes[:0]
	for _, rt := range runtimes {
		if provider, _ := rt["provider"].(string); offered[provider] {
			kept = append(kept, rt)
		}
	}
	resp["runtimes"], _ = json.Marshal(kept)
	writeJSON(w, http.StatusOK, resp)
}

func machineIndependent(raw []byte) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	for _, field := range []string{"daemon_id", "device_name", "legacy_daemon_ids", "launched_by", "failed_profiles"} {
		delete(req, field)
	}
	return json.Marshal(req)
}

func (b *Broker) serveHeartbeat(w http.ResponseWriter, r *http.Request, key string) {
	var req struct {
		RuntimeID string `json:"runtime_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !b.knowsRuntime(req.RuntimeID) {
		http.Error(w, "runtime not found", http.StatusNotFound)
		return
	}
	resp := map[string]any{"runtime_id": req.RuntimeID, "status": "ok"}
	b.mu.Lock()
	if len(b.serverCaps) > 0 {
		resp["server_capabilities"] = b.serverCaps
	}
	if key == controlKey {
		for id, cr := range b.modelRequests {
			if cr.RuntimeID == req.RuntimeID && !cr.Delivered {
				cr.Delivered = true
				resp["pending_model_list"] = map[string]string{"id": id}
				break
			}
		}
	}
	b.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}
