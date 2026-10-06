package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow (fork): what upstream issue-trigger paths do for a workflow
// assignee. Each is called from a one-line hook in an upstream file.

// ErrRerunWorkflowIssue refuses a manual rerun of a workflow-run issue or of
// a workflow task: those are driven through the run's decisions.
var ErrRerunWorkflowIssue = errors.New("this issue is run by a workflow; decide on the workflow run instead of rerunning it")

// extWorkflowRunTrigger is WillEnqueueRun's answer for a workflow assignee:
// a run starts unless the engine is off, the workflow is gone or archived, a
// run is already active, or the supervisor cannot run. The supervisor is the
// agent reported to the preview.
func (s *IssueService) extWorkflowRunTrigger(ctx context.Context, issue db.Issue, source RunEnqueueSource, canAccess func(db.Agent) bool) (IssueRunTrigger, bool) {
	if s.TaskService == nil || s.TaskService.ExtWorkflow == nil {
		return IssueRunTrigger{}, false
	}
	wf, err := s.Queries.GetExtWorkflowInWorkspace(ctx, db.GetExtWorkflowInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
	if err != nil || wf.ArchivedAt.Valid {
		return IssueRunTrigger{}, false
	}
	if _, err := s.Queries.GetActiveExtWorkflowRunByIssue(ctx, issue.ID); !errors.Is(err, pgx.ErrNoRows) {
		return IssueRunTrigger{}, false // active run, or the check failed: never over-promise
	}
	supervisor, err := s.Queries.GetAgent(ctx, wf.SupervisorAgentID)
	if err != nil || supervisor.ArchivedAt.Valid || !supervisor.RuntimeID.Valid || !canAccess(supervisor) {
		return IssueRunTrigger{}, false
	}
	return IssueRunTrigger{IssueID: issue.ID, AgentID: supervisor.ID, AssigneeType: "workflow", Source: source}, true
}

// extWorkflowStartOnAssign starts the run of a workflow-assigned issue that
// was just created outside backlog.
func (s *IssueService) extWorkflowStartOnAssign(ctx context.Context, issue db.Issue, actorType, actorID string) {
	if s.TaskService == nil || s.TaskService.ExtWorkflow == nil {
		return
	}
	actor, _ := util.ParseUUID(actorID)
	if err := s.TaskService.ExtWorkflow.StartRun(ctx, issue.ID, actorType, actor); err != nil {
		slog.Warn("ext-workflow: start run on create failed", "issue_id", util.UUIDToString(issue.ID), "error", err)
	}
}

// extWorkflowRerunRefused reports whether RerunIssue must refuse: the issue is
// workflow-assigned (no source task) or the named task is a workflow task.
func extWorkflowRerunRefused(issue db.Issue, sourceTaskID pgtype.UUID, sourceTask *db.AgentTaskQueue) bool {
	if sourceTask != nil {
		return sourceTask.ExtWorkflowRunID.Valid
	}
	return !sourceTaskID.Valid && issue.AssigneeType.String == "workflow"
}
