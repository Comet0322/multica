package sandboxbroker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"
)

// maxLeaseHistory bounds the closed leases kept for the lease query.
const maxLeaseHistory = 1000

// lease is the period in which one pod is assigned one task.
type lease struct {
	TaskID    string
	PodIP     string
	StartedAt time.Time
	EndedAt   time.Time
	EndReason string
	// Initiator and PodName are kept for the audit trail.
	Initiator string
	PodName   string

	pod *pod
	// key is the task's LiteLLM key, revoked when the lease ends.
	key *taskKey
}

// LeaseInfo is one lease in the lease query. EndedAt is nil while the lease is
// active.
type LeaseInfo struct {
	TaskID    string     `json:"task_id"`
	PodIP     string     `json:"pod_ip"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
}

// Leases lists active and recently closed leases, oldest first.
func (p *Pool) Leases() []LeaseInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]LeaseInfo, 0, len(p.history))
	for _, l := range p.history {
		info := LeaseInfo{TaskID: l.TaskID, PodIP: l.PodIP, StartedAt: l.StartedAt, EndReason: l.EndReason}
		if !l.EndedAt.IsZero() {
			ended := l.EndedAt
			info.EndedAt = &ended
		}
		out = append(out, info)
	}
	return out
}

// startLeaseLocked assigns a task to an idle pod. Callers hold p.mu.
func (p *Pool) startLeaseLocked(pd *pod, taskID, initiator string) *lease {
	l := &lease{TaskID: taskID, PodIP: pd.IP, StartedAt: p.now(), Initiator: initiator, PodName: pd.Name, pod: pd}
	pd.lease = l
	pd.State = PodLeased
	p.leases[taskID] = l
	p.history = append(p.history, l)
	p.auditLease(AuditLease, l, "pod leased to task")
	if len(p.history) > maxLeaseHistory {
		keep := p.history[:0]
		drop := len(p.history) - maxLeaseHistory
		for _, old := range p.history {
			if drop > 0 && !old.EndedAt.IsZero() {
				drop--
				continue
			}
			keep = append(keep, old)
		}
		p.history = keep
	}
	return l
}

// endLeaseLocked closes a lease and, if its pod is still the registered one
// and has not been replaced, frees the pod. Callers hold p.mu. A nil or
// already closed lease is ignored.
func (p *Pool) endLeaseLocked(l *lease, reason string) {
	if l == nil || !l.EndedAt.IsZero() {
		return
	}
	l.EndedAt = p.now()
	l.EndReason = reason
	delete(p.leases, l.TaskID)
	p.auditLease(AuditRelease, l, reason)
	p.revokeKey(l.key, l.TaskID)
	l.key = nil
	if pd := l.pod; pd != nil && pd.lease == l {
		pd.lease = nil
		if pd.State == PodLeased && p.pods[pd.IP] == pd {
			// The pod rejoins the pool only after the supervisor has cleaned
			// it and the daemon registered again.
			p.startResetLocked(pd)
		}
	}
}

// failUpstream reports a task the manager claimed as failed, with the reason.
func (p *Pool) failUpstream(ctx context.Context, taskID, reason string) {
	err := p.up.call(ctx, http.MethodPost, "/api/daemon/tasks/"+url.PathEscape(taskID)+"/fail", map[string]any{"error": reason}, nil)
	if err != nil {
		p.log.Warn("fail task upstream", "task_id", taskID, "error", err)
	}
}

// serveDaemon answers the daemon endpoints beyond registration and heartbeat.
// Endpoints not listed are refused and logged, so a newer daemon's protocol
// additions are noticed instead of silently misrouted.
func (p *Pool) serveDaemon(w http.ResponseWriter, r *http.Request, ip string) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/daemon/")
	if !ok {
		p.refuse(w, r, ip, "endpoint not supported by the pool manager")
		return
	}
	parts := strings.Split(rest, "/")
	switch {
	case rest == "deregister":
		// The manager owns the upstream runtime's lifetime.
		writeJSON(w, http.StatusOK, okBody)
	case rest == "ws":
		// No wakeup socket: the daemon keeps polling claims over HTTP.
		http.NotFound(w, r)
	case rest == "tasks/claim":
		var req struct {
			RuntimeIDs []string `json:"runtime_ids"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
		if task, ok := p.claimFor(w, r, ip, req.RuntimeIDs); ok {
			tasks := []json.RawMessage{}
			if task != nil {
				tasks = append(tasks, task)
			}
			writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
		}
	case len(parts) == 3 && parts[0] == "workspaces" && (parts[2] == "repos" || parts[2] == "runtime-profiles"):
		if !strings.EqualFold(parts[1], p.cfg.WorkspaceID) {
			p.refuse(w, r, ip, "workspace is not served by this pool manager")
			return
		}
		if parts[2] == "runtime-profiles" {
			// Custom profiles launch arbitrary commands; pods only run the
			// image's built-in providers.
			writeJSON(w, http.StatusOK, map[string]any{"workspace_id": p.cfg.WorkspaceID, "runtime_profiles": []any{}})
			return
		}
		p.forward(w, r, &proxyRoute{brokerAuth: true})
	case len(parts) >= 3 && parts[0] == "tasks":
		p.serveTask(w, r, ip, parts[1], parts[2])
	case len(parts) >= 2 && parts[0] == "runtimes":
		p.serveRuntimeScoped(w, r, ip, parts)
	default:
		p.refuse(w, r, ip, "daemon endpoint not supported by the pool manager")
	}
}

func (p *Pool) serveRuntimeScoped(w http.ResponseWriter, r *http.Request, ip string, parts []string) {
	if !p.knowsRuntime(parts[1]) {
		http.Error(w, "runtime not found", http.StatusNotFound)
		return
	}
	rest := parts[2:]
	switch {
	case len(rest) == 2 && rest[0] == "tasks" && rest[1] == "claim":
		task, ok := p.claimFor(w, r, ip, []string{parts[1]})
		if !ok {
			return
		}
		var out any
		if task != nil {
			out = task
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": out})
	case len(rest) == 1 && rest[0] == "recover-orphans":
		// Upstream this would fail every other pod's tasks on the shared
		// runtime; the manager handles lost pods itself.
		writeJSON(w, http.StatusOK, okBody)
	case len(rest) >= 3 && rest[0] == "tasks":
		p.serveTask(w, r, ip, rest[1], rest[2])
	case len(rest) >= 3 && (rest[0] == "update" || rest[0] == "local-skills"):
		// The manager answers these itself and never relays them.
		writeJSON(w, http.StatusOK, okBody)
	default:
		p.refuse(w, r, ip, "daemon endpoint not supported by the pool manager")
	}
}

func (p *Pool) knowsRuntime(id string) bool {
	return slices.Contains(p.runtimeIDs(), id)
}

// runtimeIDs lists the upstream runtime IDs of the registration.
func (p *Pool) runtimeIDs() []string {
	p.mu.Lock()
	registration := p.registration
	p.mu.Unlock()
	var resp struct {
		Runtimes []runtimeRef `json:"runtimes"`
	}
	_ = json.Unmarshal(registration, &resp)
	ids := make([]string, 0, len(resp.Runtimes))
	for _, rt := range resp.Runtimes {
		ids = append(ids, rt.ID)
	}
	return ids
}

// claimFor claims at most one task upstream for the polling pod. It claims
// only when the pod is idle: a pod that is leased, resetting or lost, or a
// caller that is not a registered pod, gets nothing and causes no upstream
// call, so tasks stay queued in Multica until a free pod asks. The returned
// task is nil when there is none. ok is false when a response was already
// written.
func (p *Pool) claimFor(w http.ResponseWriter, r *http.Request, ip string, wanted []string) (task json.RawMessage, ok bool) {
	p.mu.Lock()
	p.markLostLocked()
	pd := p.pods[ip]
	if pd == nil {
		p.mu.Unlock()
		http.Error(w, "pod not registered", http.StatusNotFound)
		return nil, false
	}
	if pd.State != PodIdle || pd.claiming {
		p.mu.Unlock()
		return nil, true
	}
	pd.claiming = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		pd.claiming = false
		p.mu.Unlock()
	}()

	known := p.runtimeIDs()
	ids := known
	if len(wanted) > 0 {
		ids = slices.DeleteFunc(slices.Clone(wanted), func(id string) bool { return !slices.Contains(known, id) })
	}
	if len(ids) == 0 {
		return nil, true
	}
	var resp struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	body := map[string]any{"daemon_id": p.cfg.BrokerID, "runtime_ids": ids, "max_tasks": 1}
	if err := p.up.call(r.Context(), http.MethodPost, "/api/daemon/tasks/claim", body, &resp); err != nil {
		p.log.Warn("claim task for pod", "pod_ip", ip, "error", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return nil, false
	}
	if len(resp.Tasks) == 0 {
		return nil, true
	}
	// max_tasks is 1; anything beyond that would be an upstream bug, and the
	// server reclaims what is never started.
	raw := resp.Tasks[0]
	var ref claimedTaskRef
	if err := json.Unmarshal(raw, &ref); err != nil || ref.ID == "" {
		p.log.Error("claimed task has no ID; it will be reclaimed by the server", "pod_ip", ip)
		return nil, true
	}
	if !strings.EqualFold(ref.WorkspaceID, p.cfg.WorkspaceID) {
		const why = "the managed runtime does not serve this task's workspace"
		p.auditDenied(ref, why)
		p.failUpstream(r.Context(), ref.ID, why)
		return nil, true
	}
	if ref.initiator() == "" {
		const why = "the task has no human initiator; the managed runtime only runs tasks on behalf of a person"
		p.auditDenied(ref, why)
		p.failUpstream(r.Context(), ref.ID, why)
		return nil, true
	}

	var key *taskKey
	if p.llm != nil {
		var err error
		if raw, key, err = p.issueTaskKey(r.Context(), raw, ref); err != nil {
			why := "the model key for this task could not be prepared"
			var lf *llmFailure
			if errors.As(err, &lf) {
				why = lf.msg
			}
			p.log.Warn("issue task key", "task_id", ref.ID, "error", err)
			p.auditDenied(ref, why)
			p.failUpstream(r.Context(), ref.ID, why)
			return nil, true
		}
	}

	p.mu.Lock()
	if p.pods[ip] != pd || pd.State != PodIdle {
		// The pod was replaced or went silent while the claim was in flight.
		p.mu.Unlock()
		p.revokeKey(key, ref.ID)
		const why = "the pod was replaced before the task could start"
		p.auditDenied(ref, why)
		p.failUpstream(r.Context(), ref.ID, why)
		return nil, true
	}
	p.startLeaseLocked(pd, ref.ID, ref.initiator()).key = key
	p.mu.Unlock()
	p.log.Info("leased pod to task", "pod_ip", ip, "task_id", ref.ID)
	return raw, true
}

// serveTask forwards a pod's report on a task to Multica, provided the task is
// leased to that pod. A terminal report ends the lease.
func (p *Pool) serveTask(w http.ResponseWriter, r *http.Request, ip, taskID, action string) {
	p.mu.Lock()
	l := p.leases[taskID]
	owned := l != nil && l.PodIP == ip && l.pod != nil && p.pods[ip] == l.pod
	p.mu.Unlock()
	if !owned {
		p.refuse(w, r, ip, "task is not assigned to this pod")
		return
	}
	route := &proxyRoute{brokerAuth: true, after: func(status int) {
		if status < 200 || status > 299 || (action != "complete" && action != "fail") {
			return
		}
		p.mu.Lock()
		p.endLeaseLocked(l, "pod reported "+action)
		p.mu.Unlock()
	}}
	if taskTokenPath(r.URL.Path) {
		// The server authorizes these with the task-scoped token the daemon
		// carries; it must reach the server unchanged.
		route.brokerAuth = false
	}
	p.forward(w, r, route)
}

func (p *Pool) refuse(w http.ResponseWriter, r *http.Request, ip, why string) {
	p.log.Warn("refused pod request", "pod_ip", ip, "method", r.Method, "path", r.URL.Path, "reason", why)
	http.Error(w, why, http.StatusForbidden)
}

// forward relays a pod request to Multica under the route's rules.
func (p *Pool) forward(w http.ResponseWriter, r *http.Request, route *proxyRoute) {
	p.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), proxyContextKey{}, route)))
}

func (p *Pool) newProxy() *httputil.ReverseProxy {
	target, _ := url.Parse(p.cfg.MulticaURL)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			if route, _ := pr.In.Context().Value(proxyContextKey{}).(*proxyRoute); route != nil && route.brokerAuth {
				pr.Out.Header.Set("Authorization", "Bearer "+p.cfg.MulticaToken)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if route, _ := resp.Request.Context().Value(proxyContextKey{}).(*proxyRoute); route != nil && route.after != nil {
				route.after(resp.StatusCode)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.log.Warn("forward pod request", "path", r.URL.Path, "error", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
}
