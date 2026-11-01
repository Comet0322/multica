package sandboxmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Placeholders usable in LiteLLMConfig.Env values.
const (
	// LiteLLMKeyPlaceholder is replaced by the task's key.
	LiteLLMKeyPlaceholder = "${LITELLM_KEY}"
	// LiteLLMURLPlaceholder is replaced by LiteLLMConfig.PodURL.
	LiteLLMURLPlaceholder = "${LITELLM_URL}"
)

// LiteLLMConfig makes the pool manager issue every task a short-lived LiteLLM
// key owned by the task's initiator. The master key stays in the manager;
// pods only ever see the task's key.
type LiteLLMConfig struct {
	// URL is the LiteLLM base URL the manager calls.
	URL string
	// PodURL is the base URL injected into agents; empty means URL.
	PodURL string
	// MasterKey is the administrative credential, supplied at runtime.
	MasterKey string
	// Models are the model names a task key may call. At least one.
	Models []string
	// MaxBudget and RPMLimit cap one task's key. Both are required: a key
	// without caps would be limited only by the initiator's own LiteLLM
	// budget.
	MaxBudget float64
	RPMLimit  int
	// TaskTimeout is the longest a task runs, and Margin the slack after it;
	// the key expires TaskTimeout+Margin after issue, whatever else happens.
	// Defaults: 2h and 15m.
	TaskTimeout time.Duration
	Margin      time.Duration
	// Env lists the agent environment variables the manager forces, after the
	// agent's own settings are merged. Values may use the placeholders above.
	// Names must not start with MULTICA_. Empty selects DefaultLiteLLMEnv.
	Env map[string]string
}

// DefaultLiteLLMEnv points the Anthropic and OpenAI SDK families at LiteLLM.
var DefaultLiteLLMEnv = map[string]string{
	"ANTHROPIC_BASE_URL":   LiteLLMURLPlaceholder,
	"ANTHROPIC_AUTH_TOKEN": LiteLLMKeyPlaceholder,
	"OPENAI_BASE_URL":      LiteLLMURLPlaceholder + "/v1",
	"OPENAI_API_KEY":       LiteLLMKeyPlaceholder,
}

func (c *LiteLLMConfig) normalize() error {
	if err := checkBaseURL(c.URL); err != nil {
		return fmt.Errorf("LiteLLM URL: %w", err)
	}
	c.URL = strings.TrimRight(c.URL, "/")
	if c.PodURL == "" {
		c.PodURL = c.URL
	}
	if err := checkBaseURL(c.PodURL); err != nil {
		return fmt.Errorf("LiteLLM pod URL: %w", err)
	}
	c.PodURL = strings.TrimRight(c.PodURL, "/")
	if strings.TrimSpace(c.MasterKey) == "" {
		return errors.New("LiteLLM master key is required")
	}
	if len(c.Models) == 0 {
		return errors.New("LiteLLM models must list at least one model")
	}
	if c.MaxBudget < 0 || c.RPMLimit < 0 || c.TaskTimeout < 0 || c.Margin < 0 {
		return errors.New("LiteLLM budget, rate limit and durations must not be negative")
	}
	// A task key without its own caps is limited only by the initiator's
	// budget, and a leaked key could spend all of it at any rate.
	if c.MaxBudget == 0 {
		return errors.New("LiteLLM task budget must be set (a per-task budget above zero)")
	}
	if c.RPMLimit == 0 {
		return errors.New("LiteLLM rate limit must be set (a per-task requests-per-minute limit above zero)")
	}
	if c.TaskTimeout == 0 {
		c.TaskTimeout = 2 * time.Hour
	}
	if c.Margin == 0 {
		c.Margin = 15 * time.Minute
	}
	if len(c.Env) == 0 {
		c.Env = DefaultLiteLLMEnv
	}
	for name := range c.Env {
		if name == "" || strings.HasPrefix(strings.ToUpper(name), "MULTICA_") {
			return fmt.Errorf("LiteLLM env name %q must be set and must not start with MULTICA_", name)
		}
	}
	return nil
}

// llmFailure is a LiteLLM problem phrased for the task's failure message.
type llmFailure struct{ msg string }

func (e *llmFailure) Error() string { return e.msg }

// litellm is the manager's LiteLLM admin client.
type litellm struct {
	cfg    LiteLLMConfig
	client *http.Client
	wg     sync.WaitGroup
}

func newLiteLLM(cfg LiteLLMConfig) *litellm {
	return &litellm{cfg: cfg, client: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// taskKey is an issued key; the secret never leaves memory and logs.
type taskKey struct{ sealed string }

// call performs one admin request. A non-2xx answer is returned as status with
// a short error detail; err is set only when LiteLLM could not be reached.
func (l *litellm) call(ctx context.Context, method, path string, in, out any, taskKeys ...string) (status int, detail string, err error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, "", err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.cfg.URL+path, body)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+l.cfg.MasterKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("LiteLLM %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, errorDetail(data, append([]string{l.cfg.MasterKey}, taskKeys...)), nil
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, "", fmt.Errorf("LiteLLM %s: unreadable response", path)
		}
	}
	return resp.StatusCode, "", nil
}

// errorDetail extracts LiteLLM's error text and bounds its length. LiteLLM
// can echo what it was sent, so the known secrets (the master key and the
// task key involved, if any) are removed before the text is cut, and so
// before it reaches an error, a log line, the audit trail or a task's failure
// message.
func errorDetail(data []byte, sealeds []string) string {
	var e struct {
		Detail any `json:"detail"`
		Error  any `json:"error"`
	}
	text := ""
	if json.Unmarshal(data, &e) == nil {
		for _, v := range []any{e.Detail, e.Error} {
			switch t := v.(type) {
			case string:
				text = t
			case map[string]any:
				if m, ok := t["message"].(string); ok {
					text = m
				}
			}
			if text != "" {
				break
			}
		}
	}
	for _, sealed := range sealeds {
		if sealed != "" {
			text = strings.ReplaceAll(text, sealed, "[redacted]")
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return text
}

// issue checks that the initiator exists in LiteLLM with budget left, then
// issues a task key owned by them. Failures are *llmFailure with the reason
// for the task's failure message; nothing falls back to a shared key.
func (l *litellm) issue(ctx context.Context, ref claimedTaskRef, initiator string) (taskKey, error) {
	var info struct {
		UserInfo struct {
			MaxBudget *float64 `json:"max_budget"`
			Spend     float64  `json:"spend"`
		} `json:"user_info"`
	}
	status, detail, err := l.call(ctx, http.MethodGet, "/user/info?user_id="+url.QueryEscape(initiator), nil, &info)
	switch {
	case err != nil:
		return taskKey{}, &llmFailure{"LiteLLM is unreachable, so no model key could be issued for this task"}
	case status == http.StatusNotFound:
		return taskKey{}, &llmFailure{fmt.Sprintf("LiteLLM has no user %s; ask an administrator to create the user in LiteLLM", initiator)}
	case status != http.StatusOK:
		return taskKey{}, &llmFailure{fmt.Sprintf("LiteLLM refused the user lookup for %s (HTTP %d%s)", initiator, status, withDetail(detail))}
	}
	if b := info.UserInfo.MaxBudget; b != nil && info.UserInfo.Spend >= *b {
		return taskKey{}, &llmFailure{fmt.Sprintf("the LiteLLM budget of %s is used up (spent %.4g of %.4g)", initiator, info.UserInfo.Spend, *b)}
	}

	body := map[string]any{
		"user_id":   initiator,
		"key_alias": ref.ID,
		"duration":  fmt.Sprintf("%ds", int((l.cfg.TaskTimeout+l.cfg.Margin)/time.Second)),
		"models":    l.cfg.Models,
		"metadata": map[string]string{
			"task": ref.ID, "issue": ref.IssueID, "workspace": ref.WorkspaceID, "agent": ref.AgentID,
		},
	}
	if l.cfg.MaxBudget > 0 {
		body["max_budget"] = l.cfg.MaxBudget
	}
	if l.cfg.RPMLimit > 0 {
		body["rpm_limit"] = l.cfg.RPMLimit
	}
	var issued struct {
		Key string `json:"key"`
	}
	status, detail, err = l.call(ctx, http.MethodPost, "/key/generate", body, &issued)
	switch {
	case err != nil:
		return taskKey{}, &llmFailure{"LiteLLM is unreachable, so no model key could be issued for this task"}
	case status != http.StatusOK || issued.Key == "":
		return taskKey{}, &llmFailure{fmt.Sprintf("LiteLLM did not issue a key for %s (HTTP %d%s)", initiator, status, withDetail(detail))}
	}
	return taskKey{sealed: issued.Key}, nil
}

func withDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// revokeOrphans deletes the task keys of a workspace that an earlier manager
// issued: keys whose metadata names a task of that workspace. It is meant for
// the first registration after the manager started, when no task of this
// manager can hold a key yet, so every such key is an orphan. Keys without
// task metadata (people's own keys) are never touched.
func (l *litellm) revokeOrphans(ctx context.Context, workspaceID string) (int, error) {
	var aliases []string
	for page := 1; page <= 100; page++ {
		var out struct {
			Keys []struct {
				Alias    string         `json:"key_alias"`
				Metadata map[string]any `json:"metadata"`
			} `json:"keys"`
			TotalPages int `json:"total_pages"`
		}
		status, detail, err := l.call(ctx, http.MethodGet, fmt.Sprintf("/key/list?return_full_object=true&size=100&page=%d", page), nil, &out)
		if err != nil {
			return 0, err
		}
		if status != http.StatusOK {
			return 0, fmt.Errorf("LiteLLM /key/list returned HTTP %d%s", status, withDetail(detail))
		}
		for _, k := range out.Keys {
			task, _ := k.Metadata["task"].(string)
			ws, _ := k.Metadata["workspace"].(string)
			if k.Alias != "" && task != "" && ws == workspaceID {
				aliases = append(aliases, k.Alias)
			}
		}
		if page >= out.TotalPages {
			break
		}
	}
	if len(aliases) == 0 {
		return 0, nil
	}
	status, detail, err := l.call(ctx, http.MethodPost, "/key/delete", map[string]any{"key_aliases": aliases}, nil)
	if err != nil {
		return 0, err
	}
	if status < 200 || status > 299 {
		return 0, fmt.Errorf("LiteLLM /key/delete returned HTTP %d%s", status, withDetail(detail))
	}
	return len(aliases), nil
}

// revokeWithRetry deletes the key, retrying a few times; a key that outlives
// every attempt still expires on its own.
func (l *litellm) revokeWithRetry(key taskKey, log func(error)) {
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		status, detail, err := l.call(ctx, http.MethodPost, "/key/delete", map[string]any{"keys": []string{key.sealed}}, nil, key.sealed)
		cancel()
		switch {
		case err != nil:
			last = errors.New("LiteLLM unreachable")
		case status < 200 || status > 299:
			last = fmt.Errorf("LiteLLM /key/delete returned HTTP %d%s", status, withDetail(detail))
		default:
			return
		}
		time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
	}
	log(last)
}

// revokeAsync deletes the key in the background: lease bookkeeping runs under
// the pool lock and must not wait on the network.
func (l *litellm) revokeAsync(key taskKey, log func(error)) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.revokeWithRetry(key, log)
	}()
}

// inject returns the claimed task with the manager's environment forced into
// agent.custom_env, after whatever the agent configured, so the user cannot
// redirect or replace the LiteLLM settings.
func (l *litellm) inject(raw json.RawMessage, key taskKey) (json.RawMessage, error) {
	var task map[string]json.RawMessage
	if err := json.Unmarshal(raw, &task); err != nil {
		return nil, err
	}
	var agent map[string]json.RawMessage
	if a := task["agent"]; len(a) > 0 && string(a) != "null" {
		if err := json.Unmarshal(a, &agent); err != nil {
			return nil, err
		}
	}
	if agent == nil {
		agent = map[string]json.RawMessage{}
	}
	env := map[string]string{}
	if e := agent["custom_env"]; len(e) > 0 && string(e) != "null" {
		if err := json.Unmarshal(e, &env); err != nil {
			return nil, err
		}
	}
	repl := strings.NewReplacer(LiteLLMKeyPlaceholder, key.sealed, LiteLLMURLPlaceholder, l.cfg.PodURL)
	for name, value := range l.cfg.Env {
		env[name] = repl.Replace(value)
	}
	var err error
	if agent["custom_env"], err = json.Marshal(env); err != nil {
		return nil, err
	}
	if task["agent"], err = json.Marshal(agent); err != nil {
		return nil, err
	}
	return json.Marshal(task)
}

// issueTaskKey issues the claimed task's LiteLLM key and returns the task body
// with it injected. The key is revoked again if injection fails.
func (p *Pool) issueTaskKey(ctx context.Context, raw json.RawMessage, ref claimedTaskRef) (json.RawMessage, *taskKey, error) {
	key, err := p.llm.issue(ctx, ref, ref.initiator())
	if err != nil {
		return nil, nil, err
	}
	out, err := p.llm.inject(raw, key)
	if err != nil {
		p.revokeKey(&key, ref.ID)
		return nil, nil, err
	}
	return out, &key, nil
}

// revokeKey deletes a task's key in the background. A nil key is ignored.
func (p *Pool) revokeKey(key *taskKey, taskID string) {
	if key == nil || p.llm == nil {
		return
	}
	p.llm.revokeAsync(*key, func(err error) {
		p.log.Error("revoke task key; it expires on its own", "task_id", taskID, "error", err)
	})
}
