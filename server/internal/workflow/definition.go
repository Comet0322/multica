package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Size caps keep one definition from creating an unbounded number of issues or
// storing unbounded prompts.
const (
	maxNodes       = 50
	maxPromptBytes = 20000
	maxYAMLBytes   = 100000
)

var nodeIDPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// Node is one step of a workflow, modelled on Archon's `nodes:` entries.
type Node struct {
	ID         string   `yaml:"id"`
	Agent      string   `yaml:"agent"`
	Prompt     string   `yaml:"prompt"`
	DependsOn  []string `yaml:"depends_on"`
	Approval   bool     `yaml:"approval"`
	MaxRetries *int     `yaml:"max_retries"`
}

// Retries is the number of redo attempts after a reject or an agent failure.
func (n Node) Retries() int {
	if n.MaxRetries == nil {
		return 1
	}
	return *n.MaxRetries
}

type Definition struct {
	Nodes []Node `yaml:"nodes"`
}

// ExtractYAML returns the body of the first fenced yaml block in description.
func ExtractYAML(description string) (string, bool) {
	lines := strings.Split(description, "\n")
	start := -1
	for i, l := range lines {
		t := strings.ToLower(strings.TrimSpace(l))
		if t == "```yaml" || t == "```yml" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}
	for j := start; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "```" {
			return strings.Join(lines[start:j], "\n"), true
		}
	}
	return "", false
}

// Parse extracts, decodes and validates the workflow in description. It
// returns either the definition and a nil slice, or every error found.
func Parse(description string) (Definition, []string) {
	raw, ok := ExtractYAML(description)
	if !ok {
		return Definition{}, []string{"no ```yaml block found in the description"}
	}
	if len(raw) > maxYAMLBytes {
		return Definition{}, []string{fmt.Sprintf("the yaml block is %d bytes; the limit is %d bytes", len(raw), maxYAMLBytes)}
	}
	var def Definition
	dec := yaml.NewDecoder(strings.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&def); err != nil {
		return Definition{}, []string{"invalid YAML: " + err.Error()}
	}
	if errs := validate(def); len(errs) > 0 {
		return Definition{}, errs
	}
	return def, nil
}

func validate(def Definition) []string {
	var errs []string
	if len(def.Nodes) == 0 {
		return []string{"workflow needs at least one node"}
	}
	if len(def.Nodes) > maxNodes {
		return []string{fmt.Sprintf("workflow has %d nodes; the limit is %d nodes", len(def.Nodes), maxNodes)}
	}
	known := map[string]bool{}
	for _, n := range def.Nodes {
		if !nodeIDPattern.MatchString(n.ID) {
			errs = append(errs, fmt.Sprintf("invalid id %q (use lowercase letters, digits, - and _)", n.ID))
			continue
		}
		if known[n.ID] {
			errs = append(errs, fmt.Sprintf("duplicate id %q", n.ID))
		}
		known[n.ID] = true
	}
	for _, n := range def.Nodes {
		if !nodeIDPattern.MatchString(n.ID) {
			continue
		}
		if strings.TrimSpace(n.Agent) == "" {
			errs = append(errs, fmt.Sprintf("node %q: agent is required", n.ID))
		}
		if strings.TrimSpace(n.Prompt) == "" {
			errs = append(errs, fmt.Sprintf("node %q: prompt is required", n.ID))
		}
		if len(n.Prompt) > maxPromptBytes {
			errs = append(errs, fmt.Sprintf("node %q: prompt is %d bytes; the limit is %d bytes", n.ID, len(n.Prompt), maxPromptBytes))
		}
		if n.MaxRetries != nil && *n.MaxRetries < 0 {
			errs = append(errs, fmt.Sprintf("node %q: max_retries must be >= 0", n.ID))
		}
		for _, d := range n.DependsOn {
			switch {
			case d == n.ID:
				errs = append(errs, fmt.Sprintf("node %q: depends on itself", n.ID))
			case !known[d]:
				errs = append(errs, fmt.Sprintf("node %q: unknown dependency %q", n.ID, d))
			}
		}
	}
	if len(errs) > 0 {
		return errs
	}
	if stuck := cycleMembers(def.Nodes); len(stuck) > 0 {
		errs = append(errs, "dependency cycle among: "+strings.Join(stuck, ", "))
	}
	return errs
}

// cycleMembers runs Kahn's algorithm and returns the ids that never reach
// in-degree zero, sorted. Empty means the graph is acyclic.
func cycleMembers(nodes []Node) []string {
	indeg := map[string]int{}
	out := map[string][]string{}
	for _, n := range nodes {
		indeg[n.ID] += 0
		for _, d := range n.DependsOn {
			indeg[n.ID]++
			out[d] = append(out[d], n.ID)
		}
	}
	var queue []string
	for id, d := range indeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	seen := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		seen++
		for _, next := range out[id] {
			indeg[next]--
			if indeg[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if seen == len(nodes) {
		return nil
	}
	var stuck []string
	for id, d := range indeg {
		if d > 0 {
			stuck = append(stuck, id)
		}
	}
	sort.Strings(stuck)
	return stuck
}
