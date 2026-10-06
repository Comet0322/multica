package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ext-workflow (fork): engine wiring, kept out of main.go so the upstream
// file only carries two hook lines.

// extWorkflowEngineEnv is the kill switch (default on). Off: no engine, no
// listeners, no reconcile job; workflow CRUD stays available and assigning an
// issue to a workflow answers 409 workflow_engine_disabled.
const extWorkflowEngineEnv = "MULTICA_WORKFLOW_ENGINE"

// extWorkflowConstraintCheck is a seam so tests can simulate a constraint that
// lost 'workflow'.
var extWorkflowConstraintCheck = extWorkflowConstraintAdmitsWorkflow

// extWorkflowConstraintAdmitsWorkflow reports whether issue_assignee_type_check
// still admits 'workflow'. An upstream migration that redefines the shared
// constraint drops it; the engine must then stay off (spec §2.3).
func extWorkflowConstraintAdmitsWorkflow(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var def string
	err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
		WHERE c.conname = 'issue_assignee_type_check' AND c.conrelid = 'issue'::regclass`).Scan(&def)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.Contains(def, "'workflow'"), nil
}

// setupExtWorkflow builds the engine when it is enabled and the schema admits
// it, wires it into the handler and the task service, and subscribes the
// task-terminal listeners. It returns nil when the engine stays off.
func setupExtWorkflow(ctx context.Context, pool *pgxpool.Pool, bus *events.Bus, h *handler.Handler) *extworkflow.Engine {
	if !envBool(extWorkflowEngineEnv, true) {
		slog.Info("ext-workflow: engine disabled by " + extWorkflowEngineEnv)
		return nil
	}
	ok, err := extWorkflowConstraintCheck(ctx, pool)
	if err != nil || !ok {
		slog.Error("ext-workflow: issue_assignee_type_check does not admit 'workflow'; engine disabled", "error", err)
		return nil
	}
	bridge := handler.NewExtWorkflowBridge(h)
	engine := extworkflow.NewEngine(extworkflow.Deps{
		Pool: pool, Queries: h.Queries, Issues: h.IssueService, Tasks: h.TaskService,
		Access: bridge, Publisher: bridge, Enabled: true,
	})
	if !engine.Enabled() {
		return nil
	}
	h.ExtWorkflow = engine
	// TaskService.ExtWorkflow is an interface checked with == nil: assign only
	// an enabled, non-nil engine so a typed nil never reads as "enabled".
	h.TaskService.ExtWorkflow = engine
	registerExtWorkflowListeners(bus, engine)
	return engine
}

type extWorkflowTaskObserver interface {
	OnTaskTerminal(ctx context.Context, taskID pgtype.UUID) error
}

// registerExtWorkflowListeners forwards terminal task events to the engine.
// A failure with a retry pending is skipped: the retry carries the same
// workflow stamp and reports again.
func registerExtWorkflowListeners(bus *events.Bus, engine extWorkflowTaskObserver) {
	ctx := context.Background()
	forward := func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		if pending, _ := payload["retry_pending"].(bool); pending {
			return
		}
		raw, _ := payload["task_id"].(string)
		taskID, err := util.ParseUUID(raw)
		if err != nil {
			return
		}
		if err := engine.OnTaskTerminal(ctx, taskID); err != nil {
			slog.Warn("ext-workflow: task terminal handling failed", "task_id", raw, "error", err)
		}
	}
	for _, eventType := range []string{protocol.EventTaskCompleted, protocol.EventTaskFailed, protocol.EventTaskCancelled} {
		bus.Subscribe(eventType, forward)
	}
}
