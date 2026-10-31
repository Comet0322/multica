package sandboxbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// DefaultSyncInterval is how often the manager looks for workspaces its account
// joined or left, the stock daemon's own rhythm.
const DefaultSyncInterval = 30 * time.Second

// Discovery. With Token set the manager serves every workspace the token's
// account belongs to, the way a member's daemon does: it lists them, registers
// a local runtime in each, and follows the account as it joins or leaves
// workspaces. Pods learn the same list from the manager, so a new workspace
// needs no configuration anywhere.

// listWorkspaces asks Multica which workspaces the token's account belongs to.
func (p *Pool) listWorkspaces(ctx context.Context) ([]WorkspaceConfig, error) {
	up := newUpstream(p.cfg.MulticaURL, p.cfg.Token, p.identityHeaders)
	var list []struct {
		ID string `json:"id"`
	}
	err := up.call(ctx, http.MethodGet, "/api/daemon/workspaces", nil, &list)
	if statusOf(err) == http.StatusNotFound {
		err = up.call(ctx, http.MethodGet, "/api/workspaces", nil, &list) // an older server
	}
	if err != nil {
		return nil, err
	}
	out := make([]WorkspaceConfig, 0, len(list))
	for _, w := range list {
		out = append(out, WorkspaceConfig{WorkspaceID: w.ID, Token: p.cfg.Token, RuntimeName: p.cfg.Name})
	}
	return out, nil
}

// Sync brings the served workspaces in line with the account's memberships.
// Added ones are claimed from at once; left ones drain as a removed workspace
// does. A failed listing, or one that comes back empty, changes nothing: the
// account losing every workspace at once is far likelier to be an error than a
// decision.
func (p *Pool) Sync(ctx context.Context) (ReloadResult, error) {
	if p.cfg.Token == "" {
		return ReloadResult{}, errors.New("workspace discovery needs a Token")
	}
	list, err := p.listWorkspaces(ctx)
	if err != nil {
		p.log.Warn("list workspaces; keeping the current ones", "error", err)
		return ReloadResult{}, err
	}
	if len(list) == 0 {
		p.log.Warn("the account belongs to no workspace; keeping the current ones")
		return ReloadResult{}, nil
	}
	res, err := p.Reload(ctx, list)
	if err == nil && (len(res.Added) > 0 || len(res.Removed) > 0) {
		p.notifyWorkspacesChanged()
	}
	return res, err
}

// SyncLoop runs Sync every SyncInterval, and whenever now fires, until ctx
// ends.
func (p *Pool) SyncLoop(ctx context.Context, now <-chan struct{}) {
	if p.cfg.Token == "" {
		return
	}
	t := time.NewTicker(p.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-now:
		}
		_, _ = p.Sync(ctx)
	}
}

// shareRuntimes sets the visibility of freshly registered runtimes. A runtime
// belongs to the account the manager runs as and starts out private, so without
// this only that account could bind agents to it. Only the owner may change
// it, which is why the manager does it with the same token. Best effort: on
// failure the runtime stays private and an administrator can share it by hand.
func (p *Pool) shareRuntimes(ctx context.Context, ws *workspace, runtimes []runtimeRef) {
	if p.cfg.Token == "" || p.cfg.RuntimeVisibility != "public" {
		return
	}
	hdr := map[string]string{"X-Workspace-ID": ws.cfg.WorkspaceID}
	body := json.RawMessage(fmt.Sprintf(`{"visibility":%q}`, p.cfg.RuntimeVisibility))
	for _, rt := range runtimes {
		if err := ws.up.callWith(ctx, hdr, http.MethodPatch, "/api/runtimes/"+rt.ID, body, nil); err != nil {
			p.log.Warn("share pool runtime with the workspace", "workspace_id", ws.cfg.WorkspaceID, "runtime_id", rt.ID, "error", err)
		}
	}
}
