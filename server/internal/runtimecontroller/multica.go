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

// Runtime is one managed runtime as the server reports it.
type Runtime struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	DaemonID    string `json:"daemon_id"`
	Provider    string `json:"provider"`
	Status      string `json:"status"` // the daemon's truth: online/offline
	DisplayName string `json:"display_name"`
	State       string `json:"state"` // last state this controller reported
	QueuedTasks int    `json:"queued_tasks"`
	ActiveTasks int    `json:"active_tasks"`
}

// Token is a freshly minted daemon token.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Server is the controller's view of the Multica internal API.
type Server interface {
	ManagedRuntimes(ctx context.Context) ([]Runtime, error)
	IssueDaemonToken(ctx context.Context, workspaceID, daemonID string, ttl time.Duration) (Token, error)
	RevokeDaemonTokens(ctx context.Context, workspaceID, daemonID string) error
	ReportStates(ctx context.Context, states []StateReport) error
}

// StateReport is one runtime's Sandbox state.
type StateReport struct {
	WorkspaceID string `json:"workspace_id"`
	DaemonID    string `json:"daemon_id"`
	State       string `json:"state"`
}

// HTTPServer implements Server against /internal/v1.
type HTTPServer struct {
	BaseURL string
	Secret  string
	Client  *http.Client
}

func NewHTTPServer(baseURL, secret string) *HTTPServer {
	return &HTTPServer{BaseURL: baseURL, Secret: secret, Client: &http.Client{Timeout: 20 * time.Second}}
}

func (s *HTTPServer) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.Secret)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (s *HTTPServer) ManagedRuntimes(ctx context.Context) ([]Runtime, error) {
	var out struct {
		Runtimes []Runtime `json:"runtimes"`
	}
	if err := s.do(ctx, http.MethodGet, "/internal/v1/managed-runtimes/state", nil, &out); err != nil {
		return nil, err
	}
	return out.Runtimes, nil
}

func (s *HTTPServer) IssueDaemonToken(ctx context.Context, workspaceID, daemonID string, ttl time.Duration) (Token, error) {
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	err := s.do(ctx, http.MethodPost, "/internal/v1/daemon-tokens", map[string]any{
		"workspace_id": workspaceID, "daemon_id": daemonID, "ttl_seconds": int(ttl.Seconds()),
	}, &out)
	if err != nil {
		return Token{}, err
	}
	exp, _ := time.Parse(time.RFC3339, out.ExpiresAt)
	return Token{Value: out.Token, ExpiresAt: exp}, nil
}

func (s *HTTPServer) RevokeDaemonTokens(ctx context.Context, workspaceID, daemonID string) error {
	return s.do(ctx, http.MethodDelete, "/internal/v1/daemon-tokens", map[string]any{
		"workspace_id": workspaceID, "daemon_id": daemonID,
	}, nil)
}

func (s *HTTPServer) ReportStates(ctx context.Context, states []StateReport) error {
	if len(states) == 0 {
		return nil
	}
	return s.do(ctx, http.MethodPut, "/internal/v1/managed-runtimes/state", map[string]any{"runtimes": states}, nil)
}
