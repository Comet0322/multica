package sandboxbroker

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// DefaultDrainTimeout is how long a workspace removed by a reload may keep its
// leased tasks before they are ended.
const DefaultDrainTimeout = 10 * time.Minute

// normalizeWorkspaces validates a workspace list in place: placement defaults
// to the pool's, runtime name to the pool's name, and IDs must be unique.
func (c *PoolConfig) normalizeWorkspaces(list []WorkspaceConfig) error {
	seen := map[string]bool{}
	for i := range list {
		if list[i].Placement == "" {
			list[i].Placement = c.Placement
		}
		if list[i].MaxLeases == 0 {
			list[i].MaxLeases = c.MaxLeases
		}
		if err := list[i].normalize(c.MulticaURL, c.Name); err != nil {
			return err
		}
		id := list[i].WorkspaceID
		if seen[id] {
			return fmt.Errorf("workspace %s is configured twice", id)
		}
		seen[id] = true
	}
	return nil
}

// workspaceList is a snapshot of the served workspaces in rotation order.
// Reload replaces the slice instead of editing it, so the snapshot is safe to
// range over without the lock.
func (p *Pool) workspaceList() []*workspace {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wsOrder
}

// ReloadResult lists the workspace IDs a reload touched.
type ReloadResult struct {
	Added   []string
	Removed []string
	// Updated had its lease cap changed, which applies immediately.
	Updated []string
	// Unapplied changed in a way that needs a restart (PAT, runtime name or
	// placement); the running values stay in effect.
	Unapplied []string
}

// Reload replaces the served workspaces with the given list without touching
// pods or other workspaces' leases.
//
// A new workspace is polled from the next claim on. A removed workspace is no
// longer claimed from at once; its leased tasks run on, and once they have
// ended, or DrainTimeout passed and the leases were ended, its upstream
// connection is closed. The drain runs until ctx ends. An invalid list is
// refused whole and the current configuration stays in effect.
func (p *Pool) Reload(ctx context.Context, workspaces []WorkspaceConfig) (ReloadResult, error) {
	var res ReloadResult
	if len(workspaces) == 0 {
		return res, fmt.Errorf("workspace config lists no workspaces")
	}
	next := slices.Clone(workspaces)
	if err := p.cfg.normalizeWorkspaces(next); err != nil {
		return res, err
	}

	p.mu.Lock()
	order := make([]*workspace, 0, len(next))
	byID := make(map[string]*workspace, len(next))
	for _, wc := range next {
		ws := p.workspaces[wc.WorkspaceID]
		if ws == nil {
			ws = &workspace{cfg: wc, up: newUpstream(p.cfg.MulticaURL, wc.Token, p.identityHeaders)}
			res.Added = append(res.Added, wc.WorkspaceID)
		} else {
			if ws.cfg.Token != wc.Token || ws.cfg.RuntimeName != wc.RuntimeName || ws.cfg.Placement != wc.Placement {
				res.Unapplied = append(res.Unapplied, wc.WorkspaceID)
			}
			if ws.cfg.MaxLeases != wc.MaxLeases {
				ws.cfg.MaxLeases = wc.MaxLeases
				res.Updated = append(res.Updated, wc.WorkspaceID)
			}
		}
		order = append(order, ws)
		byID[wc.WorkspaceID] = ws
	}
	var dropped []*workspace
	for _, ws := range p.wsOrder {
		if byID[ws.cfg.WorkspaceID] == nil {
			ws.removed = true
			dropped = append(dropped, ws)
			res.Removed = append(res.Removed, ws.cfg.WorkspaceID)
		}
	}
	p.workspaces, p.wsOrder = byID, order
	p.rotor %= len(order)
	p.draining = append(p.draining, dropped...)
	p.mu.Unlock()

	for _, ws := range dropped {
		go p.drain(ctx, ws)
	}
	p.log.Info("workspace configuration reloaded",
		"workspaces", len(next), "added", res.Added, "removed", res.Removed,
		"lease_cap_updated", res.Updated, "restart_needed", res.Unapplied)
	return res, nil
}

// workDoneLocked reports whether a removed workspace holds nothing: no lease,
// no claimed task waiting for a pod, and no claim in flight. A pod in the
// middle of claiming may be about to lease a task of any workspace, so it
// counts too. Callers hold p.mu.
func (p *Pool) workDoneLocked(ws *workspace) bool {
	if ws.claiming > 0 || p.leasedLocked(ws) > 0 || p.parkedLocked(ws) > 0 {
		return false
	}
	for _, pd := range p.pods {
		if pd.claiming {
			return false
		}
	}
	return true
}

// drain waits for a removed workspace's work to end, then disconnects it.
func (p *Pool) drain(ctx context.Context, ws *workspace) {
	timeout := time.NewTimer(p.cfg.DrainTimeout)
	defer timeout.Stop()
	tick := time.NewTicker(p.cfg.DrainPoll)
	defer tick.Stop()
	for {
		p.mu.Lock()
		done := p.workDoneLocked(ws)
		p.mu.Unlock()
		if done {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-timeout.C:
			p.endDrainingLeases(ctx, ws)
		}
	}
	ws.up.client.CloseIdleConnections()
	p.mu.Lock()
	p.draining = slices.DeleteFunc(p.draining, func(w *workspace) bool { return w == ws })
	p.mu.Unlock()
	p.log.Info("removed workspace drained and disconnected", "workspace_id", ws.cfg.WorkspaceID)
}

// endDrainingLeases ends what is left of a removed workspace's leases when the
// drain timeout passed, and fails those tasks upstream while the connection
// is still open. Claimed tasks still waiting for a pod lapse through the
// placement wait.
func (p *Pool) endDrainingLeases(ctx context.Context, ws *workspace) {
	var ended []*lease
	p.mu.Lock()
	for _, l := range p.leases {
		if l.ws == ws {
			ended = append(ended, l)
		}
	}
	for _, l := range ended {
		p.endLeaseLocked(l, "workspace removed from the configuration")
	}
	p.mu.Unlock()
	for _, l := range ended {
		p.log.Warn("lease ended: workspace was removed and the drain timeout passed", "task_id", l.TaskID, "workspace_id", ws.cfg.WorkspaceID)
		p.failUpstream(ctx, ws, l.TaskID, "the workspace was removed from the sandbox pool while this task was running")
	}
}
