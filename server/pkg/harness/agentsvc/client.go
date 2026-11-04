package agentsvc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client talks to the agent service.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// APIError is a request the service refused.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("agent service answered %d: %s", e.Status, e.Message)
}

// ErrNoResult means the stream ended before the run's result: the service died
// or the connection broke, so how the run ended is unknown.
var ErrNoResult = errors.New("agent service: the stream ended without a result")

// Run starts a run and calls handle for each event, in order, until the result
// arrives. Canceling ctx cancels the run on the service. It returns the result,
// or an error if the run could not start or the stream broke.
func (c *Client) Run(ctx context.Context, req RunRequest, handle func(Event)) (*Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/runs", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Authorization", "Bearer "+c.Token)
	hr.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{} // no overall timeout: a run lasts as long as it lasts
	}
	resp, err := hc.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var we WireError
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &we) == nil && we.Error != "" {
			msg = we.Error
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("agent service: bad event: %w", err)
		}
		if handle != nil {
			handle(ev)
		}
		if ev.Type == EventResult && ev.Result != nil {
			return ev.Result, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("agent service: %w", err)
	}
	return nil, ErrNoResult
}
