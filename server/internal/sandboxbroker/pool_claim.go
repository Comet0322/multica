package sandboxbroker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
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
	// WorkspaceID is the workspace the pod is bound to for this lease.
	WorkspaceID string

	pod *pod
	ws  *workspace
	// key is the task's LiteLLM key, revoked when the lease ends.
	key *taskKey
}

// LeaseInfo is one lease in the lease query. EndedAt is nil while the lease is
// active.
type LeaseInfo struct {
	TaskID      string     `json:"task_id"`
	WorkspaceID string     `json:"workspace_id"`
	PodIP       string     `json:"pod_ip"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	EndReason   string     `json:"end_reason,omitempty"`
}

// Leases lists active and recently closed leases, oldest first.
func (p *Pool) Leases() []LeaseInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]LeaseInfo, 0, len(p.history))
	for _, l := range p.history {
		info := LeaseInfo{TaskID: l.TaskID, WorkspaceID: l.WorkspaceID, PodIP: l.PodIP, StartedAt: l.StartedAt, EndReason: l.EndReason}
		if !l.EndedAt.IsZero() {
			ended := l.EndedAt
			info.EndedAt = &ended
		}
		out = append(out, info)
	}
	return out
}

// startLeaseLocked assigns a task to an idle pod. Callers hold p.mu.
func (p *Pool) startLeaseLocked(pd *pod, ws *workspace, taskID, initiator string) *lease {
	l := &lease{TaskID: taskID, PodIP: pd.IP, StartedAt: p.now(), Initiator: initiator, PodName: pd.Name, pod: pd, ws: ws, WorkspaceID: ws.cfg.WorkspaceID}
	if pd.leases == nil {
		pd.leases = map[string]*lease{}
	}
	pd.leases[taskID] = l
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
	if pd := l.pod; pd != nil && pd.leases[l.TaskID] == l {
		delete(pd.leases, l.TaskID)
		if pd.State == PodLeased && p.pods[pd.IP] == pd && len(pd.leases) == 0 && len(pd.queue) == 0 {
			// The pod rejoins the pool only after the supervisor has cleaned
			// it and the daemon registered again. A shared pod gets there once
			// its last task has ended.
			p.startResetLocked(pd)
		}
	}
}

// failUpstream reports a task the manager claimed as failed, with the reason.
func (p *Pool) failUpstream(ctx context.Context, ws *workspace, taskID, reason string) {
	if ws == nil {
		return
	}
	err := ws.up.call(ctx, http.MethodPost, "/api/daemon/tasks/"+url.PathEscape(taskID)+"/fail", map[string]any{"error": reason}, nil)
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
		ws := p.workspaceByID(parts[1])
		if ws == nil {
			p.refuse(w, r, ip, "workspace is not served by this pool manager")
			return
		}
		if parts[2] == "runtime-profiles" {
			// Custom profiles launch arbitrary commands; pods only run the
			// image's built-in providers.
			writeJSON(w, http.StatusOK, map[string]any{"workspace_id": ws.cfg.WorkspaceID, "runtime_profiles": []any{}})
			return
		}
		p.forward(w, r, &proxyRoute{brokerAuth: true, token: ws.cfg.Token})
	case len(parts) >= 3 && parts[0] == "tasks":
		p.serveTask(w, r, ip, parts[1], parts[2])
	case len(parts) >= 2 && parts[0] == "runtimes":
		p.serveRuntimeScoped(w, r, ip, parts)
	default:
		p.refuse(w, r, ip, "daemon endpoint not supported by the pool manager")
	}
}

func (p *Pool) serveRuntimeScoped(w http.ResponseWriter, r *http.Request, ip string, parts []string) {
	if p.workspaceByRuntime(parts[1]) == nil {
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

// claimFor hands the polling pod a task assigned to it, or claims at most one
// task upstream and places it (see placeLocked). It claims
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
	expired := p.rebalanceLocked(pd)
	var queued json.RawMessage
	if t := p.takeQueuedLocked(pd); t != nil {
		queued = t.raw
	}
	canClaim := queued == nil && p.canClaimLocked(pd)
	if canClaim {
		pd.claiming = true
	}
	p.mu.Unlock()
	p.failParked(r.Context(), expired)
	if queued != nil {
		return queued, true
	}
	if !canClaim {
		return nil, true
	}
	defer func() {
		p.mu.Lock()
		pd.claiming = false
		p.mu.Unlock()
	}()

	p.mu.Lock()
	targets := p.claimTargetsLocked(pd, wanted)
	p.mu.Unlock()

	attempted, failed := 0, 0
	for _, t := range targets {
		if !p.reserveClaim(t.ws) {
			continue
		}
		attempted++
		p.mu.Lock()
		p.rotor = (t.idx + 1) % len(p.wsOrder)
		p.mu.Unlock()
		raw, err := p.claimUpstream(r.Context(), t)
		p.finishClaim(t.ws, err != nil)
		if err != nil {
			// One workspace's upstream failing must not hold back the others.
			failed++
			p.log.Warn("claim task for pod", "pod_ip", ip, "workspace_id", t.ws.cfg.WorkspaceID, "error", err)
			continue
		}
		if raw == nil {
			continue
		}
		return p.leaseClaimed(r.Context(), pd, t.ws, raw), true
	}
	if attempted > 0 && failed == attempted {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return nil, false
	}
	return nil, true
}

// claimUpstream claims at most one task from one workspace's runtimes. A nil
// result with a nil error means the workspace has nothing queued.
func (p *Pool) claimUpstream(ctx context.Context, t claimTarget) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, upstreamClaimTimeout)
	defer cancel()
	var resp struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	body := map[string]any{"daemon_id": t.ws.cfg.BrokerID, "runtime_ids": t.runtimeIDs, "max_tasks": 1}
	if err := t.ws.up.call(ctx, http.MethodPost, "/api/daemon/tasks/claim", body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Tasks) == 0 {
		return nil, nil
	}
	// max_tasks is 1; anything beyond that would be an upstream bug, and the
	// server reclaims what is never started.
	return resp.Tasks[0], nil
}

// leaseClaimed binds a claimed task's pod to the task's workspace, or fails the
// task upstream when it cannot run. It returns the task for the pod, or nil.
func (p *Pool) leaseClaimed(ctx context.Context, pd *pod, ws *workspace, raw json.RawMessage) json.RawMessage {
	ip := pd.IP
	var ref claimedTaskRef
	if err := json.Unmarshal(raw, &ref); err != nil || ref.ID == "" {
		p.log.Error("claimed task has no ID; it will be reclaimed by the server", "pod_ip", ip, "workspace_id", ws.cfg.WorkspaceID)
		return nil
	}
	if !strings.EqualFold(ref.WorkspaceID, ws.cfg.WorkspaceID) {
		const why = "the managed runtime does not serve this task's workspace"
		p.auditDenied(ws, ref, why)
		p.failUpstream(ctx, ws, ref.ID, why)
		return nil
	}
	if ref.initiator() == "" {
		const why = "the task has no human initiator; the managed runtime only runs tasks on behalf of a person"
		p.auditDenied(ws, ref, why)
		p.failUpstream(ctx, ws, ref.ID, why)
		return nil
	}

	var key *taskKey
	if p.llm != nil {
		var err error
		if raw, key, err = p.issueTaskKey(ctx, raw, ref); err != nil {
			why := "the model key for this task could not be prepared"
			var lf *llmFailure
			if errors.As(err, &lf) {
				why = lf.msg
			}
			p.log.Warn("issue task key", "task_id", ref.ID, "error", err)
			p.auditDenied(ws, ref, why)
			p.failUpstream(ctx, ws, ref.ID, why)
			return nil
		}
	}

	t := &parkedTask{raw: raw, ref: ref, ws: ws, key: p.placementKey(ws, ref), llmKey: key, claimedAt: p.now()}
	p.mu.Lock()
	if p.pods[ip] != pd || (pd.State != PodIdle && pd.State != PodLeased) {
		// The pod was replaced or went silent while the claim was in flight.
		p.mu.Unlock()
		p.revokeKey(key, ref.ID)
		const why = "the pod was replaced before the task could start"
		p.auditDenied(ws, ref, why)
		p.failUpstream(ctx, ws, ref.ID, why)
		return nil
	}
	target := p.placeLocked(pd, ws, t.key)
	if target == nil {
		// No pod can take it yet; a later poll places it, or it times out.
		p.waiting = append(p.waiting, t)
		p.mu.Unlock()
		p.log.Info("claimed task waits for a pod", "task_id", ref.ID, "pod_ip", ip, "workspace_id", ws.cfg.WorkspaceID)
		return nil
	}
	p.assignLocked(target, t)
	if target != pd {
		p.mu.Unlock()
		p.log.Info("claimed task assigned to another pod", "task_id", ref.ID, "poll_ip", ip, "pod_ip", target.IP, "workspace_id", ws.cfg.WorkspaceID)
		return nil
	}
	p.takeQueuedLocked(pd)
	p.mu.Unlock()
	p.log.Info("leased pod to task", "pod_ip", ip, "task_id", ref.ID, "workspace_id", ws.cfg.WorkspaceID)
	return raw
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
	route := &proxyRoute{brokerAuth: true, token: l.ws.cfg.Token, after: func(status int) {
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
				pr.Out.Header.Set("Authorization", "Bearer "+route.token)
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
