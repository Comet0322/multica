package daemonbridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// Config configures a Bridge.
type Config struct {
	ServerURL string
	// Token is the personal access token of the user the runtimes belong to.
	PAT        string
	DaemonID   string
	DeviceName string
	CLIVersion string
	// Workspaces limits registration; empty means every workspace the token sees.
	Workspaces []string
	// StateDir holds the journal. It must survive restarts.
	StateDir string

	MaxConcurrent     int
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	EventInterval     time.Duration
	StatusInterval    time.Duration
	// WorkspaceRefresh is how often the account's workspaces are re-read when
	// Workspaces is empty. Zero means one minute.
	WorkspaceRefresh time.Duration
	// TaskTimeout bounds one run. Zero means two hours.
	TaskTimeout time.Duration

	// Tools executes tool requests from a run. Nil answers every request with
	// an error.
	Tools ToolRunner

	Logger *slog.Logger
}

func (c *Config) defaults() {
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 4
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 3 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 15 * time.Second
	}
	if c.EventInterval <= 0 {
		c.EventInterval = time.Second
	}
	if c.StatusInterval <= 0 {
		c.StatusInterval = 5 * time.Second
	}
	if c.WorkspaceRefresh <= 0 {
		c.WorkspaceRefresh = time.Minute
	}
	if c.TaskTimeout <= 0 {
		c.TaskTimeout = 2 * time.Hour
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.DeviceName == "" {
		c.DeviceName, _ = os.Hostname()
	}
}

// ToolRunner executes the tools a run asks the bridge for.
type ToolRunner interface {
	Specs(task daemon.Task, kind Kind) []ToolSpec
	Run(ctx context.Context, task daemon.Task, req ToolRequest) ToolResult
}

// Bridge is the daemon the server sees.
type Bridge struct {
	cfg     Config
	backend Backend
	client  *daemon.Client
	journal *Journal
	log     *slog.Logger

	mu       sync.Mutex
	runtimes map[string]string // runtime ID -> workspace ID
	active   map[string]bool   // task IDs with a goroutine
	wg       sync.WaitGroup
}

// New builds a Bridge. It does not contact the server.
func New(cfg Config, backend Backend) (*Bridge, error) {
	cfg.defaults()
	if cfg.ServerURL == "" || cfg.PAT == "" || cfg.DaemonID == "" || cfg.StateDir == "" {
		return nil, errors.New("daemonbridge: ServerURL, Token, DaemonID and StateDir are required")
	}
	caps := backend.Capabilities()
	if caps.Provider == "" {
		return nil, errors.New("daemonbridge: backend reports no provider")
	}
	j, err := OpenJournal(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	c := daemon.NewClient(strings.TrimRight(cfg.ServerURL, "/"))
	c.SetToken(cfg.PAT)
	c.SetVersion(cfg.CLIVersion)
	c.SetCapabilities(clientCapabilities(caps))
	return &Bridge{
		cfg: cfg, backend: backend, client: c, journal: j, log: cfg.Logger,
		runtimes: map[string]string{}, active: map[string]bool{},
	}, nil
}

// clientCapabilities advertises only what the backend honours, so the server
// does not hand it work shaped for features it lacks.
func clientCapabilities(c Capabilities) []string {
	caps := []string{
		protocol.DaemonCapabilitySkillBundlesV1,
		protocol.DaemonCapabilityCoalescedCommentsV1,
		protocol.DaemonCapabilityExecutionManifestV1,
		protocol.DaemonCapabilityAgentSkillV1,
		protocol.DaemonCapabilitySourceContextQuickCreateV1,
		protocol.DaemonCapabilityPlatformSkillV1,
		protocol.DaemonCapabilityCheckoutKeepsWorkV1,
		protocol.DaemonCapabilityJoinedWakeupsV1,
	}
	if c.RemoteMCP {
		caps = append(caps, protocol.DaemonCapabilityRemoteMCPV1)
	}
	if c.LocalWorkDir {
		caps = append(caps, protocol.DaemonCapabilityLocalWorktreeV1)
	}
	return caps
}

func (b *Bridge) startCapabilities() []string {
	if b.backend.Capabilities().Supplement {
		return []string{protocol.DaemonCapabilityTaskSupplementV1}
	}
	return nil
}

// Run serves until ctx ends. Runs in flight keep their journal entries so the
// next process resumes them.
func (b *Bridge) Run(ctx context.Context) error {
	if err := b.register(ctx); err != nil {
		return err
	}
	b.resume(ctx)

	var loops sync.WaitGroup
	loops.Add(2)
	go func() { defer loops.Done(); b.heartbeatLoop(ctx) }()
	go func() { defer loops.Done(); b.claimLoop(ctx) }()
	<-ctx.Done()
	loops.Wait()
	b.wg.Wait()
	return nil
}

// wantedWorkspaces is the configured list, or every workspace the token sees.
func (b *Bridge) wantedWorkspaces(ctx context.Context) ([]string, error) {
	if len(b.cfg.Workspaces) > 0 {
		return b.cfg.Workspaces, nil
	}
	list, err := b.client.ListWorkspaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	ids := make([]string, 0, len(list))
	for _, w := range list {
		ids = append(ids, w.ID)
	}
	return ids, nil
}

func (b *Bridge) register(ctx context.Context) error {
	workspaces, err := b.wantedWorkspaces(ctx)
	if err != nil {
		return err
	}
	if len(workspaces) == 0 {
		return errors.New("the token sees no workspace")
	}
	return b.registerIn(ctx, workspaces)
}

// refreshWorkspaces follows the account: it registers workspaces joined or
// created since the last look and forgets runtimes of workspaces left. Runs
// already claimed there keep going; they only need their own task token.
func (b *Bridge) refreshWorkspaces(ctx context.Context) {
	if len(b.cfg.Workspaces) > 0 {
		return
	}
	want, err := b.wantedWorkspaces(ctx)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("refresh workspaces", "error", err)
		}
		return
	}
	wantSet := map[string]bool{}
	for _, ws := range want {
		wantSet[ws] = true
	}
	b.mu.Lock()
	have := map[string]bool{}
	for rt, ws := range b.runtimes {
		if !wantSet[ws] {
			delete(b.runtimes, rt)
			b.log.Info("workspace left; runtime dropped", "workspace", ws, "runtime", rt)
			continue
		}
		have[ws] = true
	}
	b.mu.Unlock()
	var added []string
	for _, ws := range want {
		if !have[ws] {
			added = append(added, ws)
		}
	}
	if len(added) == 0 {
		return
	}
	if err := b.registerIn(ctx, added); err != nil {
		b.log.Warn("register new workspaces", "error", err)
	}
}

func (b *Bridge) registerIn(ctx context.Context, workspaces []string) error {
	caps := b.backend.Capabilities()
	name := caps.Provider
	if b.cfg.DeviceName != "" {
		name = fmt.Sprintf("%s (%s)", caps.Provider, b.cfg.DeviceName)
	}
	var failed []error
	for _, ws := range workspaces {
		resp, err := b.client.Register(ctx, map[string]any{
			"workspace_id": ws,
			"daemon_id":    b.cfg.DaemonID,
			"device_name":  b.cfg.DeviceName,
			"cli_version":  b.cfg.CLIVersion,
			"launched_by":  "daemon-bridge",
			"runtimes": []map[string]string{{
				"name": name, "type": caps.Provider, "version": caps.Version, "status": "online",
			}},
		})
		if err != nil {
			// One workspace refusing (for example a revoked membership) must not
			// take the others down; the next refresh tries again.
			b.log.Warn("register in workspace", "workspace", ws, "error", err)
			failed = append(failed, err)
			continue
		}
		b.mu.Lock()
		for _, rt := range resp.Runtimes {
			b.runtimes[rt.ID] = ws
		}
		b.mu.Unlock()
	}
	if len(failed) == len(workspaces) {
		return fmt.Errorf("register: %w", errors.Join(failed...))
	}
	return nil
}

func (b *Bridge) runtimeIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.runtimes))
	for id := range b.runtimes {
		ids = append(ids, id)
	}
	return ids
}

// resume restarts every journaled task and recovers the rest: tasks the server
// holds for these runtimes that this process does not know are failed so they
// retry rather than hang.
func (b *Bridge) resume(ctx context.Context) {
	entries, errs := b.journal.Load()
	for _, err := range errs {
		b.log.Warn("journal entry skipped", "error", err)
	}
	if len(entries) == 0 {
		for _, id := range b.runtimeIDs() {
			if err := b.client.RecoverOrphans(ctx, id); err != nil {
				b.log.Warn("recover orphans", "runtime", id, "error", err)
			}
		}
	}
	for _, e := range entries {
		b.spawn(ctx, e)
	}
}

func (b *Bridge) spawn(ctx context.Context, e *Entry) {
	b.mu.Lock()
	if b.active[e.Task.ID] {
		b.mu.Unlock()
		return
	}
	b.active[e.Task.ID] = true
	b.mu.Unlock()
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() {
			b.mu.Lock()
			delete(b.active, e.Task.ID)
			b.mu.Unlock()
		}()
		newRun(b, e).run(ctx)
	}()
}

func (b *Bridge) claimLoop(ctx context.Context) {
	t := time.NewTicker(b.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		b.mu.Lock()
		free := b.cfg.MaxConcurrent - len(b.active)
		b.mu.Unlock()
		if free <= 0 {
			continue
		}
		tasks, err := b.client.ClaimTasks(ctx, b.cfg.DaemonID, b.runtimeIDs(), free)
		if err != nil {
			if ctx.Err() == nil {
				b.log.Warn("claim failed", "error", err)
			}
			continue
		}
		for _, task := range tasks {
			if task == nil {
				continue
			}
			e := &Entry{Task: *task, Phase: PhaseClaimed}
			if err := b.journal.Save(e); err != nil {
				// Without a journal entry a crash would orphan the task, so refuse it.
				b.log.Error("journal save failed; failing task", "task", task.ID, "error", err)
				b.failNow(ctx, *task, "bridge could not persist task state: "+err.Error(), taskfailure.ReasonEnvironmentPrepareFailed)
				continue
			}
			b.spawn(ctx, e)
		}
	}
}

func (b *Bridge) failNow(ctx context.Context, task daemon.Task, msg string, reason taskfailure.Reason) {
	if err := b.client.FailTask(ctx, task.ID, msg, "", "", "", string(reason), false, "", ""); err != nil {
		b.log.Error("fail task", "task", task.ID, "error", err)
	}
}

func (b *Bridge) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(b.cfg.HeartbeatInterval)
	defer t.Stop()
	renew := time.NewTicker(6 * time.Hour)
	defer renew.Stop()
	discover := time.NewTicker(b.cfg.WorkspaceRefresh)
	defer discover.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-discover.C:
			b.refreshWorkspaces(ctx)
		case <-renew.C:
			if _, err := b.client.RenewToken(ctx); err != nil {
				b.log.Warn("renew token", "error", err)
			}
		case <-t.C:
			for _, id := range b.runtimeIDs() {
				b.heartbeat(ctx, id)
			}
		}
	}
}

func (b *Bridge) heartbeat(ctx context.Context, runtimeID string) {
	ack, err := b.client.SendHeartbeat(ctx, runtimeID)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("heartbeat failed", "runtime", runtimeID, "error", err)
		}
		return
	}
	if ack.RuntimeGone || ack.Status == protocol.HeartbeatStatusRuntimeGone {
		b.log.Warn("runtime gone; registering again", "runtime", runtimeID)
		b.mu.Lock()
		delete(b.runtimes, runtimeID)
		b.mu.Unlock()
		if err := b.register(ctx); err != nil {
			b.log.Error("re-register", "error", err)
		}
		return
	}
	b.serveRequests(ctx, runtimeID, ack)
}
