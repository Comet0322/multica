package sandboxbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Wake sockets. The stock daemon in a pod polls for tasks every 30 seconds
// and also opens a websocket on which the server can tell it a task is
// available. The manager serves that socket and uses it to dispatch: on its own
// timer, while a pod with a socket is free, it claims a task upstream, places
// it as a poll would, and sends the target pod the same hint Multica would, so
// the pod claims at once. Polling stays as the fallback and the health signal:
// a pod without a socket, or a lost hint, is served by its next poll.
//
// The manager never acknowledges the daemon's websocket heartbeats, so the
// daemon keeps claiming over HTTP at its normal polling interval.

const (
	// wakePingEvery keeps the socket alive and detects a pod that vanished.
	wakePingEvery = 20 * time.Second
	wakeWriteWait = 10 * time.Second
	// wakeReadLimit bounds the frames a pod may send; the daemon only sends
	// small heartbeats, which are ignored.
	wakeReadLimit = 1 << 20
	// wakeSendBuffer is how many hints may queue for one pod. A hint carries
	// no content, so extras are dropped.
	wakeSendBuffer = 2
)

var wakeUpgrader = websocket.Upgrader{
	// Pods are identified by source address and are not browsers.
	CheckOrigin: func(*http.Request) bool { return true },
}

// wakeFrame is the content-free hint the daemon reacts to by claiming.
var wakeFrame = func() []byte {
	payload, _ := json.Marshal(protocol.TaskAvailablePayload{})
	frame, _ := json.Marshal(protocol.Message{Type: protocol.EventDaemonTaskAvailable, Payload: payload})
	return frame
}()

// wakeConn is one pod's open socket.
type wakeConn struct {
	conn *websocket.Conn
	send chan struct{}
}

// serveWake upgrades a registered pod's connection and keeps it until either
// side closes. The daemon retries with backoff, so one that connects before it
// registered succeeds a little later.
func (p *Pool) serveWake(w http.ResponseWriter, r *http.Request, ip string) {
	if p.cfg.WakeInterval <= 0 {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	pd := p.pods[ip]
	p.mu.Unlock()
	if pd == nil {
		http.Error(w, "pod not registered", http.StatusNotFound)
		return
	}
	conn, err := wakeUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already answered
	}
	wc := &wakeConn{conn: conn, send: make(chan struct{}, wakeSendBuffer)}
	p.wakeMu.Lock()
	if old := p.wakers[ip]; old != nil {
		_ = old.conn.Close() // the pod reconnected; the old socket is stale
	}
	p.wakers[ip] = wc
	p.wakeMu.Unlock()
	defer func() {
		p.wakeMu.Lock()
		if p.wakers[ip] == wc {
			delete(p.wakers, ip)
		}
		p.wakeMu.Unlock()
		_ = conn.Close()
	}()

	conn.SetReadLimit(wakeReadLimit)
	const readDeadline = 3 * wakePingEvery
	_ = conn.SetReadDeadline(time.Now().Add(readDeadline))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(readDeadline)) })
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			// The daemon's heartbeats are ignored: the HTTP heartbeat stays
			// the liveness signal.
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(readDeadline))
		}
	}()
	p.log.Info("pod opened its wake socket", "pod_ip", ip)

	ping := time.NewTicker(wakePingEvery)
	defer ping.Stop()
	for {
		select {
		case <-wc.send:
			_ = conn.SetWriteDeadline(time.Now().Add(wakeWriteWait))
			if err := conn.WriteMessage(websocket.TextMessage, wakeFrame); err != nil {
				return
			}
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wakeWriteWait)); err != nil {
				return
			}
		case <-readDone:
			return
		}
	}
}

// wakePod tells the pod a task is waiting for it. It never blocks, and does
// nothing for a pod without a socket (its next poll finds the task).
func (p *Pool) wakePod(ip string) {
	p.wakeMu.Lock()
	wc := p.wakers[ip]
	p.wakeMu.Unlock()
	if wc == nil {
		return
	}
	select {
	case wc.send <- struct{}{}:
	default:
	}
}

func (p *Pool) wakeable(ip string) bool {
	p.wakeMu.Lock()
	defer p.wakeMu.Unlock()
	return p.wakers[ip] != nil
}

// Run dispatches tasks to pods over their wake sockets until ctx ends. It does
// nothing when WakeInterval is zero.
func (p *Pool) Run(ctx context.Context) {
	if p.cfg.WakeInterval <= 0 {
		return
	}
	t := time.NewTicker(p.cfg.WakeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.dispatchOnce(ctx)
		}
	}
}

// dispatchOnce places and expires waiting tasks as a poll would, wakes pods
// that hold a task, and claims at most one task upstream for a free pod that
// has a wake socket.
func (p *Pool) dispatchOnce(ctx context.Context) {
	p.mu.Lock()
	p.markLostLocked()
	var anchor, free *pod
	for _, pd := range p.podsByIPLocked(nil) {
		if !p.wakeable(pd.IP) {
			continue
		}
		if anchor == nil {
			anchor = pd
		}
		if free == nil && p.canClaimLocked(pd) {
			free = pd
		}
	}
	if anchor == nil {
		p.mu.Unlock()
		return
	}
	expired := p.rebalanceLocked(anchor)
	var holding []string
	for _, pd := range p.pods {
		if len(pd.queue) > 0 {
			holding = append(holding, pd.IP)
		}
	}
	// Rebalancing may have queued waiting tasks on the free pod or claimed
	// its slot; claim for it only if it can still take a task.
	var targets []claimTarget
	if free != nil && p.canClaimLocked(free) {
		free.claiming = true
		targets = p.claimTargetsLocked(free, nil)
	} else {
		free = nil
	}
	p.mu.Unlock()
	p.reportForced(ctx)
	p.failParked(ctx, expired)
	for _, ip := range holding {
		p.wakePod(ip)
	}
	if free == nil {
		return
	}
	defer func() {
		p.mu.Lock()
		free.claiming = false
		p.mu.Unlock()
	}()
	for _, t := range targets {
		if !p.reserveClaim(t.ws) {
			continue
		}
		p.mu.Lock()
		p.rotor = (t.idx + 1) % len(p.wsOrder)
		p.mu.Unlock()
		raw, err := p.claimUpstream(ctx, t)
		p.finishClaim(t.ws, err != nil)
		if err != nil {
			p.log.Warn("claim task for dispatch", "pod_ip", free.IP, "workspace_id", t.ws.cfg.WorkspaceID, "error", err)
			continue
		}
		if raw == nil {
			continue
		}
		p.leaseClaimed(ctx, free, t.ws, raw, true)
		return
	}
}
