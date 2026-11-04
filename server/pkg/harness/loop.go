// Package harness is the loop that turns a model and a set of tools into an
// agent: ask the model, run the tools it asks for, show it the results, repeat.
//
// It knows nothing about the daemon, about Multica, or about where tools run.
// It is given a model.Model and a tools.Registry and reports what happens as
// Events. That is the whole reason the sandbox can be on the other side of an
// API: the loop never touches it.
package harness

import (
	"context"
	"errors"
	"fmt"

	"github.com/multica-ai/multica/server/pkg/harness/model"
	"github.com/multica-ai/multica/server/pkg/harness/tools"
)

// EventKind identifies what happened.
type EventKind string

const (
	EventText       EventKind = "text"
	EventToolUse    EventKind = "tool-use"
	EventToolResult EventKind = "tool-result"
	EventError      EventKind = "error"
)

// Event is one thing that happened in the loop, in order.
type Event struct {
	Kind    EventKind
	Text    string         // EventText, EventError
	Tool    string         // EventToolUse, EventToolResult
	CallID  string         // EventToolUse, EventToolResult
	Input   map[string]any // EventToolUse
	Output  string         // EventToolResult
	IsError bool           // EventToolResult
}

// Config is one run of the loop.
type Config struct {
	Model model.Model
	Tools *tools.Registry
	// System is the system prompt.
	System string
	// MaxTurns bounds model calls; zero means DefaultMaxTurns.
	MaxTurns int
	// Emit receives events as they happen. It must not block for long, and it
	// may be nil.
	Emit func(Event)
}

// DefaultMaxTurns is the bound when Config.MaxTurns is zero.
const DefaultMaxTurns = 100

// Status values of an Outcome.
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Outcome is how a run ended.
type Outcome struct {
	Status string
	// Output is the model's final text.
	Output string
	Error  string
	Turns  int
	Usage  map[string]model.Usage // by the model that answered
}

// Run plays the conversation from the task prompt until the model stops asking
// for tools, the turn limit is hit, the context ends, or something breaks.
func Run(ctx context.Context, cfg Config, prompt string) Outcome {
	emit := cfg.Emit
	if emit == nil {
		emit = func(Event) {}
	}
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	out := Outcome{Usage: map[string]model.Usage{}}
	finish := func(status, errMsg string) Outcome {
		out.Status, out.Error = status, errMsg
		if errMsg != "" {
			emit(Event{Kind: EventError, Text: errMsg})
		}
		return out
	}
	stopped := func(err error) Outcome {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return finish(StatusCancelled, err.Error())
		}
		return finish(StatusFailed, err.Error())
	}

	var specs []model.ToolSpec
	if cfg.Tools != nil {
		for _, s := range cfg.Tools.Specs() {
			specs = append(specs, model.ToolSpec{Name: s.Name, Description: s.Description, Schema: s.Schema})
		}
	}
	msgs := []model.Message{{Role: model.User, Text: prompt}}

	for out.Turns < maxTurns {
		if err := ctx.Err(); err != nil {
			return stopped(err)
		}
		turn, err := cfg.Model.Next(ctx, model.Request{System: cfg.System, Messages: msgs, Tools: specs})
		if err != nil {
			return stopped(fmt.Errorf("model: %w", err))
		}
		out.Turns++
		u := out.Usage[turn.Model]
		u.InputTokens += turn.Usage.InputTokens
		u.OutputTokens += turn.Usage.OutputTokens
		u.CacheReadTokens += turn.Usage.CacheReadTokens
		u.CacheWriteTokens += turn.Usage.CacheWriteTokens
		out.Usage[turn.Model] = u

		if turn.Text != "" {
			emit(Event{Kind: EventText, Text: turn.Text})
			out.Output = turn.Text
		}
		msgs = append(msgs, model.Message{Role: model.Assistant, Text: turn.Text, ToolCalls: turn.ToolCalls})
		if len(turn.ToolCalls) == 0 {
			return finish(StatusCompleted, "")
		}

		results := make([]model.ToolResult, 0, len(turn.ToolCalls))
		for _, call := range turn.ToolCalls {
			emit(Event{Kind: EventToolUse, Tool: call.Name, CallID: call.ID, Input: call.Input})
			if cfg.Tools == nil {
				return finish(StatusFailed, "model asked for a tool but none are configured")
			}
			res, err := cfg.Tools.Run(ctx, call.Name, call.Input)
			if err != nil {
				return stopped(fmt.Errorf("tool %s: %w", call.Name, err))
			}
			emit(Event{Kind: EventToolResult, Tool: call.Name, CallID: call.ID, Output: res.Output, IsError: res.IsError})
			results = append(results, model.ToolResult{CallID: call.ID, Output: res.Output, IsError: res.IsError})
		}
		msgs = append(msgs, model.Message{Role: model.User, ToolResults: results})
	}
	return finish(StatusFailed, fmt.Sprintf("stopped after %d turns without finishing", maxTurns))
}
