package sandboxmanager_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Multica UI asks the runtime which models its agent CLI can run; the
// request arrives in the answer to the manager's own heartbeat. The CLIs are in
// the pods, so one pod's daemon gets the request, asks its CLI and reports to
// the manager, which relays the report with the workspace's account.
func TestModelListRequestIsHandedToOnePodAndItsReportRelayed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	podA := h.newPod("127.0.0.2", "pool-0")
	podB := h.newPod("127.0.0.3", "pool-1")
	podA.start()
	podB.start()

	var mu sync.Mutex
	var reported, reportAuth string
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/api/daemon/heartbeat":
			writeJSON(w, map[string]any{"runtime_id": "runtime-a", "status": "ok", "pending_model_list": map[string]string{"id": "ml-1"}})
			return true
		case strings.HasSuffix(r.URL.Path, "/models/ml-1/result"):
			mu.Lock()
			reported, reportAuth = r.URL.Path, r.Header.Get("Authorization")
			mu.Unlock()
			writeJSON(w, map[string]string{"status": "ok"})
			return true
		}
		return false
	})

	podA.heartbeat() // makes the manager heartbeat upstream, which brings the request
	deadline := time.Now().Add(2 * time.Second)
	for h.upstream.callsTo("POST /api/daemon/heartbeat") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	status, raw := podA.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	if status != 200 || !strings.Contains(string(raw), `"pending_model_list":{"id":"ml-1"}`) {
		t.Fatalf("the first pod's heartbeat should carry the request, got %d %s", status, raw)
	}
	_, other := podB.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	if strings.Contains(string(other), "pending_model_list") {
		t.Fatalf("the request was handed to a second pod: %s", other)
	}

	path := "/api/daemon/runtimes/runtime-a/models/ml-1/result"
	body := map[string]any{"status": "completed", "models": []map[string]string{{"id": "claude-haiku-5-5", "label": "Haiku"}}}
	if status, _ := podB.call(http.MethodPost, path, body); status != http.StatusForbidden {
		t.Fatalf("a pod that was not handed the request reported it: status %d", status)
	}
	if status, _ := podA.call(http.MethodPost, "/api/daemon/runtimes/runtime-a/models/other/result", body); status != http.StatusForbidden {
		t.Fatalf("a report for another request id was relayed: status %d", status)
	}
	if status, raw := podA.call(http.MethodPost, path, body); status != 200 {
		t.Fatalf("the report was refused: %d %s", status, raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if reported != path {
		t.Fatalf("relayed path = %q, want %q", reported, path)
	}
	if reportAuth != "Bearer "+harnessPAT {
		t.Fatalf("the report was not relayed with the workspace account")
	}
	var again map[string]any
	_, raw = podA.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	_ = json.Unmarshal(raw, &again)
	if _, ok := again["pending_model_list"]; ok {
		t.Fatalf("a finished request was handed out again: %s", raw)
	}
}
