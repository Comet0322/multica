package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type extTaskFixture struct {
	principalFixture
	owner   pgtype.UUID
	agent   pgtype.UUID
	runtime string
	issue   pgtype.UUID
}

func newExtTaskFixture(t *testing.T) extTaskFixture {
	t.Helper()
	f, owner := newPrincipalFixture(t)
	agent := f.privateAgentOwnedBy(t, owner, "ext-task")
	var runtimeID string
	f.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, agent).Scan(&runtimeID)
	issue := f.Issue(t, "Workflow child")
	// Fixture issues bypass the workspace counter that issue creation draws from.
	f.Exec(t, `UPDATE workspace SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) WHERE id = $1`, f.WorkspaceID)
	f.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issue)
	return extTaskFixture{principalFixture: f, owner: parseTestUUID(t, owner), agent: parseTestUUID(t, agent), runtime: runtimeID, issue: parseTestUUID(t, issue)}
}

func (f extTaskFixture) params(runID, stepID pgtype.UUID) ExtWorkflowTaskParams {
	return ExtWorkflowTaskParams{
		IssueID: f.issue, AgentID: f.agent, RunID: runID, StepID: stepID,
		Role: "step", Kind: "step", HandoffNote: "Workflow step build, attempt 1 of 3.", ActorUserID: f.owner,
	}
}

func (f extTaskFixture) enqueue(t *testing.T, p ExtWorkflowTaskParams) (db.AgentTaskQueue, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	task, err := f.svc.TaskSvc.EnqueueExtWorkflowTask(ctx, tx, p)
	if err != nil {
		return task, err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return task, nil
}

func TestEnqueueExtWorkflowTaskStampsColumns(t *testing.T) {
	f := newExtTaskFixture(t)
	runID, stepID := dbid.NewV7(), dbid.NewV7()
	task, err := f.enqueue(t, f.params(runID, stepID))
	if err != nil {
		t.Fatalf("EnqueueExtWorkflowTask: %v", err)
	}
	if task.Status != "queued" || task.IssueID != f.issue || task.AgentID != f.agent {
		t.Fatalf("task = %s issue=%v agent=%v", task.Status, task.IssueID, task.AgentID)
	}
	if task.ExtWorkflowRunID != runID || task.ExtWorkflowStepID != stepID || task.ExtWorkflowRole.String != "step" || task.ExtWorkflowKind.String != "step" {
		t.Fatalf("ext columns = %v %v %q %q", task.ExtWorkflowRunID, task.ExtWorkflowStepID, task.ExtWorkflowRole.String, task.ExtWorkflowKind.String)
	}
	if task.HandoffNote.String != "Workflow step build, attempt 1 of 3." || task.OriginatorUserID != f.owner {
		t.Fatalf("handoff=%q originator=%v", task.HandoffNote.String, task.OriginatorUserID)
	}

	again, err := f.enqueue(t, f.params(runID, stepID))
	if err != nil || again.ID != task.ID {
		t.Fatalf("second enqueue = %v, %v; want the pending task %v", again.ID, err, task.ID)
	}
}

func TestEnqueueExtWorkflowTaskAdoptsQueuedPlainRun(t *testing.T) {
	f := newExtTaskFixture(t)
	plain := f.Task(t, util.UUIDToString(f.agent), testutil.Cols{"issue_id": util.UUIDToString(f.issue), "runtime_id": f.runtime})
	runID, stepID := dbid.NewV7(), dbid.NewV7()
	task, err := f.enqueue(t, f.params(runID, stepID))
	if err != nil {
		t.Fatalf("EnqueueExtWorkflowTask: %v", err)
	}
	if util.UUIDToString(task.ID) != plain || task.ExtWorkflowRunID != runID || task.ExtWorkflowStepID != stepID {
		t.Fatalf("task %v run=%v; want the adopted plain run %s", task.ID, task.ExtWorkflowRunID, plain)
	}
}

func TestEnqueueExtWorkflowTaskReportsBusySlot(t *testing.T) {
	f := newExtTaskFixture(t)
	f.Task(t, util.UUIDToString(f.agent), testutil.Cols{"issue_id": util.UUIDToString(f.issue), "runtime_id": f.runtime, "status": "dispatched"})
	if _, err := f.enqueue(t, f.params(dbid.NewV7(), dbid.NewV7())); !errors.Is(err, ErrExtTaskSlotBusy) {
		t.Fatalf("err = %v, want ErrExtTaskSlotBusy", err)
	}
}

func TestEnqueueExtWorkflowTaskRefusesArchivedAgent(t *testing.T) {
	f := newExtTaskFixture(t)
	f.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, f.agent)
	if _, err := f.enqueue(t, f.params(dbid.NewV7(), dbid.NewV7())); !errors.Is(err, ErrExtAgentUnavailable) {
		t.Fatalf("err = %v, want ErrExtAgentUnavailable", err)
	}
}

func TestExtCreateChildIssueTxCreatesBacklogChild(t *testing.T) {
	f := newExtTaskFixture(t)
	ctx := context.Background()
	bus := events.New()
	issues := NewIssueService(f.q, f.Pool, bus, analytics.NoopClient{}, f.svc.TaskSvc)
	parent, err := f.q.GetIssue(ctx, f.issue)
	if err != nil {
		t.Fatal(err)
	}
	var published []map[string]any
	bus.Subscribe(protocol.EventIssueCreated, func(e events.Event) {
		published = append(published, e.Payload.(map[string]any))
	})

	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	child, err := issues.ExtCreateChildIssueTx(ctx, tx, IssueCreateParams{
		WorkspaceID: parent.WorkspaceID, Title: parent.Title + " · Build", Status: "backlog", Priority: "none",
		AssigneeType: pgtype.Text{String: "agent", Valid: true}, AssigneeID: f.agent,
		CreatorType: "member", CreatorID: f.owner, ParentIssueID: parent.ID,
	})
	if err != nil {
		t.Fatalf("ExtCreateChildIssueTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, `DELETE FROM issue WHERE id = $1`, child.ID)
	if child.ParentIssueID != parent.ID || child.Status != "backlog" || child.Number == parent.Number {
		t.Fatalf("child parent=%v status=%s number=%d", child.ParentIssueID, child.Status, child.Number)
	}
	if n := f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, child.ID); n != 0 {
		t.Fatalf("child has %d tasks, want none", n)
	}
	issues.ExtPublishIssueCreated(ctx, child, "member", util.UUIDToString(f.owner))
	if len(published) != 1 || published[0]["issue"].(map[string]any)["id"] != util.UUIDToString(child.ID) {
		t.Fatalf("issue:created payloads = %v", published)
	}
}
