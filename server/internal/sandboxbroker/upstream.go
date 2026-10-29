package sandboxbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type upstream struct {
	base, token string
	client      *http.Client
	// headers returns the daemon identity headers to replay on every call.
	headers func() map[string]string
}

type upstreamError struct {
	Status int
	Path   string
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
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return &upstreamError{Status: resp.StatusCode, Path: path}
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
