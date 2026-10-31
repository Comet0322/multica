package sandboxbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
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
	MulticaURL   string
	MulticaToken string
	WorkspaceID  string
	// BrokerID is the upstream daemon_id. It must stay stable across restarts
	// or the server registers a new runtime row.
	BrokerID string
	// Name is the machine and runtime name members see.
	Name string
	// ExpectedSize is the number of pods the deployment declares, reported next
	// to the live counts so a shortfall is visible.
	ExpectedSize int
	// Providers limits which agent CLIs a pod reports become upstream
	// runtimes. Empty exposes every reported provider.
	Providers []string
	// HeartbeatTimeout is how long a pod may stay silent before it is listed
	// as lost. Zero means DefaultHeartbeatTimeout.
	HeartbeatTimeout time.Duration
	// AuditLog receives the audit records as JSON Lines. Nil disables auditing.
	AuditLog io.Writer
	// LiteLLM, when set, gives every task its own short-lived LiteLLM key
	// owned by the initiator. Nil leaves model access to the agent's own
	// settings.
	LiteLLM *LiteLLMConfig
}

// DefaultHeartbeatTimeout is three daemon heartbeat intervals (15s each).
const DefaultHeartbeatTimeout = 45 * time.Second

func (c *PoolConfig) normalize() error {
	if c.HeartbeatTimeout < 0 {
		return fmt.Errorf("heartbeat timeout must not be negative")
	}
	if c.HeartbeatTimeout == 0 {
		c.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if _, err := uuid.Parse(c.WorkspaceID); err != nil {
		return fmt.Errorf("workspace ID must be a UUID")
	}
	c.WorkspaceID = strings.ToLower(c.WorkspaceID)
	if err := checkBaseURL(c.MulticaURL); err != nil {
		return fmt.Errorf("Multica URL: %w", err)
	}
	c.MulticaURL = strings.TrimRight(c.MulticaURL, "/")
	if strings.TrimSpace(c.MulticaToken) == "" {
		return fmt.Errorf("Multica token is required")
	}
	if c.LiteLLM != nil {
		llm := *c.LiteLLM
		if err := llm.normalize(); err != nil {
			return err
		}
		c.LiteLLM = &llm
	}
	if c.ExpectedSize < 0 {
		return fmt.Errorf("expected pool size must not be negative")
	}
	if c.Name == "" {
		c.Name = "Managed Sandbox"
	}
	if c.BrokerID == "" {
		sum := sha256.Sum256([]byte(c.MulticaURL + "\x00" + c.WorkspaceID))
		c.BrokerID = "sandbox-broker-" + hex.EncodeToString(sum[:])[:24]
	}
	return nil
}

// Pool is the pool manager: to Multica it is one daemon, to each pod it is the
// server. Pods announce themselves by registering; the manager keeps no
// knowledge of how they are deployed.
type Pool struct {
	cfg PoolConfig
	up  *upstream
	log *slog.Logger
	now func() time.Time

	// registerMu serializes upstream registration so concurrent first pods
	// register the shared runtime once.
	registerMu sync.Mutex

	mu sync.Mutex
	// pods is keyed by source IP.
	pods map[string]*pod
	// identity headers and registration are taken from the first pod and
	// replayed to every other one; pods are homogeneous.
	headers      map[string]string
	registration json.RawMessage
	// leases holds the active leases by task ID; history keeps them, closed
	// ones included, for the lease query.
	leases  map[string]*lease
	history []*lease
	proxy   *httputil.ReverseProxy
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
	// lease is the task the pod holds, if any. claiming is set while the
	// manager is claiming a task upstream on the pod's behalf, so one poll at a
	// time can do that.
	lease    *lease
	claiming bool
}

// markLostLocked turns every pod whose heartbeat is overdue into a lost one.
// Silence cannot tell a restart from a crash, so every state times out the
// same way. Callers hold p.mu.
func (p *Pool) markLostLocked() {
	now := p.now()
	for _, pd := range p.pods {
		if pd.State != PodLost && now.Sub(pd.LastSeen) > p.cfg.HeartbeatTimeout {
			pd.State = PodLost
			p.log.Warn("pod lost: heartbeat timed out", "pod_ip", pd.IP, "name", pd.Name, "last_seen", pd.LastSeen)
		}
	}
}

// PodInfo is one pod's entry in the inventory.
type PodInfo struct {
	IP           string    `json:"ip"`
	Name         string    `json:"name"`
	State        string    `json:"state"`
	RegisteredAt time.Time `json:"registered_at"`
	LastSeen     time.Time `json:"last_seen"`
}

// Inventory is the manager's view of the pool. Name is display-only.
type Inventory struct {
	ExpectedSize int       `json:"expected_size"`
	Idle         int       `json:"idle"`
	Leased       int       `json:"leased"`
	Resetting    int       `json:"resetting"`
	Lost         int       `json:"lost"`
	Pods         []PodInfo `json:"pods"`
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
	if cfg.LiteLLM != nil {
		p.llm = newLiteLLM(*cfg.LiteLLM)
	}
	p.up = newUpstream(cfg.MulticaURL, cfg.MulticaToken, p.identityHeaders)
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
	inv := Inventory{ExpectedSize: p.cfg.ExpectedSize, Pods: []PodInfo{}}
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
		inv.Pods = append(inv.Pods, PodInfo{IP: pd.IP, Name: pd.Name, State: pd.State, RegisteredAt: pd.RegisteredAt, LastSeen: pd.LastSeen})
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
			writeJSON(w, http.StatusOK, []map[string]string{{"id": p.cfg.WorkspaceID, "name": p.cfg.Name}})
		case "/api/tokens/current/renew":
			writeJSON(w, http.StatusOK, map[string]any{"expires_at": "", "renewed": false})
		case "/api/daemon/register":
			p.serveRegister(w, r, ip)
		case "/api/daemon/heartbeat":
			p.serveHeartbeat(w, r, ip)
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
		DeviceName string `json:"device_name"`
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

	registration, err := p.ensureRegistered(r.Context(), raw, headers)
	if err != nil {
		p.log.Warn("register pool runtime from pod", "pod_ip", ip, "error", err)
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
	var voided *lease
	p.mu.Lock()
	next := &pod{IP: ip, Name: req.DeviceName, State: PodIdle, BootNonce: req.BootNonce, RegisteredAt: now, LastSeen: now}
	if old := p.pods[ip]; old != nil {
		switch {
		case old.BootNonce != req.BootNonce:
			// Whatever the old instance held is void; the IP is a new pod's now.
			p.log.Info("pod instance replaced", "pod_ip", ip, "old_name", old.Name, "old_state", old.State)
			voided = old.lease
			p.endLeaseLocked(voided, "pod instance replaced")
		case old.lease != nil:
			// The same instance registering again still runs its task.
			next.State, next.lease = PodLeased, old.lease
			next.lease.pod = next
			next.RegisteredAt = old.RegisteredAt
		}
	}
	p.pods[ip] = next
	p.mu.Unlock()
	if voided != nil {
		p.failUpstream(r.Context(), voided.TaskID, "the pod running this task was replaced by a new instance at "+ip)
	}
	p.log.Info("pod registered", "pod_ip", ip, "name", req.DeviceName)
	writeJSON(w, http.StatusOK, resp)
}

// ensureRegistered registers the pool's runtime upstream from the first pod's
// registration and returns the stored upstream response.
func (p *Pool) ensureRegistered(ctx context.Context, captured json.RawMessage, headers map[string]string) (json.RawMessage, error) {
	p.registerMu.Lock()
	defer p.registerMu.Unlock()
	p.mu.Lock()
	registration := p.registration
	p.mu.Unlock()
	if len(registration) > 0 {
		return registration, nil
	}
	body, err := p.upstreamRegistration(captured)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.headers = headers
	p.mu.Unlock()
	var raw json.RawMessage
	if err := p.up.call(ctx, http.MethodPost, "/api/daemon/register", body, &raw); err != nil {
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
	p.registration = raw
	p.mu.Unlock()
	p.log.Info("registered pool runtimes", "runtimes", len(resp.Runtimes))
	return raw, nil
}

// upstreamRegistration rewrites a pod daemon's registration into the
// manager's: one machine identity, cloud mode, no custom profiles.
func (p *Pool) upstreamRegistration(captured json.RawMessage) (map[string]any, error) {
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
		rt["name"] = p.cfg.Name
		if len(p.cfg.Providers) != 1 {
			rt["name"] = p.cfg.Name + " (" + typ + ")"
		}
		kept = append(kept, rt)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("the pod has none of the configured providers %v", p.cfg.Providers)
	}
	return map[string]any{
		"workspace_id": p.cfg.WorkspaceID,
		"daemon_id":    p.cfg.BrokerID,
		"device_name":  p.cfg.Name,
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
