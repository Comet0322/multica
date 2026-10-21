package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Platform-admin API: the only place the implementation of managed runtimes
// (daemon id, sandbox state, device info) is visible. Access is an email
// allowlist from PLATFORM_ADMIN_EMAILS, not a workspace role: operating the
// runtime fleet is a platform concern that cuts across workspaces.

// platformAdminEmails parses a comma-separated allowlist, lower-cased.
func platformAdminEmails(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, e := range strings.Split(raw, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out[e] = struct{}{}
		}
	}
	return out
}

// isPlatformAdmin reports whether the authenticated human is on the allowlist.
// Machine credentials never qualify. The email comes from the user row, not a
// request header, so it cannot drift from the account.
func (h *Handler) isPlatformAdmin(r *http.Request) bool {
	if isMachineCredentialActor(r) {
		return false
	}
	allow := platformAdminEmails(os.Getenv("PLATFORM_ADMIN_EMAILS"))
	if len(allow) == 0 {
		return false
	}
	uid, err := parseUUIDStrict(requestUserID(r))
	if err != nil {
		return false
	}
	user, err := h.Queries.GetUser(r.Context(), uid)
	if err != nil {
		return false
	}
	_, ok := allow[strings.ToLower(strings.TrimSpace(user.Email))]
	return ok
}

// RequirePlatformAdmin gates the admin routes. Non-admins get 404 so the
// surface is not discoverable.
func (h *Handler) RequirePlatformAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.isPlatformAdmin(r) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GetPlatformAdminStatus lets the web app decide whether to show the admin
// entry. It is open to any authenticated user and only returns a boolean.
func (h *Handler) GetPlatformAdminStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"is_admin": h.isPlatformAdmin(r)})
}

type adminManagedRuntimeResponse struct {
	ID             string  `json:"id"`
	WorkspaceID    string  `json:"workspace_id"`
	WorkspaceName  string  `json:"workspace_name"`
	WorkspaceSlug  string  `json:"workspace_slug"`
	DaemonID       *string `json:"daemon_id"`
	DisplayName    string  `json:"display_name"`
	Provider       string  `json:"provider"`
	Status         string  `json:"status"`
	ManagedState   string  `json:"managed_state"`
	ManagedStateAt *string `json:"managed_state_at"`
	LastSeenAt     *string `json:"last_seen_at"`
	DeviceInfo     string  `json:"device_info"`
	Metadata       any     `json:"metadata"`
	OwnerID        *string `json:"owner_id"`
	QueuedTasks    int64   `json:"queued_tasks"`
	ActiveTasks    int64   `json:"active_tasks"`
	AgentCount     int64   `json:"agent_count"`
	CreatedAt      string  `json:"created_at"`
}

// ListPlatformManagedRuntimes returns every managed runtime, unredacted.
func (h *Handler) ListPlatformManagedRuntimes(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Queries.ListManagedRuntimesForAdmin(r.Context())
	if err != nil {
		slog.Warn("list managed runtimes for admin failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list managed runtimes")
		return
	}
	out := make([]adminManagedRuntimeResponse, 0, len(rows))
	for _, row := range rows {
		base := runtimeToResponse(db.AgentRuntime{
			ID: row.ID, WorkspaceID: row.WorkspaceID, DaemonID: row.DaemonID,
			Name: row.Name, CustomName: row.CustomName, Provider: row.Provider,
			Status: row.Status, DeviceInfo: row.DeviceInfo, Metadata: row.Metadata,
			OwnerID: row.OwnerID, LastSeenAt: row.LastSeenAt, CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
		})
		item := adminManagedRuntimeResponse{
			ID:            base.ID,
			WorkspaceID:   base.WorkspaceID,
			WorkspaceName: row.WorkspaceName,
			WorkspaceSlug: row.WorkspaceSlug,
			DaemonID:      base.DaemonID,
			DisplayName:   row.Name,
			Provider:      row.Provider,
			Status:        row.Status,
			ManagedState:  row.ManagedState,
			LastSeenAt:    base.LastSeenAt,
			DeviceInfo:    row.DeviceInfo,
			Metadata:      base.Metadata,
			OwnerID:       base.OwnerID,
			QueuedTasks:   row.QueuedTasks,
			ActiveTasks:   row.ActiveTasks,
			AgentCount:    row.AgentCount,
			CreatedAt:     base.CreatedAt,
		}
		if row.CustomName.Valid {
			item.DisplayName = row.CustomName.String
		}
		if row.ManagedStateAt.Valid {
			s := row.ManagedStateAt.Time.UTC().Format(time.RFC3339)
			item.ManagedStateAt = &s
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtimes": out})
}

// CreatePlatformManagedRuntime registers a managed runtime. The controller
// notices the new row and provisions its Sandbox.
func (h *Handler) CreatePlatformManagedRuntime(w http.ResponseWriter, r *http.Request) {
	h.ControllerUpsertManagedRuntime(w, r)
}

// loadManagedRuntimeForAdmin resolves {runtimeId} and refuses non-managed rows,
// so the admin API can never act on a user's own runtime.
func (h *Handler) loadManagedRuntimeForAdmin(w http.ResponseWriter, r *http.Request) (db.AgentRuntime, bool) {
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runtimeId"), "runtime_id")
	if !ok {
		return db.AgentRuntime{}, false
	}
	rt, err := h.Queries.GetAgentRuntime(r.Context(), id)
	if err != nil || !rt.Managed {
		writeError(w, http.StatusNotFound, "runtime not found")
		return db.AgentRuntime{}, false
	}
	return rt, true
}

// RevokePlatformManagedRuntimeTokens drops every daemon token of the runtime.
// The controller re-issues one on its next reconcile if the Sandbox should run.
func (h *Handler) RevokePlatformManagedRuntimeTokens(w http.ResponseWriter, r *http.Request) {
	rt, ok := h.loadManagedRuntimeForAdmin(w, r)
	if !ok {
		return
	}
	hashes, err := h.Queries.DeleteDaemonTokensByWorkspaceAndDaemons(r.Context(), db.DeleteDaemonTokensByWorkspaceAndDaemonsParams{
		WorkspaceID: rt.WorkspaceID,
		DaemonIds:   []string{rt.DaemonID.String},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke tokens")
		return
	}
	for _, hash := range hashes {
		h.DaemonTokenCache.Invalidate(r.Context(), hash)
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": len(hashes)})
}

// DeletePlatformManagedRuntime removes the runtime: agents bound to it are
// unbound (they keep their configuration), in-flight tasks are cancelled, and
// its tokens are revoked. The controller deletes the orphaned Sandbox.
func (h *Handler) DeletePlatformManagedRuntime(w http.ResponseWriter, r *http.Request) {
	rt, ok := h.loadManagedRuntimeForAdmin(w, r)
	if !ok {
		return
	}
	userID := requestUserID(r)

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	if _, err := qtx.LockAgentRuntime(r.Context(), rt.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock runtime")
		return
	}
	if _, err := qtx.ListUserAgentsByRuntimeForUpdate(r.Context(), rt.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock runtime dependencies")
		return
	}
	teardown, err := service.TeardownRuntime(r.Context(), qtx, rt.ID, service.RuntimeTeardownOptions{CancelNonTerminalTasks: true})
	if err != nil {
		if errors.Is(err, service.ErrRuntimeNotDrained) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "the runtime still has tasks in flight; retry in a moment.",
				"code":  "runtime_delete_not_drained",
			})
			return
		}
		slog.Error("managed runtime teardown failed", "runtime_id", uuidToString(rt.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to unbind agents")
		return
	}
	if err := qtx.DeleteAgentRuntime(r.Context(), rt.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete runtime")
		return
	}
	hashes, err := qtx.DeleteDaemonTokensByWorkspaceAndDaemons(r.Context(), db.DeleteDaemonTokensByWorkspaceAndDaemonsParams{
		WorkspaceID: rt.WorkspaceID,
		DaemonIds:   []string{rt.DaemonID.String},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke tokens")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit transaction")
		return
	}
	for _, hash := range hashes {
		h.DaemonTokenCache.Invalidate(r.Context(), hash)
	}
	h.NotifyRuntimeGone(uuidToString(rt.ID))
	h.publishRuntimeTeardown(r.Context(), teardown, uuidToString(rt.WorkspaceID), userID)

	slog.Info("managed runtime deleted by platform admin",
		"runtime_id", uuidToString(rt.ID), "deleted_by", userID,
		"agents_unbound", len(teardown.UnboundAgents), "tasks_cancelled", len(teardown.CancelledTasks))
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"agents_unbound":  len(teardown.UnboundAgents),
		"tasks_cancelled": len(teardown.CancelledTasks),
	})
}
