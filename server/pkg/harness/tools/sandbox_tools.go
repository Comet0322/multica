package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
)

const (
	maxOutputBytes    = 30_000
	defaultBashSecs   = 120
	maxBashSecs       = 600
	defaultReadLines  = 2000
	maxSearchMatches  = 200
	searchTimeoutSecs = 60
)

// Sandboxed is the set of tools that act through one sandbox, inside one
// working directory. Relative paths from the model are resolved against Workdir.
type Sandboxed struct {
	SB      sandbox.Sandbox
	Workdir string
}

// Tools returns bash, read_file, write_file, edit_file and search.
func (s Sandboxed) Tools() []Tool {
	return []Tool{bash{s}, readFile{s}, writeFile{s}, editFile{s}, search{s}}
}

func (s Sandboxed) abs(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(s.Workdir, p)
}

func obj(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

// bash

type bash struct{ Sandboxed }

func (bash) Spec() Spec {
	return Spec{
		Name:        "bash",
		Description: "Run a shell command in the sandbox and return its output and exit code. The sandbox has no network.",
		Schema: obj(map[string]any{
			"command":     str("The command line, interpreted by sh."),
			"timeout_sec": num(fmt.Sprintf("Seconds before the command is killed (default %d, max %d).", defaultBashSecs, maxBashSecs)),
		}, "command"),
	}
}

func (b bash) Run(ctx context.Context, in map[string]any) (Result, error) {
	cmd, ok := stringArg(in, "command")
	if !ok || strings.TrimSpace(cmd) == "" {
		return fail("command is required")
	}
	secs := min(intArg(in, "timeout_sec", defaultBashSecs), maxBashSecs)
	if secs <= 0 {
		secs = defaultBashSecs
	}
	res, err := b.SB.Exec(ctx, sandbox.ExecRequest{Command: cmd, Cwd: b.Workdir, Timeout: time.Duration(secs) * time.Second})
	if err != nil {
		return Result{}, fmt.Errorf("bash: %w", err)
	}
	var sb strings.Builder
	sb.WriteString(res.Stdout)
	if res.Stderr != "" {
		if sb.Len() > 0 && !strings.HasSuffix(res.Stdout, "\n") {
			sb.WriteByte('\n')
		}
		sb.WriteString("[stderr]\n" + res.Stderr)
	}
	out := truncate(sb.String(), maxOutputBytes)
	switch {
	case res.TimedOut:
		out += fmt.Sprintf("\n[killed after %ds]", secs)
	case res.ExitCode != 0:
		out += fmt.Sprintf("\n[exit code %d]", res.ExitCode)
	}
	return Result{Output: out, IsError: res.TimedOut || res.ExitCode != 0}, nil
}

// read_file

type readFile struct{ Sandboxed }

func (readFile) Spec() Spec {
	return Spec{
		Name:        "read_file",
		Description: "Read a text file. Lines come back numbered; use offset and limit for long files.",
		Schema: obj(map[string]any{
			"path":   str("File path, absolute or relative to the working directory."),
			"offset": num("First line to return, starting at 1."),
			"limit":  num(fmt.Sprintf("Number of lines to return (default %d).", defaultReadLines)),
		}, "path"),
	}
}

func (r readFile) Run(ctx context.Context, in map[string]any) (Result, error) {
	p, ok := stringArg(in, "path")
	if !ok || p == "" {
		return fail("path is required")
	}
	data, err := r.SB.Get(ctx, r.abs(p))
	if errors.Is(err, fs.ErrNotExist) {
		return fail("%s does not exist", p)
	}
	if err != nil {
		return Result{}, fmt.Errorf("read_file: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	start := max(intArg(in, "offset", 1), 1) - 1
	if start > len(lines) {
		start = len(lines)
	}
	end := min(start+max(intArg(in, "limit", defaultReadLines), 1), len(lines))
	var sb strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&sb, "%6d\t%s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&sb, "[%d more lines; continue with offset=%d]\n", len(lines)-end, end+1)
	}
	return Result{Output: truncate(sb.String(), maxOutputBytes)}, nil
}

// write_file

type writeFile struct{ Sandboxed }

func (writeFile) Spec() Spec {
	return Spec{
		Name:        "write_file",
		Description: "Create a file, or replace it entirely. Parent directories are created.",
		Schema: obj(map[string]any{
			"path":    str("File path, absolute or relative to the working directory."),
			"content": str("The complete new content."),
		}, "path", "content"),
	}
}

func (w writeFile) Run(ctx context.Context, in map[string]any) (Result, error) {
	p, ok := stringArg(in, "path")
	if !ok || p == "" {
		return fail("path is required")
	}
	content, ok := stringArg(in, "content")
	if !ok {
		return fail("content is required")
	}
	if err := w.SB.Put(ctx, w.abs(p), []byte(content), 0o644); err != nil {
		return Result{}, fmt.Errorf("write_file: %w", err)
	}
	return Result{Output: fmt.Sprintf("wrote %d bytes to %s", len(content), p)}, nil
}

// edit_file

type editFile struct{ Sandboxed }

func (editFile) Spec() Spec {
	return Spec{
		Name:        "edit_file",
		Description: "Replace text in a file. old_string must match exactly once unless replace_all is set.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        str("File path, absolute or relative to the working directory."),
				"old_string":  str("The exact text to replace, including whitespace."),
				"new_string":  str("The replacement."),
				"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence."},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
	}
}

func (e editFile) Run(ctx context.Context, in map[string]any) (Result, error) {
	p, ok := stringArg(in, "path")
	if !ok || p == "" {
		return fail("path is required")
	}
	oldS, ok1 := stringArg(in, "old_string")
	newS, ok2 := stringArg(in, "new_string")
	if !ok1 || !ok2 || oldS == "" {
		return fail("old_string (non-empty) and new_string are required")
	}
	if oldS == newS {
		return fail("old_string and new_string are identical")
	}
	data, err := e.SB.Get(ctx, e.abs(p))
	if errors.Is(err, fs.ErrNotExist) {
		return fail("%s does not exist", p)
	}
	if err != nil {
		return Result{}, fmt.Errorf("edit_file: %w", err)
	}
	text := string(data)
	n := strings.Count(text, oldS)
	switch {
	case n == 0:
		return fail("old_string was not found in %s", p)
	case n > 1 && !boolArg(in, "replace_all"):
		return fail("old_string occurs %d times in %s; add context to make it unique or set replace_all", n, p)
	}
	text = strings.ReplaceAll(text, oldS, newS)
	if err := e.SB.Put(ctx, e.abs(p), []byte(text), 0o644); err != nil {
		return Result{}, fmt.Errorf("edit_file: %w", err)
	}
	return Result{Output: fmt.Sprintf("replaced %d occurrence(s) in %s", n, p)}, nil
}

// search

type search struct{ Sandboxed }

func (search) Spec() Spec {
	return Spec{
		Name:        "search",
		Description: "Search file contents for a regular expression (grep -E). Returns file:line:text.",
		Schema: obj(map[string]any{
			"pattern": str("Extended regular expression."),
			"path":    str("Directory or file to search (default: the working directory)."),
		}, "pattern"),
	}
}

func (s search) Run(ctx context.Context, in map[string]any) (Result, error) {
	pat, ok := stringArg(in, "pattern")
	if !ok || pat == "" {
		return fail("pattern is required")
	}
	target := "."
	if p, _ := stringArg(in, "path"); p != "" {
		target = s.abs(p)
	}
	cmd := fmt.Sprintf("grep -rnIE --exclude-dir=.git -e %s -- %s | head -n %d", shellQuote(pat), shellQuote(target), maxSearchMatches)
	res, err := s.SB.Exec(ctx, sandbox.ExecRequest{Command: cmd, Cwd: s.Workdir, Timeout: searchTimeoutSecs * time.Second})
	if err != nil {
		return Result{}, fmt.Errorf("search: %w", err)
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return Result{Output: "no matches"}, nil
	}
	return Result{Output: truncate(res.Stdout, maxOutputBytes)}, nil
}
