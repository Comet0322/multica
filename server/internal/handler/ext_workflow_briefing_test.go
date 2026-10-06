package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestExtWorkflowClaimCarriesTheBriefing(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "ext-wf-claim runtime")
	agentOnRuntime := func(name string) string {
		id := dbfx.Agent(t, name, runtimeID, testutil.Cols{
			"visibility": "workspace", "permission_mode": "public_to", "instructions": "Be terse.",
			"custom_env": testutil.Raw("'{}'::jsonb"), "custom_args": testutil.Raw("'[]'::jsonb"),
		})
		dbfx.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'workspace', $2) ON CONFLICT DO NOTHING`, id, testWorkspaceID)
		return id
	}
	supervisor, worker := agentOnRuntime("ext-wf-claim supervisor"), agentOnRuntime("ext-wf-claim worker")
	wf := createExtWorkflowAs(t, "", "Claimed workflow", supervisor)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"nodes": []any{extWFNode("build", worker)}}).Want(http.StatusOK)
	var issue IssueResponse
	createWorkflowIssue(t, wf.ID, "todo").Want(http.StatusCreated).JSON(&issue)
	var stepTaskID string
	dbfx.QueryRow(t, `
		SELECT t.id FROM agent_task_queue t JOIN issue c ON c.id = t.issue_id
		WHERE c.parent_issue_id = $1 AND t.ext_workflow_role = 'step'`, issue.ID).Scan(&stepTaskID)

	taskID, instructions, isLeader, raw := claimAgentInstructionsForTest(t, runtimeID)
	if taskID != stepTaskID {
		t.Fatalf("claimed %q, want the step task %s: %s", taskID, stepTaskID, raw)
	}
	if !strings.HasPrefix(instructions, "Be terse.\n\n## Workflow step") {
		t.Fatalf("instructions do not append the briefing to the agent's own:\n%s", instructions)
	}
	for _, want := range []string{"step 1 of 1, **Title build**", "workflow **Claimed workflow**", "```ext-workflow"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("briefing lacks %q", want)
		}
	}
	if isLeader || strings.Contains(instructions, "## Squad Operating Protocol") {
		t.Fatalf("a workflow step task was delivered as a squad-leader task")
	}
}
