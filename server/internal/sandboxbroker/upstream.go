package sandboxbroker

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// registrationIdentity is the machine identity the manager or broker presents
// upstream in place of the daemon's own.
type registrationIdentity struct {
	workspaceID, daemonID, name string
	// providers limits which of the daemon's agent CLIs become runtimes; empty
	// keeps every provider.
	providers []string
	// source names what was registered, for the error when nothing is left.
	source string
	// mode is the runtime_mode registered upstream: "cloud" when empty, "local"
	// for the pool manager, which then appears like any member's own daemon.
	mode string
}

// rewriteRegistration turns a daemon's captured registration into the
// upstream one: one machine identity, no custom profiles.
func rewriteRegistration(captured json.RawMessage, id registrationIdentity) (map[string]any, error) {
	var req map[string]any
	if err := json.Unmarshal(captured, &req); err != nil {
		return nil, fmt.Errorf("decode captured registration: %w", err)
	}
	runtimes, _ := req["runtimes"].([]any)
	var kept []any
	for _, item := range runtimes {
		rt, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := rt["type"].(string)
		if profile, _ := rt["profile_id"].(string); profile != "" {
			continue
		}
		if len(id.providers) > 0 && !slices.Contains(id.providers, typ) {
			continue
		}
		rt["runtime_mode"] = cmp.Or(id.mode, "cloud")
		rt["name"] = id.name
		if len(id.providers) != 1 {
			rt["name"] = id.name + " (" + typ + ")"
		}
		kept = append(kept, rt)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("%s has none of the configured providers %v", id.source, id.providers)
	}
	return map[string]any{
		"workspace_id": id.workspaceID,
		"daemon_id":    id.daemonID,
		"device_name":  id.name,
		"cli_version":  req["cli_version"],
		"runtimes":     kept,
	}, nil
}

type upstream struct {
	base, token string
	client      *http.Client
	// headers returns the daemon identity headers to replay on every call.
	headers func() map[string]string
}

type upstreamError struct {
	Status int
	Path   string
	// RuntimeNotFound is set on a 404 whose body says "runtime not found": the
	// runtime row is gone upstream (deleted in the UI, or collected after a
	// long time offline), as the stock daemon reads it.
	RuntimeNotFound bool
}

// Bodies are omitted: they may echo task content or credentials.
func (e *upstreamError) Error() string {
	return fmt.Sprintf("Multica %s returned HTTP %d", e.Path, e.Status)
}

func statusOf(err error) int {
	if e, ok := err.(*upstreamError); ok {
		return e.Status
	}
	return 0
}

// runtimeNotFound reports whether Multica answered that the runtime is gone.
func runtimeNotFound(err error) bool {
	e, ok := err.(*upstreamError)
	return ok && e.RuntimeNotFound
}

func newUpstream(base, token string, headers func() map[string]string) *upstream {
	return &upstream{
		base: base, token: token, headers: headers,
		client: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (u *upstream) call(ctx context.Context, method, path string, in, out any) error {
	return u.callWith(ctx, nil, method, path, in, out)
}

// callWith is call with extra request headers.
func (u *upstream) callWith(ctx context.Context, extra map[string]string, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, ok := in.(json.RawMessage)
		if !ok {
			var err error
			if raw, err = json.Marshal(in); err != nil {
				return err
			}
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.base+path, body)
	if err != nil {
		return err
	}
	for k, v := range u.headers() {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("Multica %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &upstreamError{Status: resp.StatusCode, Path: path}
		if resp.StatusCode == http.StatusNotFound {
			// Only this marker is kept; the body itself is never stored.
			head, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			e.RuntimeNotFound = strings.Contains(strings.ToLower(string(head)), "runtime not found")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return e
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return err
		}
		*raw = data
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}
