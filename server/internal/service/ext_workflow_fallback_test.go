package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// completeWithOutput runs a task of the given ext-workflow shape to completion
// with a final output and no agent comment, then returns how many comments the
// platform fallback left on the task's issue.
func completeWithOutput(t *testing.T, role, kind string) int {
	t.Helper()
	f, owner := newPrincipalFixture(t)
	agent := f.privateAgentOwnedBy(t, owner, "fallback")
	issue := f.Issue(t, "Fallback issue")
	cols := testutil.Cols{
		"issue_id": issue, "status": "running", "started_at": testutil.Raw("clock_timestamp()"),
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agent + "')"),
	}
	if role != "" {
		cols["ext_workflow_run_id"] = testutil.Raw("gen_random_uuid()")
		cols["ext_workflow_role"] = role
		cols["ext_workflow_kind"] = kind
	}
	taskID := f.Task(t, agent, cols)
	svc := &TaskService{Queries: f.q, TxStarter: f.Pool, Bus: f.svc.Bus}
	result := []byte(`{"output":"final words"}`)
	if _, err := svc.CompleteTask(context.Background(), parseTestUUID(t, taskID), result, "", "", "", false, "", ""); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	return f.Count(t, "SELECT count(*) FROM comment WHERE issue_id=$1", util.UUIDToString(parseTestUUID(t, issue)))
}

func TestCompleteTaskFallbackCommentOnExtWorkflowTasks(t *testing.T) {
	for _, tc := range []struct {
		name, role, kind string
		want             int
	}{
		{"plain task keeps the fallback", "", "", 1},
		{"supervisor summary keeps the fallback", "supervisor", "summary", 1},
		{"supervisor conversation keeps the fallback", "supervisor", "conversation", 1},
		{"step task keeps the fallback", "step", "step", 1},
		{"supervisor review is skipped", "supervisor", "review", 0},
		{"supervisor failure is skipped", "supervisor", "failure", 0},
		{"supervisor rewind_request is skipped", "supervisor", "rewind_request", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := completeWithOutput(t, tc.role, tc.kind); got != tc.want {
				t.Fatalf("comments on the issue = %d, want %d", got, tc.want)
			}
		})
	}
}
