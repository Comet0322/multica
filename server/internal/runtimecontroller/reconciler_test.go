package runtimecontroller

import (
	"context"
	"errors"
	"testing"
	"time"
)

type harness struct {
	t   *testing.T
	now time.Time
	srv *fakeServer
	kb  *fakeKube
	llm *fakeLLM
	rec *Reconciler
}

func newHarness(t *testing.T, withLLM bool) *harness {
	t.Helper()
	h := &harness{t: t, now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), kb: newFakeKube(), llm: newFakeLLM()}
	h.srv = &fakeServer{now: func() time.Time { return h.now }}
	cfg := Config{
		IdleSuspendAfter: 15 * time.Minute, TokenTTL: 24 * time.Hour, StuckAfter: 5 * time.Minute,
		ReconcileInterval: time.Second, MissingGrace: 3, LiteLLMURL: "http://litellm.test",
		DrainTimeout: time.Minute, PodServerURL: "http://server", Image: "img", Namespace: "ns",
	}
	h.rec = &Reconciler{Cfg: cfg, Server: h.srv, Kube: h.kb, Now: func() time.Time { return h.now }}
	if withLLM {
		h.rec.LLM = h.llm
	}
	return h
}

func (h *harness) step(d time.Duration) {
	h.t.Helper()
	h.now = h.now.Add(d)
	if err := h.rec.Reconcile(context.Background()); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
}

func rt(status string, queued, active int) Runtime {
	return Runtime{ID: "rid", WorkspaceID: "ws1", DaemonID: "mr-abc", Provider: "claude", Status: status, QueuedTasks: queued, ActiveTasks: active}
}

func (h *harness) state() string { return h.srv.reports["mr-abc"] }

func TestCreatesSandboxTokenAndLLMKey(t *testing.T) {
	h := newHarness(t, true)
	h.srv.set(rt("offline", 0, 0))
	h.step(0)

	sb := h.kb.sandboxes["mr-abc"]
	if sb == nil || sb.Mode != ModeRunning {
		t.Fatalf("sandbox not created Running: %+v", sb)
	}
	if !h.kb.specs["mr-abc"].LLM {
		t.Fatal("sandbox should consume the LLM secret")
	}
	tok := h.kb.secrets["mr-abc-token"]
	if tok == nil || tok.Data["token"] != "mdt_1" || tok.Annotations[AnnotationTokenExpiresAt] == "" {
		t.Fatalf("token secret: %+v", tok)
	}
	llm := h.kb.secrets["mr-abc-llm"]
	if llm == nil || llm.Data["ANTHROPIC_AUTH_TOKEN"] != "sk-fake-1" || llm.Data["ANTHROPIC_BASE_URL"] != "http://litellm.test" {
		t.Fatalf("llm secret: %+v", llm)
	}
	if got := h.llm.live["sk-fake-1"]; got.WorkspaceID != "ws1" || got.DaemonID != "mr-abc" {
		t.Fatalf("llm key attribution: %+v", got)
	}
	if h.state() != StateWaking {
		t.Fatalf("state=%q want waking", h.state())
	}
}

func TestRunningOnlineThenIdleSuspendThenWakeOnQueuedTask(t *testing.T) {
	h := newHarness(t, false)
	h.srv.set(rt("online", 0, 0))
	h.step(0) // create
	h.step(time.Second)
	if h.state() != StateRunning {
		t.Fatalf("online & idle<threshold: state=%q", h.state())
	}

	h.step(14 * time.Minute)
	if h.kb.sandboxes["mr-abc"].Mode != ModeRunning {
		t.Fatal("suspended before the idle threshold")
	}
	h.step(2 * time.Minute)
	if h.kb.sandboxes["mr-abc"].Mode != ModeSuspended || h.state() != StateSuspended {
		t.Fatalf("not suspended after idle: mode=%s state=%s", h.kb.sandboxes["mr-abc"].Mode, h.state())
	}

	// Suspended and quiet: stays suspended and reports suspended.
	h.srv.set(rt("offline", 0, 0))
	h.step(time.Minute)
	if h.state() != StateSuspended || len(h.srv.issued) != 1 {
		t.Fatalf("suspended idle: state=%s tokens issued=%d", h.state(), len(h.srv.issued))
	}

	// A queued task wakes it with a fresh token.
	h.srv.set(rt("offline", 1, 0))
	h.step(time.Second)
	if h.kb.sandboxes["mr-abc"].Mode != ModeRunning || h.state() != StateWaking {
		t.Fatalf("not woken: mode=%s state=%s", h.kb.sandboxes["mr-abc"].Mode, h.state())
	}
	if len(h.srv.issued) != 2 || h.kb.secrets["mr-abc-token"].Data["token"] != "mdt_2" {
		t.Fatalf("wake must mint a fresh token: issued=%d secret=%+v", len(h.srv.issued), h.kb.secrets["mr-abc-token"])
	}
}

func TestNeverSuspendsWithQueuedOrActiveWork(t *testing.T) {
	for _, c := range []struct {
		name           string
		queued, active int
	}{{"active task", 0, 1}, {"queued task", 1, 0}} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, false)
			h.srv.set(rt("online", c.queued, c.active))
			h.step(0)
			h.step(2 * time.Hour)
			if h.kb.sandboxes["mr-abc"].Mode != ModeRunning {
				t.Fatalf("suspended while %s", c.name)
			}
		})
	}
}

func TestIdleClockRestartsWhenWorkArrives(t *testing.T) {
	h := newHarness(t, false)
	h.srv.set(rt("online", 0, 0))
	h.step(0)
	h.step(14 * time.Minute)
	h.srv.set(rt("online", 0, 1))
	h.step(time.Minute) // busy: clock resets
	h.srv.set(rt("online", 0, 0))
	h.step(14 * time.Minute)
	if h.kb.sandboxes["mr-abc"].Mode != ModeRunning {
		t.Fatal("idle clock did not restart after work")
	}
}

func TestRestartedControllerDoesNotSuspendImmediately(t *testing.T) {
	h := newHarness(t, false)
	h.kb.sandboxes["mr-abc"] = &SandboxInfo{Name: "mr-abc", Mode: ModeRunning, Labels: map[string]string{LabelWorkspaceID: "ws1", LabelDaemonID: "mr-abc"}}
	h.kb.secrets["mr-abc-token"] = &SecretInfo{Annotations: map[string]string{AnnotationTokenExpiresAt: h.now.Add(20 * time.Hour).Format(time.RFC3339)}}
	h.srv.set(rt("online", 0, 0))
	h.step(0)
	if h.kb.sandboxes["mr-abc"].Mode != ModeRunning {
		t.Fatal("a fresh controller must not suspend on first sight")
	}
}

func TestTokenRotatesAtHalfLifetimeOnly(t *testing.T) {
	h := newHarness(t, false)
	h.srv.set(rt("online", 0, 1))
	h.step(0) // creates, token expires +24h
	h.step(11 * time.Hour)
	if len(h.srv.issued) != 1 {
		t.Fatalf("rotated too early: %d", len(h.srv.issued))
	}
	h.step(90 * time.Minute) // past 12h
	if len(h.srv.issued) != 2 || h.kb.secrets["mr-abc-token"].Data["token"] != "mdt_2" {
		t.Fatalf("not rotated at half life: issued=%d", len(h.srv.issued))
	}
}

func TestUnavailableWhenRunningButNeverOnline(t *testing.T) {
	h := newHarness(t, false)
	h.srv.set(rt("offline", 1, 0))
	h.step(0) // create
	h.step(4 * time.Minute)
	if h.state() != StateWaking {
		t.Fatalf("before StuckAfter: state=%s", h.state())
	}
	h.step(2 * time.Minute)
	if h.state() != StateUnavailable {
		t.Fatalf("after StuckAfter: state=%s", h.state())
	}
	// Recovers when the daemon comes online.
	h.srv.set(rt("online", 1, 0))
	h.step(time.Second)
	if h.state() != StateRunning {
		t.Fatalf("recovery: state=%s", h.state())
	}
}

func TestOrphanSandboxIsCleanedUpOnlyAfterGrace(t *testing.T) {
	h := newHarness(t, true)
	h.srv.set(rt("online", 0, 1))
	h.step(0)
	if h.kb.secrets["mr-abc-llm"] == nil {
		t.Fatal("fixture: llm secret missing")
	}

	h.srv.set() // runtime deleted on the server
	h.step(time.Second)
	h.step(time.Second)
	if h.kb.sandboxes["mr-abc"] == nil {
		t.Fatal("deleted before the grace period")
	}
	h.step(time.Second) // 3rd consecutive miss
	if h.kb.sandboxes["mr-abc"] != nil || h.kb.secrets["mr-abc-token"] != nil || h.kb.secrets["mr-abc-llm"] != nil {
		t.Fatalf("sandbox/secrets not removed: sb=%v", h.kb.sandboxes)
	}
	if len(h.srv.revoked) != 1 || h.srv.revoked[0] != "ws1/mr-abc" {
		t.Fatalf("tokens not revoked: %v", h.srv.revoked)
	}
	if len(h.llm.deleted) != 1 || h.llm.deleted[0] != "sk-fake-1" || len(h.llm.live) != 0 {
		t.Fatalf("llm key not revoked: deleted=%v live=%v", h.llm.deleted, h.llm.live)
	}
}

func TestServerListErrorNeverDeletesAnything(t *testing.T) {
	h := newHarness(t, false)
	h.srv.set(rt("online", 0, 1))
	h.step(0)
	h.srv.listErr = errors.New("server down")
	for i := 0; i < 10; i++ {
		h.now = h.now.Add(time.Second)
		if err := h.rec.Reconcile(context.Background()); err == nil {
			t.Fatal("expected the list error to surface")
		}
	}
	if h.kb.sandboxes["mr-abc"] == nil {
		t.Fatal("sandbox deleted during a server outage")
	}
}

func TestOneMissedPollResetsOrphanCounter(t *testing.T) {
	h := newHarness(t, false)
	h.srv.set(rt("online", 0, 1))
	h.step(0)
	h.srv.set()
	h.step(time.Second)
	h.step(time.Second)
	h.srv.set(rt("online", 0, 1)) // reappears
	h.step(time.Second)
	h.srv.set()
	h.step(time.Second)
	h.step(time.Second)
	if h.kb.sandboxes["mr-abc"] == nil {
		t.Fatal("counter must restart after the runtime reappears")
	}
}

func TestLLMSecretFailureRevokesMintedKeyAndCreatesNoSandbox(t *testing.T) {
	h := newHarness(t, true)
	h.kb.failPut["mr-abc-llm"] = errors.New("quota exceeded")
	h.srv.set(rt("offline", 1, 0))
	h.step(0)
	if h.kb.sandboxes["mr-abc"] != nil {
		t.Fatal("sandbox created without its LLM secret")
	}
	if len(h.llm.live) != 0 || len(h.llm.deleted) != 1 {
		t.Fatalf("minted key leaked: live=%v deleted=%v", h.llm.live, h.llm.deleted)
	}
	if _, reported := h.srv.reports["mr-abc"]; reported {
		t.Fatal("a failed reconcile must not report a state")
	}
}

func TestInvalidDaemonIDIsSkippedNotFatal(t *testing.T) {
	h := newHarness(t, false)
	h.srv.set(Runtime{ID: "x", WorkspaceID: "ws1", DaemonID: "!!!", Status: "offline"}, rt("online", 0, 1))
	h.step(0)
	if h.kb.sandboxes["mr-abc"] == nil {
		t.Fatal("a bad runtime must not block the others")
	}
}

func TestObjectName(t *testing.T) {
	for in, want := range map[string]string{"mr-abc": "mr-abc", "MR_Abc.1": "mr-abc-1"} {
		if got, err := ObjectName(in); err != nil || got != want {
			t.Errorf("ObjectName(%q)=%q,%v want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "---", string(make([]byte, 80))} {
		if _, err := ObjectName(bad); err == nil {
			t.Errorf("ObjectName(%q) should fail", bad)
		}
	}
}
