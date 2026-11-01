package sandboxmanager_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

const wakeEvent = "daemon:task_available"

func withWake(d time.Duration) func(*sandboxmanager.PoolConfig) {
	return func(c *sandboxmanager.PoolConfig) { c.WakeInterval = d }
}

// wakeSocket is a pod daemon's wake websocket.
type wakeSocket struct {
	t    *testing.T
	conn *websocket.Conn

	once   sync.Once
	frames chan []byte
}

// dialWake tries the daemon's websocket from the pod's address and returns the
// handshake response.
func (p *simPod) dialWake() (*wakeSocket, *http.Response, error) {
	p.h.t.Helper()
	d := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		NetDialContext:   (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(p.ip)}}).DialContext,
	}
	header := http.Header{"Authorization": {"Bearer " + podPlaceholder}}
	conn, resp, err := d.Dial("ws"+strings.TrimPrefix(p.h.podAPI.URL, "http")+"/api/daemon/ws", header)
	if err != nil {
		return nil, resp, err
	}
	p.h.t.Cleanup(func() { _ = conn.Close() })
	return &wakeSocket{t: p.h.t, conn: conn}, resp, nil
}

// openWake connects the pod's socket and waits until the manager holds it.
func (p *simPod) openWake() *wakeSocket {
	p.h.t.Helper()
	before := strings.Count(p.h.logs.String(), "pod opened its wake socket")
	ws, resp, err := p.dialWake()
	if err != nil {
		p.h.t.Fatalf("wake socket: %v (response %v)", err, resp)
	}
	eventually(p.h.t, "manager holds the socket", func() bool {
		return strings.Count(p.h.logs.String(), "pod opened its wake socket") > before
	})
	return ws
}

// next returns the type of the next frame, or "" if none arrives within d. A
// websocket read that times out cannot be retried, so a reader goroutine owns
// the connection once next is used and hands the frames over a channel.
func (w *wakeSocket) next(d time.Duration) string {
	w.t.Helper()
	w.once.Do(func() {
		w.frames = make(chan []byte, 64)
		go func() {
			defer close(w.frames)
			for {
				_, raw, err := w.conn.ReadMessage()
				if err != nil {
					return
				}
				w.frames <- raw
			}
		}()
	})
	select {
	case raw, ok := <-w.frames:
		if !ok {
			w.t.Fatalf("wake socket closed")
		}
		var msg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			w.t.Fatalf("bad frame %q: %v", raw, err)
		}
		return msg.Type
	case <-time.After(d):
		return ""
	}
}

func (h *harness) dispatch() { h.pool.DispatchOnce(context.Background()) }

func TestWakeSocketUpgradesOnlyRegisteredPods(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, withWake(time.Hour))
	known := h.newPod("127.0.0.2", "pool-0")
	known.start()
	stranger := h.newPod("127.0.0.3", "pool-1")

	ws, resp, err := known.dialWake()
	if err != nil {
		t.Fatalf("registered pod: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("registered pod: want 101, got %d", resp.StatusCode)
	}
	_ = ws.conn.Close()

	if _, resp, err := stranger.dialWake(); err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unregistered pod: want 404, got %v (err %v)", resp, err)
	}
}

func TestWakeSocketIsOffWithoutWakeInterval(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	if _, resp, err := pod.dialWake(); err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("WakeInterval=0: want 404, got %v (err %v)", resp, err)
	}
	// Dispatching does nothing either.
	h.upstream.enqueue("t1")
	h.dispatch()
	if n := h.upstream.claimCount(); n != 0 {
		t.Fatalf("want no claim with dispatch off, got %d", n)
	}
}

func TestDispatchClaimsForIdlePodAndWakesIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withWake(time.Hour))
	h.upstream.enqueue("t1")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	sock := pod.openWake()

	h.dispatch()

	if got := sock.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("want a %s frame, got %q", wakeEvent, got)
	}
	if n := h.upstream.claimCount(); n != 1 {
		t.Fatalf("want 1 upstream claim, got %d", n)
	}
	if h.upstream.queued() != 0 {
		t.Fatal("task should have left the upstream queue")
	}
	// The task waits on its pod; the lease starts when the pod claims it.
	if inv := h.inventory(); inv.Leased != 1 {
		t.Fatalf("want the pod reserved, got %+v", inv)
	}
	if ls := h.leases(); len(ls) != 0 {
		t.Fatalf("no lease before the pod claims, got %+v", ls)
	}
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1 on the pod's claim, got %v", got)
	}
	ls := h.leases()
	if len(ls) != 1 || ls[0].TaskID != "t1" || ls[0].PodIP != "127.0.0.2" || ls[0].EndedAt != nil {
		t.Fatalf("want an active lease for t1, got %+v", ls)
	}
	if status := pod.report("t1", "start"); status != http.StatusOK {
		t.Fatalf("start: status %d", status)
	}
	// One task, one claim: a second round has no free pod and no frame.
	h.upstream.enqueue("t2")
	h.dispatch()
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("leased pod was woken: %q", got)
	}
	if n := h.upstream.claimCount(); n != 1 {
		t.Fatalf("want still 1 claim, got %d", n)
	}
}

func TestDispatchNeverClaimsForPodWithoutSocket(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, withWake(time.Hour))
	h.upstream.enqueue("t1")
	silent := h.newPod("127.0.0.2", "pool-0")
	silent.start()

	h.dispatch()
	if n := h.upstream.claimCount(); n != 0 {
		t.Fatalf("claimed for a pod with no socket: %d", n)
	}
	if h.upstream.queued() != 1 {
		t.Fatal("task left the upstream queue")
	}

	// A second pod with a socket is the one served.
	live := h.newPod("127.0.0.3", "pool-1")
	live.start()
	sock := live.openWake()
	h.dispatch()
	if got := sock.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("want a frame on the live pod, got %q", got)
	}
	if got := silent.claimBatch(); len(got) != 0 {
		t.Fatalf("silent pod was handed %v", got)
	}
	if got := live.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1 on the live pod, got %v", got)
	}
}

func TestDispatchWakesTheTargetPodWhenTaskIsPlacedElsewhere(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, sharedPods(3, 0), withWake(time.Hour))
	h.upstream.enqueueIn("t1", "issue-1", "")
	a, b := h.newPod("127.0.0.2", "pool-0"), h.newPod("127.0.0.3", "pool-1")
	a.start()
	b.start()
	sockA, sockB := a.openWake(), b.openWake()
	if got := a.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1 on a, got %v", got)
	}

	// a is bound to issue-1 but has a free slot, so it is the pod dispatch
	// claims for; issue-2 cannot run on it and belongs to idle b.
	h.upstream.enqueueIn("t2", "issue-2", "")
	h.dispatch()

	if got := sockB.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("target pod b: want a frame, got %q", got)
	}
	if got := sockA.next(300 * time.Millisecond); got != "" {
		t.Fatalf("bound pod a was woken for another pod's task: %q", got)
	}
	if got := a.claimBatch(); len(got) != 0 {
		t.Fatalf("a was handed %v", got)
	}
	if got := b.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("want t2 on b, got %v", got)
	}
	got := leasePods(h)
	if got["t1"] != "127.0.0.2" || got["t2"] != "127.0.0.3" {
		t.Fatalf("unexpected placement: %v", got)
	}
}

func TestReconnectReplacesTheOldWakeSocket(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withWake(time.Hour))
	h.upstream.enqueue("t1")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	oldSock := pod.openWake()
	newSock := pod.openWake()

	// The manager closes the stale socket.
	_ = oldSock.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := oldSock.conn.ReadMessage(); err == nil {
		t.Fatal("old socket still delivers frames")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("old socket was not closed: %v", err)
	}

	h.dispatch()
	if got := newSock.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("new socket: want a frame, got %q", got)
	}
	// The old socket's teardown must not drop the new registration: a further
	// wake still arrives.
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1, got %v", got)
	}
	h.upstream.enqueue("t2")
	pod.report("t1", "complete")
	cmd := pod.pollControl()
	pod.reportControl(cmd.ID, "done")
	pod.startRaw()
	h.dispatch()
	if got := newSock.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("new socket after the old one's teardown: want a frame, got %q", got)
	}
}

func TestRunDispatchesOnItsInterval(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withWake(50*time.Millisecond))
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	sock := pod.openWake()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.pool.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	h.upstream.enqueue("t1")
	if got := sock.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("want a frame from Run, got %q", got)
	}
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1, got %v", got)
	}
}

func TestRunReturnsAtOnceWithoutWakeInterval(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	done := make(chan struct{})
	go func() { defer close(done); h.pool.Run(context.Background()) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked with WakeInterval=0")
	}
}

func TestPollingIsUnchangedWhenDispatchIsOn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, withWake(time.Hour))
	h.upstream.enqueue("t1")
	h.upstream.enqueue("t2")
	withSock := h.newPod("127.0.0.2", "pool-0")
	noSock := h.newPod("127.0.0.3", "pool-1")
	withSock.start()
	noSock.start()
	sock := withSock.openWake()

	// A poll is answered in its response, as before; no hint is sent for it.
	if got := withSock.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("socket pod poll: want [t1], got %v", got)
	}
	if got := noSock.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("socket-less pod poll: want [t2], got %v", got)
	}
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("a poll caused a hint: %q", got)
	}
	if n := h.upstream.claimCount(); n != 2 {
		t.Fatalf("want 2 claims, got %d", n)
	}
	if inv := h.inventory(); inv.Leased != 2 {
		t.Fatalf("want both pods leased, got %+v", inv)
	}
	// Leased pods still cannot claim a second task.
	h.upstream.enqueue("t3")
	if got := withSock.claimBatch(); len(got) != 0 {
		t.Fatalf("leased pod was handed %v", got)
	}
}

const workspacesChangedEvent = "daemon:workspaces_changed"

// syncedWakePod syncs the pool once, registers a pod for A and opens its socket.
func syncedWakePod(t *testing.T, up *syncMultica, ids ...string) (*harness, *wakeSocket) {
	t.Helper()
	h := newSyncHarness(t, up, withWake(time.Hour))
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	pod.registerFor(ids[0])
	return h, pod.openWake()
}

func TestSyncTellsConnectedPodsAboutAddedWorkspaces(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h, sock := syncedWakePod(t, up, wsA)

	up.setWorkspaces(wsA, wsB)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sock.next(5 * time.Second); got != workspacesChangedEvent {
		t.Fatalf("want %s, got %q", workspacesChangedEvent, got)
	}
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("one change, one frame; got another: %q", got)
	}
}

func TestSyncTellsConnectedPodsAboutRemovedWorkspaces(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	h, sock := syncedWakePod(t, up, wsA)

	up.setWorkspaces(wsA)
	// The first sync without B keeps it (nothing changed, nothing to tell); the
	// second removes it.
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("a workspace missing once is kept; got a frame: %q", got)
	}
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sock.next(5 * time.Second); got != workspacesChangedEvent {
		t.Fatalf("want %s, got %q", workspacesChangedEvent, got)
	}
}

func TestSyncSendsNothingWhenTheListIsUnchangedOrUnusable(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h, sock := syncedWakePod(t, up, wsA)

	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	up.setListStatus(http.StatusInternalServerError)
	_, _ = h.pool.Sync(context.Background())
	up.setListStatus(0)
	up.setWorkspaces()
	_, _ = h.pool.Sync(context.Background())
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("nothing changed, yet got %q", got)
	}
}

func TestSyncReachesEveryConnectedPod(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h, first := syncedWakePod(t, up, wsA)
	second := h.newPod("127.0.0.3", "pool-1")
	second.nonce = "boot-2"
	second.registerFor(wsA)
	secondSock := second.openWake()

	up.setWorkspaces(wsA, wsB)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*wakeSocket{"first": first, "second": secondSock} {
		if got := s.next(5 * time.Second); got != workspacesChangedEvent {
			t.Fatalf("%s pod: want %s, got %q", name, workspacesChangedEvent, got)
		}
	}
}

func TestSyncWithoutSocketsDoesNotBlock(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up, withWake(time.Hour))
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.pool.Sync(context.Background()) // no pods at all
		pod.registerFor(wsA)                     // a pod without a socket
		up.setWorkspaces(wsA, wsB)
		_, _ = h.pool.Sync(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Sync blocked without wake sockets")
	}
}

func TestTaskHintsKeepTheirOwnFrameBesideWorkspaceChanges(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withWake(time.Hour))
	h.upstream.enqueue("t1")
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	sock := pod.openWake()

	h.dispatch()
	if got := sock.next(5 * time.Second); got != wakeEvent {
		t.Fatalf("a task hint must stay %s, got %q", wakeEvent, got)
	}
}
