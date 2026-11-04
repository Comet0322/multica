package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultAnthropicModel is used when AnthropicConfig.Model is empty.
const DefaultAnthropicModel = "claude-haiku-5-5"

// AnthropicConfig configures the Messages API client.
type AnthropicConfig struct {
	// APIKey is sent as x-api-key. A LiteLLM virtual key works the same way.
	APIKey string
	// BaseURL defaults to https://api.anthropic.com. Point it at a LiteLLM proxy to
	// route through the company gateway; the path /v1/messages is appended.
	BaseURL string
	Model   string
	// MaxTokens bounds one reply; zero means 16000.
	MaxTokens int
	// MaxRetries is how many times a retryable failure is retried; zero means 5,
	// negative means none.
	MaxRetries int
	HTTP       *http.Client
	// sleep is replaced in tests.
	sleep func(context.Context, time.Duration) error
}

// Anthropic is a Model backed by the Anthropic Messages API. It does not stream:
// a reply is bounded by MaxTokens, which keeps one call well under the time a
// non-streaming request may take.
type Anthropic struct {
	cfg AnthropicConfig
}

// NewAnthropic validates the configuration and returns the client.
func NewAnthropic(cfg AnthropicConfig) (*Anthropic, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("anthropic: API key is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.anthropic.com"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Model == "" {
		cfg.Model = DefaultAnthropicModel
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 16000
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 5
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 10 * time.Minute}
	}
	if cfg.sleep == nil {
		cfg.sleep = sleepCtx
	}
	return &Anthropic{cfg: cfg}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Wire types of the Messages API, limited to what the harness uses.

type apiCacheControl struct {
	Type string `json:"type"`
}

type apiBlock struct {
	Type         string           `json:"type"`
	Text         string           `json:"text,omitempty"`
	ID           string           `json:"id,omitempty"`
	Name         string           `json:"name,omitempty"`
	Input        json.RawMessage  `json:"input,omitempty"`
	ToolUseID    string           `json:"tool_use_id,omitempty"`
	Content      string           `json:"content,omitempty"`
	IsError      bool             `json:"is_error,omitempty"`
	CacheControl *apiCacheControl `json:"cache_control,omitempty"`
}

type apiMessage struct {
	Role    string     `json:"role"`
	Content []apiBlock `json:"content"`
}

type apiTool struct {
	Name         string           `json:"name"`
	Description  string           `json:"description"`
	InputSchema  map[string]any   `json:"input_schema"`
	CacheControl *apiCacheControl `json:"cache_control,omitempty"`
}

type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    []apiBlock   `json:"system,omitempty"`
	Messages  []apiMessage `json:"messages"`
	Tools     []apiTool    `json:"tools,omitempty"`
}

type apiResponse struct {
	Model      string     `json:"model"`
	StopReason string     `json:"stop_reason"`
	Content    []apiBlock `json:"content"`
	Usage      struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

var ephemeral = &apiCacheControl{Type: "ephemeral"}

// buildRequest maps the conversation to the wire format. Cache breakpoints go on
// the system prompt, the last tool and the last block of the conversation, so
// each turn re-reads everything before it from the cache.
func (a *Anthropic) buildRequest(req Request) apiRequest {
	out := apiRequest{Model: a.cfg.Model, MaxTokens: a.cfg.MaxTokens}
	if req.System != "" {
		out.System = []apiBlock{{Type: "text", Text: req.System, CacheControl: ephemeral}}
	}
	for i, t := range req.Tools {
		at := apiTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema}
		if i == len(req.Tools)-1 {
			at.CacheControl = ephemeral
		}
		out.Tools = append(out.Tools, at)
	}
	for _, m := range req.Messages {
		am := apiMessage{Role: string(m.Role)}
		// tool_result blocks must come first in a user message.
		for _, r := range m.ToolResults {
			am.Content = append(am.Content, apiBlock{Type: "tool_result", ToolUseID: r.CallID, Content: r.Output, IsError: r.IsError})
		}
		if m.Text != "" {
			am.Content = append(am.Content, apiBlock{Type: "text", Text: m.Text})
		}
		for _, c := range m.ToolCalls {
			in, _ := json.Marshal(c.Input)
			if c.Input == nil {
				in = []byte("{}")
			}
			am.Content = append(am.Content, apiBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: in})
		}
		if len(am.Content) == 0 {
			continue
		}
		out.Messages = append(out.Messages, am)
	}
	if n := len(out.Messages); n > 0 {
		last := out.Messages[n-1].Content
		last[len(last)-1].CacheControl = ephemeral
	}
	return out
}

// APIError is a failed Messages API call.
type APIError struct {
	Status  int
	Type    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic: %d %s: %s", e.Status, e.Type, e.Message)
}

// retryable reports whether the same request may succeed later: rate limits,
// overload (529) and server errors. Anything else is the request's own fault.
func (e *APIError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == 529 || e.Status >= 500
}

func (a *Anthropic) Next(ctx context.Context, req Request) (Turn, error) {
	body, err := json.Marshal(a.buildRequest(req))
	if err != nil {
		return Turn{}, err
	}
	var lastErr error
	attempts := max(a.cfg.MaxRetries, 0) + 1
	for attempt := 0; attempt < attempts; attempt++ {
		resp, retryAfter, err := a.post(ctx, body)
		if err == nil {
			return a.turn(resp)
		}
		lastErr = err
		var apiErr *APIError
		transient := errors.As(err, &apiErr) && apiErr.retryable()
		if !transient && !isNetworkError(err) || ctx.Err() != nil {
			return Turn{}, err
		}
		if attempt == attempts-1 {
			break
		}
		delay := retryAfter
		if delay <= 0 {
			delay = backoff(attempt)
		}
		if err := a.cfg.sleep(ctx, delay); err != nil {
			return Turn{}, err
		}
	}
	return Turn{}, lastErr
}

// backoff is exponential from 1s, capped at 30s, with jitter so a fleet that was
// rate limited together does not retry together.
func backoff(attempt int) time.Duration {
	d := min(time.Second<<attempt, 30*time.Second)
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func isNetworkError(err error) bool {
	var apiErr *APIError
	return !errors.As(err, &apiErr) && !errors.Is(err, context.Canceled)
}

func (a *Anthropic) post(ctx context.Context, body []byte) (*apiResponse, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("x-api-key", a.cfg.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("content-type", "application/json")
	res, err := a.cfg.HTTP.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, 0, err
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		var after time.Duration
		if s, err := strconv.ParseFloat(res.Header.Get("retry-after"), 64); err == nil && s > 0 {
			after = min(time.Duration(s*float64(time.Second)), 2*time.Minute)
		}
		return nil, after, &APIError{Status: res.StatusCode, Type: e.Error.Type, Message: msg}
	}
	var out apiResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, 0, fmt.Errorf("anthropic: decode reply: %w", err)
	}
	return &out, 0, nil
}

func (a *Anthropic) turn(r *apiResponse) (Turn, error) {
	t := Turn{
		Model: r.Model,
		Usage: Usage{
			InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens,
			CacheReadTokens: r.Usage.CacheReadInputTokens, CacheWriteTokens: r.Usage.CacheCreationInputTokens,
		},
	}
	var text []string
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "tool_use":
			var in map[string]any
			if len(b.Input) > 0 {
				if err := json.Unmarshal(b.Input, &in); err != nil {
					return Turn{}, fmt.Errorf("anthropic: tool input of %s is not an object: %w", b.Name, err)
				}
			}
			t.ToolCalls = append(t.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Input: in})
		}
	}
	t.Text = strings.Join(text, "\n")
	// A reply cut off by the token limit may hold half a tool call or half an
	// answer; running on from it would act on text the model did not finish.
	if r.StopReason == "max_tokens" {
		return t, errors.New("anthropic: reply was cut off at max_tokens")
	}
	if r.StopReason == "refusal" {
		return t, errors.New("anthropic: the model declined to continue")
	}
	return t, nil
}
