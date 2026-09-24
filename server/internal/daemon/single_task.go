package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
)

// terminalReplayGrace bounds how long RunSingleTask keeps retrying an
// undelivered terminal report after the task pipeline returns.
const terminalReplayGrace = 30 * time.Second

// RunSingleTask runs one already-claimed task through the normal daemon task
// pipeline (handleTask) and returns when it has finished and reported.
//
// It is the entry point of a sandbox Pod. Claiming, heartbeats and runtime
// registration belong to the controller, so none of that runs here; the daemon
// only needs its runtime and workspace tables seeded with the one runtime the
// task targets.
//
// The client authenticates with the task's own token (task.AuthToken). In the
// Kubernetes runtime cfg.ServerBaseURL points at the relay, which recognises
// that token, checks the request is for this task, and forwards it with the
// daemon credential. The Pod therefore never holds the daemon credential.
func RunSingleTask(ctx context.Context, cfg Config, task Task, rt Runtime, logger *slog.Logger) error {
	if task.ID == "" || task.WorkspaceID == "" {
		return errors.New("single task: task id and workspace id are required")
	}
	if task.AuthToken == "" {
		return errors.New("single task: task has no task-scoped auth token")
	}
	if rt.ID == "" || rt.ID != task.RuntimeID {
		return fmt.Errorf("single task: runtime %q does not match task runtime %q", rt.ID, task.RuntimeID)
	}

	d := New(cfg, logger)
	d.client.SetToken(task.AuthToken)

	d.mu.Lock()
	d.runtimeIndex[rt.ID] = rt
	d.workspaces[task.WorkspaceID] = &workspaceState{
		workspaceID:     task.WorkspaceID,
		runtimeIDs:      []string{rt.ID},
		allowedRepoURLs: repoAllowlist(task.Repos),
	}
	d.mu.Unlock()

	// The agent CLI reaches `multica repo checkout` through this loopback
	// endpoint (MULTICA_DAEMON_PORT). It is bound to the Pod's own loopback.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.HealthPort))
	if err != nil {
		return fmt.Errorf("single task: listen for repo checkout: %w", err)
	}
	healthCtx, stopHealth := context.WithCancel(ctx)
	defer stopHealth()
	go d.serveHealth(healthCtx, ln, time.Now())

	d.handleTask(ctx, task, 0)

	// handleTask persists the terminal report to a local outbox before sending
	// and normally delivers it inline. Give any leftover a bounded last chance;
	// the replay loop that would otherwise retry it does not run in a Pod.
	replayCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalReplayGrace)
	defer cancel()
	if pending, _ := d.replayPendingTerminalReports(replayCtx); pending > 0 {
		return fmt.Errorf("single task: %d terminal report(s) not delivered", pending)
	}
	return nil
}

// TaskShouldStop reports whether a GetTaskStatus result means a running task
// must be stopped: the server already finalized it, or the row is gone. Any
// other error (network, 5xx) must not stop work. It exposes the native
// daemon's cancellation rule so the sandbox controller applies the same one.
func TaskShouldStop(status string, err error) bool {
	return shouldInterruptAgent(status, err)
}
