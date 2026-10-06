package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type recordingTaskObserver struct{ ids []pgtype.UUID }

func (r *recordingTaskObserver) OnTaskTerminal(_ context.Context, id pgtype.UUID) error {
	r.ids = append(r.ids, id)
	return nil
}

func TestExtWorkflowListenersForwardTerminalTasks(t *testing.T) {
	bus := events.New()
	obs := &recordingTaskObserver{}
	registerExtWorkflowListeners(bus, obs)
	completed, failed, retried := dbid.NewV7(), dbid.NewV7(), dbid.NewV7()
	publish := func(eventType string, id pgtype.UUID, extra map[string]any) {
		payload := map[string]any{"task_id": util.UUIDToString(id)}
		for k, v := range extra {
			payload[k] = v
		}
		bus.Publish(events.Event{Type: eventType, Payload: payload})
	}
	publish(protocol.EventTaskCompleted, completed, nil)
	publish(protocol.EventTaskFailed, retried, map[string]any{"retry_pending": true})
	publish(protocol.EventTaskFailed, failed, map[string]any{"retry_pending": false})
	if len(obs.ids) != 2 || obs.ids[0] != completed || obs.ids[1] != failed {
		t.Fatalf("forwarded %v, want completed then failed (retry pending skipped)", obs.ids)
	}
}

func TestExtWorkflowConstraintAdmitsWorkflow(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ok, err := extWorkflowConstraintAdmitsWorkflow(context.Background(), testPool)
	if err != nil || !ok {
		t.Fatalf("constraint admits workflow = %v, %v; want true after the ext migrations", ok, err)
	}
}

func assertExtWorkflowOff(t *testing.T, h *handler.Handler, bus *events.Bus) {
	t.Helper()
	if h.ExtWorkflow != nil {
		t.Fatal("Handler.ExtWorkflow set while the engine must be off")
	}
	if h.TaskService.ExtWorkflow != nil {
		t.Fatal("TaskService.ExtWorkflow must stay a nil interface while the engine is off")
	}
	for _, et := range []string{protocol.EventTaskCompleted, protocol.EventTaskFailed, protocol.EventTaskCancelled} {
		if n := bus.SubscriberCount(et); n != 0 {
			t.Fatalf("%s has %d listeners, want 0", et, n)
		}
	}
}

func TestSetupExtWorkflowHonoursTheKillSwitch(t *testing.T) {
	t.Setenv(extWorkflowEngineEnv, "false")
	bus := events.New()
	h := &handler.Handler{TaskService: &service.TaskService{}}
	if engine := setupExtWorkflow(context.Background(), nil, bus, h); engine != nil {
		t.Fatal("engine built with the kill switch off")
	}
	assertExtWorkflowOff(t, h, bus)
}

func TestSetupExtWorkflowStaysOffWhenConstraintLacksWorkflow(t *testing.T) {
	t.Setenv(extWorkflowEngineEnv, "true")
	orig := extWorkflowConstraintCheck
	extWorkflowConstraintCheck = func(context.Context, *pgxpool.Pool) (bool, error) { return false, nil }
	t.Cleanup(func() { extWorkflowConstraintCheck = orig })

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	bus := events.New()
	h := &handler.Handler{TaskService: &service.TaskService{}}
	if engine := setupExtWorkflow(context.Background(), nil, bus, h); engine != nil {
		t.Fatal("engine built although the constraint lacks 'workflow'")
	}
	assertExtWorkflowOff(t, h, bus)
	if out := buf.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "issue_assignee_type_check") {
		t.Fatalf("want an error log about the constraint, got %q", out)
	}
}
