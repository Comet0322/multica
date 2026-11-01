package sandboxmanager_test

// Lease lifecycle defects of the design review: a lease must end when its task
// is over whatever the pod does or fails to say (C-1, M-1, L-4), a lease must
// not outlive LeaseMax (M-7), a failure report must reach Multica (M-11), and
// a daemon or container that restarted must not inherit the leases of the
// process that died (H-1).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

// testClock is the manager's clock plus an offset the test moves.
type testClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off += d
}

// withClock gives the pool a clock the test can move. The harness pools are
// quiet until the first request, so the swap cannot race anything.
func (h *harness) withClock() *testClock {
	h.t.Helper()
	c := &testClock{}
	h.pool.SetNow(c.now)
	return c
}

// farHeartbeat keeps a pod from being listed lost while the test moves the
// clock.
func farHeartbeat(c *sandboxmanager.PoolConfig) { c.HeartbeatTimeout = 1000 * time.Hour }

// leaseOne starts the pod and hands it task id.
func leaseOne(t *testing.T, h *harness, pod *simPod, id string) {
	t.Helper()
	h.upstream.enqueue(id)
	pod.start()
	if got := pod.claimBatch(); !slices.Equal(got, []string{id}) {
		t.Fatalf("want %s, got %v", id, got)
	}
}

func (h *harness) lease(id string) sandboxmanager.LeaseInfo {
	h.t.Helper()
	for _, l := range h.leases() {
		if l.TaskID == id {
			return l
		}
	}
	h.t.Fatalf("no lease record for %s in %+v", id, h.leases())
	return sandboxmanager.LeaseInfo{}
}

// wantEnded fails unless the lease of id has ended with a reason containing
// reason.
func (h *harness) wantEnded(id, reason string) {
	h.t.Helper()
	l := h.lease(id)
	if l.EndedAt == nil || !strings.Contains(l.EndReason, reason) {
		h.t.Fatalf("lease %s: want ended with %q, got %+v", id, reason, l)
	}
}

func (h *harness) wantHeld(id string) {
	h.t.Helper()
	if l := h.lease(id); l.EndedAt != nil {
		h.t.Fatalf("lease %s must be held, ended: %+v", id, l)
	}
}

// answerTask answers the task endpoint of the fake before its defaults: the
// answer function gets the task id and the action and returns an HTTP status
// (0 passes the request on) and, for a GET of status, the status text.
func answerTask(f *fakeMultica, answer func(id, action string) (code int, status string)) {
	f.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		rest, ok := strings.CutPrefix(r.URL.Path, "/api/daemon/tasks/")
		if !ok {
			return false
		}
		parts := strings.Split(rest, "/")
		if len(parts) != 2 {
			return false
		}
		code, status := answer(parts[0], parts[1])
		switch {
		case code != 0:
			http.Error(w, "refused", code)
		case status != "":
			writeJSON(w, map[string]any{"status": status})
		default:
			return false
		}
		return true
	})
}

func (p *simPod) event(name string) int {
	p.h.t.Helper()
	status, _ := p.call(http.MethodPost, "/pod/control", map[string]string{"event": name})
	return status
}

// C-1: a cancelled task is never reported complete or failed by the stock
// daemon, only acknowledged. The acknowledgement has to end the lease, or the
// pod stays leased until the pod dies.
func TestCancelAckEndsTheLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")

	if status := pod.report("t1", "cancel-ack"); status != http.StatusOK {
		t.Fatalf("cancel-ack: status %d", status)
	}
	h.wantEnded("t1", "pod reported cancel-ack")
	if inv := h.inventory(); inv.Resetting != 1 || inv.Leased != 0 {
		t.Fatalf("the pod must be reset after the lease: %+v", inv)
	}
	if _, failed := h.upstream.failureOf("t1"); failed {
		t.Fatalf("a cancelled task must not be failed upstream")
	}
}

func TestTerminalReportMulticaCouldNotTakeKeepsTheLease(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"cancel-ack", "complete", "fail"} {
		for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("%s %d", action, code), func(t *testing.T) {
				t.Parallel()
				h := newHarness(t, 1)
				pod := h.newPod("127.0.0.2", "pool-0")
				leaseOne(t, h, pod, "t1")
				answerTask(h.upstream, func(id, a string) (int, string) {
					if a == action {
						return code, ""
					}
					return 0, ""
				})
				if status := pod.report("t1", action); status != code {
					t.Fatalf("the pod must see Multica's answer %d, got %d", code, status)
				}
				h.wantHeld("t1")
			})
		}
	}
}

// C-1 / L-4: Multica answering 401/403/404 to a terminal report means the task
// is gone for this token; the lease ends and the task is not failed again.
func TestTerminalReportMulticaRefusedEndsTheLease(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"cancel-ack", "complete", "fail"} {
		for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
			t.Run(fmt.Sprintf("%s %d", action, code), func(t *testing.T) {
				t.Parallel()
				h := newHarness(t, 1)
				pod := h.newPod("127.0.0.2", "pool-0")
				leaseOne(t, h, pod, "t1")
				answerTask(h.upstream, func(id, a string) (int, string) {
					if a == action {
						return code, ""
					}
					return 0, ""
				})
				pod.report("t1", action)
				h.wantEnded("t1", fmt.Sprintf("upstream refused the pod's %s report (HTTP %d)", action, code))
				if n := h.upstream.callCount("POST /api/daemon/tasks/t1/fail"); action != "fail" && n != 0 {
					t.Fatalf("the manager must not fail the task again, %d fail calls", n)
				}
				if inv := h.inventory(); inv.Resetting != 1 {
					t.Fatalf("the pod must be reset: %+v", inv)
				}
			})
		}
	}
}

func TestReportThatIsNotTerminalNeverEndsTheLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")
	answerTask(h.upstream, func(id, a string) (int, string) { return http.StatusNotFound, "" })
	for _, action := range []string{"start", "progress", "messages"} {
		if status := pod.report("t1", action); status != http.StatusNotFound {
			t.Fatalf("%s: status %d", action, status)
		}
	}
	h.wantHeld("t1")
}

// C-1 / M-1: Multica knows how a task ended even when the pod never says.
func TestReconcileEndsTheLeasesOfTasksThatAreOver(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		code   int
		status string
		ended  string // empty: the lease stays
	}{
		{"completed", 0, "completed", "upstream task ended: completed"},
		{"failed", 0, "failed", "upstream task ended: failed"},
		{"cancelled", 0, "cancelled", "upstream task ended: cancelled"},
		{"running", 0, "running", ""},
		{"dispatched", 0, "dispatched", ""},
		{"gone", http.StatusNotFound, "", "upstream task ended: not found"},
		{"token revoked", http.StatusUnauthorized, "", "upstream access lost (HTTP 401)"},
		{"workspace left", http.StatusForbidden, "", "upstream access lost (HTTP 403)"},
		{"server error", http.StatusInternalServerError, "", ""},
		{"overloaded", http.StatusTooManyRequests, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, 1)
			pod := h.newPod("127.0.0.2", "pool-0")
			leaseOne(t, h, pod, "t1")
			answerTask(h.upstream, func(id, a string) (int, string) {
				if a == "status" {
					return tc.code, tc.status
				}
				return 0, ""
			})

			h.pool.ReconcileOnce(context.Background())

			if tc.ended == "" {
				h.wantHeld("t1")
			} else {
				h.wantEnded("t1", tc.ended)
				if inv := h.inventory(); inv.Resetting != 1 {
					t.Fatalf("the pod must be reset: %+v", inv)
				}
			}
			if n := h.upstream.callCount("POST /api/daemon/tasks/t1/fail"); n != 0 {
				t.Fatalf("a task Multica already closed is not failed again (%d fail calls)", n)
			}
		})
	}
}

// A status that errors one pass and answers the next ends the lease then.
func TestReconcileRetriesOnTheNextPass(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")
	var failing atomic.Bool
	failing.Store(true)
	answerTask(h.upstream, func(id, a string) (int, string) {
		if a != "status" {
			return 0, ""
		}
		if failing.Load() {
			return http.StatusInternalServerError, ""
		}
		return 0, "completed"
	})
	h.pool.ReconcileOnce(context.Background())
	h.wantHeld("t1")
	failing.Store(false)
	h.pool.ReconcileOnce(context.Background())
	h.wantEnded("t1", "upstream task ended: completed")
}

// The reconcile calls Multica without holding the pool's lock, and a Multica
// that never answers cannot hold the lease or the pool.
func TestReconcileHoldsNoLockWhileMulticaIsSlow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/daemon/tasks/t1/status" {
			return false
		}
		select {
		case asked <- struct{}{}:
		default:
		}
		select {
		case <-release:
			writeJSON(w, map[string]any{"status": "completed"})
		case <-r.Context().Done():
		}
		return true
	})

	done := make(chan struct{})
	go func() { h.pool.ReconcileOnce(context.Background()); close(done) }()
	<-asked
	// Everything else keeps answering while the lookup hangs.
	answered := make(chan struct{})
	go func() {
		h.inventory()
		if status := pod.heartbeat(); status != http.StatusOK {
			t.Errorf("heartbeat during a slow reconcile: status %d", status)
		}
		close(answered)
	}()
	select {
	case <-answered:
	case <-time.After(3 * time.Second):
		t.Fatal("the pool stopped answering while a reconcile waited for Multica")
	}
	h.wantHeld("t1")
	close(release)
	<-done
	h.wantEnded("t1", "upstream task ended: completed")
}

func TestReconcileGivesUpOnAStatusCallWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/daemon/tasks/t1/status" {
			return false
		}
		<-r.Context().Done() // never answers
		return true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	h.pool.ReconcileOnce(ctx)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the reconcile hung for %s", took)
	}
	h.wantHeld("t1")
}

func TestReconcileAsksAtMostEightAtOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, func(c *sandboxmanager.PoolConfig) {
		c.Placement = sandboxmanager.PlacementIssue
		c.PodConcurrency = 20
	})
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	const n = 20
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("t%d", i)
		h.upstream.mu.Lock()
		h.upstream.queue = append(h.upstream.queue, map[string]any{
			"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
			"workspace_id": harnessWorkspaceID, "issue_id": "one-issue", "initiator_email": "alice@example.com",
		})
		h.upstream.mu.Unlock()
	}
	for held := 0; held < n; {
		got := pod.claimBatch()
		if len(got) == 0 {
			t.Fatalf("pod stopped receiving tasks after %d", held)
		}
		held += len(got)
	}
	var inflight, peak atomic.Int32
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			return false
		}
		cur := inflight.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		inflight.Add(-1)
		writeJSON(w, map[string]any{"status": "running"})
		return true
	})
	h.pool.ReconcileOnce(context.Background())
	if p := peak.Load(); p > 8 || p < 2 {
		t.Fatalf("status calls in flight at once: peak %d, want 2..8", p)
	}
}

// The pod reports the task done while the reconcile is still asking; the
// lease must end once, with the pod's reason.
func TestReconcileDoesNotEndALeaseThatEndedMeanwhile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/daemon/tasks/t1/status" {
			return false
		}
		asked <- struct{}{}
		<-release
		writeJSON(w, map[string]any{"status": "completed"})
		return true
	})
	done := make(chan struct{})
	go func() { h.pool.ReconcileOnce(context.Background()); close(done) }()
	<-asked
	if status := pod.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	close(release)
	<-done

	var records int
	for _, l := range h.leases() {
		if l.TaskID == "t1" {
			records++
		}
	}
	if records != 1 {
		t.Fatalf("want one lease record, got %d", records)
	}
	h.wantEnded("t1", "pod reported complete")
	if n := strings.Count(h.audit.String(), `"reason":"pod reported complete"`) + strings.Count(h.audit.String(), "upstream task ended"); n > 1 {
		t.Fatalf("the lease was released twice:\n%s", h.audit.String())
	}
}

// Run starts the reconcile only when ReconcileInterval says so.
func TestRunReconcilesOnItsIntervalAndNotWithoutOne(t *testing.T) {
	t.Parallel()
	statusCalls := func(h *harness) int { return h.upstream.callCount("GET /api/daemon/tasks/t1/status") }
	t.Run("on", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, 1, func(c *sandboxmanager.PoolConfig) { c.ReconcileInterval = 20 * time.Millisecond })
		pod := h.newPod("127.0.0.2", "pool-0")
		leaseOne(t, h, pod, "t1")
		answerTask(h.upstream, func(id, a string) (int, string) {
			if a == "status" {
				return 0, "cancelled"
			}
			return 0, ""
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go h.pool.Run(ctx)
		eventually(t, "the loop to end the lease", func() bool { return h.lease("t1").EndedAt != nil })
		h.wantEnded("t1", "upstream task ended: cancelled")
	})
	t.Run("off", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, 1)
		pod := h.newPod("127.0.0.2", "pool-0")
		leaseOne(t, h, pod, "t1")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h.pool.Run(ctx) // returns at once: nothing to run
		time.Sleep(150 * time.Millisecond)
		if n := statusCalls(h); n != 0 {
			t.Fatalf("a reconcile ran without an interval: %d status calls", n)
		}
		h.wantHeld("t1")
	})
}

// M-7: whatever happens, a lease ends at LeaseMax and its task is failed.
func TestLeaseMaxEndsAnOldLeaseAndFailsItsTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, farHeartbeat, func(c *sandboxmanager.PoolConfig) { c.LeaseMax = 10 * time.Minute })
	clock := h.withClock()
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")

	clock.advance(10*time.Minute - time.Second)
	h.pool.Touch()
	h.wantHeld("t1")

	clock.advance(2 * time.Second)
	h.pool.Touch()
	h.wantEnded("t1", "lease exceeded the maximum lease time")
	if status := pod.heartbeat(); status != http.StatusOK && status != http.StatusNotFound {
		t.Fatalf("heartbeat: status %d", status)
	}
	eventually(t, "the task to be failed upstream", func() bool {
		msg, ok := h.upstream.failureOf("t1")
		return ok && msg == "task exceeded the maximum lease time"
	})
}

func TestLeaseMaxDefaultsAndValidation(t *testing.T) {
	t.Parallel()
	t.Run("two hours without LiteLLM", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, 1, farHeartbeat)
		clock := h.withClock()
		pod := h.newPod("127.0.0.2", "pool-0")
		leaseOne(t, h, pod, "t1")
		clock.advance(2*time.Hour - time.Minute)
		h.pool.Touch()
		h.wantHeld("t1")
		clock.advance(2 * time.Minute)
		h.pool.Touch()
		h.wantEnded("t1", "lease exceeded the maximum lease time")
	})
	t.Run("the LiteLLM task timeout with LiteLLM", func(t *testing.T) {
		t.Parallel()
		llm := newFakeLiteLLM(t, "alice@example.com")
		h := newHarness(t, 1, farHeartbeat, withLiteLLM(llm, func(c *sandboxmanager.LiteLLMConfig) { c.TaskTimeout = 10 * time.Minute }))
		clock := h.withClock()
		pod := h.newPod("127.0.0.2", "pool-0")
		leaseOne(t, h, pod, "t1")
		clock.advance(9 * time.Minute)
		h.pool.Touch()
		h.wantHeld("t1")
		clock.advance(2 * time.Minute)
		h.pool.Touch()
		h.wantEnded("t1", "lease exceeded the maximum lease time")
	})
	t.Run("negative is refused", func(t *testing.T) {
		t.Parallel()
		_, err := sandboxmanager.NewPool(sandboxmanager.PoolConfig{
			MulticaURL: "http://127.0.0.1:1", MulticaToken: harnessPAT, WorkspaceID: harnessWorkspaceID, LeaseMax: -time.Second,
		}, nil)
		if err == nil {
			t.Fatal("a negative LeaseMax must be refused")
		}
	})
}

// M-11: a failure report that Multica could not take is sent again.
func TestFailureReportThatMulticaCouldNotTakeIsRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")
	var down atomic.Bool
	down.Store(true)
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/daemon/tasks/t1/fail" && down.Load() {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return true
		}
		return false
	})

	if status := pod.event("daemon-exited"); status != http.StatusOK {
		t.Fatalf("event: status %d", status)
	}
	if n := h.inventory().PendingFailReports; n != 1 {
		t.Fatalf("the unsent failure must be queued, pending %d", n)
	}
	// Reconciling while Multica is still down keeps it queued, once.
	h.pool.ReconcileOnce(context.Background())
	if n := h.inventory().PendingFailReports; n != 1 {
		t.Fatalf("pending %d after a failed retry", n)
	}

	down.Store(false)
	h.pool.ReconcileOnce(context.Background())
	if n := h.inventory().PendingFailReports; n != 0 {
		t.Fatalf("pending %d after Multica took it", n)
	}
	if msg, ok := h.upstream.failureOf("t1"); !ok || msg != "the pod's daemon restarted" {
		t.Fatalf("failure upstream = %q, %v", msg, ok)
	}
}

func TestFailureReportMulticaRefusesIsDropped(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusConflict, http.StatusNotFound, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, 1)
			pod := h.newPod("127.0.0.2", "pool-0")
			leaseOne(t, h, pod, "t1")
			var first atomic.Bool
			h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/daemon/tasks/t1/fail" {
					return false
				}
				if first.CompareAndSwap(false, true) {
					http.Error(w, "bad gateway", http.StatusBadGateway)
				} else {
					http.Error(w, "closed", code)
				}
				return true
			})
			pod.event("daemon-exited")
			if n := h.inventory().PendingFailReports; n != 1 {
				t.Fatalf("pending %d", n)
			}
			h.pool.ReconcileOnce(context.Background())
			if n := h.inventory().PendingFailReports; n != 0 {
				t.Fatalf("a refusal means Multica has dealt with the task; pending %d", n)
			}
			// A refusal at the first attempt is not queued at all.
			h2 := newHarness(t, 1)
			pod2 := h2.newPod("127.0.0.2", "pool-0")
			leaseOne(t, h2, pod2, "t2")
			h2.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "/fail") {
					http.Error(w, "closed", code)
					return true
				}
				return false
			})
			pod2.event("daemon-exited")
			if n := h2.inventory().PendingFailReports; n != 0 {
				t.Fatalf("a first-attempt refusal was queued: %d", n)
			}
		})
	}
}

func TestFailureReportIsGivenUpAfterThirtyMinutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, farHeartbeat)
	clock := h.withClock()
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/fail") {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return true
		}
		return false
	})
	pod.event("daemon-exited")
	clock.advance(29 * time.Minute)
	h.pool.ReconcileOnce(context.Background())
	if n := h.inventory().PendingFailReports; n != 1 {
		t.Fatalf("still within 30 minutes, pending %d", n)
	}
	clock.advance(2 * time.Minute)
	h.pool.ReconcileOnce(context.Background())
	if n := h.inventory().PendingFailReports; n != 0 {
		t.Fatalf("pending %d after 30 minutes", n)
	}
	if !strings.Contains(h.logs.String(), "gave up failing task upstream") {
		t.Fatalf("giving up must be logged as an error:\n%s", h.logs.String())
	}
}

func TestTaskIsQueuedForRetryOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/fail") {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return true
		}
		return false
	})
	pod.event("daemon-exited")
	pod.event("daemon-exited")
	h.pool.ReconcileOnce(context.Background())
	h.pool.ReconcileOnce(context.Background())
	if n := h.inventory().PendingFailReports; n != 1 {
		t.Fatalf("one task, one queued report; got %d", n)
	}
}

// The failure still goes out when the request that caused it is abandoned.
func TestFailureReportSurvivesTheCancelledRequestThatCausedIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")
	var delivered atomic.Bool
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/daemon/tasks/t1/fail" {
			return false
		}
		time.Sleep(400 * time.Millisecond)
		if r.Context().Err() == nil {
			delivered.Store(true)
		}
		return false
	})
	short := &http.Client{Transport: pod.client.Transport, Timeout: 100 * time.Millisecond}
	req, _ := http.NewRequest(http.MethodPost, h.podAPI.URL+"/pod/control", strings.NewReader(`{"event":"daemon-exited"}`))
	req.Header.Set("Content-Type", "application/json")
	_, _ = short.Do(req) // gives up before the manager is done
	eventually(t, "the failure to reach Multica", func() bool { return delivered.Load() })
}

// H-1: the stock daemon sends no boot nonce, so a daemon that restarted fast
// registers as the same instance. The supervisor says what happened instead.
func TestDaemonExitedEndsTheLeaseAndResetsThePod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")

	if status := pod.event("daemon-exited"); status != http.StatusOK {
		t.Fatalf("event: status %d", status)
	}
	h.wantEnded("t1", "pod daemon restarted")
	if inv := h.inventory(); inv.Resetting != 1 || inv.Leased != 0 {
		t.Fatalf("the pod must be reset: %+v", inv)
	}
	if msg, ok := h.upstream.failureOf("t1"); !ok || msg != "the pod's daemon restarted" {
		t.Fatalf("task failure = %q, %v", msg, ok)
	}
}

func TestStartedEventEndsTheLeaseAsAContainerRestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	leaseOne(t, h, pod, "t1")
	if status := pod.event("started"); status != http.StatusOK {
		t.Fatalf("event: status %d", status)
	}
	h.wantEnded("t1", "pod container restarted")
	if msg, ok := h.upstream.failureOf("t1"); !ok || msg != "the pod's daemon restarted" {
		t.Fatalf("task failure = %q, %v", msg, ok)
	}
}

func TestEventsLeaveAPodThatHoldsNothingAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	idle := h.newPod("127.0.0.2", "pool-0")
	idle.start()
	resetting := h.newPod("127.0.0.3", "pool-1")
	leaseAndComplete(t, h, resetting, "t0")

	for _, ev := range []string{"started", "daemon-exited"} {
		if status := idle.event(ev); status != http.StatusOK {
			t.Fatalf("idle %s: status %d", ev, status)
		}
		if status := resetting.event(ev); status != http.StatusOK {
			t.Fatalf("resetting %s: status %d", ev, status)
		}
	}
	inv := h.inventory()
	states := map[string]string{}
	for _, p := range inv.Pods {
		states[p.IP] = p.State
	}
	if states["127.0.0.2"] != sandboxmanager.PodIdle || states["127.0.0.3"] != sandboxmanager.PodResetting {
		t.Fatalf("pod states changed: %v", states)
	}
	// A pod nobody knows is acknowledged too, so its supervisor stops resending.
	stranger := h.newPod("127.0.0.9", "stranger")
	if status := stranger.event("daemon-exited"); status != http.StatusOK {
		t.Fatalf("unknown pod: status %d", status)
	}
	// Unknown event names are ignored.
	if status := idle.event("something-new"); status != http.StatusOK {
		t.Fatalf("unknown event: status %d", status)
	}
}

func TestEventLeavesALostPodAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shortTimeout)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	time.Sleep(3500 * time.Millisecond)
	if inv := h.inventory(); inv.Lost != 1 {
		t.Fatalf("setup: %+v", inv)
	}
	if status := pod.event("daemon-exited"); status != http.StatusOK {
		t.Fatalf("event: status %d", status)
	}
	if inv := h.inventory(); inv.Lost != 1 {
		t.Fatalf("an event revived or reset a lost pod: %+v", inv)
	}
}

func issueTask(h *harness, id, issue string) {
	h.upstream.mu.Lock()
	defer h.upstream.mu.Unlock()
	h.upstream.queue = append(h.upstream.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID, "issue_id": issue, "initiator_email": "alice@example.com",
	})
}

func TestEventOnASharedPodEndsEveryLeaseAndResetsIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, func(c *sandboxmanager.PoolConfig) {
		c.Placement = sandboxmanager.PlacementIssue
		c.PodConcurrency = 4
	})
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	issueTask(h, "t1", "issue-1")
	issueTask(h, "t2", "issue-1")
	var got []string
	for i := 0; i < 4 && len(got) < 2; i++ {
		got = append(got, pod.claimBatch()...)
	}
	if !slices.Equal(got, []string{"t1", "t2"}) {
		t.Fatalf("want both tasks, got %v", got)
	}

	pod.event("daemon-exited")

	h.wantEnded("t1", "pod daemon restarted")
	h.wantEnded("t2", "pod daemon restarted")
	if inv := h.inventory(); inv.Resetting != 1 || inv.Leased != 0 {
		t.Fatalf("a shared pod must be reset, not kept bound: %+v", inv)
	}
	for _, id := range []string{"t1", "t2"} {
		if msg, ok := h.upstream.failureOf(id); !ok || msg != "the pod's daemon restarted" {
			t.Fatalf("%s failure = %q, %v", id, msg, ok)
		}
	}
}

func TestEventOnABoundSharedPodWithoutLeasesResetsIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, func(c *sandboxmanager.PoolConfig) {
		c.Placement = sandboxmanager.PlacementIssue
		c.PodConcurrency = 4
	})
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()
	issueTask(h, "t1", "issue-1")
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("got %v", got)
	}
	if status := pod.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	if inv := h.inventory(); inv.Leased != 1 {
		t.Fatalf("setup: the shared pod must stay bound between tasks: %+v", inv)
	}
	pod.event("started")
	if inv := h.inventory(); inv.Resetting != 1 || inv.Leased != 0 {
		t.Fatalf("a bound pod whose container restarted must be reset: %+v", inv)
	}
}

// A task handed to a pod but not yet picked up goes back to the line.
func TestEventPutsTasksAssignedButNotPickedUpBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, withWake(time.Hour))
	first := h.newPod("127.0.0.2", "pool-0")
	first.start()
	sock := first.openWake()
	_ = sock
	h.upstream.enqueue("t1")
	h.dispatch() // claims t1 for the idle pod with a socket and queues it there
	if q := h.upstream.queued(); q != 0 {
		t.Fatalf("setup: t1 was not claimed upstream (%d queued)", q)
	}

	first.event("daemon-exited")

	second := h.newPod("127.0.0.3", "pool-1")
	second.start()
	got := second.claimBatch()
	if !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("the unclaimed task must go to another pod, got %v", got)
	}
}

// The new daemon may register before the event arrives; the event still ends
// what the old one held.
func TestEventStillEndsLeasesAfterTheDaemonRegisteredAsTheSameInstance(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	leaseOne(t, h, pod, "t1")

	pod.startRaw() // the restarted daemon, same nonce: taken for the same instance
	h.wantHeld("t1")
	pod.event("daemon-exited")
	h.wantEnded("t1", "pod daemon restarted")
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("the pod must be reset: %+v", inv)
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
