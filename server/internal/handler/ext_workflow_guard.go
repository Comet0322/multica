package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow (fork): the agent write guard (spec §6.5). Issue writes made by
// an agent may not change the status, assignee or parent of an active run's
// issues, or delete them, except a step agent moving its own step's issue
// while it works on it. Members are never guarded, and the engine writes
// through queries, not these handlers. The hooks in issue.go stay one line.

// extWorkflowIssueChange is what writing params over prev would change.
func (h *Handler) extWorkflowIssueChange(r *http.Request, prev db.Issue, params db.UpdateIssueParams) extworkflow.IssueChange {
	var c extworkflow.IssueChange
	if params.Status.Valid && params.Status.String != prev.Status {
		c.Status = issuestatus.Effective(r.Context(), h.Queries, prev.WorkspaceID, params.Status.String)
	}
	c.Assignee = params.AssigneeType != prev.AssigneeType || params.AssigneeID != prev.AssigneeID
	c.Parent = params.ParentIssueID != prev.ParentIssueID
	return c
}

// refuseExtWorkflowAgentUpdate answers 409 when an agent's single-issue update
// would steer an active workflow run.
func (h *Handler) refuseExtWorkflowAgentUpdate(w http.ResponseWriter, r *http.Request, prev db.Issue, params db.UpdateIssueParams) bool {
	return h.refuseExtWorkflowAgentChange(w, r, prev, h.extWorkflowIssueChange(r, prev, params))
}

// refuseExtWorkflowAgentDelete answers 409 when an agent deletes an active
// run's parent or step issue.
func (h *Handler) refuseExtWorkflowAgentDelete(w http.ResponseWriter, r *http.Request, issue db.Issue) bool {
	return h.refuseExtWorkflowAgentChange(w, r, issue, extworkflow.IssueChange{Delete: true})
}

// refuseExtWorkflowAgentBatchUpdate checks a whole batch update before any of
// it is written, so a refused batch changes nothing. Items the batch loop
// would skip (bad ids, unparseable values) are left to it.
func (h *Handler) refuseExtWorkflowAgentBatchUpdate(w http.ResponseWriter, r *http.Request, workspaceID pgtype.UUID, ids []string, updates UpdateIssueRequest, rawUpdates map[string]json.RawMessage, statusKey string) bool {
	_, assigneeType := rawUpdates["assignee_type"]
	_, assigneeID := rawUpdates["assignee_id"]
	_, parent := rawUpdates["parent_issue_id"]
	if updates.Status == nil && !assigneeType && !assigneeID && !parent {
		return false
	}
	if !h.isExtWorkflowAgent(r, workspaceID) {
		return false
	}
	for _, issue := range h.extWorkflowBatchIssues(r, workspaceID, ids) {
		params := db.UpdateIssueParams{AssigneeType: issue.AssigneeType, AssigneeID: issue.AssigneeID, ParentIssueID: issue.ParentIssueID}
		if updates.Status != nil {
			params.Status = pgtype.Text{String: statusKey, Valid: true}
		}
		if assigneeType {
			params.AssigneeType = pgtype.Text{}
			if updates.AssigneeType != nil {
				params.AssigneeType = pgtype.Text{String: *updates.AssigneeType, Valid: true}
			}
		}
		if assigneeID {
			params.AssigneeID = pgtype.UUID{}
			if updates.AssigneeID != nil {
				params.AssigneeID, _ = util.ParseUUID(*updates.AssigneeID)
			}
		}
		if parent {
			params.ParentIssueID = pgtype.UUID{}
			if updates.ParentIssueID != nil {
				params.ParentIssueID, _ = util.ParseUUID(*updates.ParentIssueID)
			}
		}
		if h.refuseExtWorkflowAgentChange(w, r, issue, h.extWorkflowIssueChange(r, issue, params)) {
			return true
		}
	}
	return false
}

// refuseExtWorkflowAgentBatchDelete checks a whole batch delete before any of
// it runs: the delete loop cancels tasks as it goes.
func (h *Handler) refuseExtWorkflowAgentBatchDelete(w http.ResponseWriter, r *http.Request, workspaceID pgtype.UUID, ids []string) bool {
	if !h.isExtWorkflowAgent(r, workspaceID) {
		return false
	}
	for _, issue := range h.extWorkflowBatchIssues(r, workspaceID, ids) {
		if h.refuseExtWorkflowAgentDelete(w, r, issue) {
			return true
		}
	}
	return false
}

func (h *Handler) extWorkflowBatchIssues(r *http.Request, workspaceID pgtype.UUID, ids []string) []db.Issue {
	issues := make([]db.Issue, 0, len(ids))
	for _, id := range ids {
		issueID, err := util.ParseUUID(id)
		if err != nil {
			continue
		}
		issue, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: issueID, WorkspaceID: workspaceID})
		if err != nil {
			continue
		}
		issues = append(issues, issue)
	}
	return issues
}

func (h *Handler) isExtWorkflowAgent(r *http.Request, workspaceID pgtype.UUID) bool {
	actorType, _ := h.resolveActor(r, requestUserID(r), uuidToString(workspaceID))
	return actorType == "agent"
}

// refuseExtWorkflowAgentChange writes the 409 (or a 500 when the guard cannot
// be evaluated) and reports whether it did.
func (h *Handler) refuseExtWorkflowAgentChange(w http.ResponseWriter, r *http.Request, issue db.Issue, change extworkflow.IssueChange) bool {
	if !change.Any() {
		return false
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), uuidToString(issue.WorkspaceID))
	if actorType != "agent" {
		return false
	}
	// Both ids are set for every agent actor; a bad one only fails to match.
	agentID, _ := util.ParseUUID(actorID)
	taskID, _ := util.ParseUUID(r.Header.Get("X-Task-ID"))
	row, err := h.Queries.GetExtWorkflowIssueGuard(r.Context(), db.GetExtWorkflowIssueGuardParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, TaskID: taskID, AgentID: agentID,
	})
	if err != nil {
		slog.Error("ext-workflow: issue guard", append(logger.RequestAttrs(r), "issue_id", uuidToString(issue.ID), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to check the issue's workflow")
		return true
	}
	guard := extworkflow.IssueGuard{ActiveParent: row.ActiveParent, ActiveChild: row.ActiveChild, OwnStepTask: row.OwnStepTask}
	if msg := extworkflow.AgentIssueRefusal(guard, change); msg != "" {
		writeError(w, http.StatusConflict, msg)
		return true
	}
	return false
}
