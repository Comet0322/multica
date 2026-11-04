// Package tools is what the model can do. A tool takes the model's JSON input
// and returns text; it never sees the model, the loop or the daemon.
//
// There are two families, and they share nothing but the Tool interface. The
// sandbox tools (this file's siblings) reach the world only through a
// sandbox.Sandbox. The Multica tools in package multicatools run on the host
// with the task's credentials and never touch the sandbox. Keeping them apart
// is what keeps credentials out of the sandbox.
package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Spec is what the model is told about a tool.
type Spec struct {
	Name        string
	Description string
	// Schema is the JSON Schema of the input object.
	Schema map[string]any
}

// Result is what a tool returns to the model. A failure the model can act on
// (a missing file, a non-unique edit) is a Result with IsError, not a Go error:
// it goes back to the model as the answer. A Go error from Run means the tool
// itself broke and ends the task.
type Result struct {
	Output  string
	IsError bool
}

// Tool is one capability the model can call.
type Tool interface {
	Spec() Spec
	Run(ctx context.Context, input map[string]any) (Result, error)
}

// Registry maps tool names to tools.
type Registry struct {
	byName map[string]Tool
}

// NewRegistry returns a registry holding the given tools. A repeated name is a
// programming error, so it panics rather than letting one tool shadow another.
func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		r.Add(t)
	}
	return r
}

// Add registers a tool.
func (r *Registry) Add(t Tool) {
	name := t.Spec().Name
	if _, dup := r.byName[name]; dup {
		panic(fmt.Sprintf("tools: %q registered twice", name))
	}
	r.byName[name] = t
}

// Specs lists the tools in a stable order.
func (r *Registry) Specs() []Spec {
	out := make([]Spec, 0, len(r.byName))
	for _, t := range r.byName {
		out = append(out, t.Spec())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Run calls the named tool. An unknown name is the model's mistake, so it comes
// back as an error Result for the model to correct.
func (r *Registry) Run(ctx context.Context, name string, input map[string]any) (Result, error) {
	t, ok := r.byName[name]
	if !ok {
		return Result{Output: fmt.Sprintf("unknown tool %q", name), IsError: true}, nil
	}
	return t.Run(ctx, input)
}

func fail(format string, args ...any) (Result, error) {
	return Result{Output: fmt.Sprintf(format, args...), IsError: true}, nil
}

func stringArg(in map[string]any, key string) (string, bool) {
	s, ok := in[key].(string)
	return s, ok
}

func intArg(in map[string]any, key string, def int) int {
	switch v := in[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func boolArg(in map[string]any, key string) bool {
	b, _ := in[key].(bool)
	return b
}

// shellQuote makes s one literal word for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// truncate keeps the head and the tail of long output, which is where a build
// or test log puts the cause and the verdict.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	half := limit / 2
	return s[:half] + fmt.Sprintf("\n... [%d bytes omitted] ...\n", len(s)-limit) + s[len(s)-half:]
}
