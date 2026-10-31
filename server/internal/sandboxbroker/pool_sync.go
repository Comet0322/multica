package sandboxbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
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

// syncRemoveAfter is how many listings in a row must leave a workspace out
// before it is removed. Multica reads the list from a replica that may lag, so
// one listing short of a workspace is not taken as the account leaving it.
const syncRemoveAfter = 2

// SyncStatus is the outcome of the latest workspace listing.
type SyncStatus struct {
	OK    bool      `json:"ok"`
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// recordSync keeps the outcome of a Sync for the inventory and readiness.
func (p *Pool) recordSync(err error) {
	now := p.now()
	st := &SyncStatus{OK: err == nil, At: now}
	if err != nil {
		st.Error = boundText(err.Error(), 300)
	}
	p.mu.Lock()
	p.lastSync = st
	if err == nil {
		p.lastSyncOK = now
	}
	p.mu.Unlock()
}

// notReady explains why the manager is not ready, or returns "" when it is:
// with discovery on, a listing must have succeeded within three sync
// intervals. That is what turns a revoked or expired PAT, or an unreachable
// Multica, into a visible signal; serving goes on meanwhile.
func (p *Pool) notReady() string {
	if p.cfg.Token == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastSyncOK.IsZero() {
		return "no workspace listing has succeeded yet"
	}
	if p.now().Sub(p.lastSyncOK) > 3*p.cfg.SyncInterval {
		return "the last successful workspace listing is older than three sync intervals"
	}
	return ""
}

// Sync brings the served workspaces in line with the account's memberships.
// Added ones are claimed from at once; left ones drain as a removed workspace
// does, once syncRemoveAfter listings in a row left them out. A failed listing,
// or one that comes back empty, changes nothing: the account losing every
// workspace at once is far likelier to be an error than a decision.
//
// Each pass also retries what may have failed on the way: the visibility of
// registered runtimes, and the hint that sends pods to register a workspace
// that has no registration yet.
func (p *Pool) Sync(ctx context.Context) (ReloadResult, error) {
	if p.cfg.Token == "" {
		return ReloadResult{}, errors.New("workspace discovery needs a Token")
	}
	list, err := p.listWorkspaces(ctx)
	if err != nil {
		p.log.Warn("list workspaces; keeping the current ones", "error", err)
		p.recordSync(err)
		return ReloadResult{}, err
	}
	if len(list) == 0 {
		p.log.Warn("the account belongs to no workspace; keeping the current ones")
		p.recordSync(errors.New("the account belongs to no workspace"))
		return ReloadResult{}, nil
	}
	list = p.debounceRemovals(list)
	res, err := p.Reload(ctx, list)
	p.recordSync(err)
	if err == nil && (len(res.Added) > 0 || len(res.Removed) > 0) {
		p.mu.Lock()
		p.notifiedAt = p.now()
		p.mu.Unlock()
		p.notifyWorkspacesChanged()
	} else if p.unregisteredWithPods() {
		// A pod's registration for the workspace failed (Multica was down,
		// say); the stock daemon would look again only in 30 minutes.
		p.notifyWorkspacesChangedLimited()
	}
	p.reconcileVisibility(ctx)
	return res, err
}

// debounceRemovals adds back to a listing the served workspaces it left out
// for fewer than syncRemoveAfter listings in a row.
func (p *Pool) debounceRemovals(list []WorkspaceConfig) []WorkspaceConfig {
	listed := make(map[string]bool, len(list))
	for _, wc := range list {
		listed[strings.ToLower(wc.WorkspaceID)] = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.missing == nil {
		p.missing = map[string]int{}
	}
	served := make(map[string]bool, len(p.wsOrder))
	for _, ws := range p.wsOrder {
		id := ws.cfg.WorkspaceID
		served[id] = true
		if listed[id] {
			delete(p.missing, id)
			continue
		}
		p.missing[id]++
		if p.missing[id] >= syncRemoveAfter {
			delete(p.missing, id)
			continue
		}
		p.log.Info("workspace missing from the listing; kept until the next one confirms it", "workspace_id", id)
		list = append(list, ws.cfg)
	}
	for id := range p.missing {
		if !served[id] {
			delete(p.missing, id)
		}
	}
	return list
}

// unregisteredWithPods reports whether some served workspace has no upstream
// registration while live pods are in the pool to register it.
func (p *Pool) unregisteredWithPods() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	live := false
	for _, pd := range p.pods {
		if pd.State != PodLost {
			live = true
			break
		}
	}
	if !live {
		return false
	}
	for _, ws := range p.wsOrder {
		if len(ws.registration) == 0 {
			return true
		}
	}
	return false
}

// reconcileVisibility makes sure the registered runtimes have the wanted
// visibility, for registrations whose share call failed. Once a workspace's
// runtimes were seen right it is left alone, so an owner who changes the
// visibility by hand afterwards keeps that choice. Failures are logged and
// retried on the next Sync.
func (p *Pool) reconcileVisibility(ctx context.Context) {
	if p.cfg.Token == "" || p.cfg.RuntimeVisibility != "public" {
		return
	}
	type pending struct {
		ws  *workspace
		ids []string
	}
	var todo []pending
	p.mu.Lock()
	for _, ws := range p.wsOrder {
		if !ws.shared && len(ws.registration) > 0 {
			todo = append(todo, pending{ws, p.runtimeIDsLocked(ws)})
		}
	}
	p.mu.Unlock()
	for _, t := range todo {
		callCtx, cancel := context.WithTimeout(ctx, upstreamCallTimeout)
		var list []struct {
			ID         string `json:"id"`
			Visibility string `json:"visibility"`
		}
		err := t.ws.up.callWith(callCtx, map[string]string{"X-Workspace-ID": t.ws.cfg.WorkspaceID}, http.MethodGet, "/api/runtimes", nil, &list)
		cancel()
		if err != nil {
			p.log.Warn("check pool runtime visibility", "workspace_id", t.ws.cfg.WorkspaceID, "error", err)
			continue
		}
		var wrong []runtimeRef
		for _, rt := range list {
			if slices.Contains(t.ids, rt.ID) && rt.Visibility != p.cfg.RuntimeVisibility {
				wrong = append(wrong, runtimeRef{ID: rt.ID})
			}
		}
		if len(wrong) == 0 {
			p.mu.Lock()
			if slices.Equal(p.runtimeIDsLocked(t.ws), t.ids) {
				t.ws.shared = true
			}
			p.mu.Unlock()
			continue
		}
		p.log.Info("pool runtime not shared yet; sharing again", "workspace_id", t.ws.cfg.WorkspaceID, "runtimes", len(wrong))
		p.shareRuntimes(ctx, t.ws, wrong)
	}
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
// it, which is why the manager does it with the same token. Best effort: a
// failure is retried by every Sync (reconcileVisibility) until the runtimes are
// seen with the wanted visibility.
func (p *Pool) shareRuntimes(ctx context.Context, ws *workspace, runtimes []runtimeRef) {
	if p.cfg.Token == "" || p.cfg.RuntimeVisibility != "public" {
		return
	}
	hdr := map[string]string{"X-Workspace-ID": ws.cfg.WorkspaceID}
	body := json.RawMessage(fmt.Sprintf(`{"visibility":%q}`, p.cfg.RuntimeVisibility))
	for _, rt := range runtimes {
		ctx, cancel := context.WithTimeout(ctx, upstreamCallTimeout)
		err := ws.up.callWith(ctx, hdr, http.MethodPatch, "/api/runtimes/"+rt.ID, body, nil)
		cancel()
		if err != nil {
			p.log.Warn("share pool runtime with the workspace; Sync retries", "workspace_id", ws.cfg.WorkspaceID, "runtime_id", rt.ID, "error", err)
		}
	}
}
