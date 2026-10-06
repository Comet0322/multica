package workflow

import (
	"fmt"
	"strings"
	"testing"
)

const validDoc = "intro text\n```yaml\nnodes:\n  - id: plan\n    agent: Planner\n    prompt: make a plan\n  - id: build\n    depends_on: [plan]\n    agent: Coder\n    prompt: build it\n    max_retries: 3\n  - id: review\n    depends_on: [build]\n    agent: Reviewer\n    prompt: review it\n    approval: true\n```\ntrailing"

func TestExtractYAML(t *testing.T) {
	raw, ok := ExtractYAML(validDoc)
	if !ok || !strings.HasPrefix(raw, "nodes:") {
		t.Fatalf("ExtractYAML = %q, %v", raw, ok)
	}
	if _, ok := ExtractYAML("no block here"); ok {
		t.Fatal("expected no block")
	}
	if _, ok := ExtractYAML("```yaml\nnodes: []\n"); ok {
		t.Fatal("an unclosed fence must not match")
	}
	if _, ok := ExtractYAML("```YML\nnodes: []\n```"); !ok {
		t.Fatal("fence language is case-insensitive and accepts yml")
	}
}

func TestParseValid(t *testing.T) {
	def, errs := Parse(validDoc)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(def.Nodes) != 3 || def.Nodes[1].Retries() != 3 || def.Nodes[0].Retries() != 1 || !def.Nodes[2].Approval {
		t.Fatalf("bad parse: %+v", def)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"no block", "nothing", "no ```yaml block"},
		{"bad yaml", "```yaml\nnodes: [\n```", "invalid YAML"},
		{"unknown field", "```yaml\nnodes:\n  - id: a\n    agent: A\n    prompt: p\n    colour: red\n```", "invalid YAML"},
		{"empty nodes", "```yaml\nnodes: []\n```", "at least one node"},
		{"bad id", "```yaml\nnodes:\n  - id: Bad Id\n    agent: A\n    prompt: p\n```", `invalid id "Bad Id"`},
		{"missing agent", "```yaml\nnodes:\n  - id: a\n    prompt: p\n```", `node "a": agent is required`},
		{"missing prompt", "```yaml\nnodes:\n  - id: a\n    agent: A\n```", `node "a": prompt is required`},
		{"negative retries", "```yaml\nnodes:\n  - id: a\n    agent: A\n    prompt: p\n    max_retries: -1\n```", `node "a": max_retries must be >= 0`},
		{"duplicate", "```yaml\nnodes:\n  - id: a\n    agent: A\n    prompt: p\n  - id: a\n    agent: A\n    prompt: p\n```", `duplicate id "a"`},
		{"unknown dep", "```yaml\nnodes:\n  - id: a\n    agent: A\n    prompt: p\n    depends_on: [zzz]\n```", `node "a": unknown dependency "zzz"`},
		{"self dep", "```yaml\nnodes:\n  - id: a\n    agent: A\n    prompt: p\n    depends_on: [a]\n```", `node "a": depends on itself`},
		{"cycle", "```yaml\nnodes:\n  - id: a\n    agent: A\n    prompt: p\n    depends_on: [b]\n  - id: b\n    agent: A\n    prompt: p\n    depends_on: [a]\n```", "dependency cycle among: a, b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errs := Parse(c.body)
			if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), c.want) {
				t.Fatalf("errors = %v, want one containing %q", errs, c.want)
			}
		})
	}
}

func TestParseCollectsAllErrors(t *testing.T) {
	_, errs := Parse("```yaml\nnodes:\n  - id: a\n    prompt: p\n  - id: b\n    agent: A\n```")
	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %v", errs)
	}
}

func TestParseEnforcesSizeCaps(t *testing.T) {
	var many strings.Builder
	many.WriteString("```yaml\nnodes:\n")
	for i := 0; i < maxNodes+1; i++ {
		fmt.Fprintf(&many, "  - id: n%d\n    agent: A\n    prompt: p\n", i)
	}
	many.WriteString("```")
	if _, errs := Parse(many.String()); len(errs) != 1 || !strings.Contains(errs[0], "limit is 50 nodes") {
		t.Fatalf("node cap errors = %v", errs)
	}

	longPrompt := "```yaml\nnodes:\n  - id: a\n    agent: A\n    prompt: " + strings.Repeat("x", maxPromptBytes+1) + "\n```"
	if _, errs := Parse(longPrompt); len(errs) != 1 || !strings.Contains(errs[0], "limit is 20000 bytes") {
		t.Fatalf("prompt cap errors = %v", errs)
	}

	bigYAML := "```yaml\n# " + strings.Repeat("y", maxYAMLBytes) + "\nnodes:\n  - id: a\n    agent: A\n    prompt: p\n```"
	if _, errs := Parse(bigYAML); len(errs) != 1 || !strings.Contains(errs[0], "limit is 100000 bytes") {
		t.Fatalf("yaml cap errors = %v", errs)
	}

	ok := "```yaml\nnodes:\n  - id: a\n    agent: A\n    prompt: " + strings.Repeat("x", maxPromptBytes) + "\n```"
	if _, errs := Parse(ok); len(errs) != 0 {
		t.Fatalf("a prompt at the cap must pass: %v", errs)
	}
}
