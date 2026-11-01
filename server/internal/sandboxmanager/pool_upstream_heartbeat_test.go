package sandboxmanager_test

import (
	"testing"
	"time"
)

func (f *fakeMultica) callsTo(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

// Multica marks a runtime offline, and stops handing out its tasks, when nobody
// heartbeats it. The pods heartbeat the manager, not Multica, so the manager
// must keep the pool's runtime alive upstream for as long as a pod is alive.
func TestPodHeartbeatKeepsThePoolRuntimeOnlineUpstream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	if got := h.upstream.callsTo("POST /api/daemon/heartbeat"); got != 0 {
		t.Fatalf("heartbeats upstream before any pod heartbeat: %d", got)
	}
	if status := pod.heartbeat(); status != 200 {
		t.Fatalf("pod heartbeat: status %d", status)
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.upstream.callsTo("POST /api/daemon/heartbeat") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.upstream.callsTo("POST /api/daemon/heartbeat"); got != 1 {
		t.Fatalf("upstream heartbeats after a pod heartbeat: want 1, got %d", got)
	}
}

// Every pod heartbeats every 15 s; the runtime needs one upstream heartbeat per
// interval however many pods there are.
func TestPodHeartbeatsAreNotMultipliedUpstream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 3)
	for i, ip := range []string{"127.0.0.2", "127.0.0.3", "127.0.0.4"} {
		pod := h.newPod(ip, "pool-"+string(rune('0'+i)))
		pod.start()
		pod.heartbeat()
		pod.heartbeat()
	}
	time.Sleep(300 * time.Millisecond)
	if got := h.upstream.callsTo("POST /api/daemon/heartbeat"); got != 1 {
		t.Fatalf("upstream heartbeats for 6 pod heartbeats within an interval: want 1, got %d", got)
	}
}
