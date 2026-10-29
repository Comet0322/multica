package sandboxprovider

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
	"time"
)

// HTTP is a Provider backed by a sandbox service implementing the v1 API in
// deploy/sandbox/CONTRACT.md, so sandbox infrastructure can be operated
// separately from the broker.
type HTTP struct {
	base, token string
	client      *http.Client
}

type HTTPConfig struct {
	BaseURL string
	Token   string
	// HTTPClient overrides the default client (30s timeout).
	HTTPClient *http.Client
}

func NewHTTP(cfg HTTPConfig) (*HTTP, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("sandbox service URL must be an http(s) URL")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("sandbox service token is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTP{base: strings.TrimRight(cfg.BaseURL, "/") + "/v1/sandboxes", token: cfg.Token, client: client}, nil
}

// wireSandbox is the v1 sandbox object.
type wireSandbox struct {
	Key    string `json:"key"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Image  string `json:"image,omitempty"`
}

func (w wireSandbox) sandbox() Sandbox {
	s := Sandbox{Key: w.Key, Status: Status(w.Status), Reason: w.Reason, Image: w.Image}
	switch s.Status {
	case StatusPending, StatusRunning, StatusStopped, StatusFailed, StatusMissing:
	default:
		// Forward compatibility: wait on states this client does not know.
		s.Status, s.Reason = StatusPending, "unknown status "+w.Status
	}
	return s
}

type wireSpec struct {
	Image  string            `json:"image"`
	Env    map[string]string `json:"env"`
	Labels map[string]string `json:"labels,omitempty"`
}

// ServiceError is a non-2xx response from the sandbox service.
type ServiceError struct {
	Status  int
	Message string
}

func (e *ServiceError) Error() string {
	return fmt.Sprintf("sandbox service returned HTTP %d: %s", e.Status, e.Message)
}

func (h *HTTP) Ensure(ctx context.Context, spec Spec) (Sandbox, error) {
	if !ValidKey(spec.Key) {
		return Sandbox{}, ErrInvalidKey
	}
	var out wireSandbox
	if _, err := h.do(ctx, http.MethodPut, "/"+spec.Key, wireSpec{Image: spec.Image, Env: spec.Env, Labels: spec.Labels}, &out); err != nil {
		return Sandbox{}, err
	}
	out.Key = spec.Key
	return out.sandbox(), nil
}

func (h *HTTP) Get(ctx context.Context, key string) (Sandbox, error) {
	if !ValidKey(key) {
		return Sandbox{}, ErrInvalidKey
	}
	var out wireSandbox
	status, err := h.do(ctx, http.MethodGet, "/"+key, nil, &out)
	if status == http.StatusNotFound {
		return Sandbox{Key: key, Status: StatusMissing}, nil
	}
	if err != nil {
		return Sandbox{}, err
	}
	out.Key = key
	return out.sandbox(), nil
}

func (h *HTTP) Stop(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return ErrInvalidKey
	}
	_, err := h.do(ctx, http.MethodPost, "/"+key+"/stop", nil, nil)
	return err
}

func (h *HTTP) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return ErrInvalidKey
	}
	_, err := h.do(ctx, http.MethodDelete, "/"+key, nil, nil)
	return err
}

func (h *HTTP) List(ctx context.Context) ([]Sandbox, error) {
	var out struct {
		Sandboxes []wireSandbox `json:"sandboxes"`
	}
	if _, err := h.do(ctx, http.MethodGet, "", nil, &out); err != nil {
		return nil, err
	}
	list := make([]Sandbox, 0, len(out.Sandboxes))
	for _, w := range out.Sandboxes {
		list = append(list, w.sandbox())
	}
	return list, nil
}

// do returns the response status so callers can interpret 404 themselves.
func (h *HTTP) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("sandbox service %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, &ServiceError{Status: resp.StatusCode, Message: e.Error}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode sandbox service response: %w", err)
		}
	}
	return resp.StatusCode, nil
}
