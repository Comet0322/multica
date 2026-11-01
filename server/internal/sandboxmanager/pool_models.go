package sandboxmanager

import "time"

// A model list request is the picker in the Multica UI asking which models the
// runtime's agent CLI can run. Multica hands it to the runtime in the answer to
// a heartbeat. The runtime is the manager's, but the CLIs are in the pods, whose
// image is the same everywhere, so the manager passes the request to one pod's
// daemon, which asks its CLI and reports to the manager, which relays the
// report upstream with the workspace's account.

const (
	// modelRequestRetry is how long a pod has to report before the request is
	// handed to a pod again.
	modelRequestRetry = 45 * time.Second
	// modelRequestExpiry is when an unanswered request is forgotten; Multica
	// has timed it out by then.
	modelRequestExpiry = 3 * time.Minute
)

// modelRequest is a model list request Multica made for a runtime.
type modelRequest struct {
	id     string
	seen   time.Time
	handed time.Time
	handTo string // source address of the pod it was handed to
}

// noteModelRequest remembers the request the upstream heartbeat brought.
func (p *Pool) noteModelRequest(runtimeID, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.modelReqs == nil {
		p.modelReqs = map[string]*modelRequest{}
	}
	if cur := p.modelReqs[runtimeID]; cur != nil && cur.id == id {
		return
	}
	p.modelReqs[runtimeID] = &modelRequest{id: id, seen: p.now()}
}

// takeModelRequest returns the request id to put into the heartbeat answer for
// the pod at ip, or "" when there is none for it.
func (p *Pool) takeModelRequest(runtimeID, ip string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.modelReqs[runtimeID]
	if r == nil {
		return ""
	}
	now := p.now()
	if now.Sub(r.seen) > modelRequestExpiry {
		delete(p.modelReqs, runtimeID)
		return ""
	}
	if r.handTo != "" && now.Sub(r.handed) < modelRequestRetry {
		return ""
	}
	r.handTo, r.handed = ip, now
	return r.id
}

// modelRequestHeldBy reports whether the request was handed to the pod at ip.
func (p *Pool) modelRequestHeldBy(runtimeID, id, ip string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.modelReqs[runtimeID]
	return r != nil && r.id == id && r.handTo == ip
}

// finishModelRequest forgets the request once Multica has taken the report.
func (p *Pool) finishModelRequest(runtimeID, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.modelReqs[runtimeID]; r != nil && r.id == id {
		delete(p.modelReqs, runtimeID)
	}
}
