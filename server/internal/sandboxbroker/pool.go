package sandboxbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Pod states. A pod is identified by the source IP of its requests; the pool
// itself is a registry that pods join by registering.
const (
	PodIdle      = "idle"
	PodLeased    = "leased"
	PodResetting = "resetting"
	PodLost      = "lost"
)

// PoolConfig configures a Pool.
type PoolConfig struct {
	MulticaURL string
	// Workspaces lists the workspaces the manager serves from the shared pool.
	// For a single-workspace deployment MulticaToken, WorkspaceID and BrokerID
	// may be set instead; they describe one entry and cannot be combined with
	// Workspaces.
	Workspaces   []WorkspaceConfig
	MulticaToken string
	WorkspaceID  string
	// BrokerID is the upstream daemon_id. It must stay stable across restarts
	// or the server registers a new runtime row.
	BrokerID string
	// Name is the machine name and the default runtime name members see.
	Name string
	// UpstreamBackoff is how long a workspace is skipped after its upstream
	// failed a claim. Zero means DefaultUpstreamBackoff.
	UpstreamBackoff time.Duration
	// ExpectedSize is the number of pods the deployment declares, reported next
	// to the live counts so a shortfall is visible.
	ExpectedSize int
	// Providers limits which agent CLIs a pod reports become upstream
	// runtimes. Empty exposes every reported provider.
	Providers []string
	// HeartbeatTimeout is how long a pod may stay silent before it is listed
	// as lost. Zero means DefaultHeartbeatTimeout.
	HeartbeatTimeout time.Duration
	// ResetTimeout is how long a pod may stay in the resetting state before it
	// is listed as lost. Zero means DefaultResetTimeout.
	ResetTimeout time.Duration
	// AuditLog receives the audit records as JSON Lines. Nil disables auditing.
	AuditLog io.Writer
	// Placement decides which tasks share a pod: PlacementTask (the default,
	// one task per pod) or PlacementIssue (one issue or chat conversation per
	// pod, tasks of that unit running side by side).
	Placement Placement
	// PodConcurrency is how many tasks a shared pod runs at once. It is 1 in
	// task placement; in issue placement zero means DefaultPodConcurrency.
	PodConcurrency int
	// PlacementWait bounds how long a claimed task waits in the manager for a
	// pod that can take it. Zero means DefaultPlacementWait.
	PlacementWait time.Duration
	// PodIdleTimeout releases a shared pod that has run no task for this long.
	// Zero means DefaultPodIdleTimeout. Shared (issue) placement only.
	PodIdleTimeout time.Duration
	// PodMaxLifetime stops a shared pod from taking new tasks this long after
	// it was bound. Zero means DefaultPodMaxLifetime.
	PodMaxLifetime time.Duration
	// PodDrainGrace is how long the tasks still running at that point may
	// finish before the pod is released and they are failed. Zero means
	// DefaultPodDrainGrace.
	PodDrainGrace time.Duration
	// LiteLLM, when set, gives every task its own short-lived LiteLLM key
	// owned by the initiator. Nil leaves model access to the agent's own
	// settings.
	LiteLLM *LiteLLMConfig
	// Git, when set, gives every task a git gateway token and serves the
	// gateway's verification endpoints on AdminHandler. Nil leaves git to
	// the image's own setup.
	Git *GitGatewayConfig
}

// DefaultHeartbeatTimeout is three daemon heartbeat intervals (15s each).
const DefaultHeartbeatTimeout = 45 * time.Second

// DefaultResetTimeout bounds one reset: killing processes, emptying the
// writable areas and restarting the daemon until it registers again.
const DefaultResetTimeout = 2 * time.Minute

func (c *PoolConfig) normalize() error {
	if c.HeartbeatTimeout < 0 {
		return fmt.Errorf("heartbeat timeout must not be negative")
	}
	if c.HeartbeatTimeout == 0 {
		c.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if c.ResetTimeout < 0 {
		return fmt.Errorf("reset timeout must not be negative")
	}
	if c.ResetTimeout == 0 {
		c.ResetTimeout = DefaultResetTimeout
	}
	if c.UpstreamBackoff < 0 {
		return fmt.Errorf("upstream backoff must not be negative")
	}
	if c.UpstreamBackoff == 0 {
		c.UpstreamBackoff = DefaultUpstreamBackoff
	}
	if err := checkBaseURL(c.MulticaURL); err != nil {
		return fmt.Errorf("Multica URL: %w", err)
	}
	c.MulticaURL = strings.TrimRight(c.MulticaURL, "/")
	if c.LiteLLM != nil {
		llm := *c.LiteLLM
		if err := llm.normalize(); err != nil {
			return err
		}
		c.LiteLLM = &llm
	}
	if c.Git != nil {
		git := *c.Git
		if err := git.normalize(); err != nil {
			return err
		}
		c.Git = &git
	}
	if c.ExpectedSize < 0 {
		return fmt.Errorf("expected pool size must not be negative")
	}
	if err := c.normalizePlacement(); err != nil {
		return err
	}
	if c.Name == "" {
		c.Name = "Managed Sandbox"
	}
	legacy := c.WorkspaceID != "" || c.MulticaToken != "" || c.BrokerID != ""
	if len(c.Workspaces) > 0 && legacy {
		return fmt.Errorf("set either Workspaces or the single-workspace fields, not both")
	}
	if len(c.Workspaces) == 0 {
		c.Workspaces = []WorkspaceConfig{{WorkspaceID: c.WorkspaceID, Token: c.MulticaToken, BrokerID: c.BrokerID}}
	}
	c.Workspaces = slices.Clone(c.Workspaces)
	c.WorkspaceID, c.MulticaToken, c.BrokerID = "", "", ""
	seen := map[string]bool{}
	for i := range c.Workspaces {
		if c.Workspaces[i].Placement == "" {
			c.Workspaces[i].Placement = c.Placement
		}
		if err := c.Workspaces[i].normalize(c.MulticaURL, c.Name); err != nil {
			return err
		}
		id := c.Workspaces[i].WorkspaceID
		if seen[id] {
			return fmt.Errorf("workspace %s is configured twice", id)
		}
		seen[id] = true
	}
	return nil
}

// Pool is the pool manager: to Multica it is one daemon, to each pod it is the
// server. Pods announce themselves by registering; the manager keeps no
// knowledge of how they are deployed.
type Pool struct {
	cfg PoolConfig
	log *slog.Logger
	now func() time.Time

	// workspaces are the served workspaces by ID; wsOrder is the stable order
	// the fair rotation walks.
	workspaces map[string]*workspace
	wsOrder    []*workspace

	mu sync.Mutex
	// rotor is the index in wsOrder where the next claim starts.
	rotor int
	// pods is keyed by source IP.
	pods map[string]*pod
	// identity headers are taken from the first pod and replayed to every
	// other one; pods are homogeneous. Each workspace keeps its own runtime
	// registration.
	headers map[string]string
	// leases holds the active leases by task ID; history keeps them, closed
	// ones included, for the lease query.
	leases  map[string]*lease
	history []*lease
	// waiting holds claimed tasks no pod could take yet.
	waiting []*parkedTask
	// forced holds tasks cut off by a pod's expiry, not yet failed upstream.
	forced []forcedTask
	proxy  *httputil.ReverseProxy
	// auditLog is nil when auditing is off.
	auditLog *auditLog
	// llm is nil when tasks get no LiteLLM key.
	llm *litellm
}

type pod struct {
	IP           string
	Name         string
	State        string
	BootNonce    string
	RegisteredAt time.Time
	LastSeen     time.Time
	// workspaces are the workspaces the pod's daemon has registered for in its
	// current boot. A pod is lent only to workspaces in this set, so after a
	// reset it serves a workspace only once it registered for it again.
	workspaces map[string]struct{}
	// leases are the tasks the pod holds, by task ID: at most one in task
	// placement, up to the pod concurrency when the pod is shared. boundKey is
	// the placement key the pod serves while it holds leases or queued tasks,
	// and queue the claimed tasks assigned to the pod that it has not picked up
	// yet. claiming is set while the manager is claiming a task upstream on
	// the pod's behalf, so one poll at a time can do that.
	leases   map[string]*lease
	boundKey string
	queue    []*parkedTask
	claiming bool
	// boundAt is when the pod was bound to its key and idleSince when its last
	// task ended (zero until then); a shared pod is kept bound for the next
	// task of the key until the idle timeout. draining is set once the pod
	// passed its maximum lifetime: it takes no new task and is released when
	// its running ones end or the grace runs out.
	boundAt   time.Time
	idleSince time.Time
	draining  bool
	// dirty is set when a lease ends and cleared only by a registration with a
	// new boot nonce: until then the pod may still hold the previous task's
	// files and processes, so it is never lent out. resetID names the reset
	// command the supervisor is asked to run; resetStarted starts the reset
	// timeout; resetReported is set once the supervisor said it was done.
	dirty         bool
	resetID       string
	resetStarted  time.Time
	resetReported bool
}

// markLostLocked turns every pod whose heartbeat is overdue into a lost one.
// Silence cannot tell a restart from a crash, so every state but resetting
// times out the same way. A resetting pod has its daemon stopped on purpose, so
// only the reset timeout applies to it. Callers hold p.mu.
func (p *Pool) markLostLocked() {
	now := p.now()
	for _, pd := range p.pods {
		if pd.State == PodResetting {
			if now.Sub(pd.resetStarted) > p.cfg.ResetTimeout {
				pd.State = PodLost
				p.log.Warn("pod lost: did not register again after reset", "pod_ip", pd.IP, "name", pd.Name, "reset_started", pd.resetStarted)
			}
			continue
		}
		if pd.State != PodLost && now.Sub(pd.LastSeen) > p.cfg.HeartbeatTimeout {
			pd.State = PodLost
			p.log.Warn("pod lost: heartbeat timed out", "pod_ip", pd.IP, "name", pd.Name, "last_seen", pd.LastSeen)
		}
	}
	p.expireSharedLocked()
}

// PodInfo is one pod's entry in the inventory.
type PodInfo struct {
	IP    string `json:"ip"`
	Name  string `json:"name"`
	State string `json:"state"`
	// WorkspaceID is the workspace the pod is lent to; empty unless leased.
	WorkspaceID  string    `json:"workspace_id,omitempty"`
	RegisteredAt time.Time `json:"registered_at"`
	LastSeen     time.Time `json:"last_seen"`
}

// Inventory is the manager's view of the pool. Name is display-only.
type Inventory struct {
	ExpectedSize int `json:"expected_size"`
	Idle         int `json:"idle"`
	Leased       int `json:"leased"`
	Resetting    int `json:"resetting"`
	Lost         int `json:"lost"`
	// Workspaces lists each served workspace with its lease count and cap.
	Workspaces []WorkspaceInfo `json:"workspaces"`
	Pods       []PodInfo       `json:"pods"`
}

func NewPool(cfg PoolConfig, log *slog.Logger) (*Pool, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Pool{cfg: cfg, log: log, now: time.Now, pods: map[string]*pod{}, leases: map[string]*lease{}}
	p.proxy = p.newProxy()
	p.auditLog = newAuditLog(cfg.AuditLog, func(err error) { p.log.Error("write audit record", "error", err) })
	p.workspaces = map[string]*workspace{}
	for _, wc := range cfg.Workspaces {
		ws := &workspace{cfg: wc, up: newUpstream(cfg.MulticaURL, wc.Token, p.identityHeaders)}
		p.workspaces[wc.WorkspaceID] = ws
		p.wsOrder = append(p.wsOrder, ws)
	}
	if cfg.LiteLLM != nil {
		p.llm = newLiteLLM(*cfg.LiteLLM)
	}
	return p, nil
}

func (p *Pool) identityHeaders() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.headers
}

// Inventory reports pod counts by state and the pods themselves, ordered by IP.
func (p *Pool) Inventory() Inventory {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.markLostLocked()
	inv := Inventory{ExpectedSize: p.cfg.ExpectedSize, Pods: []PodInfo{}, Workspaces: []WorkspaceInfo{}}
	for _, ws := range p.wsOrder {
		inv.Workspaces = append(inv.Workspaces, WorkspaceInfo{
			WorkspaceID: ws.cfg.WorkspaceID, Name: ws.cfg.RuntimeName,
			Leased: p.leasedLocked(ws), MaxLeases: ws.cfg.MaxLeases,
		})
	}
	for _, pd := range p.pods {
		switch pd.State {
		case PodIdle:
			inv.Idle++
		case PodLeased:
			inv.Leased++
		case PodResetting:
			inv.Resetting++
		case PodLost:
			inv.Lost++
		}
		info := PodInfo{IP: pd.IP, Name: pd.Name, State: pd.State, RegisteredAt: pd.RegisteredAt, LastSeen: pd.LastSeen}
		if l := firstLease(pd); l != nil {
			info.WorkspaceID = l.WorkspaceID
		}
		inv.Pods = append(inv.Pods, info)
	}
	sort.Slice(inv.Pods, func(i, j int) bool { return inv.Pods[i].IP < inv.Pods[j].IP })
	return inv
}

// AdminHandler serves the read-only query interface for administrators. It
// must not be reachable from pods.
func (p *Pool) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/inventory", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Inventory())
	})
	mux.HandleFunc("GET /v1/leases", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Leases())
	})
	p.mountGit(mux)
	return mux
}

// PodHandler serves the standard daemon protocol to pods. Callers are
// identified by source IP only; the bearer token is a placeholder and ignored.
func (p *Pool) PodHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, err := sourceIP(r)
		if err != nil {
			http.Error(w, "unknown source address", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/healthz":
			writeJSON(w, http.StatusOK, okBody)
		case "/api/workspaces", "/api/daemon/workspaces":
			list := make([]map[string]string, 0, len(p.wsOrder))
			for _, ws := range p.wsOrder {
				list = append(list, map[string]string{"id": ws.cfg.WorkspaceID, "name": ws.cfg.RuntimeName})
			}
			writeJSON(w, http.StatusOK, list)
		case "/api/tokens/current/renew":
			writeJSON(w, http.StatusOK, map[string]any{"expires_at": "", "renewed": false})
		case "/api/daemon/register":
			p.serveRegister(w, r, ip)
		case "/api/daemon/heartbeat":
			p.serveHeartbeat(w, r, ip)
		case controlPath:
			p.serveControl(w, r, ip)
		default:
			p.serveDaemon(w, r, ip)
		}
	})
}

func sourceIP(r *http.Request) (string, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", errors.New("source address is not an IP")
	}
	return ip.String(), nil
}

func (p *Pool) serveRegister(w http.ResponseWriter, r *http.Request, ip string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		DeviceName  string `json:"device_name"`
		// BootNonce is random per pod start; a new one on a known IP means the
		// IP now belongs to a new instance.
		BootNonce string `json:"boot_nonce"`
		// PodIP is the address the daemon believes it has. Optional.
		PodIP    string `json:"pod_ip"`
		Runtimes []struct {
			Type string `json:"type"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// A self-reported address that differs from the source address means
	// something rewrote it (SNAT, a proxy). Pod identity rests on the source IP,
	// so this is a deployment error to surface on day one; it is not a security
	// check.
	if req.PodIP != "" {
		if reported := net.ParseIP(req.PodIP); reported == nil || !reported.Equal(net.ParseIP(ip)) {
			reason := fmt.Sprintf("registration rejected: pod reports IP %q but the request came from %s; the source address is being rewritten (SNAT?)", req.PodIP, ip)
			p.log.Warn("refused pod registration", "pod_ip", ip, "reported_ip", req.PodIP, "reason", reason)
			http.Error(w, reason, http.StatusForbidden)
			return
		}
	}
	headers := map[string]string{}
	for _, h := range []string{"X-Client-Platform", "X-Client-Version", "X-Client-OS", "X-Client-Capabilities"} {
		if v := r.Header.Get(h); v != "" {
			headers[h] = v
		}
	}

	ws := p.workspaceByID(req.WorkspaceID)
	if ws == nil && req.WorkspaceID == "" && len(p.wsOrder) == 1 {
		ws = p.wsOrder[0]
	}
	if ws == nil {
		p.refuse(w, r, ip, "workspace is not served by this pool manager")
		return
	}
	registration, err := p.ensureRegistered(r.Context(), ws, raw, headers)
	if err != nil {
		p.log.Warn("register pool runtime from pod", "pod_ip", ip, "workspace_id", ws.cfg.WorkspaceID, "error", err)
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

	now := p.now()
	var voided []*lease
	p.mu.Lock()
	next := &pod{IP: ip, Name: req.DeviceName, State: PodIdle, BootNonce: req.BootNonce, RegisteredAt: now, LastSeen: now,
		workspaces: map[string]struct{}{ws.cfg.WorkspaceID: {}}}
	if old := p.pods[ip]; old != nil {
		switch {
		case old.BootNonce == req.BootNonce && old.dirty:
			// The daemon that ran the previous task is still the one talking;
			// the reset has not taken effect, so the pod stays out of the pool.
			old.LastSeen = now
			next = old
		case old.BootNonce != req.BootNonce:
			// Whatever the old instance held is void; the IP is a new pod's now.
			p.log.Info("pod instance replaced", "pod_ip", ip, "old_name", old.Name, "old_state", old.State)
			for _, l := range old.leases {
				voided = append(voided, l)
			}
			for _, l := range voided {
				p.endLeaseLocked(l, "pod instance replaced")
			}
			// Tasks queued for the old instance never started; place them again.
			p.waiting = append(p.waiting, old.queue...)
		case len(old.leases) > 0 || len(old.queue) > 0 || p.idleLocked(old):
			// The same instance registering again still runs its tasks.
			next.State, next.leases = PodLeased, old.leases
			next.boundKey, next.queue = old.boundKey, old.queue
			next.boundAt, next.idleSince, next.draining = old.boundAt, old.idleSince, old.draining
			for _, l := range next.leases {
				l.pod = next
			}
			next.RegisteredAt = old.RegisteredAt
			maps.Copy(next.workspaces, old.workspaces)
		default:
			// Another workspace's registration from the same instance.
			maps.Copy(next.workspaces, old.workspaces)
		}
	}
	p.pods[ip] = next
	p.mu.Unlock()
	for _, l := range voided {
		p.failUpstream(r.Context(), l.ws, l.TaskID, "the pod running this task was replaced by a new instance at "+ip)
	}
	p.log.Info("pod registered", "pod_ip", ip, "name", req.DeviceName)
	writeJSON(w, http.StatusOK, resp)
}

// ensureRegistered registers the pool's runtime upstream from the first pod's
// registration and returns the stored upstream response.
func (p *Pool) ensureRegistered(ctx context.Context, ws *workspace, captured json.RawMessage, headers map[string]string) (json.RawMessage, error) {
	ws.registerMu.Lock()
	defer ws.registerMu.Unlock()
	p.mu.Lock()
	registration := ws.registration
	p.mu.Unlock()
	if len(registration) > 0 {
		return registration, nil
	}
	body, err := p.upstreamRegistration(ws, captured)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.headers == nil {
		p.headers = headers
	}
	p.mu.Unlock()
	var raw json.RawMessage
	if err := ws.up.call(ctx, http.MethodPost, "/api/daemon/register", body, &raw); err != nil {
		return nil, fmt.Errorf("register runtime: %w", err)
	}
	var resp struct {
		Runtimes []runtimeRef `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode registration: %w", err)
	}
	if len(resp.Runtimes) == 0 {
		return nil, errors.New("registration returned no runtimes")
	}
	p.mu.Lock()
	ws.registration = raw
	p.mu.Unlock()
	p.log.Info("registered pool runtimes", "workspace_id", ws.cfg.WorkspaceID, "runtimes", len(resp.Runtimes))
	return raw, nil
}

// upstreamRegistration rewrites a pod daemon's registration into the
// manager's: one machine identity, cloud mode, no custom profiles.
func (p *Pool) upstreamRegistration(ws *workspace, captured json.RawMessage) (map[string]any, error) {
	var req map[string]any
	if err := json.Unmarshal(captured, &req); err != nil {
		return nil, fmt.Errorf("decode captured registration: %w", err)
	}
	runtimes, _ := req["runtimes"].([]any)
	var kept []any
	for _, item := range runtimes {
		rt, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := rt["type"].(string)
		if profile, _ := rt["profile_id"].(string); profile != "" {
			continue
		}
		if len(p.cfg.Providers) > 0 && !slices.Contains(p.cfg.Providers, typ) {
			continue
		}
		rt["runtime_mode"] = "cloud"
		rt["name"] = ws.cfg.RuntimeName
		if len(p.cfg.Providers) != 1 {
			rt["name"] = ws.cfg.RuntimeName + " (" + typ + ")"
		}
		kept = append(kept, rt)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("the pod has none of the configured providers %v", p.cfg.Providers)
	}
	return map[string]any{
		"workspace_id": ws.cfg.WorkspaceID,
		"daemon_id":    ws.cfg.BrokerID,
		"device_name":  ws.cfg.RuntimeName,
		"cli_version":  req["cli_version"],
		"runtimes":     kept,
	}, nil
}

func (p *Pool) serveHeartbeat(w http.ResponseWriter, r *http.Request, ip string) {
	var req struct {
		RuntimeID string `json:"runtime_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.markLostLocked()
	pd := p.pods[ip]
	lost := pd != nil && pd.State == PodLost
	if pd != nil && !lost {
		pd.LastSeen = p.now()
	}
	p.mu.Unlock()
	p.reportForced(r.Context())
	if pd == nil {
		http.Error(w, "pod not registered", http.StatusNotFound)
		return
	}
	if lost {
		// A lost pod comes back only by registering again.
		http.Error(w, "pod was marked lost after its heartbeat timed out; register again", http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtime_id": req.RuntimeID, "status": "ok"})
}
