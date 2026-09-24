// Package controller runs the Kubernetes sandbox runtime's control loop.
//
// The controller behaves like one daemon towards the Multica server: it
// registers runtimes, heartbeats them, claims tasks and reports failures. Each
// claimed task is run in its own Pod by the runner (daemon.RunSingleTask). The
// controller holds the daemon credential; the Pod never does (see package
// relay).
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/k8sruntime/relay"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// Server is the subset of the daemon HTTP client the controller uses.
type Server interface {
	Register(ctx context.Context, req map[string]any) (*daemon.RegisterResponse, error)
	SendHeartbeat(ctx context.Context, runtimeID string) (*daemon.HeartbeatResponse, error)
	ClaimTasks(ctx context.Context, daemonID string, runtimeIDs []string, maxTasks int) ([]*daemon.Task, error)
	ExtendTaskPrepareLease(ctx context.Context, runtimeID, taskID string) error
	GetTaskStatus(ctx context.Context, taskID string) (string, error)
	FailTask(ctx context.Context, taskID, errMsg, sessionID, workDir, branchName, failureReason string, sessionRolloutMissing bool, retiredSessionID, durableWorkDir string) error
}

// Tokens is the relay surface the controller drives.
type Tokens interface {
	Register(token string, t relay.Task)
	Unregister(token string)
}

// Phase is a Pod lifecycle phase as far as the controller cares.
type Phase string

const (
	PhasePending   Phase = "Pending"
	PhaseRunning   Phase = "Running"
	PhaseSucceeded Phase = "Succeeded"
	PhaseFailed    Phase = "Failed"
)

// Annotation keys the controller keeps on each Pod. They are the controller's
// only durable state, so a restarted controller can adopt running Pods.
const (
	AnnTaskID    = "multica.ai/task-id"
	AnnRuntimeID = "multica.ai/runtime-id"
)

// PodSpec describes the sandbox to create for one task.
type PodSpec struct {
	Name        string
	Annotations map[string]string
	// Input is the runner input (RunnerInput as JSON). It contains the task
	// token, so the driver must deliver it through a Secret, not the Pod spec.
	Input []byte
}

// PodInfo is the observed state of a sandbox Pod.
type PodInfo struct {
	Name        string
	Phase       Phase
	Annotations map[string]string
	CreatedAt   time.Time
	// Reason is a short explanation for a failed Pod (for example OOMKilled).
	Reason string
}

// Driver creates and observes sandbox Pods. Create must be idempotent: an
// existing Pod with the same name is success.
type Driver interface {
	Create(ctx context.Context, spec PodSpec) error
	List(ctx context.Context) ([]PodInfo, error)
	// Input returns the runner input a Pod was created with.
	Input(ctx context.Context, name string) ([]byte, error)
	// Delete removes the Pod and its input. A missing Pod is success.
	Delete(ctx context.Context, name string) error
}

// RunnerInput is what the runner reads from the Pod.
type RunnerInput struct {
	Task    daemon.Task    `json:"task"`
	Runtime daemon.Runtime `json:"runtime"`
}

// Config configures a Controller.
type Config struct {
	DaemonID   string
	DeviceName string
	CLIVersion string
	// Workspaces to register runtimes for; Providers are the agent providers
	// the sandbox image can run. One runtime per (workspace, provider).
	Workspaces []string
	Providers  []string
	// MaxPods caps concurrent sandbox Pods (and so the claim size).
	MaxPods int

	PollInterval      time.Duration // claim + reconcile tick
	HeartbeatInterval time.Duration
	LeaseInterval     time.Duration // prepare-lease refresh while a Pod starts
	StatusInterval    time.Duration // cancellation poll for running tasks
	// PendingTimeout bounds scheduling and image pull. It must stay below the
	// server's 300s dispatched-task limit so the failure is ours and retryable.
	PendingTimeout time.Duration
	// MaxRunDuration is an absolute cap per task; 0 disables it. The runner
	// already enforces the agent idle watchdog.
	MaxRunDuration time.Duration
	// CancelGrace is how long a Pod may keep running after the server finalizes
	// its task, so the runner can send its own cancel-ack.
	CancelGrace time.Duration
}

func (c *Config) applyDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 15 * time.Second
	}
	if c.LeaseInterval <= 0 {
		c.LeaseInterval = 15 * time.Second
	}
	if c.StatusInterval <= 0 {
		c.StatusInterval = 5 * time.Second
	}
	if c.PendingTimeout <= 0 {
		c.PendingTimeout = 4 * time.Minute
	}
	if c.CancelGrace <= 0 {
		c.CancelGrace = 30 * time.Second
	}
	if c.MaxPods <= 0 {
		c.MaxPods = 4
	}
}

type entry struct {
	taskID     string
	runtimeID  string
	podName    string
	token      string
	createdAt  time.Time
	phase      Phase
	lastLease  time.Time
	lastStatus time.Time
	// stopAt is set once the server finalized the task; the Pod is removed
	// after CancelGrace.
	stopAt time.Time
}

// Controller is the sandbox runtime control loop.
type Controller struct {
	cfg    Config
	server Server
	tokens Tokens
	driver Driver
	log    *slog.Logger
	now    func() time.Time

	runtimes   map[string]daemon.Runtime // runtime id -> runtime
	runtimeIDs []string

	mu      sync.Mutex
	entries map[string]*entry // task id -> entry
}

// New returns a Controller.
func New(cfg Config, server Server, tokens Tokens, driver Driver, log *slog.Logger) (*Controller, error) {
	cfg.applyDefaults()
	if cfg.DaemonID == "" {
		return nil, errors.New("controller: daemon id is required")
	}
	if len(cfg.Workspaces) == 0 || len(cfg.Providers) == 0 {
		return nil, errors.New("controller: at least one workspace and provider are required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Controller{
		cfg: cfg, server: server, tokens: tokens, driver: driver, log: log,
		now:      time.Now,
		runtimes: map[string]daemon.Runtime{},
		entries:  map[string]*entry{},
	}, nil
}

// Run registers runtimes, adopts existing Pods, then loops until ctx ends.
// Pods are left running on shutdown so a restarted controller adopts them.
func (c *Controller) Run(ctx context.Context) error {
	if err := c.register(ctx); err != nil {
		return err
	}
	if err := c.adopt(ctx); err != nil {
		return err
	}

	go c.heartbeatLoop(ctx)

	t := time.NewTicker(c.cfg.PollInterval)
	defer t.Stop()
	for {
		c.Tick(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// register creates one runtime per (workspace, provider).
func (c *Controller) register(ctx context.Context) error {
	for _, ws := range c.cfg.Workspaces {
		runtimes := make([]map[string]any, 0, len(c.cfg.Providers))
		for _, p := range c.cfg.Providers {
			runtimes = append(runtimes, map[string]any{
				"name":    fmt.Sprintf("%s (k8s sandbox)", p),
				"type":    p,
				"version": c.cfg.CLIVersion,
				"status":  "online",
			})
		}
		resp, err := c.server.Register(ctx, map[string]any{
			"workspace_id": ws,
			"daemon_id":    c.cfg.DaemonID,
			"device_name":  c.cfg.DeviceName,
			"cli_version":  c.cfg.CLIVersion,
			"launched_by":  "k8s-controller",
			"runtimes":     runtimes,
		})
		if err != nil {
			return fmt.Errorf("register workspace %s: %w", ws, err)
		}
		for _, rt := range resp.Runtimes {
			c.runtimes[rt.ID] = rt
			c.runtimeIDs = append(c.runtimeIDs, rt.ID)
		}
	}
	if len(c.runtimeIDs) == 0 {
		return errors.New("controller: server returned no runtimes")
	}
	return nil
}

// adopt rebuilds state from existing Pods after a restart. It never calls
// recover-orphans: that would fail tasks whose Pods are still running.
func (c *Controller) adopt(ctx context.Context) error {
	pods, err := c.driver.List(ctx)
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	for _, p := range pods {
		taskID := p.Annotations[AnnTaskID]
		raw, ierr := c.driver.Input(ctx, p.Name)
		var in RunnerInput
		if taskID == "" || ierr != nil || json.Unmarshal(raw, &in) != nil || in.Task.AuthToken == "" {
			// Not recoverable: without the task token the Pod cannot talk to
			// the relay, so it can never finish. Remove it; the server-side
			// task times out or is reclaimed.
			c.log.Warn("removing unadoptable sandbox pod", "pod", p.Name)
			_ = c.driver.Delete(ctx, p.Name)
			continue
		}
		c.track(in.Task, p.Name, p.CreatedAt, p.Phase)
		c.log.Info("adopted sandbox pod", "pod", p.Name, "task", taskID)
	}
	return nil
}

func (c *Controller) track(task daemon.Task, podName string, createdAt time.Time, phase Phase) {
	// The per-claim MCP token lives only in the relay and is not stored in the
	// Pod, so an adopted task loses Remote MCP callbacks (they are denied).
	c.tokens.Register(task.AuthToken, relay.Task{
		TaskID:      task.ID,
		RuntimeID:   task.RuntimeID,
		WorkspaceID: task.WorkspaceID,
	})
	c.mu.Lock()
	c.entries[task.ID] = &entry{
		taskID: task.ID, runtimeID: task.RuntimeID, podName: podName,
		token: task.AuthToken, createdAt: createdAt, phase: phase,
	}
	c.mu.Unlock()
}

func (c *Controller) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(c.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		for _, id := range c.runtimeIDs {
			if _, err := c.server.SendHeartbeat(ctx, id); err != nil && ctx.Err() == nil {
				c.log.Warn("heartbeat failed", "runtime", id, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs one reconcile pass and then claims new work. Exported for tests.
func (c *Controller) Tick(ctx context.Context) {
	c.reconcile(ctx)
	c.claim(ctx)
}

func (c *Controller) active() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *Controller) claim(ctx context.Context) {
	free := c.cfg.MaxPods - c.active()
	if free <= 0 {
		return
	}
	tasks, err := c.server.ClaimTasks(ctx, c.cfg.DaemonID, c.runtimeIDs, free)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Warn("claim failed", "error", err)
		}
		return
	}
	for _, task := range tasks {
		c.launch(ctx, task)
	}
}

// launch starts a sandbox for a freshly claimed task. Any failure is reported
// as runtime_offline, which the server retries.
func (c *Controller) launch(ctx context.Context, task *daemon.Task) {
	rt, ok := c.runtimes[task.RuntimeID]
	if !ok || task.AuthToken == "" {
		c.failTask(ctx, task.ID, "controller cannot run this task", taskfailure.ReasonRuntimeOffline)
		return
	}
	input, err := buildInput(*task, rt)
	if err != nil {
		c.failTask(ctx, task.ID, "controller could not prepare the task: "+err.Error(), taskfailure.ReasonRuntimeOffline)
		return
	}
	name := PodName(task.ID)
	// Register the token before the Pod can possibly call the relay.
	c.tokens.Register(task.AuthToken, relay.Task{
		TaskID: task.ID, RuntimeID: task.RuntimeID, WorkspaceID: task.WorkspaceID,
		RemoteMCPToken: task.RemoteMCPDaemonToken,
	})
	if err := c.driver.Create(ctx, PodSpec{
		Name:        name,
		Annotations: map[string]string{AnnTaskID: task.ID, AnnRuntimeID: task.RuntimeID},
		Input:       input,
	}); err != nil {
		c.log.Error("create sandbox pod failed", "task", task.ID, "error", err)
		c.tokens.Unregister(task.AuthToken)
		_ = c.driver.Delete(ctx, name)
		c.failTask(ctx, task.ID, "could not start a sandbox for this task", taskfailure.ReasonRuntimeOffline)
		return
	}
	now := c.now()
	c.mu.Lock()
	c.entries[task.ID] = &entry{
		taskID: task.ID, runtimeID: task.RuntimeID, podName: name, token: task.AuthToken,
		createdAt: now, phase: PhasePending, lastLease: now, lastStatus: now,
	}
	c.mu.Unlock()
	c.log.Info("sandbox pod created", "task", task.ID, "pod", name)
}

// buildInput serialises the runner input. The per-claim MCP token stays in the
// relay; the Pod gets its own task token in that field so the runner still sees
// a non-empty value and the relay can map it back.
func buildInput(task daemon.Task, rt daemon.Runtime) ([]byte, error) {
	if task.RemoteMCPDaemonToken != "" {
		task.RemoteMCPDaemonToken = task.AuthToken
	}
	return json.Marshal(RunnerInput{Task: task, Runtime: rt})
}

// PodName is the deterministic sandbox name for a task, so a duplicate create
// after a controller restart or reclaim is an AlreadyExists.
func PodName(taskID string) string {
	return "multica-task-" + strings.ToLower(taskID)
}

func (c *Controller) failTask(ctx context.Context, taskID, msg string, reason taskfailure.Reason) {
	if err := c.server.FailTask(ctx, taskID, msg, "", "", "", reason.String(), false, "", ""); err != nil {
		c.log.Warn("fail task report failed", "task", taskID, "error", err)
	}
}

// reconcile advances every tracked task once.
func (c *Controller) reconcile(ctx context.Context) {
	pods, err := c.driver.List(ctx)
	if err != nil {
		c.log.Warn("list pods failed", "error", err)
		return
	}
	byName := make(map[string]PodInfo, len(pods))
	for _, p := range pods {
		byName[p.Name] = p
	}

	c.mu.Lock()
	entries := make([]*entry, 0, len(c.entries))
	for _, e := range c.entries {
		entries = append(entries, e)
	}
	c.mu.Unlock()

	now := c.now()
	for _, e := range entries {
		pod, present := byName[e.podName]
		switch {
		case !present:
			c.finish(ctx, e, "sandbox pod disappeared")
		case pod.Phase == PhaseSucceeded:
			c.finish(ctx, e, "sandbox exited without reporting a result")
		case pod.Phase == PhaseFailed:
			reason := "sandbox pod failed"
			if pod.Reason != "" {
				reason += ": " + pod.Reason
			}
			c.finish(ctx, e, reason)
		case pod.Phase == PhasePending:
			c.tendPending(ctx, e, now)
		default:
			c.tendRunning(ctx, e, now)
		}
	}
}

// tendPending keeps the prepare lease alive while the Pod is scheduled and its
// image pulled, and gives up before the server's own dispatched timeout.
func (c *Controller) tendPending(ctx context.Context, e *entry, now time.Time) {
	if now.Sub(e.createdAt) > c.cfg.PendingTimeout {
		c.log.Warn("sandbox pod did not start in time", "task", e.taskID, "pod", e.podName)
		c.failTask(ctx, e.taskID, "sandbox did not start in time", taskfailure.ReasonTimeout)
		c.cleanup(ctx, e)
		return
	}
	if now.Sub(e.lastLease) >= c.cfg.LeaseInterval {
		e.lastLease = now
		if err := c.server.ExtendTaskPrepareLease(ctx, e.runtimeID, e.taskID); err != nil {
			c.log.Warn("extend prepare lease failed", "task", e.taskID, "error", err)
		}
	}
	// A task cancelled while its Pod is still starting needs no sandbox.
	c.pollStatus(ctx, e, now)
}

// tendRunning is the cancellation backstop. The runner polls status itself and
// acknowledges cancellation; the controller only removes a Pod that outlives
// its finalized task.
func (c *Controller) tendRunning(ctx context.Context, e *entry, now time.Time) {
	if c.cfg.MaxRunDuration > 0 && now.Sub(e.createdAt) > c.cfg.MaxRunDuration {
		c.log.Warn("sandbox exceeded max run duration", "task", e.taskID)
		c.failTask(ctx, e.taskID, "task exceeded the maximum run duration", taskfailure.ReasonTimeout)
		c.cleanup(ctx, e)
		return
	}
	c.pollStatus(ctx, e, now)
	if !e.stopAt.IsZero() && now.After(e.stopAt) {
		c.log.Info("removing sandbox after task was finalized", "task", e.taskID)
		c.cleanup(ctx, e)
	}
}

func (c *Controller) pollStatus(ctx context.Context, e *entry, now time.Time) {
	if now.Sub(e.lastStatus) < c.cfg.StatusInterval {
		return
	}
	e.lastStatus = now
	status, err := c.server.GetTaskStatus(ctx, e.taskID)
	if !daemon.TaskShouldStop(status, err) {
		return
	}
	if e.stopAt.IsZero() {
		e.stopAt = now.Add(c.cfg.CancelGrace)
	}
}

// finish handles a Pod that has stopped. If the runner (or the server) already
// finalized the task nothing more is owed; otherwise the controller fails the
// task, because the server would not notice a dead Pod for hours. When the
// task state cannot be confirmed the entry is kept and retried next tick,
// rather than failing a task that may have completed.
func (c *Controller) finish(ctx context.Context, e *entry, msg string) {
	status, err := c.server.GetTaskStatus(ctx, e.taskID)
	switch {
	case daemon.TaskShouldStop(status, err):
		// already finalized
	case err != nil:
		c.log.Warn("could not confirm task state", "task", e.taskID, "error", err)
		return
	default:
		c.failTask(ctx, e.taskID, msg, taskfailure.ReasonRuntimeRecovery)
	}
	c.cleanup(ctx, e)
}

func (c *Controller) cleanup(ctx context.Context, e *entry) {
	if err := c.driver.Delete(ctx, e.podName); err != nil {
		c.log.Warn("delete sandbox pod failed", "pod", e.podName, "error", err)
		return // retry next tick
	}
	c.tokens.Unregister(e.token)
	c.mu.Lock()
	delete(c.entries, e.taskID)
	c.mu.Unlock()
}
