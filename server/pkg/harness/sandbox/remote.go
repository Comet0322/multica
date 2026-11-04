package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RemoteConfig points a Remote at a sandbox API.
type RemoteConfig struct {
	BaseURL string
	Token   string
	// Options is what to ask for when creating the sandbox.
	Options CreateOptions
	HTTP    *http.Client
}

// Remote is a Sandbox that lives behind the sandbox API. It is the only thing the
// agent service needs to know about where its sandboxes run.
type Remote struct {
	base  string
	token string
	id    string
	hc    *http.Client
}

// NewRemote creates a sandbox on the server and returns the handle to it.
func NewRemote(ctx context.Context, cfg RemoteConfig) (*Remote, error) {
	if cfg.BaseURL == "" || cfg.Token == "" {
		return nil, errors.New("remote sandbox: base URL and token are required")
	}
	r := &Remote{base: strings.TrimRight(cfg.BaseURL, "/"), token: cfg.Token, hc: cfg.HTTP}
	if r.hc == nil {
		// No overall timeout: how long a call may take is the caller's context.
		r.hc = &http.Client{}
	}
	var created CreatedSandbox
	if err := r.json(ctx, http.MethodPost, "/v1/sandboxes", cfg.Options, &created); err != nil {
		return nil, fmt.Errorf("remote sandbox: create: %w", err)
	}
	if created.ID == "" {
		return nil, errors.New("remote sandbox: server returned no id")
	}
	r.id = created.ID
	return r, nil
}

// ID is the server's name for the sandbox, for logs.
func (r *Remote) ID() string { return r.id }

// APIError is a failure answered by the sandbox API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("sandbox API answered %d: %s", e.Status, e.Message)
}

func (r *Remote) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var we WireError
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &we) == nil && we.Error != "" {
			msg = we.Error
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}
	return resp, nil
}

func (r *Remote) json(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := r.do(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r *Remote) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	secs := int((req.Timeout + time.Second - 1) / time.Second)
	if req.Timeout > 0 {
		// The server enforces the timeout and kills the command; this only
		// bounds the wait if the server itself is gone.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout+time.Minute)
		defer cancel()
	}
	var out WireExecResult
	err := r.json(ctx, http.MethodPost, "/v1/sandboxes/"+r.id+"/exec",
		WireExecRequest{Command: req.Command, Cwd: req.Cwd, Env: req.Env, TimeoutSec: secs}, &out)
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode, TimedOut: out.TimedOut}, nil
}

func (r *Remote) Put(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	q := url.Values{"path": {path}, "mode": {strconv.FormatUint(uint64(mode.Perm()), 8)}}
	resp, err := r.do(ctx, http.MethodPut, "/v1/sandboxes/"+r.id+"/files?"+q.Encode(), bytes.NewReader(data), "application/octet-stream")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (r *Remote) Get(ctx context.Context, path string) ([]byte, error) {
	resp, err := r.do(ctx, http.MethodGet, "/v1/sandboxes/"+r.id+"/files?"+url.Values{"path": {path}}.Encode(), nil, "")
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, fmt.Errorf("remote sandbox: get %s: %w", path, fs.ErrNotExist)
		}
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Close deletes the sandbox. One that is already gone counts as closed.
func (r *Remote) Close(ctx context.Context) error {
	err := r.json(ctx, http.MethodDelete, "/v1/sandboxes/"+r.id, nil, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}
