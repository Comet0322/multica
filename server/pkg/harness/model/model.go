// Package model is the harness's view of the language model: one call that
// takes the conversation so far and returns the next assistant turn. Providers
// (and the scripted Mock) implement Model; nothing above it knows which.
package model

import "context"

// ToolSpec is a tool as the model is told about it.
type ToolSpec struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ToolCall is the model asking for a tool to be run.
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// ToolResult answers one ToolCall.
type ToolResult struct {
	CallID  string
	Output  string
	IsError bool
}

// Role of a conversation message.
type Role string

const (
	// User messages carry the task prompt and, after a tool turn, the tool results.
	User Role = "user"
	// Assistant messages are the model's own turns.
	Assistant Role = "assistant"
)

// Message is one entry of the conversation. An Assistant message holds Text
// and/or ToolCalls; a User message holds Text and/or ToolResults.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// Request is everything the model sees for one turn.
type Request struct {
	System   string
	Messages []Message
	Tools    []ToolSpec
}

// Usage counts tokens for one call. Input excludes cache reads and writes.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Turn is the model's reply. No ToolCalls means it considers the task done.
type Turn struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
	// Model is the model that actually answered, for usage accounting.
	Model string
}

// Model produces the next assistant turn.
type Model interface {
	Next(ctx context.Context, req Request) (Turn, error)
}
