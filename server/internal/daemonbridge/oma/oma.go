// Package oma is a daemonbridge Backend that runs tasks on Open Managed Agents.
//
// OMA runs the model loop and the sandbox. This package only drives its HTTP
// API: one session per task, a per-task vault holding the bearer the bridge's
// MCP tool server expects, and a poll of the session's event log. No credential
// the model needs is handled here: the LLM key lives in OMA, and the MCP bearer
// is stored in an OMA vault, which keeps it out of the sandbox.
package oma

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

	"github.com/multica-ai/multica/server/internal/daemonbridge"
)

const (
	betaHeader  = "managed-agents-2026-04-01"
	metaTaskKey = "multica_task_id"
	metaAgent   = "multica_agent_key"
)

// Config configures the backend.
type Config struct {
	BaseURL string // OMA API root, for example http://127.0.0.1:18800
	Auth    string // OMA API key; empty when OMA runs with auth disabled
	// EnvironmentID is the OMA environment sessions run in. It must allow MCP
	// servers (networking.allow_mcp_servers) so the model can reach the tools.
	EnvironmentID string
	// Model is used when the Multica agent names none.
	Model string
	// Version is shown as the runtime's agent version.
	Version string
	HTTP    *http.Client
}

// Backend implements daemonbridge.Backend on OMA.
type Backend struct {
	cfg Config
	hc  *http.Client

	mu     sync.Mutex
	agents map[string]string // agent key -> OMA agent id
}

// New returns a Backend.
func New(cfg Config) (*Backend, error) {
	if cfg.BaseURL == "" || cfg.EnvironmentID == "" || cfg.Model == "" {
		return nil, errors.New("oma: BaseURL, EnvironmentID and Model are required")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Version == "" {
		cfg.Version = "oma"
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Backend{cfg: cfg, hc: hc, agents: map[string]string{}}, nil
}

func (b *Backend) Capabilities() daemonbridge.Capabilities {
	return daemonbridge.Capabilities{
		Provider:   "claude",
		Version:    b.cfg.Version,
		Supplement: true,
		Model:      true,
		Skills:     true,
	}
}

func (b *Backend) Models(context.Context) ([]daemonbridge.Model, error) {
	return []daemonbridge.Model{{ID: b.cfg.Model, Label: b.cfg.Model, Provider: "anthropic", Default: true}}, nil
}

type opaque struct {
	VaultID string `json:"vault_id,omitempty"`
}

// Start creates the session for the task, or re-attaches to the one an earlier
// attempt created: sessions carry the task ID in their metadata.
func (b *Backend) Start(ctx context.Context, job daemonbridge.Job) (daemonbridge.Handle, error) {
	if job.MCP == nil {
		return daemonbridge.Handle{}, daemonbridge.ErrUnsupported
	}
	if sid, vault, ok, err := b.findSession(ctx, job.TaskID); err != nil {
		return daemonbridge.Handle{}, err
	} else if ok {
		return handle(sid, vault), nil
	}

	agentID, err := b.ensureAgent(ctx, job)
	if err != nil {
		return daemonbridge.Handle{}, err
	}
	vault, err := b.createVault(ctx, job)
	if err != nil {
		return daemonbridge.Handle{}, err
	}
	var ses struct {
		ID string `json:"id"`
	}
	body := map[string]any{
		"agent":          agentID,
		"environment_id": b.cfg.EnvironmentID,
		"vault_ids":      []string{vault},
		"metadata":       map[string]string{metaTaskKey: job.TaskID, "multica_vault_id": vault},
	}
	if err := b.do(ctx, http.MethodPost, "/v1/sessions", body, &ses); err != nil {
		_ = b.deleteVault(ctx, vault)
		return daemonbridge.Handle{}, err
	}
	h := handle(ses.ID, vault)
	if err := b.Send(ctx, h, firstMessage(job)); err != nil {
		return daemonbridge.Handle{}, err
	}
	return h, nil
}

// firstMessage is the brief, the agent's skills and the first prompt. OMA has no
// skill directory the daemon prompt points at, so skills travel as text.
func firstMessage(job daemonbridge.Job) string {
	var sb strings.Builder
	sb.WriteString(job.System)
	if len(job.Agent.Skills) > 0 {
		sb.WriteString("\n\n## Skills\n")
		for _, s := range job.Agent.Skills {
			fmt.Fprintf(&sb, "\n### Skill: %s\n", s.Name)
			if s.Description != "" {
				sb.WriteString(s.Description + "\n\n")
			}
			sb.WriteString(s.Content + "\n")
			for _, f := range s.Files {
				fmt.Fprintf(&sb, "\n#### %s\n```\n%s\n```\n", f.Path, f.Content)
			}
		}
	}
	sb.WriteString("\n\n" + job.Prompt)
	return sb.String()
}

func handle(sid, vault string) daemonbridge.Handle {
	raw, _ := json.Marshal(opaque{VaultID: vault})
	return daemonbridge.Handle{SessionID: sid, Opaque: raw}
}

// findSession looks for a session an earlier Start created for this task.
func (b *Backend) findSession(ctx context.Context, taskID string) (sid, vault string, ok bool, err error) {
	page := ""
	for i := 0; i < 20; i++ {
		var out struct {
			Data []struct {
				ID       string            `json:"id"`
				Metadata map[string]string `json:"metadata"`
			} `json:"data"`
			NextPage string `json:"next_page"`
		}
		q := url.Values{"limit": {"100"}}
		if page != "" {
			q.Set("page", page)
		}
		if err := b.do(ctx, http.MethodGet, "/v1/sessions?"+q.Encode(), nil, &out); err != nil {
			return "", "", false, err
		}
		for _, s := range out.Data {
			if s.Metadata[metaTaskKey] == taskID {
				return s.ID, s.Metadata["multica_vault_id"], true, nil
			}
		}
		if out.NextPage == "" {
			return "", "", false, nil
		}
		page = out.NextPage
	}
	return "", "", false, nil
}

// ensureAgent returns the OMA agent for a Multica agent. The per-task brief
// travels in the first message, so one OMA agent serves every task of an agent
// and nothing is versioned per task.
func (b *Backend) ensureAgent(ctx context.Context, job daemonbridge.Job) (string, error) {
	model := job.Agent.Model
	if model == "" {
		model = b.cfg.Model
	}
	key := job.AgentID + "|" + model + "|" + job.MCP.URL
	b.mu.Lock()
	id, ok := b.agents[key]
	b.mu.Unlock()
	if ok {
		return id, nil
	}
	if id, err := b.findAgent(ctx, key); err != nil {
		return "", err
	} else if id != "" {
		b.remember(key, id)
		return id, nil
	}
	allow := map[string]any{"enabled": true, "permission_policy": map[string]string{"type": "always_allow"}}
	var created struct {
		ID string `json:"id"`
	}
	err := b.do(ctx, http.MethodPost, "/v1/agents", map[string]any{
		"name":        "multica-" + job.AgentID,
		"model":       model,
		"system":      "You work on Multica tasks. Follow the task brief you are given and use the multica tools to read and write Multica.",
		"metadata":    map[string]string{metaAgent: key},
		"mcp_servers": []map[string]string{{"type": "url", "name": "multica", "url": job.MCP.URL}},
		"tools": []map[string]any{
			{"type": "agent_toolset_20260401", "default_config": allow},
			{"type": "mcp_toolset", "mcp_server_name": "multica", "default_config": allow},
		},
	}, &created)
	if err != nil {
		return "", err
	}
	b.remember(key, created.ID)
	return created.ID, nil
}

func (b *Backend) remember(key, id string) {
	b.mu.Lock()
	b.agents[key] = id
	b.mu.Unlock()
}

func (b *Backend) findAgent(ctx context.Context, key string) (string, error) {
	page := ""
	for i := 0; i < 20; i++ {
		var out struct {
			Data []struct {
				ID       string            `json:"id"`
				Metadata map[string]string `json:"metadata"`
			} `json:"data"`
			NextPage string `json:"next_page"`
		}
		q := url.Values{"limit": {"100"}}
		if page != "" {
			q.Set("page", page)
		}
		if err := b.do(ctx, http.MethodGet, "/v1/agents?"+q.Encode(), nil, &out); err != nil {
			return "", err
		}
		for _, a := range out.Data {
			if a.Metadata[metaAgent] == key {
				return a.ID, nil
			}
		}
		if out.NextPage == "" {
			return "", nil
		}
		page = out.NextPage
	}
	return "", nil
}

// createVault stores the task's MCP bearer where only OMA's host side reads it.
func (b *Backend) createVault(ctx context.Context, job daemonbridge.Job) (string, error) {
	var v struct {
		ID string `json:"id"`
	}
	if err := b.do(ctx, http.MethodPost, "/v1/vaults", map[string]any{
		"display_name": "multica-task-" + job.TaskID,
		"metadata":     map[string]string{metaTaskKey: job.TaskID},
	}, &v); err != nil {
		return "", err
	}
	err := b.do(ctx, http.MethodPost, "/v1/vaults/"+v.ID+"/credentials", map[string]any{
		"display_name": "multica-mcp",
		"auth":         map[string]string{"type": "static_bearer", "mcp_server_url": job.MCP.URL, "token": job.MCP.Auth},
	}, nil)
	if err != nil {
		_ = b.deleteVault(ctx, v.ID)
		return "", err
	}
	return v.ID, nil
}

func (b *Backend) deleteVault(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	return ignoreGone(b.do(ctx, http.MethodDelete, "/v1/vaults/"+id, nil, nil))
}

type omaEvent struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Name       string          `json:"name"`
	ToolUseID  string          `json:"tool_use_id"`
	Input      map[string]any  `json:"input"`
	Content    json.RawMessage `json:"content"`
	IsError    bool            `json:"is_error"`
	StopReason *struct {
		Type string `json:"type"`
	} `json:"stop_reason"`
	ModelUsage *struct {
		In         int64 `json:"input_tokens"`
		Out        int64 `json:"output_tokens"`
		CacheRead  int64 `json:"cache_read_input_tokens"`
		CacheWrite int64 `json:"cache_creation_input_tokens"`
	} `json:"model_usage"`
	Error json.RawMessage `json:"error"`
}

// Events reads the session's log after cursor, which is the ID of the last
// event already delivered.
func (b *Backend) Events(ctx context.Context, h daemonbridge.Handle, cursor string) (daemonbridge.Batch, error) {
	all, err := b.allEvents(ctx, h.SessionID)
	if err != nil {
		return daemonbridge.Batch{}, err
	}
	start := 0
	if cursor != "" {
		for i, e := range all {
			if e.ID == cursor {
				start = i + 1
				break
			}
		}
	}
	batch := daemonbridge.Batch{Cursor: cursor}
	for i := start; i < len(all); i++ {
		e := all[i]
		batch.Cursor = e.ID
		switch {
		case e.Type == "agent.message":
			batch.Events = append(batch.Events, daemonbridge.Event{ID: e.ID, Kind: daemonbridge.EventText, Text: text(e.Content)})
		case e.Type == "agent.thinking":
			batch.Events = append(batch.Events, daemonbridge.Event{ID: e.ID, Kind: daemonbridge.EventThinking, Text: text(e.Content)})
		case strings.HasSuffix(e.Type, "tool_use") && strings.HasPrefix(e.Type, "agent."):
			batch.Events = append(batch.Events, daemonbridge.Event{ID: e.ID, Kind: daemonbridge.EventToolUse, Tool: e.Name, CallID: e.ID, Input: e.Input})
		case strings.HasSuffix(e.Type, "tool_result") && strings.HasPrefix(e.Type, "agent."):
			batch.Events = append(batch.Events, daemonbridge.Event{ID: e.ID, Kind: daemonbridge.EventToolResult, CallID: e.ToolUseID, Output: text(e.Content)})
		case e.Type == "span.model_request_end" && e.ModelUsage != nil:
			batch.Events = append(batch.Events, daemonbridge.Event{ID: e.ID, Kind: daemonbridge.EventUsage, Usage: []daemonbridge.Usage{{
				Provider: "anthropic", Model: b.cfg.Model,
				InputTokens: e.ModelUsage.In, OutputTokens: e.ModelUsage.Out,
				CacheReadTokens: e.ModelUsage.CacheRead, CacheWriteTokens: e.ModelUsage.CacheWrite,
			}}})
		case e.Type == "session.error":
			batch.Events = append(batch.Events, daemonbridge.Event{ID: e.ID, Kind: daemonbridge.EventError, Text: string(e.Error)})
		case e.Type == "session.status_terminated":
			ev := daemonbridge.Done(0, daemonbridge.Outcome{Status: daemonbridge.OutcomeFailed, Error: "the OMA session terminated"})
			ev.ID = e.ID
			batch.Events = append(batch.Events, ev)
		case e.Type == "session.status_idle" && e.StopReason != nil && e.StopReason.Type == "end_turn":
			// A finished turn: the last agent message is the answer.
			out := daemonbridge.Outcome{Status: daemonbridge.OutcomeCompleted, Output: lastAnswer(all[:i+1])}
			ev := daemonbridge.Done(0, out)
			ev.ID = e.ID
			batch.Events = append(batch.Events, ev)
		}
	}
	return batch, nil
}

// lastAnswer is the text of the last agent message of the final turn.
func lastAnswer(events []omaEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "agent.message" {
			return text(events[i].Content)
		}
	}
	return ""
}

func (b *Backend) allEvents(ctx context.Context, sid string) ([]omaEvent, error) {
	var all []omaEvent
	page := ""
	for i := 0; i < 200; i++ {
		var out struct {
			Data     []omaEvent `json:"data"`
			NextPage string     `json:"next_page"`
		}
		q := url.Values{"limit": {"100"}, "order": {"asc"}}
		if page != "" {
			q.Set("page", page)
		}
		if err := b.do(ctx, http.MethodGet, "/v1/sessions/"+sid+"/events?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)
		if out.NextPage == "" {
			break
		}
		page = out.NextPage
	}
	return all, nil
}

func (b *Backend) Send(ctx context.Context, h daemonbridge.Handle, input string) error {
	return b.do(ctx, http.MethodPost, "/v1/sessions/"+h.SessionID+"/events", map[string]any{
		"events": []map[string]any{{"type": "user.message", "content": []map[string]string{{"type": "text", "text": input}}}},
	}, nil)
}

// ToolResult is unused: tools are served over MCP.
func (b *Backend) ToolResult(context.Context, daemonbridge.Handle, string, daemonbridge.ToolResult) error {
	return daemonbridge.ErrUnsupported
}

func (b *Backend) Interrupt(ctx context.Context, h daemonbridge.Handle) error {
	return ignoreGone(b.do(ctx, http.MethodPost, "/v1/sessions/"+h.SessionID+"/events", map[string]any{
		"events": []map[string]any{{"type": "user.interrupt"}},
	}, nil))
}

// Release deletes the session, which frees its sandbox, and the task's vault.
func (b *Backend) Release(ctx context.Context, h daemonbridge.Handle) error {
	var o opaque
	_ = json.Unmarshal(h.Opaque, &o)
	err := ignoreGone(b.do(ctx, http.MethodDelete, "/v1/sessions/"+h.SessionID, nil, nil))
	if verr := b.deleteVault(ctx, o.VaultID); err == nil {
		err = verr
	}
	return err
}

// text joins the text blocks of a message or tool result.
func text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("oma: HTTP %d: %s", e.Status, e.Body) }

func ignoreGone(err error) error {
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}

func (b *Backend) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	return b.send(ctx, method, path, "application/json", body, out)
}

func (b *Backend) send(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, b.cfg.BaseURL+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("content-type", contentType)
	}
	req.Header.Set("anthropic-beta", betaHeader)
	if b.cfg.Auth != "" {
		req.Header.Set("x-api-key", b.cfg.Auth)
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound && strings.Contains(path, "/events") {
			return fmt.Errorf("%w: %s", daemonbridge.ErrGone, strings.TrimSpace(string(raw)))
		}
		return &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}
