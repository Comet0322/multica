package runtimecontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// LLMKeys mints and revokes a per-Sandbox LLM key. The key is what the agent
// process uses; the upstream provider keys stay behind the gateway.
type LLMKeys interface {
	Generate(ctx context.Context, req LLMKeyRequest) (string, error)
	Delete(ctx context.Context, key string) error
}

// LLMKeyRequest describes the limits and attribution of one Sandbox's key.
type LLMKeyRequest struct {
	Alias       string // stable name, e.g. the daemon id
	WorkspaceID string
	DaemonID    string
	RuntimeID   string
}

// LiteLLM implements LLMKeys with the LiteLLM proxy key-management API.
type LiteLLM struct {
	BaseURL  string
	AdminKey string
	Models   []string
	Budget   float64
	RPM      int
	Client   *http.Client
}

func NewLiteLLM(c Config) *LiteLLM {
	return &LiteLLM{BaseURL: c.LiteLLMURL, AdminKey: c.LiteLLMAdminKey, Models: c.LiteLLMModels,
		Budget: c.LiteLLMBudget, RPM: c.LiteLLMRPM, Client: &http.Client{Timeout: 20 * time.Second}}
}

func (l *LiteLLM) post(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.AdminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		// The body can echo request fields; it never contains the admin key.
		return fmt.Errorf("litellm %s: %d %s", path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (l *LiteLLM) Generate(ctx context.Context, r LLMKeyRequest) (string, error) {
	body := map[string]any{
		"key_alias": r.Alias,
		"metadata": map[string]string{
			"multica_workspace_id": r.WorkspaceID,
			"multica_runtime_id":   r.RuntimeID,
			"multica_daemon_id":    r.DaemonID,
		},
	}
	if len(l.Models) > 0 {
		body["models"] = l.Models
	}
	if l.Budget > 0 {
		body["max_budget"] = l.Budget
	}
	if l.RPM > 0 {
		body["rpm_limit"] = l.RPM
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := l.post(ctx, "/key/generate", body, &out); err != nil {
		return "", err
	}
	if out.Key == "" {
		return "", fmt.Errorf("litellm returned no key")
	}
	return out.Key, nil
}

func (l *LiteLLM) Delete(ctx context.Context, key string) error {
	return l.post(ctx, "/key/delete", map[string]any{"keys": []string{key}}, nil)
}
