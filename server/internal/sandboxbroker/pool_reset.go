package sandboxbroker

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// controlPath is the control channel pod supervisors poll (GET) and report to
// (POST). Like the rest of the pod API it identifies the caller by source IP,
// so a pod only ever sees and answers its own command.
const controlPath = "/pod/control"

// How a reset ends. A pod comes back in two steps, neither of which needs the
// daemon to cooperate: the supervisor reports the reset done after it started
// the new daemon (the acknowledgement), and the next registration from the
// pod's IP after that is the new daemon's, which completes the reset. A
// registration before the acknowledgement is the old daemon's (or races the
// report) and changes nothing. A daemon that also sends a new boot nonce is
// recognised as a new instance at once.

// startResetLocked marks a pod as resetting and issues a new reset command for
// its supervisor. l is the lease whose end caused the reset, nil when none did.
// Callers hold p.mu.
func (p *Pool) startResetLocked(pd *pod, reason string, l *lease) {
	pd.State = PodResetting
	pd.boundKey, pd.draining = "", false
	pd.dirty = true
	pd.resetID = uuid.NewString()
	pd.resetStarted = p.now()
	pd.resetReported = false
	pd.resetDelivered, pd.earlyReg = false, nil
	pd.resetLease = l
	p.log.Info("pod reset started", "pod_ip", pd.IP, "reset_id", pd.resetID, "reason", reason)
	p.auditReset(pd, AuditAllow, "reset started: "+reason)
}

// completeResetLocked records that a dirty pod's reset finished. The caller
// replaces the pod's entry with a clean one. Callers hold p.mu.
func (p *Pool) completeResetLocked(pd *pod) {
	p.log.Info("pod reset completed", "pod_ip", pd.IP, "reset_id", pd.resetID)
	p.auditReset(pd, AuditAllow, "reset completed: the daemon registered again")
}

// auditReset records a reset event of the pod, tied to the task whose end
// caused it when there was one. Callers hold p.mu.
func (p *Pool) auditReset(pd *pod, decision, reason string) {
	rec := AuditRecord{Action: AuditReset, Decision: decision, Reason: reason, PodIP: pd.IP, PodName: pd.Name}
	if l := pd.resetLease; l != nil {
		rec.TaskID, rec.WorkspaceID, rec.Initiator = l.TaskID, l.WorkspaceID, l.Initiator
	}
	p.audit(rec)
}

// boundText flattens whitespace and cuts s to at most n bytes, for text that
// came from outside and goes into logs and audit records.
func boundText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

// initiatorsOf lists the distinct initiators of the tasks the pod holds, sorted.
func initiatorsOf(pd *pod) []string {
	var out []string
	for _, l := range pd.leases {
		if l.Initiator != "" && !slices.Contains(out, l.Initiator) {
			out = append(out, l.Initiator)
		}
	}
	sort.Strings(out)
	return out
}

// announceWindow is how long a supervisor's last control request keeps its
// address known for the workspace list (see knownSource).
const announceWindow = 10 * time.Minute

// announceLocked records that the supervisor at ip used the control channel.
// Callers hold p.mu.
func (p *Pool) announceLocked(ip string) {
	now := p.now()
	if p.announced == nil {
		p.announced = map[string]time.Time{}
	}
	for addr, at := range p.announced {
		if now.Sub(at) > announceWindow {
			delete(p.announced, addr)
		}
	}
	p.announced[ip] = now
}

// knownSource reports whether ip is a pool pod: registered (in any state), or
// its supervisor used the control channel recently. The supervisor announces
// itself before it starts the daemon, whose first call lists the workspaces.
func (p *Pool) knownSource(ip string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pods[ip] != nil {
		return true
	}
	at, ok := p.announced[ip]
	return ok && p.now().Sub(at) <= announceWindow
}

// serveControl answers a supervisor's poll with its pending reset command, or
// takes its report on one, or a supervisor event.
func (p *Pool) serveControl(w http.ResponseWriter, r *http.Request, ip string) {
	switch r.Method {
	case http.MethodGet:
		p.mu.Lock()
		p.announceLocked(ip)
		p.markLostLocked()
		pd := p.pods[ip]
		var id string
		if pd != nil && pd.State == PodResetting && !pd.resetReported {
			id = pd.resetID
			pd.resetDelivered = true
		}
		p.mu.Unlock()
		if pd == nil {
			http.Error(w, "pod not registered", http.StatusNotFound)
			return
		}
		if id == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "command": "reset"})
	case http.MethodPost:
		var rep struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  string `json:"error"`
			// Event is a supervisor event instead of a command outcome (see
			// podEventLocked).
			Event string `json:"event"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rep); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.announceLocked(ip)
		p.markLostLocked()
		pd := p.pods[ip]
		if rep.Event != "" {
			// A pod the manager does not know holds nothing; the event is
			// acknowledged either way so the supervisor does not resend it.
			if pd != nil {
				p.log.Info("pod supervisor event", "pod_ip", ip, "event", boundText(rep.Event, 40), "state", pd.State)
				p.podEventLocked(pd, rep.Event)
			}
			p.mu.Unlock()
			p.reportForced(r.Context())
			writeJSON(w, http.StatusOK, okBody)
			return
		}
		if pd == nil {
			p.mu.Unlock()
			http.Error(w, "pod not registered", http.StatusNotFound)
			return
		}
		// A report for any other command than the pending one is stale (the
		// pod registered again, or the reset timed out); acknowledge it so the
		// supervisor stops resending, but change nothing.
		if pd.State == PodResetting && rep.ID == pd.resetID {
			switch rep.Status {
			case "done":
				pd.resetReported = true
				if early := pd.earlyReg; early != nil {
					// The new daemon registered before this report; the
					// acknowledgement is what was missing.
					early.LastSeen = p.now()
					p.completeResetLocked(pd)
					p.pods[ip] = early
				}
			case "failed":
				p.log.Warn("pod lost: reset failed", "pod_ip", ip, "reset_id", rep.ID, "error", rep.Error)
				p.auditReset(pd, AuditDeny, "reset failed: "+boundText(rep.Error, 200))
				p.loseLocked(pd)
			}
		}
		p.mu.Unlock()
		writeJSON(w, http.StatusOK, okBody)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
