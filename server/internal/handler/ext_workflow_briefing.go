package handler

import (
	"context"
	"log/slog"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// appendExtWorkflowBriefing appends the workflow briefing to a claimed
// workflow task's instructions, after any squad briefing. Workflow tasks
// never carry is_leader_task or squad_id, so the daemon runs them as plain
// agent turns. A briefing that cannot be built leaves the claim untouched.
func (h *Handler) appendExtWorkflowBriefing(ctx context.Context, task *db.AgentTaskQueue, resp *AgentTaskResponse) {
	if task == nil || !task.ExtWorkflowRunID.Valid || resp.Agent == nil || !h.ExtWorkflow.Enabled() {
		return
	}
	text, ok, err := h.ExtWorkflow.BuildBriefing(ctx, *task)
	if err != nil {
		slog.Warn("ext-workflow: briefing failed", "task_id", uuidToString(task.ID), "error", err)
		return
	}
	if !ok || text == "" {
		return
	}
	if strings.TrimSpace(resp.Agent.Instructions) == "" {
		resp.Agent.Instructions = text
	} else {
		resp.Agent.Instructions += "\n\n" + text
	}
}
