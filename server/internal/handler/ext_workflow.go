package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ext-workflow: workflow template CRUD. Run endpoints live in ext_workflow_run.go.

const (
	extWorkflowDefaultMaxRewinds  = 3
	extWorkflowDefaultMaxAttempts = 3
	extWorkflowMaxNameRunes       = 200
)

// ── Response types ──────────────────────────────────────────────────────────

type ExtWorkflowNodeResponse struct {
	ID             string   `json:"id"`
	Key            string   `json:"key"`
	Title          string   `json:"title"`
	AgentID        string   `json:"agent_id"`
	Prompt         string   `json:"prompt"`
	RequiresReview bool     `json:"requires_review"`
	MaxAttempts    int      `json:"max_attempts"`
	DependsOn      []string `json:"depends_on"`
	Position       int      `json:"position"`
}

type ExtWorkflowResponse struct {
	ID                string                    `json:"id"`
	WorkspaceID       string                    `json:"workspace_id"`
	Name              string                    `json:"name"`
	Description       string                    `json:"description"`
	SupervisorAgentID string                    `json:"supervisor_agent_id"`
	MaxRewinds        int                       `json:"max_rewinds"`
	CreatorID         string                    `json:"creator_id"`
	AvatarURL         *string                   `json:"avatar_url"`
	ArchivedAt        *string                   `json:"archived_at"`
	CreatedAt         string                    `json:"created_at"`
	UpdatedAt         string                    `json:"updated_at"`
	NodeCount         int                       `json:"node_count"`
	ActiveRunCount    int                       `json:"active_run_count"`
	LastRunAt         *string                   `json:"last_run_at"`
	Nodes             []ExtWorkflowNodeResponse `json:"nodes,omitempty"`
}

// ── Converters ──────────────────────────────────────────────────────────────

func (h *Handler) extWorkflowToResponse(wf db.ExtWorkflow) ExtWorkflowResponse {
	return ExtWorkflowResponse{
		ID:                uuidToString(wf.ID),
		WorkspaceID:       uuidToString(wf.WorkspaceID),
		Name:              wf.Name,
		Description:       wf.Description,
		SupervisorAgentID: uuidToString(wf.SupervisorAgentID),
		MaxRewinds:        int(wf.MaxRewinds),
		CreatorID:         uuidToString(wf.CreatorID),
		AvatarURL:         h.resolveAvatarURLPtr(textToPtr(wf.AvatarUrl)),
		ArchivedAt:        timestampToPtr(wf.ArchivedAt),
		CreatedAt:         timestampToString(wf.CreatedAt),
		UpdatedAt:         timestampToString(wf.UpdatedAt),
	}
}

func extWorkflowNodeToResponse(n db.ExtWorkflowNode) ExtWorkflowNodeResponse {
	deps := n.DependsOn
	if deps == nil {
		deps = []string{}
	}
	return ExtWorkflowNodeResponse{
		ID:             uuidToString(n.ID),
		Key:            n.Key,
		Title:          n.Title,
		AgentID:        uuidToString(n.AgentID),
		Prompt:         n.Prompt,
		RequiresReview: n.RequiresReview,
		MaxAttempts:    int(n.MaxAttempts),
		DependsOn:      deps,
		Position:       int(n.Position),
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// canManageExtWorkflow mirrors canManageSquad: workspace owner/admin manage
// every workflow; a regular member manages only the workflows they created.
func canManageExtWorkflow(member db.Member, wf db.ExtWorkflow) bool {
	if roleAllowed(member.Role, "owner", "admin") {
		return true
	}
	return uuidToString(wf.CreatorID) == uuidToString(member.UserID)
}

func (h *Handler) loadExtWorkflowInWorkspace(w http.ResponseWriter, r *http.Request) (db.ExtWorkflow, bool) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	wfUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workflow id")
	if !ok {
		return db.ExtWorkflow{}, false
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return db.ExtWorkflow{}, false
	}
	wf, err := h.Queries.GetExtWorkflowInWorkspace(r.Context(), db.GetExtWorkflowInWorkspaceParams{
		ID:          wfUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "workflow not found")
		return db.ExtWorkflow{}, false
	}
	return wf, true
}

func writeExtWorkflowValidationFailed(w http.ResponseWriter, errs []extworkflow.ValidationError) {
	if errs == nil {
		errs = []extworkflow.ValidationError{}
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":  "validation_failed",
		"errors": errs,
	})
}

// extAgentChecker resolves and vets the agents a workflow definition refers to.
// Results are cached per agent id so a definition that reuses one agent across
// many nodes costs one lookup.
type extAgentChecker struct {
	h           *Handler
	ctx         context.Context
	member      db.Member
	workspaceID string
	wsUUID      pgtype.UUID
	cache       map[string]string // agent id -> problem ("" = ok)
}

func (h *Handler) newExtAgentChecker(ctx context.Context, member db.Member, workspaceID string, wsUUID pgtype.UUID) *extAgentChecker {
	return &extAgentChecker{h: h, ctx: ctx, member: member, workspaceID: workspaceID, wsUUID: wsUUID, cache: map[string]string{}}
}

// problem returns "" when the member may wire agentID into a workflow, and a
// user-facing message otherwise. Workspace owner/admin may wire any workspace
// agent; other members only agents they can invoke (memberCanWireAgent).
func (c *extAgentChecker) problem(agentID string) string {
	if msg, ok := c.cache[agentID]; ok {
		return msg
	}
	msg := c.lookup(agentID)
	c.cache[agentID] = msg
	return msg
}

func (c *extAgentChecker) lookup(agentID string) string {
	agentUUID, err := util.ParseUUID(agentID)
	if err != nil {
		return "invalid agent id"
	}
	agent, err := c.h.Queries.GetAgentInWorkspace(c.ctx, db.GetAgentInWorkspaceParams{
		ID:          agentUUID,
		WorkspaceID: c.wsUUID,
	})
	if err != nil {
		return "agent not found in this workspace"
	}
	if agent.ArchivedAt.Valid {
		return "agent is archived"
	}
	if !c.h.memberCanWireAgent(c.ctx, c.member, agent, c.workspaceID) {
		return "you do not have access to this agent"
	}
	return ""
}

// check returns the agent problems of a definition: the supervisor (field
// supervisor_agent_id) and, when withNodes is set, every node agent (field
// agent_id on that node). Blank ids are left to extworkflow.ValidateStructure.
func (c *extAgentChecker) check(d extworkflow.Definition, withNodes bool) []extworkflow.ValidationError {
	var errs []extworkflow.ValidationError
	if d.SupervisorAgentID != "" {
		if msg := c.problem(d.SupervisorAgentID); msg != "" {
			errs = append(errs, extworkflow.ValidationError{Field: "supervisor_agent_id", Message: msg})
		}
	}
	if withNodes {
		for _, n := range d.Nodes {
			if n.AgentID == "" {
				continue
			}
			if msg := c.problem(n.AgentID); msg != "" {
				errs = append(errs, extworkflow.ValidationError{NodeKey: n.Key, Field: "agent_id", Message: msg})
			}
		}
	}
	return errs
}

func (h *Handler) loadExtWorkflowNodes(ctx context.Context, q *db.Queries, workflowID pgtype.UUID) ([]ExtWorkflowNodeResponse, error) {
	rows, err := q.ListExtWorkflowNodes(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	out := make([]ExtWorkflowNodeResponse, len(rows))
	for i, n := range rows {
		out[i] = extWorkflowNodeToResponse(n)
	}
	return out, nil
}

// extWorkflowDetailResponse is the single-workflow shape: the row, its nodes
// and its live counters.
func (h *Handler) extWorkflowDetailResponse(ctx context.Context, wf db.ExtWorkflow) (ExtWorkflowResponse, error) {
	resp := h.extWorkflowToResponse(wf)
	nodes, err := h.loadExtWorkflowNodes(ctx, h.Queries, wf.ID)
	if err != nil {
		return resp, err
	}
	resp.Nodes = nodes
	resp.NodeCount = len(nodes)
	active, err := h.Queries.CountActiveExtWorkflowRuns(ctx, wf.ID)
	if err != nil {
		return resp, err
	}
	resp.ActiveRunCount = int(active)
	return resp, nil
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (h *Handler) ListExtWorkflows(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	rows, err := h.Queries.ListExtWorkflows(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list workflows")
		return
	}
	resp := make([]ExtWorkflowResponse, len(rows))
	for i, row := range rows {
		resp[i] = h.extWorkflowToResponse(db.ExtWorkflow{
			ID:                row.ID,
			WorkspaceID:       row.WorkspaceID,
			Name:              row.Name,
			Description:       row.Description,
			SupervisorAgentID: row.SupervisorAgentID,
			MaxRewinds:        row.MaxRewinds,
			CreatorID:         row.CreatorID,
			AvatarUrl:         row.AvatarUrl,
			ArchivedAt:        row.ArchivedAt,
			ArchivedBy:        row.ArchivedBy,
			CreatedAt:         row.CreatedAt,
			UpdatedAt:         row.UpdatedAt,
		})
		resp[i].NodeCount = int(row.NodeCount)
		resp[i].ActiveRunCount = int(row.ActiveRunCount)
		resp[i].LastRunAt = timestampToPtr(row.LastRunAt)
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": resp})
}

func (h *Handler) CreateExtWorkflow(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	// Any workspace member can create a workflow and becomes its creator;
	// management stays creator-scoped (see canManageExtWorkflow).
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}

	var req struct {
		Name              string `json:"name"`
		Description       string `json:"description"`
		SupervisorAgentID string `json:"supervisor_agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len([]rune(name)) > extWorkflowMaxNameRunes {
		writeError(w, http.StatusBadRequest, "name is too long")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	d := extworkflow.Definition{SupervisorAgentID: req.SupervisorAgentID, MaxRewinds: extWorkflowDefaultMaxRewinds}
	errs := extworkflow.ValidateSettings(d.SupervisorAgentID, d.MaxRewinds)
	if len(errs) == 0 {
		errs = h.newExtAgentChecker(r.Context(), member, workspaceID, wsUUID).check(d, false)
	}
	if len(errs) > 0 {
		writeExtWorkflowValidationFailed(w, errs)
		return
	}
	supervisorUUID, ok := parseUUIDOrBadRequest(w, req.SupervisorAgentID, "supervisor_agent_id")
	if !ok {
		return
	}

	wf, err := h.Queries.CreateExtWorkflow(r.Context(), db.CreateExtWorkflowParams{
		WorkspaceID:       wsUUID,
		Name:              name,
		Description:       req.Description,
		SupervisorAgentID: supervisorUUID,
		MaxRewinds:        extWorkflowDefaultMaxRewinds,
		CreatorID:         member.UserID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create workflow")
		return
	}

	h.publish(protocol.EventExtWorkflowCreated, workspaceID, "member", uuidToString(member.UserID), map[string]any{
		"workflow_id": uuidToString(wf.ID),
	})
	writeJSON(w, http.StatusCreated, h.extWorkflowToResponse(wf))
}

func (h *Handler) GetExtWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := h.loadExtWorkflowInWorkspace(w, r)
	if !ok {
		return
	}
	resp, err := h.extWorkflowDetailResponse(r.Context(), wf)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workflow")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type extWorkflowNodeRequest struct {
	Key            string   `json:"key"`
	Title          string   `json:"title"`
	AgentID        string   `json:"agent_id"`
	Prompt         string   `json:"prompt"`
	RequiresReview bool     `json:"requires_review"`
	MaxAttempts    int      `json:"max_attempts"`
	DependsOn      []string `json:"depends_on"`
}

func (h *Handler) UpdateExtWorkflow(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}
	wf, ok := h.loadExtWorkflowInWorkspace(w, r)
	if !ok {
		return
	}
	if !canManageExtWorkflow(member, wf) {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	var req struct {
		Name              *string                   `json:"name"`
		Description       *string                   `json:"description"`
		SupervisorAgentID *string                   `json:"supervisor_agent_id"`
		MaxRewinds        *int                      `json:"max_rewinds"`
		Nodes             *[]extWorkflowNodeRequest `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	params := db.UpdateExtWorkflowParams{ID: wf.ID}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		if len([]rune(name)) > extWorkflowMaxNameRunes {
			writeError(w, http.StatusBadRequest, "name is too long")
			return
		}
		params.Name = pgtype.Text{String: name, Valid: true}
	}
	if req.Description != nil {
		params.Description = pgtype.Text{String: *req.Description, Valid: true}
	}

	// Effective definition after the update; unspecified fields keep their
	// stored value.
	d := extworkflow.Definition{
		SupervisorAgentID: uuidToString(wf.SupervisorAgentID),
		MaxRewinds:        int(wf.MaxRewinds),
	}
	if req.SupervisorAgentID != nil {
		d.SupervisorAgentID = *req.SupervisorAgentID
	}
	if req.MaxRewinds != nil {
		d.MaxRewinds = *req.MaxRewinds
	}
	if req.Nodes != nil {
		d.Nodes = make([]extworkflow.Node, len(*req.Nodes))
		for i, n := range *req.Nodes {
			// Spec default: an omitted max_attempts means 3.
			if n.MaxAttempts == 0 {
				n.MaxAttempts = extWorkflowDefaultMaxAttempts
			}
			deps := n.DependsOn
			if deps == nil {
				deps = []string{}
			}
			d.Nodes[i] = extworkflow.Node{
				Key: n.Key, Title: n.Title, AgentID: n.AgentID, Prompt: n.Prompt,
				RequiresReview: n.RequiresReview, MaxAttempts: n.MaxAttempts, DependsOn: deps,
			}
		}
	}

	var errs []extworkflow.ValidationError
	if req.Nodes != nil {
		errs = extworkflow.ValidateStructure(d)
	} else {
		errs = extworkflow.ValidateSettings(d.SupervisorAgentID, d.MaxRewinds)
	}
	// Agent checks only for what this request changes, so renaming a workflow
	// is never blocked by a node agent that was later made private.
	if req.SupervisorAgentID != nil || req.Nodes != nil {
		checkDef := extworkflow.Definition{Nodes: d.Nodes}
		if req.SupervisorAgentID != nil {
			checkDef.SupervisorAgentID = d.SupervisorAgentID
		}
		errs = append(errs, h.newExtAgentChecker(r.Context(), member, workspaceID, wsUUID).check(checkDef, req.Nodes != nil)...)
	}
	if len(errs) > 0 {
		writeExtWorkflowValidationFailed(w, errs)
		return
	}
	if req.SupervisorAgentID != nil {
		supervisorUUID, ok := parseUUIDOrBadRequest(w, *req.SupervisorAgentID, "supervisor_agent_id")
		if !ok {
			return
		}
		params.SupervisorAgentID = supervisorUUID
	}
	if req.MaxRewinds != nil {
		params.MaxRewinds = pgtype.Int4{Int32: int32(*req.MaxRewinds), Valid: true}
	}

	// Parse node agent ids before opening the transaction so a malformed id
	// cannot leave it half applied.
	type nodeWrite struct {
		node  extworkflow.Node
		agent pgtype.UUID
	}
	var writes []nodeWrite
	if req.Nodes != nil {
		writes = make([]nodeWrite, len(d.Nodes))
		for i, n := range d.Nodes {
			agentUUID, ok := parseUUIDOrBadRequest(w, n.AgentID, "agent_id")
			if !ok {
				return
			}
			writes[i] = nodeWrite{node: n, agent: agentUUID}
		}
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update workflow")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	// Serialize concurrent saves of the same workflow: the node set is replaced
	// as a whole, so two interleaved replacements could mix their rows.
	locked, err := qtx.LockExtWorkflowForUpdate(r.Context(), db.LockExtWorkflowForUpdateParams{
		ID:          wf.ID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update workflow")
		return
	}
	if locked.ArchivedAt.Valid {
		writeError(w, http.StatusConflict, "workflow is archived")
		return
	}
	if _, err := qtx.UpdateExtWorkflow(r.Context(), params); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update workflow")
		return
	}
	if req.Nodes != nil {
		if err := qtx.DeleteExtWorkflowNodes(r.Context(), wf.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update workflow")
			return
		}
		for i, nw := range writes {
			if _, err := qtx.CreateExtWorkflowNode(r.Context(), db.CreateExtWorkflowNodeParams{
				WorkflowID:     wf.ID,
				WorkspaceID:    wsUUID,
				Key:            nw.node.Key,
				Title:          strings.TrimSpace(nw.node.Title),
				AgentID:        nw.agent,
				Prompt:         nw.node.Prompt,
				RequiresReview: nw.node.RequiresReview,
				MaxAttempts:    int32(nw.node.MaxAttempts),
				DependsOn:      nw.node.DependsOn,
				Position:       int32(i),
			}); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to update workflow")
				return
			}
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update workflow")
		return
	}

	updated, err := h.Queries.GetExtWorkflowInWorkspace(r.Context(), db.GetExtWorkflowInWorkspaceParams{ID: wf.ID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workflow")
		return
	}
	resp, err := h.extWorkflowDetailResponse(r.Context(), updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workflow")
		return
	}
	h.publish(protocol.EventExtWorkflowUpdated, workspaceID, "member", requestUserID(r), map[string]any{
		"workflow_id": uuidToString(wf.ID),
	})
	writeJSON(w, http.StatusOK, resp)
}

// DeleteExtWorkflow archives the workflow. Active runs keep running against
// their snapshot; the workflow just stops accepting new assignments.
func (h *Handler) DeleteExtWorkflow(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}
	wf, ok := h.loadExtWorkflowInWorkspace(w, r)
	if !ok {
		return
	}
	if !canManageExtWorkflow(member, wf) {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return
	}
	if wf.ArchivedAt.Valid {
		writeError(w, http.StatusBadRequest, "workflow is already archived")
		return
	}
	if _, err := h.Queries.ArchiveExtWorkflow(r.Context(), db.ArchiveExtWorkflowParams{
		ID:         wf.ID,
		ArchivedBy: member.UserID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to archive workflow")
		return
	}
	h.publish(protocol.EventExtWorkflowDeleted, workspaceID, "member", uuidToString(member.UserID), map[string]any{
		"workflow_id": uuidToString(wf.ID),
	})
	w.WriteHeader(http.StatusNoContent)
}
