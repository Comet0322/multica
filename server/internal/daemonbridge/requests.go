package daemonbridge

import (
	"context"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var reportBackoffs = []time.Duration{0, 500 * time.Millisecond, 2 * time.Second, 4 * time.Second}

// retry runs fn on the daemon-result schedule: the server waits about a minute
// for an answer, so a transient failure must not drop it.
func retry(ctx context.Context, fn func(context.Context) error) error {
	var err error
	for _, wait := range reportBackoffs {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if err = fn(ctx); err == nil {
			return nil
		}
	}
	return err
}

// serveRequests answers what the server asked for in a heartbeat ack. Updating
// a CLI and reading local skills have no meaning for a backend that is not a
// local install, so they are answered honestly rather than ignored, which would
// leave the request spinning until the server times it out.
func (b *Bridge) serveRequests(ctx context.Context, runtimeID string, ack *daemon.HeartbeatResponse) {
	if p := ack.PendingModelList; p != nil {
		go b.reportModels(ctx, runtimeID, p.ID)
	}
	if p := ack.PendingLocalSkills; p != nil {
		go func() {
			_ = retry(ctx, func(ctx context.Context) error {
				return b.client.ReportLocalSkillListResult(ctx, runtimeID, p.ID, map[string]any{
					"status": "completed", "skills": []any{}, "supported": false,
					"mcp_servers": []any{}, "mcp_supported": false,
				})
			})
		}()
	}
	imports := ack.PendingLocalSkillImports
	if ack.PendingLocalSkillImport != nil {
		imports = append(imports, *ack.PendingLocalSkillImport)
	}
	for _, p := range imports {
		go func(p protocol.DaemonHeartbeatPendingLocalSkillImport) {
			_ = retry(ctx, func(ctx context.Context) error {
				return b.client.ReportLocalSkillImportResult(ctx, runtimeID, p.ID, map[string]any{
					"status": "failed",
					"error":  "this runtime is not a local install and has no local skills to import",
				})
			})
		}(p)
	}
	if p := ack.PendingUpdate; p != nil {
		go func() {
			_ = retry(ctx, func(ctx context.Context) error {
				return b.client.ReportUpdateResult(ctx, runtimeID, p.ID, map[string]any{
					"status": "failed",
					"error":  "this runtime is managed by the bridge and cannot be updated from here",
				})
			})
		}()
	}
}

func (b *Bridge) reportModels(ctx context.Context, runtimeID, requestID string) {
	caps := b.backend.Capabilities()
	models, err := b.backend.Models(ctx)
	if err != nil {
		_ = retry(ctx, func(ctx context.Context) error {
			return b.client.ReportModelListResult(ctx, runtimeID, requestID, map[string]any{
				"status": "failed", "error": err.Error(),
			})
		})
		return
	}
	type wire struct {
		ID       string `json:"id"`
		Label    string `json:"label"`
		Provider string `json:"provider,omitempty"`
		Default  bool   `json:"default,omitempty"`
	}
	out := make([]wire, 0, len(models))
	for _, m := range models {
		out = append(out, wire{ID: m.ID, Label: m.Label, Provider: m.Provider, Default: m.Default})
	}
	_ = retry(ctx, func(ctx context.Context) error {
		return b.client.ReportModelListResult(ctx, runtimeID, requestID, map[string]any{
			"status":             "completed",
			"models":             out,
			"supported":          caps.Model,
			"unavailable_models": []any{},
		})
	})
}
