package sandboxbroker_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The stock daemon re-registers only when a heartbeat is answered 404 with a
// "runtime not found" body (daemon.isRuntimeNotFoundError). Any other answer
// leaves it heartbeating forever against a manager that no longer knows it,
// which is exactly the state after a manager restart.
func TestHeartbeatFromUnknownPodTellsTheStockDaemonToRegisterAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shortTimeout)
	pod := h.newPod("127.0.0.2", "pool-0")

	status, body := pod.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	if status != http.StatusNotFound || !strings.Contains(strings.ToLower(string(body)), "runtime not found") {
		t.Fatalf("unknown pod heartbeat: want 404 \"runtime not found\", got %d %q", status, body)
	}
}

// A pod marked lost comes back only by registering, so it is told the same.
func TestHeartbeatFromLostPodTellsTheStockDaemonToRegisterAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shortTimeout)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	time.Sleep(3500 * time.Millisecond)

	status, body := pod.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	if status != http.StatusNotFound || !strings.Contains(strings.ToLower(string(body)), "runtime not found") {
		t.Fatalf("lost pod heartbeat: want 404 \"runtime not found\", got %d %q", status, body)
	}
}
