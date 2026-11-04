package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
	"github.com/multica-ai/multica/server/pkg/harness/tools"
)

func newRegistry(t *testing.T) (*tools.Registry, sandbox.Sandbox) {
	t.Helper()
	sb, err := sandbox.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Put(context.Background(), "/w/.keep", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return tools.NewRegistry(tools.Sandboxed{SB: sb, Workdir: "/w"}.Tools()...), sb
}

func run(t *testing.T, r *tools.Registry, name string, in map[string]any) tools.Result {
	t.Helper()
	res, err := r.Run(context.Background(), name, in)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func TestBashReportsOutputAndFailure(t *testing.T) {
	r, _ := newRegistry(t)

	ok := run(t, r, "bash", map[string]any{"command": "echo hi"})
	if ok.IsError || strings.TrimSpace(ok.Output) != "hi" {
		t.Errorf("echo: %+v", ok)
	}

	bad := run(t, r, "bash", map[string]any{"command": "echo oops >&2; exit 7"})
	if !bad.IsError || !strings.Contains(bad.Output, "oops") || !strings.Contains(bad.Output, "exit code 7") {
		t.Errorf("failing command should say why: %+v", bad)
	}

	long := run(t, r, "bash", map[string]any{"command": "yes x | head -c 100000"})
	if len(long.Output) > 31_000 || !strings.Contains(long.Output, "bytes omitted") {
		t.Errorf("output not bounded: %d bytes", len(long.Output))
	}
}

func TestWriteReadEditRoundTrip(t *testing.T) {
	r, _ := newRegistry(t)

	run(t, r, "write_file", map[string]any{"path": "src/a.txt", "content": "one\ntwo\nthree\n"})
	got := run(t, r, "read_file", map[string]any{"path": "src/a.txt"})
	if !strings.Contains(got.Output, "     2\ttwo") {
		t.Errorf("read_file should number lines: %q", got.Output)
	}

	part := run(t, r, "read_file", map[string]any{"path": "src/a.txt", "offset": 2, "limit": 1})
	if !strings.Contains(part.Output, "two") || strings.Contains(part.Output, "one") || !strings.Contains(part.Output, "offset=3") {
		t.Errorf("offset/limit: %q", part.Output)
	}

	if res := run(t, r, "edit_file", map[string]any{"path": "src/a.txt", "old_string": "two", "new_string": "2"}); res.IsError {
		t.Fatalf("edit: %+v", res)
	}
	if got := run(t, r, "read_file", map[string]any{"path": "src/a.txt"}); !strings.Contains(got.Output, "2\n") || strings.Contains(got.Output, "two") {
		t.Errorf("edit not applied: %q", got.Output)
	}
}

func TestEditRefusesWhatItCannotDoSafely(t *testing.T) {
	r, _ := newRegistry(t)
	run(t, r, "write_file", map[string]any{"path": "a", "content": "x x"})

	for name, in := range map[string]map[string]any{
		"ambiguous": {"path": "a", "old_string": "x", "new_string": "y"},
		"missing":   {"path": "a", "old_string": "zzz", "new_string": "y"},
		"no file":   {"path": "nope", "old_string": "x", "new_string": "y"},
		"no change": {"path": "a", "old_string": "x", "new_string": "x"},
	} {
		if res := run(t, r, "edit_file", in); !res.IsError {
			t.Errorf("%s: want an error result, got %+v", name, res)
		}
	}
	if res := run(t, r, "edit_file", map[string]any{"path": "a", "old_string": "x", "new_string": "y", "replace_all": true}); res.IsError {
		t.Errorf("replace_all: %+v", res)
	}
}

func TestMissingFileIsTheModelsProblemNotTheTasks(t *testing.T) {
	r, _ := newRegistry(t)
	res := run(t, r, "read_file", map[string]any{"path": "nope"})
	if !res.IsError || !strings.Contains(res.Output, "does not exist") {
		t.Errorf("%+v", res)
	}
	if res := run(t, r, "no_such_tool", nil); !res.IsError {
		t.Errorf("unknown tool should be an error result: %+v", res)
	}
}

func TestSearchFindsMatchesAndTreatsThePatternAsData(t *testing.T) {
	r, _ := newRegistry(t)
	run(t, r, "write_file", map[string]any{"path": "m.go", "content": "func main() {}\n"})

	hit := run(t, r, "search", map[string]any{"pattern": "func (main|init)"})
	if !strings.Contains(hit.Output, "m.go:1:func main()") {
		t.Errorf("search: %q", hit.Output)
	}
	// Quotes and command substitutions in a pattern must not reach the shell.
	odd := run(t, r, "search", map[string]any{"pattern": "'; touch pwned; '$(touch pwned)"})
	if odd.Output != "no matches" {
		t.Errorf("odd pattern: %q", odd.Output)
	}
	if res := run(t, r, "bash", map[string]any{"command": "ls"}); strings.Contains(res.Output, "pwned") {
		t.Errorf("a pattern was executed: %q", res.Output)
	}
}

func TestEverySpecIsUsable(t *testing.T) {
	r, _ := newRegistry(t)
	specs := r.Specs()
	if len(specs) != 5 {
		t.Fatalf("want 5 tools, got %d", len(specs))
	}
	for _, s := range specs {
		if s.Name == "" || s.Description == "" || s.Schema["type"] != "object" {
			t.Errorf("incomplete spec: %+v", s)
		}
	}
}
