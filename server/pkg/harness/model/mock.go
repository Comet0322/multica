package model

import (
	"context"
	"fmt"
	"sync"
)

// Step produces one scripted turn. It sees the request, so a step can react to
// what the previous tool returned (for example, pass an id it created along).
type Step func(req Request) Turn

// Say is a step that answers with text and stops.
func Say(text string) Step {
	return func(Request) Turn {
		return Turn{Text: text, Usage: Usage{InputTokens: 10, OutputTokens: 5}, Model: "mock"}
	}
}

// Call is a step that asks for one tool.
func Call(id, name string, input map[string]any) Step {
	return func(Request) Turn {
		return Turn{ToolCalls: []ToolCall{{ID: id, Name: name, Input: input}}, Usage: Usage{InputTokens: 10, OutputTokens: 5}, Model: "mock"}
	}
}

// CallWith is a step that builds its tool call from the request.
func CallWith(f func(req Request) ToolCall) Step {
	return func(req Request) Turn {
		return Turn{ToolCalls: []ToolCall{f(req)}, Usage: Usage{InputTokens: 10, OutputTokens: 5}, Model: "mock"}
	}
}

// Mock is a Model that replays a script. It exists so the loop, the tools and
// the daemon integration can be exercised end to end with no credentials, and
// so a test can assert exactly what the model was shown.
type Mock struct {
	mu       sync.Mutex
	steps    []Step
	next     int
	requests []Request
}

// NewMock returns a Mock that plays the steps in order.
func NewMock(steps ...Step) *Mock { return &Mock{steps: steps} }

// Next plays the next step. Running past the end of the script is an error: a
// loop that keeps asking has not stopped when the test expected it to.
func (m *Mock) Next(ctx context.Context, req Request) (Turn, error) {
	if err := ctx.Err(); err != nil {
		return Turn{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	if m.next >= len(m.steps) {
		return Turn{}, fmt.Errorf("mock model: script of %d steps exhausted", len(m.steps))
	}
	step := m.steps[m.next]
	m.next++
	return step(req), nil
}

// Requests returns every request received, in order.
func (m *Mock) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.requests...)
}
