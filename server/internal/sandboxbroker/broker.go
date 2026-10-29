package sandboxbroker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxprovider"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// controlKey names the sandbox used to learn the image's daemon identity and
// to answer model-list requests while no task sandbox is running.
const controlKey = "control"

// sandboxStartGrace is how long a requested sandbox may stay not-running
// before the broker treats it as crashed.
const sandboxStartGrace = 45 * time.Second

const (
	statusCheckInterval = 20 * time.Second
	controlRequestTTL   = 2 * time.Minute
)

type Broker struct {
	cfg      Config
	provider sandboxprovider.Provider
	up       *upstream
	log      *slog.Logger
	path     string
	now      func() time.Time

	mu    sync.Mutex
	state *journal
	// tokens maps sandbox credential -> sandbox key.
	tokens map[string]string
	// serverCaps is the latest upstream heartbeat ack's capability list,
	// replayed to sandbox daemons.
	serverCaps []string
	// modelRequests are upstream model-list requests waiting for, or being
	// answered by, the control sandbox.
	modelRequests map[string]*controlRequest
	lastHeartbeat time.Time
	// identityReady is signalled when a sandbox registration refreshed the
	// identity during bootstrap.
	identityReady chan struct{}
}

type controlRequest struct {
	RuntimeID string
	Delivered bool
	Deadline  time.Time
}

func New(cfg Config, provider sandboxprovider.Provider, log *slog.Logger) (*Broker, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, errors.New("sandbox provider is required")
	}
	if log == nil {
		log = slog.Default()
	}
	b := &Broker{
		cfg: cfg, provider: provider, log: log,
		path:          filepath.Join(cfg.StateDir, "journal.json"),
		now:           time.Now,
		tokens:        map[string]string{},
		modelRequests: map[string]*controlRequest{},
		identityReady: make(chan struct{}, 1),
	}
	b.up = newUpstream(cfg.MulticaURL, cfg.MulticaToken, b.identityHeaders)
	return b, nil
}

func (b *Broker) binding() string { return b.cfg.MulticaURL + "|" + b.cfg.WorkspaceID }

func (b *Broker) identityHeaders() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == nil || b.state.Identity == nil {
		return nil
	}
	return b.state.Identity.Headers
}

// Run loads the journal, serves the sandbox API and drives the control loop
// until ctx ends. Only one broker may use a state directory at a time.
func (b *Broker) Run(ctx context.Context) error {
	if err := os.MkdirAll(b.cfg.StateDir, 0o700); err != nil {
		return err
	}
	unlock, err := lockStateDir(b.cfg.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := b.load(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", b.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen for sandboxes: %w", err)
	}
	srv := &http.Server{Handler: b.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	b.log.InfoContext(ctx, "sandbox broker started", "listen", listener.Addr().String(), "workspace_id", b.cfg.WorkspaceID, "daemon_id", b.cfg.BrokerID)

	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := b.Step(ctx); err != nil && ctx.Err() == nil {
			b.log.WarnContext(ctx, "sandbox broker step failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-serveErr:
			return fmt.Errorf("sandbox API stopped: %w", err)
		case <-ticker.C:
		}
	}
}

func (b *Broker) load() error {
	j, err := loadJournal(b.path, b.binding())
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = j
	for key, sb := range j.Sandboxes {
		b.tokens[sb.Token] = key
	}
	return nil
}

func (b *Broker) persistLocked() {
	if err := saveJournal(b.path, b.state); err != nil {
		b.log.Error("persist broker journal", "error", err)
	}
}

// Step runs one control-loop iteration. Each phase logs and continues on
// error so one failing upstream call cannot stall sandbox supervision.
func (b *Broker) Step(ctx context.Context) error {
	var errs []error
	if !b.registered() {
		if err := b.bootstrap(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if b.registered() {
		if err := b.heartbeat(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := b.claim(ctx); err != nil {
			errs = append(errs, err)
		}
		b.maintainTasks(ctx)
		b.expireControlRequests(ctx)
	}
	b.supervise(ctx)
	return errors.Join(errs...)
}

func (b *Broker) registered() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.state.Runtimes) > 0 && b.state.Identity != nil && b.state.Identity.Image == b.cfg.Image
}

// bootstrap re-registers from a stored identity, or waits for the control
// sandbox's daemon to register so its identity can be captured.
func (b *Broker) bootstrap(ctx context.Context) error {
	b.mu.Lock()
	have := b.state.Identity != nil && b.state.Identity.Image == b.cfg.Image
	b.mu.Unlock()
	if have {
		return b.registerUpstream(ctx)
	}
	// supervise() keeps the control sandbox running while unregistered; its
	// registration arrives through the proxy and calls registerUpstream.
	return nil
}

func (b *Broker) registerUpstream(ctx context.Context) error {
	b.mu.Lock()
	if b.state.Identity == nil {
		b.mu.Unlock()
		return errors.New("no daemon identity captured yet")
	}
	body, err := b.upstreamRegistration(b.state.Identity.Register)
	b.mu.Unlock()
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := b.up.call(ctx, http.MethodPost, "/api/daemon/register", body, &raw); err != nil {
		return fmt.Errorf("register runtime: %w", err)
	}
	var resp struct {
		Runtimes []runtimeRef `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode registration: %w", err)
	}
	if len(resp.Runtimes) == 0 {
		return errors.New("registration returned no runtimes")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state.Registration = raw
	b.state.Runtimes = resp.Runtimes
	b.persistLocked()
	b.log.InfoContext(ctx, "registered managed runtimes", "runtimes", len(resp.Runtimes))
	return nil
}

// upstreamRegistration rewrites the image daemon's registration into the
// broker's: one machine identity, cloud mode, no custom profiles.
func (b *Broker) upstreamRegistration(captured json.RawMessage) (map[string]any, error) {
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
		if len(b.cfg.Providers) > 0 && !slices.Contains(b.cfg.Providers, typ) {
			continue
		}
		rt["runtime_mode"] = "cloud"
		rt["name"] = b.cfg.Name
		if len(b.cfg.Providers) != 1 {
			rt["name"] = b.cfg.Name + " (" + typ + ")"
		}
		kept = append(kept, rt)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("the sandbox image has none of the configured providers %v", b.cfg.Providers)
	}
	return map[string]any{
		"workspace_id": b.cfg.WorkspaceID,
		"daemon_id":    b.cfg.BrokerID,
		"device_name":  b.cfg.Name,
		"cli_version":  req["cli_version"],
		"runtimes":     kept,
	}, nil
}

func (b *Broker) runtimeIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.state.Runtimes))
	for _, rt := range b.state.Runtimes {
		ids = append(ids, rt.ID)
	}
	return ids
}

func (b *Broker) heartbeat(ctx context.Context) error {
	if b.now().Sub(b.lastHeartbeat) < b.cfg.HeartbeatInterval {
		return nil
	}
	b.lastHeartbeat = b.now()
	for _, id := range b.runtimeIDs() {
		var ack protocol.DaemonHeartbeatAckPayload
		err := b.up.call(ctx, http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": id, "supports_batch_import": true}, &ack)
		if statusOf(err) == http.StatusNotFound || ack.RuntimeGone || ack.Status == protocol.HeartbeatStatusRuntimeGone {
			b.log.WarnContext(ctx, "managed runtime is gone upstream; registering again", "runtime_id", id)
			return b.registerUpstream(ctx)
		}
		if err != nil {
			return fmt.Errorf("heartbeat: %w", err)
		}
		b.mu.Lock()
		b.serverCaps = ack.ServerCapabilities
		b.mu.Unlock()
		b.handlePending(ctx, id, &ack)
	}
	return nil
}

// handlePending answers runtime capability requests. Local skills and MCP of
// the image are deliberately reported unsupported so the UI hides them; agents
// still load them from the image's provider home directories.
func (b *Broker) handlePending(ctx context.Context, runtimeID string, ack *protocol.DaemonHeartbeatAckPayload) {
	report := func(path string, body map[string]any) {
		if err := b.up.call(ctx, http.MethodPost, "/api/daemon/runtimes/"+url.PathEscape(runtimeID)+path, body, nil); err != nil {
			b.log.WarnContext(ctx, "report runtime request result", "path", path, "error", err)
		}
	}
	if p := ack.PendingUpdate; p != nil {
		report("/update/"+url.PathEscape(p.ID)+"/result", map[string]any{"status": "failed", "error": "managed runtimes are updated by rolling out a new sandbox image"})
	}
	if p := ack.PendingLocalSkills; p != nil {
		report("/local-skills/"+url.PathEscape(p.ID)+"/result", map[string]any{"status": "completed", "supported": false, "skills": []any{}, "mcp_supported": false, "mcp_servers": []any{}})
	}
	imports := ack.PendingLocalSkillImports
	if ack.PendingLocalSkillImport != nil {
		imports = append(imports, *ack.PendingLocalSkillImport)
	}
	for _, p := range imports {
		report("/local-skills/import/"+url.PathEscape(p.ID)+"/result", map[string]any{"status": "failed", "error": "managed runtime skills cannot be imported"})
	}
	if p := ack.PendingModelList; p != nil {
		b.mu.Lock()
		if _, ok := b.modelRequests[p.ID]; !ok {
			b.modelRequests[p.ID] = &controlRequest{RuntimeID: runtimeID, Deadline: b.now().Add(controlRequestTTL)}
		}
		b.mu.Unlock()
	}
}

func (b *Broker) expireControlRequests(ctx context.Context) {
	b.mu.Lock()
	var expired []string
	runtimes := map[string]string{}
	for id, req := range b.modelRequests {
		if b.now().After(req.Deadline) {
			expired = append(expired, id)
			runtimes[id] = req.RuntimeID
			delete(b.modelRequests, id)
		}
	}
	b.mu.Unlock()
	for _, id := range expired {
		path := "/api/daemon/runtimes/" + url.PathEscape(runtimes[id]) + "/models/" + url.PathEscape(id) + "/result"
		if err := b.up.call(ctx, http.MethodPost, path, map[string]any{"status": "failed", "error": "the managed sandbox did not answer in time"}, nil); err != nil {
			b.log.WarnContext(ctx, "report model list timeout", "error", err)
		}
	}
}

type claimedTaskRef struct {
	ID          string `json:"id"`
	AgentID     string `json:"agent_id"`
	RuntimeID   string `json:"runtime_id"`
	WorkspaceID string `json:"workspace_id"`
}

func (b *Broker) claim(ctx context.Context) error {
	b.mu.Lock()
	capacity := b.cfg.MaxConcurrency - len(b.state.Tasks)
	b.mu.Unlock()
	if capacity <= 0 {
		return nil
	}
	var resp struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	body := map[string]any{"daemon_id": b.cfg.BrokerID, "runtime_ids": b.runtimeIDs(), "max_tasks": capacity}
	if err := b.up.call(ctx, http.MethodPost, "/api/daemon/tasks/claim", body, &resp); err != nil {
		return fmt.Errorf("claim tasks: %w", err)
	}
	for _, raw := range resp.Tasks {
		var ref claimedTaskRef
		if err := json.Unmarshal(raw, &ref); err != nil || ref.ID == "" {
			b.log.ErrorContext(ctx, "claimed task has no ID; it will be reclaimed by the server")
			continue
		}
		key, err := sandboxKeyFor(ref.AgentID)
		if err != nil || !strings.EqualFold(ref.WorkspaceID, b.cfg.WorkspaceID) {
			b.failTask(ctx, ref.ID, "the managed runtime cannot place this task in a sandbox")
			continue
		}
		b.mu.Lock()
		b.state.Tasks[ref.ID] = &task{ID: ref.ID, Key: key, RuntimeID: ref.RuntimeID, Claim: raw, State: taskQueued, ClaimedAt: b.now(), LeaseAt: b.now(), CheckedAt: b.now()}
		b.persistLocked()
		b.mu.Unlock()
		b.log.InfoContext(ctx, "claimed task for sandbox", "task_id", ref.ID, "sandbox", key)
	}
	return nil
}

// sandboxKeyFor gives each agent its own sandbox and volume, so an agent's
// next task reuses its workdirs and agent sessions.
func sandboxKeyFor(agentID string) (string, error) {
	agentID = strings.ToLower(strings.TrimSpace(agentID))
	key := "agent-" + agentID
	if agentID == "" || !sandboxprovider.ValidKey(key) {
		return "", fmt.Errorf("invalid agent ID")
	}
	return key, nil
}

// maintainTasks extends prepare leases until each sandbox daemon starts its
// task, and drops tasks the server no longer considers dispatched to us.
func (b *Broker) maintainTasks(ctx context.Context) {
	type work struct {
		id, runtimeID string
		state         taskState
		lease, check  bool
		expired       bool
	}
	var items []work
	b.mu.Lock()
	now := b.now()
	for _, t := range b.state.Tasks {
		w := work{id: t.ID, runtimeID: t.RuntimeID, state: t.State}
		if t.State != taskStarted {
			w.expired = now.Sub(t.ClaimedAt) > b.cfg.StartTimeout
			w.lease = now.Sub(t.LeaseAt) >= b.cfg.LeaseInterval
			if w.lease {
				t.LeaseAt = now
			}
		}
		w.check = now.Sub(t.CheckedAt) >= statusCheckInterval
		if w.check {
			t.CheckedAt = now
		}
		items = append(items, w)
	}
	b.mu.Unlock()

	for _, w := range items {
		if w.expired {
			b.failTask(ctx, w.id, "the managed sandbox did not start this task in time")
			continue
		}
		if w.lease {
			path := "/api/daemon/runtimes/" + url.PathEscape(w.runtimeID) + "/tasks/" + url.PathEscape(w.id) + "/prepare-lease"
			if err := b.up.call(ctx, http.MethodPost, path, map[string]any{}, nil); err != nil {
				b.log.WarnContext(ctx, "extend prepare lease", "task_id", w.id, "error", err)
				w.check = true
			}
		}
		if w.check {
			var resp struct {
				Status string `json:"status"`
			}
			if err := b.up.call(ctx, http.MethodGet, "/api/daemon/tasks/"+url.PathEscape(w.id)+"/status", nil, &resp); err != nil {
				if statusOf(err) == http.StatusNotFound {
					b.dropTask(w.id, "task no longer exists")
				}
				continue
			}
			if resp.Status != "dispatched" && resp.Status != "running" {
				b.dropTask(w.id, "server status "+resp.Status)
			}
		}
	}
}

func (b *Broker) dropTask(id, why string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.state.Tasks[id]
	if !ok {
		return
	}
	delete(b.state.Tasks, id)
	b.persistLocked()
	b.log.Info("released task", "task_id", id, "sandbox", t.Key, "reason", why)
}

func (b *Broker) failTask(ctx context.Context, id, message string) {
	err := b.up.call(ctx, http.MethodPost, "/api/daemon/tasks/"+url.PathEscape(id)+"/fail", map[string]any{"error": message}, nil)
	if err != nil {
		b.log.WarnContext(ctx, "fail task upstream", "task_id", id, "error", err)
	}
	b.dropTask(id, message)
}

// supervise reconciles each sandbox's compute with the work assigned to it.
func (b *Broker) supervise(ctx context.Context) {
	b.mu.Lock()
	needed := map[string]bool{}
	for _, t := range b.state.Tasks {
		needed[t.Key] = true
	}
	unregistered := len(b.state.Runtimes) == 0 || b.state.Identity == nil || b.state.Identity.Image != b.cfg.Image
	for _, req := range b.modelRequests {
		if !req.Delivered {
			needed[controlKey] = true
		}
	}
	if unregistered {
		needed[controlKey] = true
	}
	keys := map[string]bool{}
	for key := range needed {
		keys[key] = true
	}
	for key, sb := range b.state.Sandboxes {
		if sb.Running {
			keys[key] = true
		}
	}
	b.mu.Unlock()

	for key := range keys {
		if needed[key] {
			b.ensureSandbox(ctx, key)
		} else {
			b.idleSandbox(ctx, key)
		}
	}
}

func (b *Broker) ensureSandbox(ctx context.Context, key string) {
	b.mu.Lock()
	sb := b.state.Sandboxes[key]
	if sb == nil {
		sb = &sandboxState{Key: key, Token: newToken()}
		b.state.Sandboxes[key] = sb
		b.tokens[sb.Token] = key
		b.persistLocked()
	}
	sb.IdleSince = time.Time{}
	requested, wasRunning := sb.RequestedAt, sb.Running
	b.mu.Unlock()

	current, err := b.provider.Get(ctx, key)
	if err != nil {
		b.log.WarnContext(ctx, "inspect sandbox", "sandbox", key, "error", err)
		return
	}
	if current.Status == sandboxprovider.StatusRunning && (current.Image == b.cfg.Image || b.hasStartedTask(key)) {
		// An outdated image is replaced only between runs.
		return
	}
	// The grace period also spaces out restarts of a crash-looping sandbox.
	if b.now().Sub(requested) < sandboxStartGrace {
		return
	}
	if wasRunning {
		b.sandboxLost(ctx, key, current)
	}
	if _, err := b.provider.Ensure(ctx, b.spec(key, sb.Token)); err != nil {
		b.log.WarnContext(ctx, "start sandbox", "sandbox", key, "error", err)
		return
	}
	b.mu.Lock()
	sb.Running = true
	sb.RequestedAt = b.now()
	sb.Image = b.cfg.Image
	b.persistLocked()
	b.mu.Unlock()
	b.log.InfoContext(ctx, "sandbox requested", "sandbox", key)
}

// sandboxLost handles compute that stopped without the broker asking.
// Delivered tasks were never started, so they go back to the queue; started
// tasks lost their daemon and fail so the server can surface the error.
func (b *Broker) sandboxLost(ctx context.Context, key string, current sandboxprovider.Sandbox) {
	b.log.WarnContext(ctx, "sandbox stopped unexpectedly", "sandbox", key, "status", current.Status, "reason", current.Reason)
	var lost []string
	b.mu.Lock()
	for _, t := range b.state.Tasks {
		if t.Key != key {
			continue
		}
		switch t.State {
		case taskDelivered:
			t.State = taskQueued
		case taskStarted:
			lost = append(lost, t.ID)
		}
	}
	b.persistLocked()
	b.mu.Unlock()
	for _, id := range lost {
		b.failTask(ctx, id, "the managed sandbox stopped while running this task ("+current.Reason+")")
	}
}

func (b *Broker) hasStartedTask(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.state.Tasks {
		if t.Key == key && t.State == taskStarted {
			return true
		}
	}
	return false
}

func (b *Broker) idleSandbox(ctx context.Context, key string) {
	b.mu.Lock()
	sb := b.state.Sandboxes[key]
	if sb == nil || !sb.Running {
		b.mu.Unlock()
		return
	}
	if sb.IdleSince.IsZero() {
		sb.IdleSince = b.now()
	}
	idle := b.now().Sub(sb.IdleSince) >= b.cfg.IdleTimeout
	b.mu.Unlock()
	if !idle {
		return
	}
	if err := b.provider.Stop(ctx, key); err != nil {
		b.log.WarnContext(ctx, "stop idle sandbox", "sandbox", key, "error", err)
		return
	}
	b.mu.Lock()
	sb.Running = false
	b.persistLocked()
	b.mu.Unlock()
	b.log.InfoContext(ctx, "stopped idle sandbox", "sandbox", key)
}

func (b *Broker) spec(key, token string) sandboxprovider.Spec {
	env := map[string]string{}
	for k, v := range b.cfg.SandboxEnv {
		env[k] = v
	}
	env["MULTICA_SANDBOX_BROKER_URL"] = b.cfg.SandboxURL
	env["MULTICA_SANDBOX_TOKEN"] = token
	env["MULTICA_DAEMON_ID"] = "sandbox-" + key
	env["MULTICA_DAEMON_DEVICE_NAME"] = b.cfg.Name
	env["MULTICA_DAEMON_MAX_CONCURRENT_TASKS"] = fmt.Sprint(b.cfg.MaxConcurrency)
	return sandboxprovider.Spec{Key: key, Image: b.cfg.Image, Env: env, Labels: map[string]string{"ai.multica.workspace": b.cfg.WorkspaceID}}
}

func newToken() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return "msb_" + hex.EncodeToString(raw[:])
}
