package sandboxmanager

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultUpstreamBackoff is how long a workspace whose upstream failed is left
// alone before the manager claims from it again.
const DefaultUpstreamBackoff = 10 * time.Second

// upstreamClaimTimeout bounds one claim call so a hanging workspace upstream
// delays the other workspaces' claims by at most this long.
const upstreamClaimTimeout = 10 * time.Second

// WorkspaceConfig is one workspace served by the manager; the manager finds
// them itself from its PAT (see Sync). The pool is shared;
// the workspace is bound to a pod only for the duration of a lease.
type WorkspaceConfig struct {
	WorkspaceID string
	// Token is the PAT the manager acts with in this workspace. It is a secret
	// and never logged.
	Token string
	// RuntimeName is the runtime name members see in this workspace.
	RuntimeName string
	// MaxLeases caps concurrent leases for the workspace; 0 means no cap.
	MaxLeases int
	// Placement is the workspace's placement unit: task, issue or chat. Empty
	// inherits the pool's Placement.
	Placement Placement

	// ManagerID is the upstream daemon_id, derived when empty.
	ManagerID string
}

func (w *WorkspaceConfig) normalize(multicaURL, defaultName string) error {
	if _, err := uuid.Parse(w.WorkspaceID); err != nil {
		return fmt.Errorf("workspace ID must be a UUID")
	}
	w.WorkspaceID = strings.ToLower(w.WorkspaceID)
	if strings.TrimSpace(w.Token) == "" {
		return fmt.Errorf("workspace %s: Multica token is required", w.WorkspaceID)
	}
	if w.MaxLeases < 0 {
		return fmt.Errorf("workspace %s: max leases must not be negative", w.WorkspaceID)
	}
	switch w.Placement {
	case "":
	case PlacementTask, PlacementIssue, PlacementChat:
	default:
		return fmt.Errorf("workspace %s: placement must be task, issue or chat", w.WorkspaceID)
	}
	if w.RuntimeName == "" {
		w.RuntimeName = defaultName
	}
	if w.ManagerID == "" {
		sum := sha256.Sum256([]byte(multicaURL + "\x00" + w.WorkspaceID))
		// The prefix is the id the program registered under before it was
		// renamed; changing it would register a second runtime per workspace.
		w.ManagerID = "sandbox-broker-" + hex.EncodeToString(sum[:])[:24]
	}
	return nil
}

// workspace is the manager's live state for one configured workspace: its own
// upstream connection and runtime registration, so one workspace failing
// cannot touch another.
type workspace struct {
	cfg WorkspaceConfig
	up  *upstream
	// registerMu serializes this workspace's upstream registration.
	registerMu sync.Mutex

	// The fields below are guarded by Pool.mu.
	registration json.RawMessage
	// claiming counts upstream claims in flight, reserved against the quota.
	claiming int
	// backoffUntil skips the workspace after an upstream failure.
	backoffUntil time.Time
	// removed is set when a reload drops the workspace; it is not claimed
	// from while set. A workspace that comes back before its drain ended is
	// served again with this same object, and drainGen tells the drain that
	// was started for an earlier removal to stop.
	removed  bool
	drainGen int
	// goneRuntimes are runtime IDs Multica said no longer exist; a pod that
	// heartbeats one is told so, which makes its daemon register again.
	goneRuntimes map[string]bool
	// shared is set once the registered runtimes were seen with the wanted
	// visibility; until then every Sync tries again.
	shared bool
	// health is the outcome of the latest claim or heartbeat upstream.
	health *UpstreamHealth
}

// UpstreamHealth is the outcome of a workspace's latest call to Multica.
type UpstreamHealth struct {
	OK bool `json:"ok"`
	// Op is the call: "claim" or "heartbeat".
	Op    string    `json:"op"`
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// WorkspaceInfo is one workspace's entry in the inventory.
type WorkspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Leased      int    `json:"leased"`
	// MaxLeases is 0 when the workspace has no cap.
	MaxLeases int `json:"max_leases"`
	// Upstream is the latest claim or heartbeat outcome; nil before the first.
	Upstream *UpstreamHealth `json:"upstream,omitempty"`
}

// healthInfo copies the workspace's upstream health. Callers hold Pool.mu.
func (ws *workspace) healthInfo() *UpstreamHealth {
	if ws.health == nil {
		return nil
	}
	h := *ws.health
	return &h
}

// recordUpstream keeps the outcome of a claim or heartbeat for the inventory.
// The error text is the upstream error's, which carries no response body.
func (p *Pool) recordUpstream(ws *workspace, op string, err error) {
	h := &UpstreamHealth{OK: err == nil, Op: op, At: p.now()}
	if err != nil {
		h.Error = boundText(err.Error(), 300)
	}
	p.mu.Lock()
	ws.health = h
	p.mu.Unlock()
}

// workspaceBusyLocked reports whether the workspace holds anything in this
// manager: a lease, a claimed task waiting for or queued on a pod, or a claim
// in flight. Removed and live objects of the same ID count alike. Callers hold
// p.mu.
func (p *Pool) workspaceBusyLocked(id string) bool {
	for _, l := range p.leases {
		if l.WorkspaceID == id {
			return true
		}
	}
	for _, t := range p.waiting {
		if t.ws.cfg.WorkspaceID == id {
			return true
		}
	}
	for _, pd := range p.pods {
		for _, t := range pd.queue {
			if t.ws.cfg.WorkspaceID == id {
				return true
			}
		}
	}
	if ws := p.workspaces[id]; ws != nil && ws.claiming > 0 {
		return true
	}
	for _, ws := range p.draining {
		if ws.cfg.WorkspaceID == id && ws.claiming > 0 {
			return true
		}
	}
	return false
}

// runtimeGoneLocked reports whether Multica said the runtime no longer exists.
// Callers hold p.mu.
func (p *Pool) runtimeGoneLocked(runtimeID string) bool {
	if runtimeID == "" {
		return false
	}
	for _, ws := range p.workspaces {
		if ws.goneRuntimes[runtimeID] {
			return true
		}
	}
	return false
}

// dropRegistration forgets a workspace's upstream registration after Multica
// answered that one of its runtimes is gone (deleted in the UI, or collected
// after a long time offline). The next pod registration for the workspace
// registers it upstream again; pods heartbeating the gone runtime are told so
// (serveHeartbeat), which is what makes the stock daemon register again.
func (p *Pool) dropRegistration(ws *workspace, runtimeID string) {
	p.mu.Lock()
	if !slices.Contains(p.runtimeIDsLocked(ws), runtimeID) {
		p.mu.Unlock()
		return // already dropped, or registered again since
	}
	if ws.goneRuntimes == nil {
		ws.goneRuntimes = map[string]bool{}
	}
	ws.goneRuntimes[runtimeID] = true
	ws.registration = nil
	ws.shared = false
	p.mu.Unlock()
	p.log.Warn("runtime gone upstream; the workspace registers again with the next pod", "workspace_id", ws.cfg.WorkspaceID, "runtime_id", runtimeID)
	p.notifyWorkspacesChangedLimited()
}

func (p *Pool) workspaceByID(id string) *workspace {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.workspaces[strings.ToLower(id)]
}

// workspaceByRuntime finds the workspace owning an upstream runtime ID.
func (p *Pool) workspaceByRuntime(id string) *workspace {
	for _, ws := range p.workspaceList() {
		for _, rid := range p.runtimeIDs(ws) {
			if rid == id {
				return ws
			}
		}
	}
	return nil
}

// leasedLocked counts the workspace's active leases. Callers hold p.mu.
func (p *Pool) leasedLocked(ws *workspace) int {
	n := 0
	for _, l := range p.leases {
		if l.ws == ws {
			n++
		}
	}
	return n
}

// claimTargets is the ordered list of workspaces a polling pod may be served
// from: the pod must have registered for the workspace in its current boot,
// and the workspace's runtimes must be among those the pod asked for. The
// order starts at the fair-rotation cursor. Callers hold p.mu.
func (p *Pool) claimTargetsLocked(pd *pod, wanted []string) []claimTarget {
	var out []claimTarget
	n := len(p.wsOrder)
	for i := 0; i < n; i++ {
		idx := (p.rotor + i) % n
		ws := p.wsOrder[idx]
		if _, ok := pd.workspaces[ws.cfg.WorkspaceID]; !ok {
			continue
		}
		ids := p.runtimeIDsLocked(ws)
		if len(wanted) > 0 {
			kept := ids[:0:0]
			for _, id := range ids {
				for _, w := range wanted {
					if w == id {
						kept = append(kept, id)
						break
					}
				}
			}
			ids = kept
		}
		if len(ids) > 0 {
			out = append(out, claimTarget{ws: ws, idx: idx, runtimeIDs: ids})
		}
	}
	return out
}

type claimTarget struct {
	ws         *workspace
	idx        int
	runtimeIDs []string
}

// runtimeIDsLocked lists the upstream runtime IDs of a workspace's
// registration. Callers hold p.mu.
func (p *Pool) runtimeIDsLocked(ws *workspace) []string {
	var resp struct {
		Runtimes []runtimeRef `json:"runtimes"`
	}
	_ = json.Unmarshal(ws.registration, &resp)
	ids := make([]string, 0, len(resp.Runtimes))
	for _, rt := range resp.Runtimes {
		ids = append(ids, rt.ID)
	}
	return ids
}

func (p *Pool) runtimeIDs(ws *workspace) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runtimeIDsLocked(ws)
}

// reserveClaim reserves quota for one upstream claim on ws. It reports false
// when the workspace is at its cap or backing off after an upstream failure;
// the task then stays queued in Multica.
func (p *Pool) reserveClaim(ws *workspace) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ws.removed || p.now().Before(ws.backoffUntil) {
		return false
	}
	if max := ws.cfg.MaxLeases; max > 0 && p.leasedLocked(ws)+p.parkedLocked(ws)+ws.claiming >= max {
		return false
	}
	ws.claiming++
	return true
}

// finishClaim releases the reservation and records the upstream's health.
func (p *Pool) finishClaim(ws *workspace, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ws.claiming--
	if failed {
		ws.backoffUntil = p.now().Add(p.cfg.UpstreamBackoff)
	} else {
		ws.backoffUntil = time.Time{}
	}
}

// parkedLocked counts the workspace's claimed tasks that hold no lease yet:
// queued on a pod or waiting for one. They count against the quota.
func (p *Pool) parkedLocked(ws *workspace) int {
	n := 0
	for _, t := range p.waiting {
		if t.ws == ws {
			n++
		}
	}
	for _, pd := range p.pods {
		for _, t := range pd.queue {
			if t.ws == ws {
				n++
			}
		}
	}
	return n
}
