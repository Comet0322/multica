package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Internal API used by the platform's runtime controller (gated by
// middleware.ControllerAuth, not exposed through the public ingress). It
// provisions managed runtime rows, mints and revokes the daemon tokens (mdt_)
// the Sandbox daemons authenticate with, and reports queue state so the
// controller can wake and suspend Sandboxes.

const (
	managedDaemonTokenDefaultTTL = time.Hour
	managedDaemonTokenMaxTTL     = 24 * time.Hour
)

var managedDaemonIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type managedRuntimeRequest struct {
	WorkspaceID string `json:"workspace_id"`
	DaemonID    string `json:"daemon_id"`
	Provider    string `json:"provider"`
	DisplayName string `json:"display_name"`
	// OwnerID is the user the runtime is attributed to (a workspace member).
	OwnerID string `json:"owner_id"`
}

// ControllerUpsertManagedRuntime creates or relabels a managed runtime row.
func (h *Handler) ControllerUpsertManagedRuntime(w http.ResponseWriter, r *http.Request) {
	var req managedRuntimeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, req.WorkspaceID, "workspace_id")
	if !ok {
		return
	}
	ownerUUID, ok := parseUUIDOrBadRequest(w, req.OwnerID, "owner_id")
	if !ok {
		return
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Provider = strings.TrimSpace(req.Provider)
	if !managedDaemonIDPattern.MatchString(req.DaemonID) {
		writeError(w, http.StatusBadRequest, "daemon_id must match [A-Za-z0-9._-], 1-128 chars")
		return
	}
	if req.Provider == "" || req.DisplayName == "" || len(req.DisplayName) > 120 {
		writeError(w, http.StatusBadRequest, "provider and display_name (<=120 chars) are required")
		return
	}
	if _, err := h.getWorkspaceMember(r.Context(), req.OwnerID, req.WorkspaceID); err != nil {
		writeError(w, http.StatusBadRequest, "owner_id is not a member of the workspace")
		return
	}

	rt, err := h.Queries.UpsertManagedAgentRuntime(r.Context(), db.UpsertManagedAgentRuntimeParams{
		WorkspaceID: wsUUID,
		DaemonID:    strToText(req.DaemonID),
		Name:        req.DisplayName,
		Provider:    req.Provider,
		OwnerID:     ownerUUID,
		CustomName:  strToText(req.DisplayName),
	})
	if err != nil {
		if isNotFound(err) {
			// The (workspace, daemon_id, provider) slot is held by a runtime
			// that is not managed; refuse to take it over.
			writeError(w, http.StatusConflict, "daemon_id is already used by an unmanaged runtime")
			return
		}
		slog.Warn("upsert managed runtime failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save managed runtime")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           uuidToString(rt.ID),
		"workspace_id": uuidToString(rt.WorkspaceID),
		"daemon_id":    rt.DaemonID.String,
		"provider":     rt.Provider,
		"status":       rt.Status,
	})
}

type managedDaemonTokenRequest struct {
	WorkspaceID string `json:"workspace_id"`
	DaemonID    string `json:"daemon_id"`
	TTLSeconds  int    `json:"ttl_seconds"`
}

// ControllerIssueDaemonToken mints an mdt_ token bound to one managed daemon.
// The raw token is returned once and is not recoverable.
func (h *Handler) ControllerIssueDaemonToken(w http.ResponseWriter, r *http.Request) {
	var req managedDaemonTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, req.WorkspaceID, "workspace_id")
	if !ok {
		return
	}
	if !managedDaemonIDPattern.MatchString(req.DaemonID) {
		writeError(w, http.StatusBadRequest, "invalid daemon_id")
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = managedDaemonTokenDefaultTTL
	}
	if ttl > managedDaemonTokenMaxTTL {
		writeError(w, http.StatusBadRequest, "ttl_seconds must be <= 86400")
		return
	}
	if _, err := h.Queries.GetManagedAgentRuntimeByDaemon(r.Context(), db.GetManagedAgentRuntimeByDaemonParams{
		WorkspaceID: wsUUID,
		DaemonID:    strToText(req.DaemonID),
	}); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "managed runtime not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load managed runtime")
		return
	}

	raw, err := auth.GenerateDaemonToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	expires := time.Now().Add(ttl)
	if _, err := h.Queries.CreateDaemonToken(r.Context(), db.CreateDaemonTokenParams{
		TokenHash:   auth.HashToken(raw),
		WorkspaceID: wsUUID,
		DaemonID:    req.DaemonID,
		ExpiresAt:   pgtype.Timestamptz{Time: expires, Valid: true},
	}); err != nil {
		slog.Warn("create managed daemon token failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": raw, "expires_at": expires.UTC().Format(time.RFC3339)})
}

// ControllerRevokeDaemonTokens deletes every token for the daemon and drops them
// from the auth cache so they stop working immediately.
func (h *Handler) ControllerRevokeDaemonTokens(w http.ResponseWriter, r *http.Request) {
	var req managedDaemonTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, req.WorkspaceID, "workspace_id")
	if !ok {
		return
	}
	if !managedDaemonIDPattern.MatchString(req.DaemonID) {
		writeError(w, http.StatusBadRequest, "invalid daemon_id")
		return
	}
	hashes, err := h.Queries.DeleteDaemonTokensByWorkspaceAndDaemons(r.Context(), db.DeleteDaemonTokensByWorkspaceAndDaemonsParams{
		WorkspaceID: wsUUID,
		DaemonIds:   []string{req.DaemonID},
	})
	if err != nil {
		slog.Warn("revoke managed daemon tokens failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to revoke tokens")
		return
	}
	for _, hash := range hashes {
		h.DaemonTokenCache.Invalidate(r.Context(), hash)
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": len(hashes)})
}

// ControllerManagedRuntimeState lists every managed runtime with its liveness
// and pending work.
func (h *Handler) ControllerManagedRuntimeState(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Queries.ListManagedRuntimeState(r.Context())
	if err != nil {
		slog.Warn("list managed runtime state failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load state")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := map[string]any{
			"id":           uuidToString(row.ID),
			"workspace_id": uuidToString(row.WorkspaceID),
			"daemon_id":    row.DaemonID.String,
			"provider":     row.Provider,
			"status":       row.Status,
			"display_name": row.CustomName.String,
			"queued_tasks": row.QueuedTasks,
			"active_tasks": row.ActiveTasks,
		}
		if row.LastSeenAt.Valid {
			item["last_seen_at"] = row.LastSeenAt.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtimes": out})
}
