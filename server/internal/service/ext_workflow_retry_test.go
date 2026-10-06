package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The engine's run, step, event and task queries exist (B5 and B6 use them).
var _ = []any{
	(*db.Queries).CreateExtWorkflowRun, (*db.Queries).LockExtWorkflowRun, (*db.Queries).UpdateExtWorkflowRunStep,
	(*db.Queries).CreateExtWorkflowRunEvent, (*db.Queries).CreateExtWorkflowTask, (*db.Queries).CancelExtWorkflowRunTasks,
	(*db.Queries).ListExtWorkflowRunsForReconcile, (*db.Queries).ListExtWorkflowRunSummariesByIssue,
}

// A platform retry of a workflow task is the same attempt: the engine finds
// it through the ext columns, so CreateRetryTask must copy all four.
func TestCreateRetryTaskCopiesExtWorkflowColumns(t *testing.T) {
	f, owner := newPrincipalFixture(t)
	ctx := context.Background()
	agent := f.privateAgentOwnedBy(t, owner, "ext-retry")
	issue := f.Issue(t, "Workflow step")
	var runtimeID string
	f.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, agent).Scan(&runtimeID)
	runID, stepID := dbid.NewV7(), dbid.NewV7()
	parent := f.Task(t, agent, testutil.Cols{
		"runtime_id":           runtimeID,
		"issue_id":             issue,
		"status":               "failed",
		"attempt":              1,
		"max_attempts":         3,
		"ext_workflow_run_id":  runID,
		"ext_workflow_step_id": stepID,
		"ext_workflow_role":    "step",
		"ext_workflow_kind":    "step",
	})
	f.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id = $1`, parent)

	retry, err := f.q.CreateRetryTask(ctx, db.CreateRetryTaskParams{ID: parseTestUUID(t, parent)})
	if err != nil {
		t.Fatalf("CreateRetryTask: %v", err)
	}
	if retry.ExtWorkflowRunID != runID || retry.ExtWorkflowStepID != stepID ||
		retry.ExtWorkflowRole.String != "step" || retry.ExtWorkflowKind.String != "step" {
		t.Fatalf("retry ext columns = %v %v %q %q", retry.ExtWorkflowRunID, retry.ExtWorkflowStepID, retry.ExtWorkflowRole.String, retry.ExtWorkflowKind.String)
	}
}
