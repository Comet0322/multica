package sandboxbroker

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
)

// controlPath is the control channel pod supervisors poll (GET) and report to
// (POST). Like the rest of the pod API it identifies the caller by source IP,
// so a pod only ever sees and answers its own command.
const controlPath = "/pod/control"

// startResetLocked marks a pod as resetting and issues a new reset command for
// its supervisor. Callers hold p.mu.
func (p *Pool) startResetLocked(pd *pod) {
	pd.State = PodResetting
	pd.boundKey, pd.draining = "", false
	pd.dirty = true
	pd.resetID = uuid.NewString()
	pd.resetStarted = p.now()
	pd.resetReported = false
	p.log.Info("pod reset started", "pod_ip", pd.IP, "reset_id", pd.resetID)
}

// serveControl answers a supervisor's poll with its pending reset command, or
// takes its report on one.
func (p *Pool) serveControl(w http.ResponseWriter, r *http.Request, ip string) {
	switch r.Method {
	case http.MethodGet:
		p.mu.Lock()
		p.markLostLocked()
		pd := p.pods[ip]
		var id string
		if pd != nil && pd.State == PodResetting && !pd.resetReported {
			id = pd.resetID
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
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rep); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.markLostLocked()
		pd := p.pods[ip]
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
			case "failed":
				pd.State = PodLost
				p.log.Warn("pod lost: reset failed", "pod_ip", ip, "reset_id", rep.ID, "error", rep.Error)
			}
		}
		p.mu.Unlock()
		writeJSON(w, http.StatusOK, okBody)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
