package sandboxmanager_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

// Heartbeat 1s, timeout 3s, as the ticket specifies.
func shortTimeout(c *sandboxmanager.PoolConfig) { c.HeartbeatTimeout = 3 * time.Second }

func TestPodWithSteadyHeartbeatStaysIdle(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shortTimeout)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	for i := 0; i < 5; i++ {
		time.Sleep(time.Second)
		if status := pod.heartbeat(); status != http.StatusOK {
			t.Fatalf("heartbeat %d: status %d", i, status)
		}
	}
	if inv := h.inventory(); inv.Idle != 1 || inv.Lost != 0 {
		t.Fatalf("want 1 idle, got %+v", inv)
	}
}

func TestSilentPodBecomesLostAndNeedsToReregister(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shortTimeout)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	time.Sleep(3500 * time.Millisecond)
	inv := h.inventory()
	if inv.Lost != 1 || inv.Idle != 0 || inv.Pods[0].State != sandboxmanager.PodLost {
		t.Fatalf("want pod lost after timeout, got %+v", inv)
	}

	// Heartbeats resuming must not bring it back.
	if status := pod.heartbeat(); status == http.StatusOK {
		t.Fatal("heartbeat from a lost pod was accepted")
	}
	if inv := h.inventory(); inv.Lost != 1 || inv.Idle != 0 {
		t.Fatalf("heartbeat revived a lost pod: %+v", inv)
	}

	// Registering again does.
	pod.start()
	if inv := h.inventory(); inv.Idle != 1 || inv.Lost != 0 || len(inv.Pods) != 1 {
		t.Fatalf("want 1 idle after re-register, got %+v", inv)
	}
}

func TestNewBootNonceOnSameIPReplacesTheOldInstance(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shortTimeout)
	old := h.newPod("127.0.0.2", "pool-0")
	old.nonce = "boot-1"
	old.start()
	before := h.inventory().Pods[0]

	time.Sleep(3500 * time.Millisecond)
	if got := h.inventory().Lost; got != 1 {
		t.Fatalf("old instance should be lost, got %d lost", got)
	}

	// A replacement pod reuses the IP with a fresh nonce.
	repl := h.newPod("127.0.0.2", "pool-0-new")
	repl.nonce = "boot-2"
	repl.start()

	inv := h.inventory()
	if len(inv.Pods) != 1 || inv.Idle != 1 || inv.Lost != 0 {
		t.Fatalf("inventory must keep one entry for the IP, got %+v", inv)
	}
	if got := inv.Pods[0]; got.Name != "pool-0-new" || !got.RegisteredAt.After(before.RegisteredAt) {
		t.Fatalf("old state was not replaced: before %+v after %+v", before, got)
	}
}

func TestNewBootNonceReplacesAHealthyInstanceToo(t *testing.T) {
	h := newHarness(t, 1, shortTimeout)
	a := h.newPod("127.0.0.2", "first")
	a.nonce = "boot-1"
	a.start()
	b := h.newPod("127.0.0.2", "second")
	b.nonce = "boot-2"
	b.start()

	inv := h.inventory()
	if len(inv.Pods) != 1 || inv.Pods[0].Name != "second" {
		t.Fatalf("want only the new instance, got %+v", inv)
	}
}

func TestRegistrationWithMismatchedSelfReportedIPIsRejected(t *testing.T) {
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.reportedIP = "10.1.2.3"

	status, body := pod.register()

	if status != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", status, body)
	}
	for _, want := range []string{"10.1.2.3", "127.0.0.2"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("response %q does not name %s", body, want)
		}
		if !strings.Contains(h.logs.String(), want) {
			t.Fatalf("log does not record %s: %s", want, h.logs.String())
		}
	}
	if inv := h.inventory(); len(inv.Pods) != 0 {
		t.Fatalf("rejected pod entered the inventory: %+v", inv)
	}
	if n := h.upstream.registrationCount(); n != 0 {
		t.Fatalf("rejected registration reached upstream %d times", n)
	}
}

func TestRegistrationWithMatchingSelfReportedIPIsAccepted(t *testing.T) {
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.reportedIP = "127.0.0.2"
	pod.start()
	if inv := h.inventory(); inv.Idle != 1 {
		t.Fatalf("want 1 idle, got %+v", inv)
	}
}

func TestPodNameIsDisplayOnly(t *testing.T) {
	h := newHarness(t, 2)
	h.newPod("127.0.0.2", "pool-0").start()
	h.newPod("127.0.0.3", "pool-0").start()

	inv := h.inventory()
	if len(inv.Pods) != 2 || inv.Idle != 2 {
		t.Fatalf("same-named pods must stay distinct, got %+v", inv)
	}
}
