package sandboxbroker_test

// Runs against a real LiteLLM only when pointed at one, e.g. the local stack:
//
//	POOL_LITELLM_URL=http://127.0.0.1:14000 POOL_LITELLM_MASTER_KEY=<master key>
//
// POOL_LITELLM_MODEL names a model the task key may call (default mock-model).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxbroker"
)

func litellmDo(t *testing.T, method, base, path, bearer string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestTaskKeyAgainstRealLiteLLM(t *testing.T) {
	base, master := os.Getenv("POOL_LITELLM_URL"), os.Getenv("POOL_LITELLM_MASTER_KEY")
	if base == "" || master == "" {
		t.Skip("POOL_LITELLM_URL and POOL_LITELLM_MASTER_KEY not set")
	}
	model := os.Getenv("POOL_LITELLM_MODEL")
	if model == "" {
		model = "mock-model"
	}
	email := fmt.Sprintf("pool-it-%d@example.com", time.Now().UnixNano())
	taskID := fmt.Sprintf("task-it-%d", time.Now().UnixNano())

	h := newHarness(t, 1, func(c *sandboxbroker.PoolConfig) {
		c.LiteLLM = &sandboxbroker.LiteLLMConfig{URL: base, MasterKey: master, Models: []string{model}, MaxBudget: 1, RPMLimit: 30}
	})
	pod := h.newPod("127.0.0.2", "pool-0")
	pod.start()

	// An unknown user fails the task before any key is issued.
	h.upstream.enqueueTask("missing-user-task", email, nil)
	if pod.claimTask() != nil {
		t.Fatal("task started for a user LiteLLM does not know")
	}
	if msg, _ := h.upstream.failureOf("missing-user-task"); msg == "" {
		t.Fatal("task was not failed")
	}

	if status, body := litellmDo(t, http.MethodPost, base, "/user/new", master, map[string]any{"user_id": email, "user_email": email, "max_budget": 10}); status != http.StatusOK {
		t.Fatalf("create user: %d %s", status, body)
	}
	h.upstream.enqueueTask(taskID, email, map[string]string{"OPENAI_BASE_URL": "https://evil.example"})
	task := pod.claimTask()
	if task == nil {
		t.Fatal("task did not start")
	}
	env := envOf(t, task)
	key := env["OPENAI_API_KEY"]
	if key == "" || key == master || env["OPENAI_BASE_URL"] != base+"/v1" {
		t.Fatalf("bad injected env: %v", env)
	}

	chat := func(m string) int {
		status, _ := litellmDo(t, http.MethodPost, base, "/v1/chat/completions", key, map[string]any{
			"model": m, "messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		return status
	}
	if status := chat(model); status != http.StatusOK {
		t.Fatalf("allowed model: want 200, got %d", status)
	}
	if status := chat("not-" + model); status != http.StatusForbidden {
		t.Fatalf("unauthorized model: want 403, got %d", status)
	}

	pod.report(taskID, "complete")
	eventually(t, "key rejected after the task ended", func() bool { return chat(model) == http.StatusUnauthorized })

	// Spend logs are written with a delay.
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, raw := litellmDo(t, http.MethodGet, base, "/spend/logs?user_id="+email, master, nil)
		var logs []map[string]any
		if json.Unmarshal(raw, &logs) == nil {
			for _, l := range logs {
				if l["user"] == email && spendLogAlias(l) == taskID {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no usage record for %s with alias %s; last response: %s", email, taskID, raw)
		}
		time.Sleep(2 * time.Second)
	}
}

func spendLogAlias(l map[string]any) string {
	if s, ok := l["key_alias"].(string); ok {
		return s
	}
	m, _ := l["metadata"].(map[string]any)
	s, _ := m["user_api_key_alias"].(string)
	return s
}
