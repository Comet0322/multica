package extworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// ValidateAssignment answers whether the actor may assign an issue to the
// workflow (spec §4.1). Refusals are *AssignError carrying the HTTP status.
func (e *Engine) ValidateAssignment(ctx context.Context, workspaceID, workflowID pgtype.UUID, actorType string, actorID pgtype.UUID) error {
	if !e.Enabled() {
		return &AssignError{Status: http.StatusConflict, Code: "workflow_engine_disabled", Message: "workflow_engine_disabled", Err: ErrEngineDisabled}
	}
	wf, err := e.q.GetExtWorkflowInWorkspace(ctx, db.GetExtWorkflowInWorkspaceParams{ID: workflowID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return &AssignError{Status: http.StatusBadRequest, Code: "workflow_not_found", Message: "assignee_id does not refer to a workflow in this workspace", Err: ErrWorkflowNotFound}
	}
	if err != nil {
		return fmt.Errorf("load workflow: %w", err)
	}
	if wf.ArchivedAt.Valid {
		return &AssignError{Status: http.StatusBadRequest, Code: "workflow_archived", Message: "cannot assign to an archived workflow", Err: ErrWorkflowArchived}
	}
	nodes, err := e.q.ListExtWorkflowNodes(ctx, wf.ID)
	if err != nil {
		return fmt.Errorf("load workflow nodes: %w", err)
	}
	if len(nodes) == 0 {
		return &AssignError{Status: http.StatusBadRequest, Code: "workflow_has_no_nodes", Message: "the workflow has no nodes", Err: ErrNoNodes}
	}
	supervisor, err := e.q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: wf.SupervisorAgentID, WorkspaceID: workspaceID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load supervisor: %w", err)
	}
	if err != nil || supervisor.ArchivedAt.Valid || !supervisor.RuntimeID.Valid {
		return &AssignError{Status: http.StatusBadRequest, Code: "workflow_supervisor_unavailable", Message: "the workflow's supervisor agent is archived or has no runtime", Err: ErrSupervisorUnavailable}
	}
	agents := []pgtype.UUID{wf.SupervisorAgentID}
	seen := map[pgtype.UUID]bool{wf.SupervisorAgentID: true}
	for _, n := range nodes {
		if !seen[n.AgentID] {
			seen[n.AgentID] = true
			agents = append(agents, n.AgentID)
		}
	}
	if e.access != nil {
		for _, agentID := range agents {
			ok, err := e.access.CanInvokeAgent(ctx, workspaceID, actorType, actorID, agentID)
			if err != nil {
				return fmt.Errorf("check agent access: %w", err)
			}
			if !ok {
				return &AssignError{Status: http.StatusForbidden, Code: "workflow_agent_not_invokable", Message: "you do not have permission to assign work to every agent in this workflow", Err: ErrAgentNotInvokable}
			}
		}
	}
	return nil
}

// StartRun creates a run for a workflow-assigned issue in one transaction:
// the run with a definition snapshot, one backlog child issue and one pending
// step per node, the parent in progress, then the first scheduling pass
// (spec §4.2). It is a no-op when the issue is not assigned to a workflow or
// already has an active run.
func (e *Engine) StartRun(ctx context.Context, issueID pgtype.UUID, actorType string, actorID pgtype.UUID) error {
	if !e.Enabled() {
		return ErrEngineDisabled
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := e.q.WithTx(tx)
	parent, err := q.LockIssueForExtWorkflowStart(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock issue: %w", err)
	}
	if parent.AssigneeType.String != "workflow" || !parent.AssigneeID.Valid {
		return nil
	}
	if _, err := q.GetActiveExtWorkflowRunByIssue(ctx, parent.ID); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check active run: %w", err)
	}
	wf, err := q.GetExtWorkflowInWorkspace(ctx, db.GetExtWorkflowInWorkspaceParams{ID: parent.AssigneeID, WorkspaceID: parent.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkflowNotFound
	}
	if err != nil {
		return fmt.Errorf("load workflow: %w", err)
	}
	if wf.ArchivedAt.Valid {
		return ErrWorkflowArchived
	}
	nodes, err := q.ListExtWorkflowNodes(ctx, wf.ID)
	if err != nil {
		return fmt.Errorf("load workflow nodes: %w", err)
	}
	if len(nodes) == 0 {
		return ErrNoNodes
	}
	def := definitionFromRows(wf, nodes)
	raw, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("encode definition: %w", err)
	}
	// The run, the children and their creator name a real actor; a system
	// trigger is attributed to the issue's creator.
	if (actorType != "member" && actorType != "agent") || !actorID.Valid {
		actorType, actorID = parent.CreatorType, parent.CreatorID
	}
	run, err := q.CreateExtWorkflowRun(ctx, db.CreateExtWorkflowRunParams{
		ID: dbid.NewV7(), WorkspaceID: parent.WorkspaceID, WorkflowID: wf.ID, IssueID: parent.ID,
		TriggeredByType: actorType, TriggeredByID: actorID, Definition: raw,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil // a concurrent StartRun won
		}
		return fmt.Errorf("create run: %w", err)
	}
	out := &outbox{run: run}
	for _, n := range def.Nodes {
		agentID, err := util.ParseUUID(n.AgentID)
		if err != nil {
			return fmt.Errorf("node %q agent: %w", n.Key, err)
		}
		child, err := e.issues.ExtCreateChildIssueTx(ctx, tx, service.IssueCreateParams{
			WorkspaceID:   parent.WorkspaceID,
			Title:         parent.Title + " · " + n.Title,
			Description:   pgtype.Text{String: n.Prompt, Valid: n.Prompt != ""},
			Status:        "backlog",
			Priority:      parent.Priority,
			AssigneeType:  pgtype.Text{String: "agent", Valid: true},
			AssigneeID:    agentID,
			CreatorType:   actorType,
			CreatorID:     actorID,
			ParentIssueID: parent.ID,
			ProjectID:     parent.ProjectID,
		})
		if err != nil {
			return fmt.Errorf("create child issue for %q: %w", n.Key, err)
		}
		if _, err := q.CreateExtWorkflowRunStep(ctx, db.CreateExtWorkflowRunStepParams{
			ID: dbid.NewV7(), RunID: run.ID, WorkspaceID: run.WorkspaceID, NodeKey: n.Key, AgentID: agentID, IssueID: child.ID,
		}); err != nil {
			return fmt.Errorf("create step %q: %w", n.Key, err)
		}
		out.created = append(out.created, createdIssue{issue: child, actorType: actorType, actorID: util.UUIDToString(actorID)})
	}
	if err := e.setIssueStatus(ctx, q, parent.ID, "in_progress", out); err != nil {
		return err
	}
	snap, err := LoadRunSnapshot(ctx, q, run)
	if err != nil {
		return err
	}
	starter := Actor{Type: actorType, ID: actorID}
	if err := e.recordEvent(ctx, q, snap, record(RunEventRunStarted, "", map[string]any{"workflow_id": util.UUIDToString(wf.ID), "steps": len(def.Nodes)}), starter); err != nil {
		return err
	}
	if err := e.milestone(ctx, q, snap, MilestoneRunStarted, "", "", out); err != nil {
		return err
	}
	if err := e.apply(ctx, tx, q, snap, nil, out); err != nil {
		return err
	}
	out.runChanged = true
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	// The run is committed: a client disconnect must not drop its
	// notifications.
	e.flush(context.WithoutCancel(ctx), out)
	return nil
}

// definitionFromRows is the run's snapshot of a workflow and its nodes.
func definitionFromRows(wf db.ExtWorkflow, nodes []db.ExtWorkflowNode) Definition {
	d := Definition{SupervisorAgentID: util.UUIDToString(wf.SupervisorAgentID), MaxRewinds: int(wf.MaxRewinds)}
	for _, n := range nodes {
		d.Nodes = append(d.Nodes, Node{
			Key: n.Key, Title: n.Title, AgentID: util.UUIDToString(n.AgentID), Prompt: n.Prompt,
			RequiresReview: n.RequiresReview, MaxAttempts: int(n.MaxAttempts), DependsOn: append([]string{}, n.DependsOn...),
		})
	}
	return d
}
