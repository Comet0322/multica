// Package extworkflow implements the first-class workflow feature of this fork:
// reusable DAG templates, a deterministic run engine, and a supervisor agent.
//
// This file is the pure definition layer: types and structural validation with
// no database or network access, so it can be shared by the CRUD handler (which
// rejects invalid templates) and the engine (which snapshots a validated
// template into each run).
package extworkflow

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// MaxNodes bounds the size of one workflow definition.
	MaxNodes = 50
	// MaxPromptBytes bounds one node prompt (UTF-8 bytes, not runes).
	MaxPromptBytes = 20000

	maxTitleRunes  = 200
	minMaxAttempts = 1
	maxMaxAttempts = 10
	maxMaxRewinds  = 10
)

var nodeKeyPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

// Node is one step template: a prompt run by one agent.
type Node struct {
	Key            string   `json:"key"`
	Title          string   `json:"title"`
	AgentID        string   `json:"agent_id"` // uuid string
	Prompt         string   `json:"prompt"`
	RequiresReview bool     `json:"requires_review"`
	MaxAttempts    int      `json:"max_attempts"`
	DependsOn      []string `json:"depends_on"`
}

// Definition is a workflow template. It is also the JSON snapshot stored in
// ext_workflow_run.definition.
type Definition struct {
	SupervisorAgentID string `json:"supervisor_agent_id"`
	MaxRewinds        int    `json:"max_rewinds"`
	Nodes             []Node `json:"nodes"` // slice order = position
}

// ValidationError is one problem found in a definition. NodeKey is empty for
// definition-level problems.
type ValidationError struct {
	NodeKey string `json:"node_key,omitempty"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e ValidationError) Error() string {
	if e.NodeKey != "" {
		return fmt.Sprintf("node %q: %s: %s", e.NodeKey, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidateStructure checks everything that can be decided without the
// database: limits, key format and uniqueness, dependency references, self
// dependencies, cycles, attempt and rewind ranges. Agent existence and
// invokability are checked by the caller. The result is empty for a valid
// definition and ordered deterministically (definition-level errors first, then
// by node position, then cycles).
func ValidateStructure(d Definition) []ValidationError {
	var errs []ValidationError

	errs = append(errs, ValidateSettings(d.SupervisorAgentID, d.MaxRewinds)...)
	switch {
	case len(d.Nodes) == 0:
		errs = append(errs, ValidationError{Field: "nodes", Message: "at least one node is required"})
	case len(d.Nodes) > MaxNodes:
		errs = append(errs, ValidationError{Field: "nodes", Message: fmt.Sprintf("at most %d nodes are allowed", MaxNodes)})
	}

	// First occurrence of each key wins; later ones are reported as duplicates
	// and excluded from the graph so one typo does not cascade.
	known := make(map[string]bool, len(d.Nodes))
	for _, n := range d.Nodes {
		if nodeKeyPattern.MatchString(n.Key) {
			known[n.Key] = true
		}
	}
	seen := make(map[string]bool, len(d.Nodes))
	for _, n := range d.Nodes {
		switch {
		case !nodeKeyPattern.MatchString(n.Key):
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "key", Message: "must match [a-z0-9_-] and be 1-40 characters"})
		case seen[n.Key]:
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "key", Message: "duplicate key"})
		}
		seen[n.Key] = true

		if strings.TrimSpace(n.Title) == "" {
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "title", Message: "title is required"})
		} else if utf8.RuneCountInString(n.Title) > maxTitleRunes {
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "title", Message: fmt.Sprintf("must be at most %d characters", maxTitleRunes)})
		}
		if strings.TrimSpace(n.AgentID) == "" {
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "agent_id", Message: "agent is required"})
		}
		if len(n.Prompt) > MaxPromptBytes {
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "prompt", Message: fmt.Sprintf("must be at most %d bytes", MaxPromptBytes)})
		}
		if n.MaxAttempts < minMaxAttempts || n.MaxAttempts > maxMaxAttempts {
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "max_attempts", Message: fmt.Sprintf("must be between %d and %d", minMaxAttempts, maxMaxAttempts)})
		}

		depSeen := make(map[string]bool, len(n.DependsOn))
		for _, dep := range n.DependsOn {
			switch {
			case dep == n.Key:
				errs = append(errs, ValidationError{NodeKey: n.Key, Field: "depends_on", Message: "a node cannot depend on itself"})
			case !known[dep]:
				errs = append(errs, ValidationError{NodeKey: n.Key, Field: "depends_on", Message: fmt.Sprintf("unknown dependency %q", dep)})
			case depSeen[dep]:
				errs = append(errs, ValidationError{NodeKey: n.Key, Field: "depends_on", Message: fmt.Sprintf("duplicate dependency %q", dep)})
			}
			depSeen[dep] = true
		}
	}

	for _, key := range cyclicNodeKeys(d, known) {
		errs = append(errs, ValidationError{NodeKey: key, Field: "depends_on", Message: "node is part of a dependency cycle"})
	}
	return errs
}

// ValidateSettings checks the definition-level fields that do not depend on
// the node set. The CRUD handler calls it on its own when an update changes
// the supervisor or the rewind budget without replacing the nodes.
func ValidateSettings(supervisorAgentID string, maxRewinds int) []ValidationError {
	var errs []ValidationError
	if strings.TrimSpace(supervisorAgentID) == "" {
		errs = append(errs, ValidationError{Field: "supervisor_agent_id", Message: "supervisor agent is required"})
	}
	if maxRewinds < 0 || maxRewinds > maxMaxRewinds {
		errs = append(errs, ValidationError{Field: "max_rewinds", Message: fmt.Sprintf("must be between 0 and %d", maxMaxRewinds)})
	}
	return errs
}

// NodeByKey returns the node with the given key.
func (d Definition) NodeByKey(key string) (Node, bool) {
	for _, n := range d.Nodes {
		if n.Key == key {
			return n, true
		}
	}
	return Node{}, false
}

// Ancestors returns every node transitively upstream of key (its dependencies,
// their dependencies, ...), excluding key itself.
func Ancestors(d Definition, key string) map[string]bool {
	deps := make(map[string][]string, len(d.Nodes))
	for _, n := range d.Nodes {
		deps[n.Key] = n.DependsOn
	}
	out := reach(key, func(k string) []string { return deps[k] })
	delete(out, key)
	return out
}

// Descendants returns every node transitively downstream of key (nodes that
// depend on it, directly or not), excluding key itself.
func Descendants(d Definition, key string) map[string]bool {
	dependents := make(map[string][]string, len(d.Nodes))
	for _, n := range d.Nodes {
		for _, dep := range n.DependsOn {
			dependents[dep] = append(dependents[dep], n.Key)
		}
	}
	out := reach(key, func(k string) []string { return dependents[k] })
	delete(out, key)
	return out
}

// DepthOf returns the longest-path depth of every node: roots are 0 and a node
// is one deeper than its deepest dependency. Unknown dependencies are ignored.
// On a cyclic (invalid) definition the edge that closes a cycle is ignored, so
// the call always terminates.
func DepthOf(d Definition) map[string]int {
	byKey := make(map[string]Node, len(d.Nodes))
	for _, n := range d.Nodes {
		byKey[n.Key] = n
	}
	depth := make(map[string]int, len(d.Nodes))
	visiting := make(map[string]bool, len(d.Nodes))
	var visit func(key string) int
	visit = func(key string) int {
		if v, ok := depth[key]; ok {
			return v
		}
		visiting[key] = true
		best := 0
		for _, dep := range byKey[key].DependsOn {
			if _, ok := byKey[dep]; !ok || visiting[dep] {
				continue
			}
			if v := visit(dep) + 1; v > best {
				best = v
			}
		}
		visiting[key] = false
		depth[key] = best
		return best
	}
	for _, n := range d.Nodes {
		visit(n.Key)
	}
	return depth
}

// reach returns every node reachable from start by following next one or more
// times. start is included only when it lies on a cycle.
func reach(start string, next func(string) []string) map[string]bool {
	out := map[string]bool{}
	queue := append([]string(nil), next(start)...)
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		if out[k] {
			continue
		}
		out[k] = true
		queue = append(queue, next(k)...)
	}
	return out
}

// cyclicNodeKeys returns, in node order, the keys that lie on a dependency
// cycle. Nodes that merely depend on a cycle are not reported.
func cyclicNodeKeys(d Definition, known map[string]bool) []string {
	deps := make(map[string][]string, len(d.Nodes))
	for _, n := range d.Nodes {
		for _, dep := range n.DependsOn {
			if known[dep] && dep != n.Key {
				deps[n.Key] = append(deps[n.Key], dep)
			}
		}
	}
	var keys []string
	done := map[string]bool{}
	for _, n := range d.Nodes {
		if !known[n.Key] || done[n.Key] {
			continue
		}
		done[n.Key] = true
		if reach(n.Key, func(k string) []string { return deps[k] })[n.Key] {
			keys = append(keys, n.Key)
		}
	}
	return keys
}
