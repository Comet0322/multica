package sandboxbroker_test

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/sandboxbroker"
)

func TestInventoryStartsEmptyWithExpectedSize(t *testing.T) {
	h := newHarness(t, 3)
	inv := h.inventory()
	if inv.ExpectedSize != 3 || inv.Idle != 0 || len(inv.Pods) != 0 {
		t.Fatalf("unexpected empty inventory: %+v", inv)
	}
}

func TestFirstPodRegistersAndShowsIdle(t *testing.T) {
	h := newHarness(t, 2)
	pod := h.newPod("127.0.0.2", "pool-0")

	resp := pod.start()

	if resp["runtimes"] == nil {
		t.Fatalf("registration response has no runtimes: %v", resp)
	}
	inv := h.inventory()
	if inv.Idle != 1 || inv.ExpectedSize != 2 || len(inv.Pods) != 1 {
		t.Fatalf("want 1 idle of 2 expected, got %+v", inv)
	}
	if got := inv.Pods[0]; got.IP != "127.0.0.2" || got.State != sandboxbroker.PodIdle || got.Name != "pool-0" {
		t.Fatalf("unexpected pod entry: %+v", got)
	}
	if n := h.upstream.registrationCount(); n != 1 {
		t.Fatalf("manager registered the runtime upstream %d times, want 1", n)
	}
}

func TestSecondPodIsToldApartBySourceIP(t *testing.T) {
	h := newHarness(t, 2)
	h.newPod("127.0.0.2", "pool-0").start()
	h.newPod("127.0.0.3", "pool-1").start()

	inv := h.inventory()
	if inv.Idle != 2 || len(inv.Pods) != 2 {
		t.Fatalf("want 2 idle pods, got %+v", inv)
	}
	ips := map[string]bool{}
	for _, p := range inv.Pods {
		ips[p.IP] = true
	}
	if !ips["127.0.0.2"] || !ips["127.0.0.3"] {
		t.Fatalf("pods are not keyed by source IP: %+v", inv.Pods)
	}
	// Pods share one upstream runtime: the second registration is answered from
	// the first pod's registration.
	if n := h.upstream.registrationCount(); n != 1 {
		t.Fatalf("manager registered upstream %d times, want 1", n)
	}
}
