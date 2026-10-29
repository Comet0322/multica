package sandboxbroker

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/multica-ai/multica/server/internal/issuestatus"
)

const cleanupCheckInterval = time.Minute

// cleanup deletes the sandboxes, volumes included, whose scope is over: a
// task sandbox once its task ends, an issue/chat/autopilot sandbox once the
// scope is finished for CleanupGrace (the daemon's own GC rules), and any
// sandbox unused for VolumeTTL.
func (b *Broker) cleanup(ctx context.Context) {
	type candidate struct {
		key     string
		kind    scopeKind
		scopeID string
		idle    bool
	}
	var candidates []candidate
	b.mu.Lock()
	now := b.now()
	busy := map[string]bool{}
	for _, t := range b.state.Tasks {
		busy[t.Key] = true
	}
	for key, sb := range b.state.Sandboxes {
		if key == controlKey || busy[key] {
			continue
		}
		if sb.LastUsed.IsZero() {
			// Entries from before usage tracking start their TTL now.
			sb.LastUsed = now
		}
		if sb.ScopeKind != scopeTask && now.Sub(sb.CleanupCheckedAt) < cleanupCheckInterval {
			continue
		}
		sb.CleanupCheckedAt = now
		candidates = append(candidates, candidate{key, sb.ScopeKind, sb.ScopeID, now.Sub(sb.LastUsed) >= b.cfg.VolumeTTL})
	}
	b.mu.Unlock()

	for _, c := range candidates {
		reason := "unused for " + b.cfg.VolumeTTL.String()
		if !c.idle {
			finished, why := b.scopeFinished(ctx, c.kind, c.scopeID)
			if !finished {
				continue
			}
			reason = why
		}
		b.deleteSandbox(ctx, c.key, reason)
	}
}

func (b *Broker) scopeFinished(ctx context.Context, kind scopeKind, id string) (bool, string) {
	grace := func(at time.Time) bool { return b.now().Sub(at) >= b.cfg.CleanupGrace }
	check := func(path string, out any) (gone bool, ok bool) {
		err := b.up.call(ctx, http.MethodGet, path, nil, out)
		if statusOf(err) == http.StatusNotFound {
			return true, true
		}
		if err != nil {
			b.log.WarnContext(ctx, "check sandbox scope", "kind", kind, "error", err)
			return false, false
		}
		return false, true
	}
	switch kind {
	case scopeTask:
		return true, "task finished"
	case scopeIssue:
		var s struct {
			Status    string    `json:"status"`
			Category  string    `json:"category"`
			UpdatedAt time.Time `json:"updated_at"`
		}
		gone, ok := check("/api/daemon/issues/"+url.PathEscape(id)+"/gc-check", &s)
		if gone {
			return true, "issue deleted"
		}
		terminal := s.Status == issuestatus.Done || s.Status == issuestatus.Cancelled
		if issuestatus.IsCategory(s.Category) {
			terminal = s.Category == issuestatus.CategoryDone || s.Category == issuestatus.CategoryClosed
		}
		if ok && terminal && grace(s.UpdatedAt) {
			return true, "issue " + s.Status
		}
	case scopeChat:
		var s struct {
			Status    string    `json:"status"`
			UpdatedAt time.Time `json:"updated_at"`
		}
		gone, ok := check("/api/daemon/chat-sessions/"+url.PathEscape(id)+"/gc-check", &s)
		if gone {
			return true, "chat session deleted"
		}
		if ok && s.Status == "archived" && grace(s.UpdatedAt) {
			return true, "chat session archived"
		}
	case scopeAutopilot:
		var s struct {
			Status      string    `json:"status"`
			CompletedAt time.Time `json:"completed_at"`
		}
		gone, ok := check("/api/daemon/autopilot-runs/"+url.PathEscape(id)+"/gc-check", &s)
		if gone {
			return true, "autopilot run deleted"
		}
		if ok && (s.Status == "completed" || s.Status == "failed" || s.Status == "cancelled") && grace(s.CompletedAt) {
			return true, "autopilot run " + s.Status
		}
	}
	return false, ""
}

func (b *Broker) deleteSandbox(ctx context.Context, key, reason string) {
	if err := b.provider.Delete(ctx, key); err != nil {
		b.log.WarnContext(ctx, "delete sandbox", "sandbox", key, "error", err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if sb := b.state.Sandboxes[key]; sb != nil {
		delete(b.tokens, sb.Token)
		delete(b.state.Sandboxes, key)
		b.persistLocked()
	}
	b.log.InfoContext(ctx, "deleted sandbox and volume", "sandbox", key, "reason", reason)
}
