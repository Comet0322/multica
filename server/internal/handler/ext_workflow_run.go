package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow: run endpoints (spec §7.1). Reads work with the engine off;
// decisions and cancellation need it (409 workflow_engine_disabled).

const (
	extWorkflowRunsDefaultLimit = 50
	extWorkflowRunsMaxLimit     = 200
)

// ── Response types ──────────────────────────────────────────────────────────

type ExtWorkflowRunSummaryResponse struct {
	ID              string  `json:"id"`
	WorkspaceID     string  `json:"workspace_id"`
	WorkflowID      string  `json:"workflow_id"`
	WorkflowName    string  `json:"workflow_name"`
	IssueID         string  `json:"issue_id"`
	IssueIdentifier string  `json:"issue_identifier"`
	IssueTitle      string  `json:"issue_title"`
	TriggeredByType string  `json:"triggered_by_type"`
	TriggeredByID   string  `json:"triggered_by_id"`
	Status          string  `json:"status"`
	RewindsUsed     int     `json:"rewinds_used"`
	MaxRewinds      int     `json:"max_rewinds"`
	StartedAt       string  `json:"started_at"`
	FinishedAt      *string `json:"finished_at"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

type ExtWorkflowStepResponse struct {
	ID               string   `json:"id"`
	NodeKey          string   `json:"node_key"`
	Title            string   `json:"title"`
	AgentID          string   `json:"agent_id"`
	IssueID          string   `json:"issue_id"`
	Status           string   `json:"status"`
	Attempts         int      `json:"attempts"`
	MaxAttempts      int      `json:"max_attempts"`
	RequiresReview   bool     `json:"requires_review"`
	DependsOn        []string `json:"depends_on"`
	PendingReason    *string  `json:"pending_reason"`
	LastFeedback     *string  `json:"last_feedback"`
	EscalationReason *string  `json:"escalation_reason"`
	StartedAt        *string  `json:"started_at"`
	FinishedAt       *string  `json:"finished_at"`
}

type ExtWorkflowRunEventResponse struct {
	ID         string          `json:"id"`
	StepID     *string         `json:"step_id"`
	Kind       string          `json:"kind"`
	ActorType  string          `json:"actor_type"`
	ActorID    *string         `json:"actor_id"`
	OnBehalfOf *string         `json:"on_behalf_of"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  string          `json:"created_at"`
}

type ExtWorkflowRunResponse struct {
	ExtWorkflowRunSummaryResponse
	Steps  []ExtWorkflowStepResponse     `json:"steps"`
	Events []ExtWorkflowRunEventResponse `json:"events"`
}

type ExtWorkflowStepOfResponse struct {
	RunID         string `json:"run_id"`
	StepID        string `json:"step_id"`
	NodeKey       string `json:"node_key"`
	Index         int    `json:"index"` // 1-based position in the definition
	Total         int    `json:"total"`
	ParentIssueID string `json:"parent_issue_id"`
}

// ── Converters ──────────────────────────────────────────────────────────────

func decodeExtRunDefinition(raw []byte) extworkflow.Definition {
	var def extworkflow.Definition
	if err := json.Unmarshal(raw, &def); err != nil {
		slog.Warn("ext-workflow: undecodable run definition", "error", err)
	}
	return def
}

// extRunSummaryToResponse converts a summary row. The by-issue and
// by-workflow list rows have the same fields and convert to this type.
func extRunSummaryToResponse(row db.GetExtWorkflowRunSummaryRow, prefix string) ExtWorkflowRunSummaryResponse {
	identifier := ""
	if row.IssueNumber.Valid && prefix != "" {
		identifier = prefix + "-" + strconv.Itoa(int(row.IssueNumber.Int32))
	}
	return ExtWorkflowRunSummaryResponse{
		ID:              uuidToString(row.ID),
		WorkspaceID:     uuidToString(row.WorkspaceID),
		WorkflowID:      uuidToString(row.WorkflowID),
		WorkflowName:    row.WorkflowName,
		IssueID:         uuidToString(row.IssueID),
		IssueIdentifier: identifier,
		IssueTitle:      row.IssueTitle.String,
		TriggeredByType: row.TriggeredByType,
		TriggeredByID:   uuidToString(row.TriggeredByID),
		Status:          row.Status,
		RewindsUsed:     int(row.RewindsUsed),
		MaxRewinds:      decodeExtRunDefinition(row.Definition).MaxRewinds,
		StartedAt:       timestampToString(row.StartedAt),
		FinishedAt:      timestampToPtr(row.FinishedAt),
		CreatedAt:       timestampToString(row.CreatedAt),
		UpdatedAt:       timestampToString(row.UpdatedAt),
	}
}

func extStepToResponse(s db.ExtWorkflowRunStep, def extworkflow.Definition) ExtWorkflowStepResponse {
	resp := ExtWorkflowStepResponse{
		ID:               uuidToString(s.ID),
		NodeKey:          s.NodeKey,
		Title:            s.NodeKey,
		AgentID:          uuidToString(s.AgentID),
		IssueID:          uuidToString(s.IssueID),
		Status:           s.Status,
		Attempts:         int(s.Attempts),
		MaxAttempts:      1,
		DependsOn:        []string{},
		PendingReason:    textToPtr(s.PendingReason),
		LastFeedback:     textToPtr(s.LastFeedback),
		EscalationReason: textToPtr(s.EscalationReason),
		StartedAt:        timestampToPtr(s.StartedAt),
		FinishedAt:       timestampToPtr(s.FinishedAt),
	}
	if n, ok := def.NodeByKey(s.NodeKey); ok {
		resp.Title = n.Title
		resp.MaxAttempts = n.MaxAttempts
		resp.RequiresReview = n.RequiresReview
		if n.DependsOn != nil {
			resp.DependsOn = n.DependsOn
		}
	}
	return resp
}

func extRunEventToResponse(ev db.ExtWorkflowRunEvent) ExtWorkflowRunEventResponse {
	payload := json.RawMessage(ev.Payload)
	if len(payload) == 0 || string(payload) == "null" {
		payload = json.RawMessage("{}")
	}
	return ExtWorkflowRunEventResponse{
		ID:         uuidToString(ev.ID),
		StepID:     uuidToPtr(ev.StepID),
		Kind:       ev.Kind,
		ActorType:  ev.ActorType,
		ActorID:    uuidToPtr(ev.ActorID),
		OnBehalfOf: uuidToPtr(ev.OnBehalfOf),
		Payload:    payload,
		CreatedAt:  timestampToString(ev.CreatedAt),
	}
}

// extRunResponse loads a run's summary, steps and events. found=false means
// the run is not in this workspace.
func (h *Handler) extRunResponse(ctx context.Context, wsUUID, runID pgtype.UUID) (ExtWorkflowRunResponse, bool, error) {
	row, err := h.Queries.GetExtWorkflowRunSummary(ctx, db.GetExtWorkflowRunSummaryParams{ID: runID, WorkspaceID: wsUUID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ExtWorkflowRunResponse{}, false, nil
	}
	if err != nil {
		return ExtWorkflowRunResponse{}, false, err
	}
	steps, err := h.Queries.ListExtWorkflowRunSteps(ctx, runID)
	if err != nil {
		return ExtWorkflowRunResponse{}, false, err
	}
	events, err := h.Queries.ListExtWorkflowRunEvents(ctx, runID)
	if err != nil {
		return ExtWorkflowRunResponse{}, false, err
	}
	def := decodeExtRunDefinition(row.Definition)
	resp := ExtWorkflowRunResponse{
		ExtWorkflowRunSummaryResponse: extRunSummaryToResponse(row, h.getIssuePrefix(ctx, wsUUID)),
		Steps:                         make([]ExtWorkflowStepResponse, 0, len(steps)),
		Events:                        make([]ExtWorkflowRunEventResponse, 0, len(events)),
	}
	for _, s := range steps {
		resp.Steps = append(resp.Steps, extStepToResponse(s, def))
	}
	for _, ev := range events {
		resp.Events = append(resp.Events, extRunEventToResponse(ev))
	}
	return resp, true, nil
}

// extRunStepOf locates the step whose child issue this is.
func (h *Handler) extRunStepOf(ctx context.Context, wsUUID, issueID pgtype.UUID) (*ExtWorkflowStepOfResponse, error) {
	step, err := h.Queries.GetExtWorkflowRunStepByIssue(ctx, db.GetExtWorkflowRunStepByIssueParams{IssueID: issueID, WorkspaceID: wsUUID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run, err := h.Queries.GetExtWorkflowRunInWorkspace(ctx, db.GetExtWorkflowRunInWorkspaceParams{ID: step.RunID, WorkspaceID: wsUUID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	def := decodeExtRunDefinition(run.Definition)
	index := 0
	for i, n := range def.Nodes {
		if n.Key == step.NodeKey {
			index = i + 1
			break
		}
	}
	return &ExtWorkflowStepOfResponse{
		RunID:         uuidToString(run.ID),
		StepID:        uuidToString(step.ID),
		NodeKey:       step.NodeKey,
		Index:         index,
		Total:         len(def.Nodes),
		ParentIssueID: uuidToString(run.IssueID),
	}, nil
}

// writeExtWorkflowEngineError maps engine errors to HTTP statuses.
func writeExtWorkflowEngineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, extworkflow.ErrEngineDisabled):
		writeError(w, http.StatusConflict, "workflow_engine_disabled")
	case errors.Is(err, extworkflow.ErrRunNotFound):
		writeError(w, http.StatusNotFound, "workflow run not found")
	case errors.Is(err, extworkflow.ErrStepNotFound):
		writeError(w, http.StatusNotFound, "workflow run step not found")
	case errors.Is(err, extworkflow.ErrForbidden):
		writeError(w, http.StatusForbidden, "only the member who started the run, the workflow's creator or a workspace admin can do this")
	case errors.Is(err, extworkflow.ErrStatusMismatch):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, extworkflow.ErrIllegalDecision):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		slog.Warn("ext-workflow: run action failed", "error", err)
		writeError(w, http.StatusInternalServerError, "workflow run action failed")
	}
}

// loadExtRunForMutation resolves the member, the workspace and the run id
// for cancel/decision. Only human actors may call them, and the engine must
// be on.
func (h *Handler) loadExtRunForMutation(w http.ResponseWriter, r *http.Request) (db.Member, pgtype.UUID, pgtype.UUID, bool) {
	if !requireExtHumanActor(w, r) {
		return db.Member{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return db.Member{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	if !h.ExtWorkflow.Enabled() {
		writeError(w, http.StatusConflict, "workflow_engine_disabled")
		return db.Member{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	runID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "run id")
	if !ok {
		return db.Member{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	if _, err := h.Queries.GetExtWorkflowRunInWorkspace(r.Context(), db.GetExtWorkflowRunInWorkspaceParams{ID: runID, WorkspaceID: member.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "workflow run not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to load workflow run")
		}
		return db.Member{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	return member, member.WorkspaceID, runID, true
}

// ── Handlers ────────────────────────────────────────────────────────────────

// ListExtWorkflowRunsForWorkflow: GET /api/ext/workflows/{id}/runs?limit&offset
func (h *Handler) ListExtWorkflowRunsForWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := h.loadExtWorkflowInWorkspace(w, r)
	if !ok {
		return
	}
	limit, offset := extWorkflowRunsDefaultLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = min(n, extWorkflowRunsMaxLimit)
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		offset = n
	}
	rows, err := h.Queries.ListExtWorkflowRunSummariesByWorkflow(r.Context(), db.ListExtWorkflowRunSummariesByWorkflowParams{
		WorkflowID: wf.ID, WorkspaceID: wf.WorkspaceID, PageLimit: int32(limit), PageOffset: int32(offset),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list workflow runs")
		return
	}
	total, err := h.Queries.CountExtWorkflowRunsByWorkflow(r.Context(), db.CountExtWorkflowRunsByWorkflowParams{WorkflowID: wf.ID, WorkspaceID: wf.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count workflow runs")
		return
	}
	prefix := h.getIssuePrefix(r.Context(), wf.WorkspaceID)
	runs := make([]ExtWorkflowRunSummaryResponse, 0, len(rows))
	for _, row := range rows {
		runs = append(runs, extRunSummaryToResponse(db.GetExtWorkflowRunSummaryRow(row), prefix))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "total": total})
}

// ListExtWorkflowRunsForIssue: GET /api/ext/workflow-runs?issue_id= — the
// runs of a parent issue, newest first, and step_of when the issue is a
// workflow step's child issue.
func (h *Handler) ListExtWorkflowRunsForIssue(w http.ResponseWriter, r *http.Request) {
	issueParam := r.URL.Query().Get("issue_id")
	if issueParam == "" {
		writeError(w, http.StatusBadRequest, "issue_id is required")
		return
	}
	issue, ok := h.loadIssueForUser(w, r, issueParam)
	if !ok {
		return
	}
	rows, err := h.Queries.ListExtWorkflowRunSummariesByIssue(r.Context(), db.ListExtWorkflowRunSummariesByIssueParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list workflow runs")
		return
	}
	prefix := h.getIssuePrefix(r.Context(), issue.WorkspaceID)
	runs := make([]ExtWorkflowRunSummaryResponse, 0, len(rows))
	for _, row := range rows {
		runs = append(runs, extRunSummaryToResponse(db.GetExtWorkflowRunSummaryRow(row), prefix))
	}
	stepOf, err := h.extRunStepOf(r.Context(), issue.WorkspaceID, issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workflow step")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "step_of": stepOf})
}

// GetExtWorkflowRun: GET /api/ext/workflow-runs/{id}
func (h *Handler) GetExtWorkflowRun(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceIDFromURL(r, "workspaceId"), "workspace_id")
	if !ok {
		return
	}
	runID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "run id")
	if !ok {
		return
	}
	resp, found, err := h.extRunResponse(r.Context(), wsUUID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workflow run")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "workflow run not found")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// CancelExtWorkflowRun: POST /api/ext/workflow-runs/{id}/cancel
func (h *Handler) CancelExtWorkflowRun(w http.ResponseWriter, r *http.Request) {
	member, _, runID, ok := h.loadExtRunForMutation(w, r)
	if !ok {
		return
	}
	if err := h.ExtWorkflow.CancelRun(r.Context(), runID, "member", member.UserID); err != nil {
		writeExtWorkflowEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type extWorkflowDecisionRequest struct {
	Action         string `json:"action"`
	To             string `json:"to"`
	Reason         string `json:"reason"`
	Feedback       string `json:"feedback"`
	ExpectedStatus string `json:"expected_status"`
}

// DecideExtWorkflowStep: POST /api/ext/workflow-runs/{id}/steps/{stepId}/decision
// answers the run after the decision.
func (h *Handler) DecideExtWorkflowStep(w http.ResponseWriter, r *http.Request) {
	member, wsUUID, runID, ok := h.loadExtRunForMutation(w, r)
	if !ok {
		return
	}
	stepID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "stepId"), "step id")
	if !ok {
		return
	}
	var req extWorkflowDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	err := h.ExtWorkflow.Decide(r.Context(), extworkflow.DecideInput{
		RunID:  runID,
		StepID: stepID,
		Decision: extworkflow.Decision{
			Action: extworkflow.DecisionAction(req.Action), To: req.To, Reason: req.Reason, Feedback: req.Feedback,
		},
		ActorType:      "member",
		ActorID:        member.UserID,
		ExpectedStatus: extworkflow.StepStatus(req.ExpectedStatus),
	})
	if err != nil {
		writeExtWorkflowEngineError(w, err)
		return
	}
	resp, found, err := h.extRunResponse(r.Context(), wsUUID, runID)
	writeExtDecisionResult(w, runID, resp, found, err)
}

// writeExtDecisionResult answers a decision that was already applied: the run
// when it reloads, else 204, never a failure the client would retry against a
// decision that no longer applies.
func writeExtDecisionResult(w http.ResponseWriter, runID pgtype.UUID, resp ExtWorkflowRunResponse, found bool, err error) {
	if err != nil || !found {
		slog.Warn("ext-workflow: decision applied but the run could not be reloaded",
			"run_id", uuidToString(runID), "found", found, "error", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
