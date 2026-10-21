package handler

import (
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// daemonAuthzMode controls how the per-runtime daemon authorization rule
// (authorizeDaemonRuntime) is applied.
type daemonAuthzMode string

const (
	// daemonAuthzOff skips the rule entirely (pre-fix behavior).
	daemonAuthzOff daemonAuthzMode = "off"
	// daemonAuthzLog evaluates the rule, records denials, and lets the request through.
	daemonAuthzLog daemonAuthzMode = "log"
	// daemonAuthzEnforce rejects requests the rule denies.
	daemonAuthzEnforce daemonAuthzMode = "enforce"
)

// parseDaemonAuthzMode maps the DAEMON_AUTHZ_MODE value to a mode. Anything
// unrecognized, including empty, is log: visible without breaking existing daemons.
func parseDaemonAuthzMode(v string) daemonAuthzMode {
	switch daemonAuthzMode(strings.ToLower(strings.TrimSpace(v))) {
	case daemonAuthzOff:
		return daemonAuthzOff
	case daemonAuthzEnforce:
		return daemonAuthzEnforce
	default:
		return daemonAuthzLog
	}
}

func daemonAuthzModeFromEnv() daemonAuthzMode {
	return parseDaemonAuthzMode(os.Getenv("DAEMON_AUTHZ_MODE"))
}

// daemonRuntimeDenial reports why the caller of a daemon route must not act on
// rt, or ("", "") when it may. Credential is the middleware auth-path label.
//
// Rules:
//   - mdt_ (daemon token): same workspace AND token daemon_id == runtime daemon_id
//     (or the runtime's legacy daemon_id, which names the same machine).
//   - PAT / JWT / mcn_: the requester must be the runtime's owner. A runtime
//     without an owner is denied.
//
// Workspace membership is checked separately by requireDaemonWorkspaceAccess.
func daemonRuntimeDenial(r *http.Request, rt db.AgentRuntime) (credential, reason string) {
	credential = middleware.DaemonAuthPathFromContext(r.Context())

	if wsID := middleware.DaemonWorkspaceIDFromContext(r.Context()); wsID != "" {
		if wsID != uuidToString(rt.WorkspaceID) {
			return credential, "wrong_workspace"
		}
		tokenDaemon := middleware.DaemonIDFromContext(r.Context())
		if tokenDaemon == "" {
			return credential, "wrong_daemon"
		}
		if rt.DaemonID.Valid && rt.DaemonID.String == tokenDaemon {
			return "", ""
		}
		if rt.LegacyDaemonID.Valid && rt.LegacyDaemonID.String == tokenDaemon {
			return "", ""
		}
		return credential, "wrong_daemon"
	}

	if !rt.OwnerID.Valid {
		return credential, "no_owner"
	}
	if requestUserID(r) != uuidToString(rt.OwnerID) {
		return credential, "not_owner"
	}
	return "", ""
}

// daemonRuntimeAllowed applies the rule according to the configured mode and
// reports whether the request may proceed. Denials are always counted and
// logged unless the mode is off.
func (h *Handler) daemonRuntimeAllowed(r *http.Request, rt db.AgentRuntime) bool {
	mode := h.DaemonAuthzMode
	if mode == "" {
		mode = daemonAuthzLog
	}
	if mode == daemonAuthzOff {
		return true
	}
	credential, reason := daemonRuntimeDenial(r, rt)
	if reason == "" {
		return true
	}
	h.Metrics.RecordDaemonAuthzDenied(credential, reason)
	slog.Warn("daemon authz denied",
		"mode", string(mode),
		"credential", credential,
		"reason", reason,
		"runtime_id", uuidToString(rt.ID),
		"workspace_id", uuidToString(rt.WorkspaceID),
		"user_id", requestUserID(r),
		"path", r.URL.Path,
	)
	return mode != daemonAuthzEnforce
}

// requireDaemonRuntimeOwnership is daemonRuntimeAllowed that writes the 404 itself.
func (h *Handler) requireDaemonRuntimeOwnership(w http.ResponseWriter, r *http.Request, rt db.AgentRuntime) bool {
	if h.daemonRuntimeAllowed(r, rt) {
		return true
	}
	writeError(w, http.StatusNotFound, "not found")
	return false
}
