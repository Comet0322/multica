package sandboxmanager_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

func TestLiteLLMNeedsBudgetAndRateCaps(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t)
	cases := []struct {
		name      string
		budget    float64
		rpm       int
		wantError string
	}{
		{"both set", 5, 60, ""},
		{"no budget", 0, 60, "budget"},
		{"no rate limit", 5, 0, "rate limit"},
		{"neither", 0, 0, "budget"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := sandboxmanager.NewPool(sandboxmanager.PoolConfig{
				MulticaURL: "http://127.0.0.1:1", MulticaTicket: harnessPAT, WorkspaceID: harnessWorkspaceID,
				LiteLLM: &sandboxmanager.LiteLLMConfig{
					URL: llm.URL, MasterKey: fakeLiteLLMMaster, Models: []string{"m"},
					MaxBudget: c.budget, RPMLimit: c.rpm,
				},
			}, nil)
			switch {
			case c.wantError == "" && err != nil:
				t.Fatalf("valid config refused: %v", err)
			case c.wantError != "" && (err == nil || !strings.Contains(err.Error(), c.wantError)):
				t.Fatalf("want an error naming the %s, got %v", c.wantError, err)
			}
		})
	}
}

func TestLiteLLMErrorTextEchoingTheMasterKeyIsRedacted(t *testing.T) {
	t.Parallel()
	llm := newFakeLiteLLM(t, "alice@example.com")
	llm.generateStatus = http.StatusBadRequest
	llm.generateMessage = "invalid request from " + fakeLiteLLMMaster + " for alice"
	h := newHarness(t, 1, withLiteLLM(llm))
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	if pod.claimTask() != nil {
		t.Fatal("task started without a key")
	}
	msg, _ := h.upstream.failureOf("t1")
	if !strings.Contains(msg, "invalid request") {
		t.Fatalf("the reason should survive redaction, got %q", msg)
	}
	for name, got := range map[string]string{"failure message": msg, "logs": h.logs.String(), "audit": h.audit.String()} {
		if strings.Contains(got, fakeLiteLLMMaster) {
			t.Errorf("%s echoes the master key: %s", name, got)
		}
	}
}

func TestSelfReportedPodIPIsNeverEchoedUnparsed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.reportedIP = "<script>alert(1)</script>\nforged log line"

	status, body := pod.register()

	if status != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", status, body)
	}
	for name, got := range map[string]string{"response": string(body), "logs": h.logs.String()} {
		if strings.Contains(got, "script") || strings.Contains(got, "forged") {
			t.Errorf("%s reflects the reported value: %q", name, got)
		}
		if !strings.Contains(got, "127.0.0.2") {
			t.Errorf("%s does not name the source address: %q", name, got)
		}
	}
}
