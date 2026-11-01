package sandboxmanager_test

// A fake LiteLLM for the task-lifecycle harness: it knows users, records the
// keys issued and deleted, and can be made to fail.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

const fakeLiteLLMMaster = "sk-fake-master"

type fakeLiteLLM struct {
	*httptest.Server

	mu sync.Mutex
	// users maps user_id to {max_budget, spend}; absent users answer 404.
	users map[string][2]float64
	// generated holds the /key/generate request bodies in order; keys the
	// secrets handed out, by task ID (key_alias).
	generated []map[string]any
	keys      map[string]string
	deleted   []string
	// generateStatus, when non-zero, makes /key/generate fail with it.
	generateStatus int
	// generateMessage, when set, is the error text of that failure.
	generateMessage string
	// userInfoCalls counts /user/info lookups.
	userInfoCalls int
	// existing are keys already in LiteLLM when the manager starts (what an
	// earlier manager left behind), as /key/list shows them.
	existing []map[string]any
	// deletedAliases are the key_aliases passed to /key/delete.
	deletedAliases []string
}

// seedKey adds a key that an earlier manager issued (or any other key) to what
// /key/list returns. An empty task gives a key without task metadata.
func (f *fakeLiteLLM) seedKey(alias, workspace, task string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	meta := map[string]any{}
	if task != "" {
		meta = map[string]any{"task": task, "workspace": workspace}
	}
	f.existing = append(f.existing, map[string]any{"key_alias": alias, "metadata": meta})
}

func (f *fakeLiteLLM) aliasDeleted(alias string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.deletedAliases {
		if a == alias {
			return true
		}
	}
	return false
}

func newFakeLiteLLM(t *testing.T, users ...string) *fakeLiteLLM {
	t.Helper()
	f := &fakeLiteLLM{users: map[string][2]float64{}, keys: map[string]string{}}
	for _, u := range users {
		f.users[u] = [2]float64{0, 0}
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeLiteLLM) setBudget(user string, max, spend float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[user] = [2]float64{max, spend}
}

func (f *fakeLiteLLM) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+fakeLiteLLMMaster {
		http.Error(w, `{"error":{"message":"bad master key"}}`, http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/user/info":
		f.userInfoCalls++
		id := r.URL.Query().Get("user_id")
		u, ok := f.users[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"detail": map[string]any{"error": "User not found, passed user_id=" + id}})
			return
		}
		info := map[string]any{"user_id": id, "spend": u[1], "max_budget": nil}
		if u[0] > 0 {
			info["max_budget"] = u[0]
		}
		writeJSON(w, map[string]any{"user_id": id, "user_info": info})
	case "/key/generate":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.generateStatus != 0 {
			w.WriteHeader(f.generateStatus)
			message := "budget exceeded"
			if f.generateMessage != "" {
				message = f.generateMessage
			}
			writeJSON(w, map[string]any{"error": map[string]any{"message": message}})
			return
		}
		f.generated = append(f.generated, body)
		alias, _ := body["key_alias"].(string)
		key := fmt.Sprintf("sk-task-%s-%d", alias, len(f.generated))
		f.keys[alias] = key
		writeJSON(w, map[string]any{"key": key})
	case "/key/list":
		writeJSON(w, map[string]any{"keys": f.existing, "total_count": len(f.existing), "current_page": 1, "total_pages": 1})
	case "/key/delete":
		var body struct {
			Keys    []string `json:"keys"`
			Aliases []string `json:"key_aliases"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.deleted = append(f.deleted, body.Keys...)
		f.deletedAliases = append(f.deletedAliases, body.Aliases...)
		writeJSON(w, map[string]any{"deleted_keys": body.Keys})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeLiteLLM) generatedBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.generated...)
}

func (f *fakeLiteLLM) keyOf(task string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys[task]
}

func (f *fakeLiteLLM) isDeleted(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.deleted {
		if k == key {
			return true
		}
	}
	return false
}

func (f *fakeLiteLLM) generateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.generated)
}

func (f *fakeLiteLLM) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.userInfoCalls
}

// withLiteLLM configures the pool manager to issue task keys from the fake.
func withLiteLLM(f *fakeLiteLLM, mutate ...func(*sandboxmanager.LiteLLMConfig)) func(*sandboxmanager.PoolConfig) {
	return func(c *sandboxmanager.PoolConfig) {
		cfg := &sandboxmanager.LiteLLMConfig{
			URL: f.URL, PodURL: "http://litellm.pool.svc:4000", MasterKey: fakeLiteLLMMaster,
			Models: []string{"mock-model"}, MaxBudget: 5, RPMLimit: 60,
			TaskTimeout: time.Hour, Margin: 10 * time.Minute,
		}
		for _, m := range mutate {
			m(cfg)
		}
		c.LiteLLM = cfg
	}
}

// enqueueTask adds a task with the given initiator and the agent's own env.
func (f *fakeMultica) enqueueTask(id, initiator string, customEnv map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	task := map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID, "issue_id": "issue-" + id,
		"initiator_email": initiator,
		"agent":           map[string]any{"id": "agent-1", "name": "A", "instructions": "keep", "custom_env": customEnv},
	}
	f.queue = append(f.queue, task)
}

// claimTask plays the daemon's machine-level claim and returns the raw task.
func (p *simPod) claimTask() map[string]any {
	p.h.t.Helper()
	status, raw := p.call(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{
		"daemon_id": "pod-" + p.name, "runtime_ids": []string{"runtime-a"}, "max_tasks": 4,
	})
	if status != http.StatusOK {
		p.h.t.Fatalf("claim: status %d: %s", status, raw)
	}
	var resp struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		p.h.t.Fatal(err)
	}
	if len(resp.Tasks) == 0 {
		return nil
	}
	return resp.Tasks[0]
}

func envOf(t *testing.T, task map[string]any) map[string]string {
	t.Helper()
	agent, _ := task["agent"].(map[string]any)
	raw, _ := agent["custom_env"].(map[string]any)
	out := map[string]string{}
	for k, v := range raw {
		out[k], _ = v.(string)
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hasPrefixFold(s, prefix string) bool {
	return strings.HasPrefix(strings.ToUpper(s), prefix)
}
