package sandboxmanager_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

func TestTaskKeyIsRequestedForTheInitiatorWithEveryField(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t, "alice@example.com")
	h := newHarness(t, 1, withLiteLLM(llm))
	h.upstream.enqueueTask("t1", "Alice@Example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	if pod.claimTask() == nil {
		t.Fatal("task was not handed to the pod")
	}
	gen := llm.generatedBodies()
	if len(gen) != 1 {
		t.Fatalf("want 1 key request, got %d", len(gen))
	}
	body := gen[0]
	if body["user_id"] != "alice@example.com" || body["key_alias"] != "t1" {
		t.Fatalf("wrong owner or alias: %v", body)
	}
	meta, _ := body["metadata"].(map[string]any)
	want := map[string]string{"task": "t1", "issue": "issue-t1", "workspace": harnessWorkspaceID, "agent": "agent-1"}
	for k, v := range want {
		if meta[k] != v {
			t.Fatalf("metadata %s = %v, want %s (all: %v)", k, meta[k], v, meta)
		}
	}
	if models, _ := body["models"].([]any); len(models) != 1 || models[0] != "mock-model" {
		t.Fatalf("wrong models: %v", body["models"])
	}
	if body["max_budget"] != 5.0 || body["rpm_limit"] != 60.0 {
		t.Fatalf("wrong budget/rate: %v", body)
	}
	// Task timeout plus margin: 70 minutes.
	if body["duration"] != "4200s" {
		t.Fatalf("wrong duration: %v", body["duration"])
	}
}

func TestKeyAndBaseURLAreForcedOverTheAgentsOwnEnv(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t, "alice@example.com")
	h := newHarness(t, 1, withLiteLLM(llm))
	h.upstream.enqueueTask("t1", "alice@example.com", map[string]string{
		"ANTHROPIC_BASE_URL": "https://evil.example",
		"OPENAI_API_KEY":     "sk-shared",
		"KEEP_ME":            "yes",
	})
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	task := pod.claimTask()
	env := envOf(t, task)
	key := llm.keyOf("t1")
	if key == "" {
		t.Fatal("no key issued")
	}
	if env["ANTHROPIC_BASE_URL"] != "http://litellm.pool.svc:4000" || env["OPENAI_BASE_URL"] != "http://litellm.pool.svc:4000/v1" {
		t.Fatalf("base URLs not forced: %v", env)
	}
	if env["OPENAI_API_KEY"] != key || env["ANTHROPIC_AUTH_TOKEN"] != key {
		t.Fatalf("task key not forced: %v", env)
	}
	if env["KEEP_ME"] != "yes" {
		t.Fatalf("the agent's other variables were lost: %v", env)
	}
	if agent, _ := task["agent"].(map[string]any); agent["instructions"] != "keep" {
		t.Fatalf("other agent fields were lost: %v", agent)
	}
	for name := range env {
		if hasPrefixFold(name, "MULTICA_") {
			t.Fatalf("injected variable %s starts with MULTICA_", name)
		}
	}
	if strings.Contains(h.logs.String(), key) || strings.Contains(h.audit.String(), key) {
		t.Fatal("the task key leaked into logs or the audit trail")
	}
}

func TestMulticaPrefixedEnvNamesAreRejected(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t)
	_, err := sandboxmanager.NewPool(sandboxmanager.PoolConfig{
		MulticaURL: "http://127.0.0.1:1", MulticaToken: harnessPAT, WorkspaceID: harnessWorkspaceID,
		LiteLLM: &sandboxmanager.LiteLLMConfig{
			URL: llm.URL, MasterKey: fakeLiteLLMMaster, Models: []string{"m"}, MaxBudget: 5, RPMLimit: 60,
			Env: map[string]string{"MULTICA_TOKEN": sandboxmanager.LiteLLMKeyPlaceholder},
		},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "MULTICA_") {
		t.Fatal("want an error for a MULTICA_ variable name")
	}
}

func TestKeyIsRevokedWhenTheTaskCompletesOrFails(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t, "alice@example.com", "bob@example.com")
	h := newHarness(t, 2, withLiteLLM(llm))
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	h.upstream.enqueueTask("t2", "bob@example.com", nil)
	a := h.newPod("127.0.0.2", "pool-0")
	b := h.newPod("127.0.0.3", "pool-1")
	a.start()
	b.start()
	a.claimTask()
	b.claimTask()
	k1, k2 := llm.keyOf("t1"), llm.keyOf("t2")
	if llm.isDeleted(k1) || llm.isDeleted(k2) {
		t.Fatal("keys revoked while the tasks run")
	}

	a.report("t1", "complete")
	b.report("t2", "fail")
	eventually(t, "both keys revoked", func() bool { return llm.isDeleted(k1) && llm.isDeleted(k2) })
}

func TestKeyIsRevokedWhenTheLeasePodIsReplaced(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t, "alice@example.com")
	h := newHarness(t, 1, withLiteLLM(llm))
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.nonce = "boot-1"
	pod.start()
	pod.claimTask()
	key := llm.keyOf("t1")

	pod.nonce = "boot-2"
	pod.start()
	eventually(t, "key revoked after the pod was replaced", func() bool { return llm.isDeleted(key) })
}

func TestUnknownInitiatorFailsTheTaskWithoutIssuingAKey(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t) // nobody exists
	h := newHarness(t, 1, withLiteLLM(llm))
	h.upstream.enqueueTask("t1", "ghost@example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	if task := pod.claimTask(); task != nil {
		t.Fatalf("task started: %v", task)
	}
	msg, ok := h.upstream.failureOf("t1")
	if !ok || !strings.Contains(msg, "ghost@example.com") || !strings.Contains(msg, "LiteLLM") {
		t.Fatalf("failure should name the missing LiteLLM user, got %q (failed=%v)", msg, ok)
	}
	if llm.generateCount() != 0 {
		t.Fatal("a key was issued for a user LiteLLM does not know")
	}
	if inv := h.inventory(); inv.Idle != 1 || inv.Leased != 0 {
		t.Fatalf("pod should stay idle, got %+v", inv)
	}
}

func TestExhaustedBudgetFailsTheTask(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t, "alice@example.com")
	llm.setBudget("alice@example.com", 10, 10)
	h := newHarness(t, 1, withLiteLLM(llm))
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	if pod.claimTask() != nil {
		t.Fatal("task started despite an exhausted budget")
	}
	msg, _ := h.upstream.failureOf("t1")
	if !strings.Contains(msg, "budget") {
		t.Fatalf("failure should mention the budget, got %q", msg)
	}
	if llm.generateCount() != 0 {
		t.Fatal("a key was issued")
	}
}

func TestKeyRefusedByLiteLLMFailsTheTaskWithTheReason(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t, "alice@example.com")
	llm.generateStatus = http.StatusBadRequest
	h := newHarness(t, 1, withLiteLLM(llm))
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	if pod.claimTask() != nil {
		t.Fatal("task started without a key")
	}
	msg, _ := h.upstream.failureOf("t1")
	if !strings.Contains(msg, "budget exceeded") {
		t.Fatalf("failure should carry LiteLLM's reason, got %q", msg)
	}
}

func TestUnreachableLiteLLMFailsTheTaskAndStartsNothing(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // nothing listens any more
	llm := &fakeLiteLLM{}
	llm.Server = down
	h := newHarness(t, 1, withLiteLLM(llm))
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	if pod.claimTask() != nil {
		t.Fatal("task started while LiteLLM was down")
	}
	msg, ok := h.upstream.failureOf("t1")
	if !ok || !strings.Contains(msg, "unreachable") {
		t.Fatalf("failure should say LiteLLM is unreachable, got %q", msg)
	}
	if h.inventory().Leased != 0 {
		t.Fatal("a pod was leased")
	}
}

func TestConcurrentTasksOnTwoPodsGetTheirOwnKeys(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t, "alice@example.com", "bob@example.com")
	h := newHarness(t, 2, withLiteLLM(llm))
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	h.upstream.enqueueTask("t2", "bob@example.com", nil)
	a := h.newPod("127.0.0.2", "pool-0")
	b := h.newPod("127.0.0.3", "pool-1")
	a.start()
	b.start()

	ta, tb := a.claimTask(), b.claimTask()
	ea, eb := envOf(t, ta), envOf(t, tb)
	k1, k2 := llm.keyOf("t1"), llm.keyOf("t2")
	if k1 == "" || k2 == "" || k1 == k2 {
		t.Fatalf("want two distinct keys, got %q and %q", k1, k2)
	}
	if ta["id"] != "t1" || ea["OPENAI_API_KEY"] != k1 || eb["OPENAI_API_KEY"] != k2 {
		t.Fatalf("keys crossed: %v / %v", ea, eb)
	}
	owners := map[string]any{}
	for _, g := range llm.generatedBodies() {
		owners[g["key_alias"].(string)] = g["user_id"]
	}
	if owners["t1"] != "alice@example.com" || owners["t2"] != "bob@example.com" {
		t.Fatalf("wrong owners: %v", owners)
	}
}

func TestWithoutLiteLLMConfiguredTasksAreUntouched(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueueTask("t1", "alice@example.com", map[string]string{"X": "1"})
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	env := envOf(t, pod.claimTask())
	if len(env) != 1 || env["X"] != "1" {
		t.Fatalf("env was altered: %v", env)
	}
}
