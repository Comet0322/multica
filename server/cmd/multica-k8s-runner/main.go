// Command multica-k8s-runner runs exactly one Multica task inside a sandbox
// Pod. The controller creates the Pod and writes the claimed task to
// MULTICA_TASK_INPUT; the runner executes it through the normal daemon task
// pipeline and exits.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/k8sruntime/controller"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// The task pipeline prepares the execution environment in a killable
	// child that re-executes this binary; serve that private helper protocol.
	if len(os.Args) == 2 && os.Args[1] == execenv.PreparationHelperArg {
		if err := execenv.RunPreparationHelper(os.Stdin, os.Stdout, logger); err != nil {
			logger.Error("execution environment helper failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := run(logger); err != nil {
		logger.Error("runner failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	path := os.Getenv("MULTICA_TASK_INPUT")
	if path == "" {
		return fmt.Errorf("MULTICA_TASK_INPUT is not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read task input: %w", err)
	}
	var in controller.RunnerInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("parse task input: %w", err)
	}

	// MULTICA_SERVER_URL points at the relay, so every daemon call and every
	// agent CLI call made from this Pod goes through it.
	cfg, err := daemon.LoadConfig(daemon.Overrides{})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if _, ok := cfg.Agents[in.Runtime.Provider]; !ok {
		return fmt.Errorf("agent CLI %q is not installed in this image", in.Runtime.Provider)
	}

	// SIGTERM means the platform is removing this Pod (eviction, node drain,
	// rollout). It must not be turned into a task result: cancelling the
	// pipeline would report the task as "cancelled", which the server never
	// retries. Exit without reporting so the controller sees a failed Pod and
	// fails the task with a retryable reason. A real user cancellation is
	// discovered by the task pipeline's own status polling, not by a signal.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sig
		logger.Warn("terminated by the platform; exiting without reporting", "signal", s.String())
		os.Exit(143)
	}()

	logger.Info("running task", "task", in.Task.ID, "provider", in.Runtime.Provider)
	return daemon.RunSingleTask(context.Background(), cfg, in.Task, in.Runtime, logger)
}
