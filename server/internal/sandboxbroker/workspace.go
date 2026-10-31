package sandboxbroker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
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

// WorkspaceConfig is one workspace served by the manager. The pool is shared;
// the workspace is bound to a pod only for the duration of a lease.
type WorkspaceConfig struct {
	WorkspaceID string `json:"workspace_id"`
	// Token is the workspace's PAT. It is a secret and never logged.
	Token string `json:"pat"`
	// RuntimeName is the runtime name members see in this workspace.
	RuntimeName string `json:"runtime_name"`
	// MaxLeases caps concurrent leases for the workspace; 0 means no cap.
	MaxLeases int `json:"max_leases"`
	// Placement is the workspace's placement unit: task, issue or chat. Empty
	// inherits the pool's Placement.
	Placement Placement `json:"placement"`

	// BrokerID is the upstream daemon_id, derived when empty.
	BrokerID string `json:"-"`
}

type workspaceFile struct {
	Workspaces []WorkspaceConfig `json:"workspaces"`
}

// ParseWorkspaces reads the workspace entries of the configuration file.
// Unknown fields are rejected, so a misspelled key cannot silently turn into
// a default.
func ParseWorkspaces(r io.Reader) ([]WorkspaceConfig, error) {
	dec := json.NewDecoder(io.LimitReader(r, 1<<20))
	dec.DisallowUnknownFields()
	var f workspaceFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse workspace config: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parse workspace config: trailing data after the JSON document")
	}
	if len(f.Workspaces) == 0 {
		return nil, errors.New("workspace config lists no workspaces")
	}
	return f.Workspaces, nil
}

type loadOptions struct{ groupRead bool }

// LoadOption adjusts LoadWorkspaces.
type LoadOption func(*loadOptions)

// AllowGroupRead accepts a workspace file that its group can also read.
func AllowGroupRead() LoadOption { return func(o *loadOptions) { o.groupRead = true } }

// LoadWorkspaces reads the workspace configuration file. The file holds PATs,
// so it is refused when anyone but its owner can read or write it.
//
// AllowGroupRead relaxes the rule to owner-or-group read-only for volumes whose
// files cannot be owner-only, such as a Kubernetes Secret with fsGroup.
func LoadWorkspaces(path string, opts ...LoadOption) ([]WorkspaceConfig, error) {
	var o loadOptions
	for _, opt := range opts {
		opt(&o)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open workspace config: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat workspace config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("workspace config %s is not a regular file", path)
	}
	forbidden := os.FileMode(0o077)
	if o.groupRead {
		forbidden = 0o037 // group read stays; group write/execute and any other access do not
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&forbidden != 0 {
		return nil, fmt.Errorf("workspace config %s has mode %04o; it holds credentials and must be readable by its owner only (chmod 600)", path, info.Mode().Perm())
	}
	return ParseWorkspaces(f)
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
	if w.BrokerID == "" {
		sum := sha256.Sum256([]byte(multicaURL + "\x00" + w.WorkspaceID))
		w.BrokerID = "sandbox-broker-" + hex.EncodeToString(sum[:])[:24]
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
	// removed is set when a reload drops the workspace; it is never claimed
	// from again.
	removed bool
}

// WorkspaceInfo is one workspace's entry in the inventory.
type WorkspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Leased      int    `json:"leased"`
	// MaxLeases is 0 when the workspace has no cap.
	MaxLeases int `json:"max_leases"`
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
