package extworkflow

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func node(key string, deps ...string) Node {
	return Node{
		Key:         key,
		Title:       "Title " + key,
		AgentID:     "11111111-1111-1111-1111-111111111111",
		Prompt:      "do " + key,
		MaxAttempts: 3,
		DependsOn:   deps,
	}
}

func def(nodes ...Node) Definition {
	return Definition{
		SupervisorAgentID: "22222222-2222-2222-2222-222222222222",
		MaxRewinds:        3,
		Nodes:             nodes,
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diamond: a -> b, a -> c, b -> d, c -> d  (arrow = "is depended on by")
func diamond() Definition {
	return def(node("a"), node("b", "a"), node("c", "a"), node("d", "b", "c"))
}

func TestValidateStructure(t *testing.T) {
	longPrompt := strings.Repeat("x", MaxPromptBytes+1)
	tooMany := make([]Node, MaxNodes+1)
	for i := range tooMany {
		tooMany[i] = node("n" + strings.Repeat("0", 3) + string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}

	tests := []struct {
		name string
		d    Definition
		want []ValidationError // exact, ordered
	}{
		{name: "valid diamond", d: diamond(), want: nil},
		{name: "valid single node", d: def(node("only")), want: nil},
		{
			name: "no nodes",
			d:    def(),
			want: []ValidationError{{Field: "nodes", Message: "at least one node is required"}},
		},
		{
			name: "missing supervisor",
			d:    Definition{MaxRewinds: 3, Nodes: []Node{node("a")}},
			want: []ValidationError{{Field: "supervisor_agent_id", Message: "supervisor agent is required"}},
		},
		{
			name: "max rewinds out of range",
			d:    Definition{SupervisorAgentID: "s", MaxRewinds: 11, Nodes: []Node{node("a")}},
			want: []ValidationError{{Field: "max_rewinds", Message: "must be between 0 and 10"}},
		},
		{
			name: "negative max rewinds",
			d:    Definition{SupervisorAgentID: "s", MaxRewinds: -1, Nodes: []Node{node("a")}},
			want: []ValidationError{{Field: "max_rewinds", Message: "must be between 0 and 10"}},
		},
		{
			name: "zero max rewinds is allowed",
			d:    Definition{SupervisorAgentID: "s", MaxRewinds: 0, Nodes: []Node{node("a")}},
			want: nil,
		},
		{
			name: "too many nodes",
			d:    def(tooMany...),
			want: []ValidationError{{Field: "nodes", Message: "at most 50 nodes are allowed"}},
		},
		{
			name: "invalid key uppercase",
			d:    def(node("Build")),
			want: []ValidationError{{NodeKey: "Build", Field: "key", Message: "must match [a-z0-9_-] and be 1-40 characters"}},
		},
		{
			name: "invalid key empty",
			d:    def(node("")),
			want: []ValidationError{{NodeKey: "", Field: "key", Message: "must match [a-z0-9_-] and be 1-40 characters"}},
		},
		{
			name: "invalid key too long",
			d:    def(node(strings.Repeat("a", 41))),
			want: []ValidationError{{NodeKey: strings.Repeat("a", 41), Field: "key", Message: "must match [a-z0-9_-] and be 1-40 characters"}},
		},
		{
			name: "valid key at 40 chars with dash and underscore",
			d:    def(node(strings.Repeat("a", 38) + "-_")),
			want: nil,
		},
		{
			name: "duplicate key reported once on the second occurrence",
			d:    def(node("a"), node("a")),
			want: []ValidationError{{NodeKey: "a", Field: "key", Message: "duplicate key"}},
		},
		{
			name: "missing title",
			d:    def(Node{Key: "a", Title: "  ", AgentID: "x", MaxAttempts: 1}),
			want: []ValidationError{{NodeKey: "a", Field: "title", Message: "title is required"}},
		},
		{
			name: "title too long",
			d:    def(Node{Key: "a", Title: strings.Repeat("é", 201), AgentID: "x", MaxAttempts: 1}),
			want: []ValidationError{{NodeKey: "a", Field: "title", Message: "must be at most 200 characters"}},
		},
		{
			name: "missing agent",
			d:    def(Node{Key: "a", Title: "t", MaxAttempts: 1}),
			want: []ValidationError{{NodeKey: "a", Field: "agent_id", Message: "agent is required"}},
		},
		{
			name: "prompt too long",
			d:    def(Node{Key: "a", Title: "t", AgentID: "x", MaxAttempts: 1, Prompt: longPrompt}),
			want: []ValidationError{{NodeKey: "a", Field: "prompt", Message: "must be at most 20000 bytes"}},
		},
		{
			name: "prompt at the limit is allowed",
			d:    def(Node{Key: "a", Title: "t", AgentID: "x", MaxAttempts: 1, Prompt: strings.Repeat("x", MaxPromptBytes)}),
			want: nil,
		},
		{
			name: "max attempts zero",
			d:    def(Node{Key: "a", Title: "t", AgentID: "x", MaxAttempts: 0}),
			want: []ValidationError{{NodeKey: "a", Field: "max_attempts", Message: "must be between 1 and 10"}},
		},
		{
			name: "max attempts eleven",
			d:    def(Node{Key: "a", Title: "t", AgentID: "x", MaxAttempts: 11}),
			want: []ValidationError{{NodeKey: "a", Field: "max_attempts", Message: "must be between 1 and 10"}},
		},
		{
			name: "self dependency",
			d:    def(node("a", "a")),
			want: []ValidationError{{NodeKey: "a", Field: "depends_on", Message: "a node cannot depend on itself"}},
		},
		{
			name: "unknown dependency",
			d:    def(node("a", "ghost")),
			want: []ValidationError{{NodeKey: "a", Field: "depends_on", Message: `unknown dependency "ghost"`}},
		},
		{
			name: "duplicate dependency",
			d:    def(node("a"), node("b", "a", "a")),
			want: []ValidationError{{NodeKey: "b", Field: "depends_on", Message: `duplicate dependency "a"`}},
		},
		{
			name: "two node cycle",
			d:    def(node("a", "b"), node("b", "a")),
			want: []ValidationError{
				{NodeKey: "a", Field: "depends_on", Message: "node is part of a dependency cycle"},
				{NodeKey: "b", Field: "depends_on", Message: "node is part of a dependency cycle"},
			},
		},
		{
			name: "cycle with a healthy tail only flags cycle members",
			d:    def(node("root"), node("a", "root", "c"), node("b", "a"), node("c", "b"), node("tail", "c")),
			want: []ValidationError{
				{NodeKey: "a", Field: "depends_on", Message: "node is part of a dependency cycle"},
				{NodeKey: "b", Field: "depends_on", Message: "node is part of a dependency cycle"},
				{NodeKey: "c", Field: "depends_on", Message: "node is part of a dependency cycle"},
			},
		},
		{
			name: "errors are ordered definition first then by node",
			d: Definition{SupervisorAgentID: "", MaxRewinds: 99, Nodes: []Node{
				{Key: "a", Title: "", AgentID: "", MaxAttempts: 0},
				{Key: "b", Title: "t", AgentID: "x", MaxAttempts: 1, DependsOn: []string{"zzz"}},
			}},
			want: []ValidationError{
				{Field: "supervisor_agent_id", Message: "supervisor agent is required"},
				{Field: "max_rewinds", Message: "must be between 0 and 10"},
				{NodeKey: "a", Field: "title", Message: "title is required"},
				{NodeKey: "a", Field: "agent_id", Message: "agent is required"},
				{NodeKey: "a", Field: "max_attempts", Message: "must be between 1 and 10"},
				{NodeKey: "b", Field: "depends_on", Message: `unknown dependency "zzz"`},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateStructure(tc.d)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ValidateStructure mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

func TestValidationErrorString(t *testing.T) {
	if got := (ValidationError{Field: "nodes", Message: "bad"}).Error(); got != "nodes: bad" {
		t.Fatalf("definition-level error string = %q", got)
	}
	if got := (ValidationError{NodeKey: "a", Field: "key", Message: "bad"}).Error(); got != `node "a": key: bad` {
		t.Fatalf("node-level error string = %q", got)
	}
}

func TestNodeByKey(t *testing.T) {
	d := diamond()
	n, ok := d.NodeByKey("c")
	if !ok || n.Key != "c" {
		t.Fatalf("NodeByKey(c) = %+v, %v", n, ok)
	}
	if _, ok := d.NodeByKey("nope"); ok {
		t.Fatal("NodeByKey(nope) should not be found")
	}
}

func TestAncestorsAndDescendants(t *testing.T) {
	d := diamond()
	tests := []struct {
		key             string
		wantAncestors   []string
		wantDescendants []string
	}{
		{"a", nil, []string{"b", "c", "d"}},
		{"b", []string{"a"}, []string{"d"}},
		{"c", []string{"a"}, []string{"d"}},
		{"d", []string{"a", "b", "c"}, nil},
		{"missing", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			if got := sortedKeys(Ancestors(d, tc.key)); !reflect.DeepEqual(got, orEmpty(tc.wantAncestors)) {
				t.Errorf("Ancestors(%s) = %v, want %v", tc.key, got, tc.wantAncestors)
			}
			if got := sortedKeys(Descendants(d, tc.key)); !reflect.DeepEqual(got, orEmpty(tc.wantDescendants)) {
				t.Errorf("Descendants(%s) = %v, want %v", tc.key, got, tc.wantDescendants)
			}
		})
	}
}

func TestAncestorsDescendantsExcludeSelfOnCycle(t *testing.T) {
	d := def(node("a", "b"), node("b", "a"))
	if Ancestors(d, "a")["a"] {
		t.Error("Ancestors must exclude the key itself even on a cycle")
	}
	if Descendants(d, "a")["a"] {
		t.Error("Descendants must exclude the key itself even on a cycle")
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func TestDepthOf(t *testing.T) {
	tests := []struct {
		name string
		d    Definition
		want map[string]int
	}{
		{"diamond", diamond(), map[string]int{"a": 0, "b": 1, "c": 1, "d": 2}},
		{
			"longest path wins",
			def(node("a"), node("b", "a"), node("c", "b"), node("d", "a", "c")),
			map[string]int{"a": 0, "b": 1, "c": 2, "d": 3},
		},
		{"independent roots", def(node("x"), node("y")), map[string]int{"x": 0, "y": 0}},
		{"unknown dependency ignored", def(node("a", "ghost")), map[string]int{"a": 0}},
		{"empty", def(), map[string]int{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DepthOf(tc.d); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DepthOf = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDepthOfTerminatesOnCycle(t *testing.T) {
	d := def(node("a", "b"), node("b", "a"))
	got := DepthOf(d)
	if len(got) != 2 {
		t.Fatalf("DepthOf on a cycle should still cover every node, got %v", got)
	}
}

func TestValidateSettings(t *testing.T) {
	tests := []struct {
		name       string
		supervisor string
		rewinds    int
		want       []ValidationError
	}{
		{"ok", "s", 3, nil},
		{"zero rewinds ok", "s", 0, nil},
		{"ten rewinds ok", "s", 10, nil},
		{"blank supervisor", "  ", 3, []ValidationError{{Field: "supervisor_agent_id", Message: "supervisor agent is required"}}},
		{"rewinds too high", "s", 11, []ValidationError{{Field: "max_rewinds", Message: "must be between 0 and 10"}}},
		{"rewinds negative", "s", -1, []ValidationError{{Field: "max_rewinds", Message: "must be between 0 and 10"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateSettings(tc.supervisor, tc.rewinds)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}
