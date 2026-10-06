# Workflow Engine (prototype) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a person describe a workflow as YAML in an issue labelled `flow:<name>`; a backend component expands it into step issues, dispatches them to agents in dependency order, gates `approval` steps on `/accept` / `/reject`, supports `/retry`, and completes the definition issue when every step is done.

**Architecture:** New package `server/internal/workflow/` holds all logic: a pure YAML parser, a pure data-driven transition table, and an `Engine` whose `Tick` is run by the existing DB-backed scheduler. All state lives in `issue.metadata` (key `workflow`); no tables, no migrations. The engine reuses `IssueService.Create`, `TaskService.EnqueueTask*`, the event bus and the scheduler. Existing code is touched only at: `cmd/server/main.go` wiring, one new job file in `internal/scheduler/`, one new bridge file in `internal/handler/`, and a one-line change to `isNoteComment` in `internal/handler/comment.go`.

**Tech Stack:** Go, pgx/v5, sqlc, `gopkg.in/yaml.v3` (already in `server/go.mod`), Postgres JSONB (`issue.metadata`, GIN-indexed).

**Spec:** `docs/superpowers/specs/2026-10-06-workflow-engine-design.md`

## Deviations from the spec (decided while planning, all additive)

1. Step metadata also stores `agent_id` (the resolved agent UUID) next to `agent` (the name), so dispatch never re-resolves names.
2. Definition metadata also stores `claimed_at` (to reclaim a stale `expanding` claim) and `total` (number of nodes, to detect a deleted step issue).
3. Step titles are `"<definition title> · <node id>"`. On resume after a crash between issue creation and its metadata write, an orphan child with that exact title and no `workflow` metadata is adopted instead of duplicated.
4. Because `IssueCreateParams` has no metadata field, step metadata is written by `SetIssueMetadataKey` immediately after `Create`.
5. The engine does not stop issue wakeups when it closes an issue (that logic lives only in `Handler.UpdateIssue`); a user-set wakeup on a step issue is out of scope for the prototype.
6. Status keys are compared as built-in literals (`done`, `in_review`, `todo`), like the autopilot listener does. Custom statuses are not supported in the prototype.
7. Steps are located by `metadata.workflow.run` (`ListWorkflowSteps`), not by `parent_issue_id`; the parent link is still set on creation and verified (a warning comment is posted if a step is not a direct child).
8. `handler.WorkflowEvents` (new file `internal/handler/workflow_bridge.go`) is the adapter that lets the engine emit events in the exact shapes existing listeners and the UI expect, without `workflow` importing `handler`.

## Global Constraints

- All new code in `server/internal/workflow/`; Go comments in English.
- No foreign keys, no cascades, no migrations (AGENTS.md: Database and Migration Rules).
- SQL changes: run `make sqlc` from the repo root after editing `server/pkg/db/queries/workflow.sql`.
- UUID parsing outside handlers: `util.ParseUUID(s)` and check the error (AGENTS.md: Backend UUID Rules). `workflow` must not import `internal/handler`.
- Workspace-scoped queries filter by `workspace_id`.
- DB-backed tests use `server/internal/testutil` (`dbfx.New(pool, ws, user)` and its builders). Default tests must not resolve or execute agent CLIs: use fake `TaskEnqueuer`s.
- Conventional commits, one per task. Commit messages end with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- Check commands (repo root): `cd server && go build ./... && go vet ./internal/workflow/... ./internal/scheduler/... ./internal/handler/...`, `cd server && go test ./internal/workflow/... -count=1`. DB-backed tests skip themselves when Postgres is unreachable; run them with the checkout's environment up (`make up`).

## Review Focus

Each line below has a pinned test in the task named in brackets.

1. Definition issue created by an **agent** (not a member): `/accept` etc. get a visible refusal comment, never silence. [Task 7]
2. A user **deletes a step issue** mid-run: the workflow must not complete early; it blocks with a comment. [Task 6]
3. Two ticks (two server instances) **expand the same definition concurrently**: exactly one set of step issues. [Task 5]
4. An invalid YAML that is later **edited into a valid one** is re-expanded; an unedited invalid one does not re-comment every tick. [Task 5]
5. **Agent archived or missing at dispatch**: the step fails with a comment, the workflow blocks, `/retry` after fixing resumes it. [Task 6, Task 7]
6. `/ACCEPT` matches, `/accepted` and `see /accept` do not; a command on an unrelated issue is silent. [Task 7]
7. A step issue **without its parent link** (the failure seen in an earlier attempt at this feature, where the first sub-issue of a batch had no parent): it is still tracked and dispatched, never duplicated, never makes the run look broken, and the missing link is reported in a comment. Steps are found by `metadata.workflow.run`, not by `parent_issue_id`. [Task 5, Task 6]

---

### Task 1: YAML definition parser (pure)

**Files:**
- Create: `server/internal/workflow/definition.go`
- Test: `server/internal/workflow/definition_test.go`

**Interfaces:**
- Produces:
  - `type Node struct { ID, Agent, Prompt string; DependsOn []string; Approval bool; MaxRetries *int }` and `func (n Node) Retries() int` (default 1)
  - `type Definition struct { Nodes []Node }`
  - `func ExtractYAML(description string) (string, bool)`
  - `func Parse(description string) (Definition, []string)` — returns all validation errors, or the definition with a nil slice.

- [ ] **Step 1: Write the failing tests**

```go
// server/internal/workflow/definition_test.go
package workflow

import (
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd server && go test ./internal/workflow/ -run 'TestExtractYAML|TestParse' -count=1`
Expected: FAIL to compile (`undefined: ExtractYAML`, `Parse`).

- [ ] **Step 3: Implement**

```go
// server/internal/workflow/definition.go
package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd server && go test ./internal/workflow/ -run 'TestExtractYAML|TestParse' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/internal/workflow/definition.go server/internal/workflow/definition_test.go
git commit -m "feat(workflow): parse and validate YAML workflow definitions"
```

---

### Task 2: Metadata types, transition tables, run evaluation (pure)

**Files:**
- Create: `server/internal/workflow/meta.go`, `server/internal/workflow/transitions.go`, `server/internal/workflow/run.go`
- Test: `server/internal/workflow/transitions_test.go`

**Interfaces:**
- Produces (used by Tasks 3–7):
  - `const MetaKey = "workflow"`
  - `type Phase string` with `PhasePending|PhaseRunning|PhaseBlocked|PhaseDone|PhaseFailed`
  - `type RunState string` with `RunExpanding|RunRunning|RunInvalid|RunBlocked|RunDone`
  - `type StepMeta struct { Run, Node string; Deps []string; Agent, AgentID string; Approval bool; MaxRetries, Attempts int; Phase Phase }` (JSON tags `run,node,deps,agent,agent_id,approval,max_retries,attempts,phase`)
  - `type DefMeta struct { State RunState; ErrorHash, ClaimedAt string; Total int }` (JSON tags `state,error_hash,claimed_at,total`, the last three `omitempty`)
  - `type Event string` with `EventDepsMet|EventAgentFinished|EventAgentFailed|EventAccept|EventReject|EventRetry`
  - `type Action string` with `ActionDispatch|ActionDispatchWithFeedback|ActionRequestReview|ActionCommentFailure`
  - `type Rule struct { From Phase; Event Event; Guard func(StepMeta) bool; To Phase; Actions []Action }`
  - `var Rules []Rule`, `var PhaseStatus map[Phase]string`
  - `func Next(s StepMeta, ev Event) (Rule, bool)`, `func (s StepMeta) Apply(r Rule) StepMeta`
  - `type Outcome string` (`OutcomeActive|OutcomeDone|OutcomeBlocked|OutcomeBroken`), `func Evaluate(steps []StepMeta, total int) Outcome`, `func ReadyNodes(steps []StepMeta) []string`

- [ ] **Step 1: Write the failing tests**

```go
// server/internal/workflow/transitions_test.go
package workflow

import "testing"

func step(phase Phase, approval bool, attempts, max int) StepMeta {
	return StepMeta{Run: "r", Node: "n", Phase: phase, Approval: approval, Attempts: attempts, MaxRetries: max}
}

func TestNextTable(t *testing.T) {
	cases := []struct {
		name   string
		in     StepMeta
		ev     Event
		wantOK bool
		to     Phase
		attempts int
	}{
		{"deps met starts running", step(PhasePending, false, 0, 1), EventDepsMet, true, PhaseRunning, 0},
		{"finish without approval", step(PhaseRunning, false, 0, 1), EventAgentFinished, true, PhaseDone, 0},
		{"finish with approval waits", step(PhaseRunning, true, 0, 1), EventAgentFinished, true, PhaseBlocked, 0},
		{"failure with retries left retries", step(PhaseRunning, false, 0, 1), EventAgentFailed, true, PhaseRunning, 1},
		{"failure with none left fails", step(PhaseRunning, false, 1, 1), EventAgentFailed, true, PhaseFailed, 1},
		{"accept completes", step(PhaseBlocked, true, 0, 1), EventAccept, true, PhaseDone, 0},
		{"reject with retries left redoes", step(PhaseBlocked, true, 0, 2), EventReject, true, PhaseRunning, 1},
		{"reject with none left fails", step(PhaseBlocked, true, 2, 2), EventReject, true, PhaseFailed, 2},
		{"retry resets attempts", step(PhaseFailed, false, 3, 1), EventRetry, true, PhaseRunning, 0},
		{"accept on a running step is ignored", step(PhaseRunning, true, 0, 1), EventAccept, false, "", 0},
		{"zero max_retries never retries", step(PhaseRunning, false, 0, 0), EventAgentFailed, true, PhaseFailed, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rule, ok := Next(c.in, c.ev)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			got := c.in.Apply(rule)
			if got.Phase != c.to || got.Attempts != c.attempts {
				t.Fatalf("got phase=%s attempts=%d, want %s/%d", got.Phase, got.Attempts, c.to, c.attempts)
			}
		})
	}
}

func TestPhaseStatusCoversEveryPhase(t *testing.T) {
	for _, p := range []Phase{PhasePending, PhaseRunning, PhaseBlocked, PhaseDone, PhaseFailed} {
		if PhaseStatus[p] == "" {
			t.Fatalf("no status for phase %s", p)
		}
	}
}

func TestEvaluate(t *testing.T) {
	mk := func(node string, p Phase, deps ...string) StepMeta {
		return StepMeta{Run: "r", Node: node, Phase: p, Deps: deps}
	}
	cases := []struct {
		name  string
		steps []StepMeta
		total int
		want  Outcome
	}{
		{"all done", []StepMeta{mk("a", PhaseDone), mk("b", PhaseDone, "a")}, 2, OutcomeDone},
		{"running is active", []StepMeta{mk("a", PhaseRunning)}, 1, OutcomeActive},
		{"awaiting approval is active", []StepMeta{mk("a", PhaseBlocked)}, 1, OutcomeActive},
		{"ready pending is active", []StepMeta{mk("a", PhaseDone), mk("b", PhasePending, "a")}, 2, OutcomeActive},
		{"failed with nothing active blocks", []StepMeta{mk("a", PhaseFailed), mk("b", PhasePending, "a")}, 2, OutcomeBlocked},
		{"failed but sibling running stays active", []StepMeta{mk("a", PhaseFailed), mk("b", PhaseRunning)}, 2, OutcomeActive},
		{"missing step is broken", []StepMeta{mk("a", PhaseDone)}, 2, OutcomeBroken},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Evaluate(c.steps, c.total); got != c.want {
				t.Fatalf("Evaluate = %s, want %s", got, c.want)
			}
		})
	}
}

func TestReadyNodes(t *testing.T) {
	steps := []StepMeta{
		{Node: "a", Phase: PhaseDone},
		{Node: "b", Phase: PhasePending, Deps: []string{"a"}},
		{Node: "c", Phase: PhasePending, Deps: []string{"a", "b"}},
		{Node: "d", Phase: PhasePending},
	}
	got := ReadyNodes(steps)
	if len(got) != 2 || got[0] != "b" || got[1] != "d" {
		t.Fatalf("ReadyNodes = %v, want [b d]", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd server && go test ./internal/workflow/ -run 'TestNext|TestPhase|TestEvaluate|TestReady' -count=1`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

```go
// server/internal/workflow/meta.go
package workflow

// MetaKey is the issue.metadata key holding all workflow state.
const MetaKey = "workflow"

type Phase string

const (
	PhasePending Phase = "pending"
	PhaseRunning Phase = "running"
	PhaseBlocked Phase = "blocked"
	PhaseDone    Phase = "done"
	PhaseFailed  Phase = "failed"
)

type RunState string

const (
	RunExpanding RunState = "expanding"
	RunRunning   RunState = "running"
	RunInvalid   RunState = "invalid"
	RunBlocked   RunState = "blocked"
	RunDone      RunState = "done"
)

// StepMeta is the metadata of one step issue.
type StepMeta struct {
	Run        string   `json:"run"`
	Node       string   `json:"node"`
	Deps       []string `json:"deps"`
	Agent      string   `json:"agent"`
	AgentID    string   `json:"agent_id"`
	Approval   bool     `json:"approval"`
	MaxRetries int      `json:"max_retries"`
	Attempts   int      `json:"attempts"`
	Phase      Phase    `json:"phase"`
}

// DefMeta is the metadata of a definition issue.
type DefMeta struct {
	State     RunState `json:"state"`
	ErrorHash string   `json:"error_hash,omitempty"`
	ClaimedAt string   `json:"claimed_at,omitempty"`
	Total     int      `json:"total,omitempty"`
}
```

```go
// server/internal/workflow/transitions.go
package workflow

// This file is the only place that branches on phase or issue status. To
// change how steps move, edit Rules and PhaseStatus.

type Event string

const (
	EventDepsMet       Event = "deps_met"
	EventAgentFinished Event = "agent_finished"
	EventAgentFailed   Event = "agent_failed"
	EventAccept        Event = "accept"
	EventReject        Event = "reject"
	EventRetry         Event = "retry"
)

type Action string

const (
	ActionDispatch             Action = "dispatch"
	ActionDispatchWithFeedback Action = "dispatch_with_feedback"
	ActionRequestReview        Action = "request_review"
	ActionCommentFailure       Action = "comment_failure"
	actionBumpAttempts         Action = "bump_attempts"
	actionResetAttempts        Action = "reset_attempts"
)

type Rule struct {
	From    Phase
	Event   Event
	Guard   func(StepMeta) bool
	To      Phase
	Actions []Action
}

func approvalStep(s StepMeta) bool   { return s.Approval }
func noApprovalStep(s StepMeta) bool { return !s.Approval }
func canRetry(s StepMeta) bool       { return s.Attempts < s.MaxRetries }
func noRetriesLeft(s StepMeta) bool  { return s.Attempts >= s.MaxRetries }

// Rules is evaluated top to bottom; the first rule whose From, Event and
// Guard match wins.
var Rules = []Rule{
	{From: PhasePending, Event: EventDepsMet, To: PhaseRunning, Actions: []Action{ActionDispatch}},
	{From: PhaseRunning, Event: EventAgentFinished, Guard: noApprovalStep, To: PhaseDone},
	{From: PhaseRunning, Event: EventAgentFinished, Guard: approvalStep, To: PhaseBlocked, Actions: []Action{ActionRequestReview}},
	{From: PhaseRunning, Event: EventAgentFailed, Guard: canRetry, To: PhaseRunning, Actions: []Action{actionBumpAttempts, ActionDispatch}},
	{From: PhaseRunning, Event: EventAgentFailed, Guard: noRetriesLeft, To: PhaseFailed, Actions: []Action{ActionCommentFailure}},
	{From: PhaseBlocked, Event: EventAccept, To: PhaseDone},
	{From: PhaseBlocked, Event: EventReject, Guard: canRetry, To: PhaseRunning, Actions: []Action{actionBumpAttempts, ActionDispatchWithFeedback}},
	{From: PhaseBlocked, Event: EventReject, Guard: noRetriesLeft, To: PhaseFailed, Actions: []Action{ActionCommentFailure}},
	{From: PhaseFailed, Event: EventRetry, To: PhaseRunning, Actions: []Action{actionResetAttempts, ActionDispatch}},
}

// PhaseStatus maps each phase to the issue status that represents it.
var PhaseStatus = map[Phase]string{
	PhasePending: "backlog",
	PhaseRunning: "in_progress",
	PhaseBlocked: "blocked",
	PhaseDone:    "done",
	PhaseFailed:  "blocked",
}

// Next returns the rule that applies to step s for event ev.
func Next(s StepMeta, ev Event) (Rule, bool) {
	for _, r := range Rules {
		if r.From == s.Phase && r.Event == ev && (r.Guard == nil || r.Guard(s)) {
			return r, true
		}
	}
	return Rule{}, false
}

// Apply returns s moved to the rule's phase with its attempt bookkeeping
// applied. Side-effecting actions are performed by the engine.
func (s StepMeta) Apply(r Rule) StepMeta {
	s.Phase = r.To
	for _, a := range r.Actions {
		switch a {
		case actionBumpAttempts:
			s.Attempts++
		case actionResetAttempts:
			s.Attempts = 0
		}
	}
	return s
}
```

```go
// server/internal/workflow/run.go
package workflow

type Outcome string

const (
	OutcomeActive  Outcome = "active"
	OutcomeDone    Outcome = "done"
	OutcomeBlocked Outcome = "blocked"
	// OutcomeBroken means a step issue disappeared (len(steps) != total).
	OutcomeBroken Outcome = "broken"
)

// ReadyNodes returns, in input order, the pending nodes whose dependencies
// are all done.
func ReadyNodes(steps []StepMeta) []string {
	done := map[string]bool{}
	for _, s := range steps {
		if s.Phase == PhaseDone {
			done[s.Node] = true
		}
	}
	var ready []string
	for _, s := range steps {
		if s.Phase != PhasePending {
			continue
		}
		ok := true
		for _, d := range s.Deps {
			if !done[d] {
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, s.Node)
		}
	}
	return ready
}

// Evaluate classifies a run from its step metadata.
func Evaluate(steps []StepMeta, total int) Outcome {
	if len(steps) != total {
		return OutcomeBroken
	}
	done, failed, active := 0, false, false
	for _, s := range steps {
		switch s.Phase {
		case PhaseDone:
			done++
		case PhaseFailed:
			failed = true
		case PhaseRunning, PhaseBlocked:
			active = true
		}
	}
	if done == total {
		return OutcomeDone
	}
	if active || len(ReadyNodes(steps)) > 0 {
		return OutcomeActive
	}
	if failed {
		return OutcomeBlocked
	}
	return OutcomeActive
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd server && go test ./internal/workflow/ -count=1`
Expected: PASS (Task 1 and Task 2 tests).

- [ ] **Step 5: Commit**

```bash
git add server/internal/workflow
git commit -m "feat(workflow): data-driven step transition table and run evaluation"
```

---

### Task 3: SQL queries, metadata helpers, test harness

**Files:**
- Create: `server/pkg/db/queries/workflow.sql`, `server/internal/workflow/store.go`, `server/internal/workflow/testenv_test.go`
- Test: `server/internal/workflow/store_test.go`
- Generated: `server/pkg/db/generated/workflow.sql.go` (by `make sqlc`)

**Interfaces:**
- Produces sqlc methods on `*db.Queries`:
  - `ListWorkflowDefinitionCandidates(ctx, rowLimit int32) ([]db.Issue, error)`
  - `ClaimWorkflowDefinition(ctx, db.ClaimWorkflowDefinitionParams{Value []byte, ID, WorkspaceID pgtype.UUID, StaleBefore pgtype.Timestamptz}) (db.Issue, error)` — `pgx.ErrNoRows` when not claimable
  - `ListRunningWorkflowDefinitions(ctx, rowLimit int32) ([]db.Issue, error)`
  - `ListWorkflowChildren(ctx, db.ListWorkflowChildrenParams{WorkspaceID, ParentIssueID pgtype.UUID}) ([]db.Issue, error)` — by parent; only for orphan adoption and parentage checks
  - `ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID pgtype.UUID, Run string}) ([]db.Issue, error)` — **the way the engine finds a run's steps** (by `metadata.workflow.run`, never by parent)
  - `LatestWorkflowTaskStatus(ctx, db.LatestWorkflowTaskStatusParams{IssueID, AgentID pgtype.UUID}) (string, error)`
- Produces in `store.go`: `readStepMeta(db.Issue) (StepMeta, bool)`, `readDefMeta(db.Issue) (DefMeta, bool)`, `(*Engine).writeMeta(ctx, db.Issue, any) error` (Engine is defined in Task 5; this task defines the `Engine` struct and interfaces too — see Step 3).
- Test helper: `newEnv(t) *env` with fields `pool, q, fx, ws, user, userUUID, wsUUID`, plus `env.agent(name) string`, `env.flowIssue(title, desc string) db.Issue` (creates a `todo` issue with a `flow:test` label, creator = the member).

- [ ] **Step 1: Write the SQL**

```sql
-- server/pkg/db/queries/workflow.sql

-- name: ListWorkflowDefinitionCandidates :many
-- Todo issues carrying a flow:<name> label that have not been expanded yet
-- (no workflow metadata) or were previously marked invalid.
SELECT i.* FROM issue i
WHERE i.status = 'todo'
  AND (NOT (i.metadata ? 'workflow') OR i.metadata->'workflow'->>'state' = 'invalid')
  AND EXISTS (
      SELECT 1 FROM issue_to_label itl
      JOIN issue_label l ON l.id = itl.label_id
      WHERE itl.issue_id = i.id
        AND l.workspace_id = i.workspace_id
        AND l.resource_type = 'issue'
        AND LOWER(l.name) LIKE 'flow:_%'
  )
ORDER BY i.created_at ASC
LIMIT sqlc.arg('row_limit')::int;

-- name: ClaimWorkflowDefinition :one
-- Atomically claims a definition for expansion. Only one caller wins: the issue
-- must have no workflow metadata, be marked invalid, or hold an expanding claim
-- older than stale_before.
UPDATE issue SET
    metadata = jsonb_set(metadata, '{workflow}', sqlc.arg('value')::jsonb),
    revision = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND (
      NOT (metadata ? 'workflow')
      OR metadata->'workflow'->>'state' = 'invalid'
      OR (metadata->'workflow'->>'state' = 'expanding'
          AND (metadata->'workflow'->>'claimed_at')::timestamptz < sqlc.arg('stale_before')::timestamptz)
  )
RETURNING *;

-- name: ListRunningWorkflowDefinitions :many
SELECT * FROM issue
WHERE metadata @> '{"workflow": {"state": "running"}}'::jsonb
ORDER BY created_at ASC
LIMIT sqlc.arg('row_limit')::int;

-- name: ListWorkflowChildren :many
-- Direct children by parent_issue_id. Used only to find orphaned steps (a step
-- issue created but not yet stamped with metadata) and to verify parentage.
SELECT * FROM issue
WHERE workspace_id = sqlc.arg('workspace_id') AND parent_issue_id = sqlc.arg('parent_issue_id')
ORDER BY created_at ASC, number ASC;

-- name: ListWorkflowSteps :many
-- The authoritative way to find a run's steps: by the run id stamped in
-- metadata, not by parent_issue_id, so a step whose parent link is ever lost
-- (or cleared by another flow) is still tracked. GIN-indexed via metadata.
SELECT * FROM issue
WHERE workspace_id = sqlc.arg('workspace_id')
  AND metadata @> jsonb_build_object('workflow', jsonb_build_object('run', sqlc.arg('run')::text))
ORDER BY created_at ASC, number ASC;

-- name: LatestWorkflowTaskStatus :one
SELECT status FROM agent_task_queue
WHERE issue_id = sqlc.arg('issue_id') AND agent_id = sqlc.arg('agent_id')
ORDER BY created_at DESC
LIMIT 1;
```

- [ ] **Step 2: Generate and build**

Run: `make sqlc && cd server && go build ./pkg/db/...`
Expected: `workflow.sql.go` appears in `server/pkg/db/generated/`; build succeeds. If sqlc reports a parameter-name conflict, rename the arg in the SQL, not the Go.

- [ ] **Step 3: Write `store.go` (Engine skeleton + metadata helpers)**

```go
// server/internal/workflow/store.go
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// IssueCreator is *service.IssueService.
type IssueCreator interface {
	Create(ctx context.Context, p service.IssueCreateParams, opts service.IssueCreateOpts) (service.IssueCreateResult, error)
}

// TaskEnqueuer is *service.TaskService.
type TaskEnqueuer interface {
	EnqueueTaskForIssue(ctx context.Context, issue db.Issue, triggerCommentID ...pgtype.UUID) (db.AgentTaskQueue, error)
	EnqueueTaskForMention(ctx context.Context, issue db.Issue, agentID, triggerCommentID pgtype.UUID, origin service.RunOrigin) (db.AgentTaskQueue, error)
}

// EventPublisher emits events in the shapes existing listeners and the UI
// expect. handler.WorkflowEvents implements it.
type EventPublisher interface {
	IssuePayload(issue db.Issue, atts []db.Attachment, labels []db.IssueLabel) map[string]any
	IssueUpdated(ctx context.Context, prev, cur db.Issue)
	CommentCreated(ctx context.Context, issue db.Issue, c db.Comment)
}

type Engine struct {
	Q      *db.Queries
	Issues IssueCreator
	Tasks  TaskEnqueuer
	Events EventPublisher
	// Now is overridable in tests; nil means time.Now.
	Now func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func metaOf(issue db.Issue, v any) bool {
	var bag map[string]json.RawMessage
	if err := json.Unmarshal(issue.Metadata, &bag); err != nil {
		return false
	}
	raw, ok := bag[MetaKey]
	if !ok {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

func readStepMeta(i db.Issue) (StepMeta, bool) {
	var m StepMeta
	if !metaOf(i, &m) || m.Run == "" {
		return StepMeta{}, false
	}
	return m, true
}

func readDefMeta(i db.Issue) (DefMeta, bool) {
	var m DefMeta
	if !metaOf(i, &m) || m.State == "" {
		return DefMeta{}, false
	}
	return m, true
}

// writeMeta stores v under MetaKey. SetIssueMetadataKey returns no rows when
// the value is unchanged, which is not an error here.
func (e *Engine) writeMeta(ctx context.Context, issue db.Issue, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = e.Q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{
		Key: MetaKey, Value: raw, ID: issue.ID, WorkspaceID: issue.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
```

- [ ] **Step 4: Write the test environment**

```go
// server/internal/workflow/testenv_test.go
package workflow

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	seq      atomic.Int64
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		url := os.Getenv("DATABASE_URL")
		if url == "" {
			url = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
		}
		p, err := pgxpool.New(context.Background(), url)
		if err == nil && p.Ping(context.Background()) == nil {
			pool = p
		}
	})
	if pool == nil {
		t.Skip("database not reachable")
	}
	return pool
}

// recorder is a fake TaskEnqueuer and EventPublisher.
type recorder struct {
	mu       sync.Mutex
	enqueued []string // "issue:<id>" or "mention:<id>:<comment>"
	failNext error
	updated  int
	comments []db.Comment
}

func (r *recorder) EnqueueTaskForIssue(_ context.Context, issue db.Issue, _ ...pgtype.UUID) (db.AgentTaskQueue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext != nil {
		err := r.failNext
		r.failNext = nil
		return db.AgentTaskQueue{}, err
	}
	r.enqueued = append(r.enqueued, "issue:"+util.UUIDToString(issue.ID))
	return db.AgentTaskQueue{}, nil
}

func (r *recorder) EnqueueTaskForMention(_ context.Context, issue db.Issue, _ pgtype.UUID, comment pgtype.UUID, _ service.RunOrigin) (db.AgentTaskQueue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enqueued = append(r.enqueued, "mention:"+util.UUIDToString(issue.ID)+":"+util.UUIDToString(comment))
	return db.AgentTaskQueue{}, nil
}

func (r *recorder) IssuePayload(db.Issue, []db.Attachment, []db.IssueLabel) map[string]any {
	return map[string]any{}
}
func (r *recorder) IssueUpdated(context.Context, db.Issue, db.Issue) {
	r.mu.Lock()
	r.updated++
	r.mu.Unlock()
}
func (r *recorder) CommentCreated(_ context.Context, _ db.Issue, c db.Comment) {
	r.mu.Lock()
	r.comments = append(r.comments, c)
	r.mu.Unlock()
}

type env struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	fx      *dbfx.Fixture
	ws      string
	user    string
	wsUUID  pgtype.UUID
	runtime string
	rec     *recorder
	engine  *Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	p := testPool(t)
	n := seq.Add(1)
	fx := dbfx.New(p, "", "")
	user := fx.User(t, "Flow User", fmt.Sprintf("flow-%d-%d@example.test", os.Getpid(), n))
	ws := fx.Workspace(t, "Flow WS", fmt.Sprintf("flow-%d-%d", os.Getpid(), n))
	fx.WorkspaceID, fx.UserID = ws, user
	fx.Member(t, ws, user, "owner")
	rt := fx.Runtime(t, "flow-runtime")
	// Registered after the workspace fixture, so it runs before the workspace
	// is deleted and removes the issues the engine creates.
	fx.Cleanup(t, `DELETE FROM issue WHERE workspace_id = $1`, ws)
	q := db.New(p)
	rec := &recorder{}
	issues := service.NewIssueService(q, p, events.New(), analytics.NoopClient{}, nil)
	wsUUID, _ := util.ParseUUID(ws)
	return &env{pool: p, q: q, fx: fx, ws: ws, user: user, wsUUID: wsUUID, runtime: rt, rec: rec,
		engine: &Engine{Q: q, Issues: issues, Tasks: rec, Events: rec}}
}

func (e *env) agent(t *testing.T, name string) string {
	t.Helper()
	return e.fx.Agent(t, name, e.runtime)
}

// flowIssue creates a todo issue labelled flow:test, created by the member.
func (e *env) flowIssue(t *testing.T, title, desc string) db.Issue {
	t.Helper()
	id := e.fx.Issue(t, title, dbfx.Cols{"description": desc, "status": "todo"})
	label := e.fx.Insert(t, "issue_label", dbfx.Cols{"workspace_id": e.ws, "name": "flow:test", "resource_type": "issue", "color": "#888888"})
	e.fx.Exec(t, `INSERT INTO issue_to_label (issue_id, label_id) VALUES ($1, $2)`, id, label)
	e.fx.Cleanup(t, `DELETE FROM issue_to_label WHERE issue_id = $1`, id)
	uid, _ := util.ParseUUID(id)
	issue, err := e.q.GetIssue(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return issue
}
```

(If `Fixture.Issue` does not accept `description`/`status` columns or sets another `creator_type`, adjust the Cols to match the `issue` table; the helper's contract is a `todo` issue, creator = member `e.user`, with the given description.)

- [ ] **Step 5: Write failing store tests**

```go
// server/internal/workflow/store_test.go
package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCandidatesAndAtomicClaim(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	def := e.flowIssue(t, "Ship", "```yaml\nnodes: []\n```")
	plain := e.fx.Issue(t, "no label", dbfxCols("todo"))
	_ = plain

	cands, err := e.q.ListWorkflowDefinitionCandidates(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cands {
		if c.ID == def.ID {
			found = true
		}
		if c.Title == "no label" {
			t.Fatal("unlabelled issue must not be a candidate")
		}
	}
	if !found {
		t.Fatal("labelled todo issue should be a candidate")
	}

	value, _ := json.Marshal(DefMeta{State: RunExpanding, ClaimedAt: time.Now().UTC().Format(time.RFC3339)})
	params := db.ClaimWorkflowDefinitionParams{
		Value: value, ID: def.ID, WorkspaceID: def.WorkspaceID,
		StaleBefore: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	}
	if _, err := e.q.ClaimWorkflowDefinition(ctx, params); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := e.q.ClaimWorkflowDefinition(ctx, params); err != pgx.ErrNoRows {
		t.Fatalf("second claim = %v, want ErrNoRows", err)
	}
	// A claim older than stale_before is reclaimable.
	params.StaleBefore = pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	if _, err := e.q.ClaimWorkflowDefinition(ctx, params); err != nil {
		t.Fatalf("stale reclaim: %v", err)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	issue := e.flowIssue(t, "Meta", "x")
	want := StepMeta{Run: "r1", Node: "build", Deps: []string{"plan"}, Agent: "Coder", AgentID: "a1", MaxRetries: 2, Phase: PhasePending}
	if err := e.engine.writeMeta(ctx, issue, want); err != nil {
		t.Fatal(err)
	}
	if err := e.engine.writeMeta(ctx, issue, want); err != nil {
		t.Fatalf("rewriting the same value must be a no-op, got %v", err)
	}
	got, err := e.q.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := readStepMeta(got)
	if !ok || m.Node != "build" || m.Deps[0] != "plan" || m.Phase != PhasePending {
		t.Fatalf("readStepMeta = %+v, %v", m, ok)
	}
	if _, ok := readDefMeta(got); ok {
		t.Fatal("step metadata must not parse as definition metadata")
	}
}
```

Add this helper at the bottom of `testenv_test.go`:

```go
func dbfxCols(status string) dbfx.Cols { return dbfx.Cols{"status": status} }
```

- [ ] **Step 6: Run to verify**

Run: `cd server && go test ./internal/workflow/ -run 'TestCandidates|TestMeta' -count=1 -v`
Expected: PASS (or SKIP with "database not reachable"; run with `make up` first so it actually executes).

- [ ] **Step 7: Commit**

```bash
git add server/pkg/db/queries/workflow.sql server/pkg/db/generated server/internal/workflow
git commit -m "feat(workflow): sqlc queries, metadata helpers and DB test harness"
```

---

### Task 4: Handler bridge (`WorkflowEvents`)

**Files:**
- Create: `server/internal/handler/workflow_bridge.go`
- Test: `server/internal/handler/workflow_bridge_test.go`

**Interfaces:**
- Produces: `func (h *Handler) WorkflowEvents() *WorkflowEvents` with methods matching `workflow.EventPublisher`:
  - `IssuePayload(issue db.Issue, atts []db.Attachment, labels []db.IssueLabel) map[string]any`
  - `IssueUpdated(ctx context.Context, prev, cur db.Issue)`
  - `CommentCreated(ctx context.Context, issue db.Issue, c db.Comment)`
- Consumes: existing unexported helpers in package `handler`: `issueToResponse`, `h.getIssuePrefix`, `h.fillStatusCategory`, `buildAttachmentResponses`, `labelsToResponse`, `commentToResponse`, `h.publish`, `uuidToString`, `uuidToPtr`, `textToPtr`.

- [ ] **Step 1: Write the failing test**

```go
// server/internal/handler/workflow_bridge_test.go
package handler

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestWorkflowEventsIssueUpdatedShape(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Workflow bridge")
	cur, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	prev := cur
	prev.Status = "todo"
	cur.Status = "in_progress"

	got := make(chan events.Event, 1)
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		if p, ok := e.Payload.(map[string]any); ok {
			if r, ok := p["issue"].(IssueResponse); ok && r.ID == issueID {
				got <- e
			}
		}
	})
	testHandler.WorkflowEvents().IssueUpdated(context.Background(), prev, cur)

	select {
	case e := <-got:
		p := e.Payload.(map[string]any)
		if p["status_changed"] != true || p["prev_status"] != "todo" {
			t.Fatalf("payload = %+v", p)
		}
		if e.ActorType != "system" {
			t.Fatalf("actor type = %q, want system", e.ActorType)
		}
	case <-time.After(time.Second):
		t.Fatal("issue:updated was not published with an IssueResponse payload")
	}
}

func TestWorkflowEventsCommentCreated(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Workflow bridge comment")
	issue, _ := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	got := make(chan events.Event, 1)
	testHandler.Bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
		if p, ok := e.Payload.(map[string]any); ok {
			if c, ok := p["comment"].(CommentResponse); ok && c.IssueID == issueID {
				got <- e
			}
		}
	})
	testHandler.WorkflowEvents().CommentCreated(context.Background(), issue, db.Comment{
		ID: issue.ID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "system", Content: "hi", Type: "system",
	})
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("comment:created was not published with a CommentResponse payload")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd server && go test ./internal/handler/ -run TestWorkflowEvents -count=1`
Expected: FAIL to compile (`WorkflowEvents` undefined).

- [ ] **Step 3: Implement**

```go
// server/internal/handler/workflow_bridge.go
package handler

import (
	"context"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// WorkflowEvents lets the workflow engine publish events in the exact shapes
// the realtime hub, activity log and listeners already expect, without the
// workflow package importing handler.
type WorkflowEvents struct{ h *Handler }

func (h *Handler) WorkflowEvents() *WorkflowEvents { return &WorkflowEvents{h: h} }

// IssuePayload is the IssueCreateOpts.BroadcastPayload hook.
func (w *WorkflowEvents) IssuePayload(issue db.Issue, atts []db.Attachment, labels []db.IssueLabel) map[string]any {
	ctx := context.Background()
	resp := issueToResponse(issue, w.h.getIssuePrefix(ctx, issue.WorkspaceID))
	w.h.fillStatusCategory(ctx, issue.WorkspaceID, &resp)
	resp.Attachments = buildAttachmentResponses(atts)
	lr := labelsToResponse(labels)
	resp.Labels = &lr
	return map[string]any{"issue": resp}
}

// IssueUpdated publishes issue:updated for a status change made by the engine.
func (w *WorkflowEvents) IssueUpdated(ctx context.Context, prev, cur db.Issue) {
	resp := issueToResponse(cur, w.h.getIssuePrefix(ctx, cur.WorkspaceID))
	w.h.fillStatusCategory(ctx, cur.WorkspaceID, &resp)
	w.h.publish(protocol.EventIssueUpdated, uuidToString(cur.WorkspaceID), "system", "", map[string]any{
		"issue":               resp,
		"assignee_changed":    false,
		"status_changed":      prev.Status != cur.Status,
		"priority_changed":    false,
		"project_changed":     false,
		"start_date_changed":  false,
		"due_date_changed":    false,
		"description_changed": false,
		"title_changed":       false,
		"prev_title":          prev.Title,
		"prev_assignee_type":  textToPtr(prev.AssigneeType),
		"prev_assignee_id":    uuidToPtr(prev.AssigneeID),
		"prev_status":         prev.Status,
		"prev_priority":       prev.Priority,
		"prev_description":    textToPtr(prev.Description),
		"creator_type":        prev.CreatorType,
		"creator_id":          uuidToString(prev.CreatorID),
	})
}

// CommentCreated publishes comment:created for a system comment the engine
// inserted. It does not trigger agents (that happens only in CreateComment).
func (w *WorkflowEvents) CommentCreated(ctx context.Context, issue db.Issue, c db.Comment) {
	w.h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(c, nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      issue.Revision,
	})
}
```

- [ ] **Step 4: Run to verify pass, then build the package**

Run: `cd server && go test ./internal/handler/ -run TestWorkflowEvents -count=1 && go build ./...`
Expected: PASS. If a helper's signature differs from the call (for example `fillStatusCategory` or `buildAttachmentResponses`), mirror the call used at `internal/handler/issue.go:3426-3441` exactly.

- [ ] **Step 5: Commit**

```bash
git add server/internal/handler/workflow_bridge.go server/internal/handler/workflow_bridge_test.go
git commit -m "feat(workflow): handler bridge publishing events in existing shapes"
```

---

### Task 5: Expansion (claim, validate, create step issues)

**Files:**
- Create: `server/internal/workflow/expand.go`
- Test: `server/internal/workflow/expand_test.go`

**Interfaces:**
- Consumes: `Parse`, `StepMeta`, `DefMeta`, `Engine`, sqlc queries from Task 3, `service.IssueCreateParams`, `ListAgents`.
- Produces:
  - `func (e *Engine) ExpandCandidates(ctx context.Context) error` — runs `Expand` over `ListWorkflowDefinitionCandidates(ctx, 50)`.
  - `func (e *Engine) Expand(ctx context.Context, def db.Issue) error`
  - `func (e *Engine) systemComment(ctx context.Context, issue db.Issue, text string) (db.Comment, error)`
  - `const claimStaleAfter = 2 * time.Minute`

- [ ] **Step 1: Write the failing tests**

```go
// server/internal/workflow/expand_test.go
package workflow

import (
	"context"
	"strings"
	"sync"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func flowDoc(agentA, agentB string) string {
	return "```yaml\nnodes:\n  - id: plan\n    agent: " + agentA + "\n    prompt: make a plan\n  - id: build\n    depends_on: [plan]\n    agent: " + agentB + "\n    prompt: build it\n    approval: true\n```"
}

func TestExpandCreatesBacklogStepsWithMetadata(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Ship it", flowDoc("Planner", "Coder"))

	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	kids, err := e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	if err != nil || len(kids) != 2 {
		t.Fatalf("children = %d, err %v", len(kids), err)
	}
	byNode := map[string]StepMeta{}
	for _, k := range kids {
		m, ok := readStepMeta(k)
		if !ok {
			t.Fatalf("child %q has no workflow metadata", k.Title)
		}
		if k.Status != "backlog" || m.Phase != PhasePending || !k.AssigneeID.Valid {
			t.Fatalf("child %q: status=%s phase=%s assignee valid=%v", k.Title, k.Status, m.Phase, k.AssigneeID.Valid)
		}
		if k.CreatorID != def.CreatorID || k.CreatorType != def.CreatorType {
			t.Fatalf("child creator must equal the definition creator")
		}
		// Every step, including the first one created, must hang off the definition.
		if k.ParentIssueID != def.ID {
			t.Fatalf("step %q: parent_issue_id = %v, want the definition issue", m.Node, k.ParentIssueID)
		}
		byNode[m.Node] = m
	}
	if byNode["build"].Deps[0] != "plan" || !byNode["build"].Approval || byNode["plan"].MaxRetries != 1 {
		t.Fatalf("metadata = %+v", byNode)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	dm, ok := readDefMeta(got)
	if !ok || dm.State != RunRunning || dm.Total != 2 || got.Status != "in_progress" {
		t.Fatalf("definition meta = %+v status=%s", dm, got.Status)
	}
	if len(e.rec.enqueued) != 0 {
		t.Fatal("expansion must not dispatch anything")
	}
}

func TestExpandInvalidYAMLCommentsOnceAndRecoversOnEdit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Bad", flowDoc("Ghost", "Coder")) // Ghost does not exist

	for i := 0; i < 3; i++ {
		cur, _ := e.q.GetIssue(ctx, def.ID)
		if err := e.engine.Expand(ctx, cur); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.rec.comments) != 1 || !strings.Contains(e.rec.comments[0].Content, `unknown agent "Ghost"`) {
		t.Fatalf("want exactly one error comment naming the agent, got %d: %+v", len(e.rec.comments), e.rec.comments)
	}
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunInvalid {
		t.Fatalf("state = %s, want invalid", dm.State)
	}

	e.agent(t, "Ghost")
	e.fx.Exec(t, `UPDATE issue SET description = $2 WHERE id = $1`, uuidStr(def), flowDoc("Ghost", "Coder")+" ")
	cur, _ := e.q.GetIssue(ctx, def.ID)
	if err := e.engine.Expand(ctx, cur); err != nil {
		t.Fatal(err)
	}
	got, _ = e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunRunning {
		t.Fatalf("after the edit state = %s, want running", dm.State)
	}
}

func TestExpandConcurrentClaimsCreateOneSet(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Race", flowDoc("Planner", "Coder"))

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.engine.Expand(ctx, def)
		}()
	}
	wg.Wait()
	kids, _ := e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	if len(kids) != 2 {
		t.Fatalf("concurrent expansion created %d step issues, want 2", len(kids))
	}
}

func TestExpandDoesNotDuplicateAStepThatLostItsParent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Parentless", flowDoc("Planner", "Coder"))
	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	kids, _ := e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	// The first step loses its parent link, then the claim goes stale so the
	// definition is expanded again.
	e.fx.Exec(t, `UPDATE issue SET parent_issue_id = NULL WHERE id = $1`, uuidStr(kids[0]))
	e.fx.Exec(t, `UPDATE issue SET metadata = '{"workflow":{"state":"expanding","claimed_at":"2000-01-01T00:00:00Z"}}'::jsonb WHERE id = $1`, uuidStr(def))
	before := len(e.rec.comments)

	cur, _ := e.q.GetIssue(ctx, def.ID)
	if err := e.engine.Expand(ctx, cur); err != nil {
		t.Fatal(err)
	}
	steps, _ := e.q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidStr(def)})
	if len(steps) != 2 {
		t.Fatalf("a parentless step must be recognised, not recreated: %d steps", len(steps))
	}
	if len(e.rec.comments) != before+1 || !strings.Contains(e.rec.comments[before].Content, "not linked") {
		t.Fatal("the lost parent link must be surfaced in a comment")
	}
}

func TestExpandResumesAfterCrashAndAdoptsOrphans(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Resume", flowDoc("Planner", "Coder"))
	if err := e.engine.Expand(ctx, def); err != nil {
		t.Fatal(err)
	}
	kids, _ := e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	// Simulate a crash: first step lost its metadata, second step never created,
	// and the claim is stale.
	e.fx.Exec(t, `UPDATE issue SET metadata = '{}'::jsonb WHERE id = $1`, uuidStr(kids[0]))
	e.fx.Exec(t, `DELETE FROM issue WHERE id = $1`, uuidStr(kids[1]))
	e.fx.Exec(t, `UPDATE issue SET metadata = '{"workflow":{"state":"expanding","claimed_at":"2000-01-01T00:00:00Z"}}'::jsonb WHERE id = $1`, uuidStr(def))

	cur, _ := e.q.GetIssue(ctx, def.ID)
	if err := e.engine.Expand(ctx, cur); err != nil {
		t.Fatal(err)
	}
	kids, _ = e.q.ListWorkflowChildren(ctx, dbListChildren(def))
	if len(kids) != 2 {
		t.Fatalf("after resume %d step issues, want 2 (orphan adopted, missing created)", len(kids))
	}
	for _, k := range kids {
		if _, ok := readStepMeta(k); !ok {
			t.Fatalf("step %q still lacks metadata", k.Title)
		}
	}
}
```

Add these helpers to `testenv_test.go`:

```go
func uuidStr(i db.Issue) string { return util.UUIDToString(i.ID) }

func dbListChildren(def db.Issue) db.ListWorkflowChildrenParams {
	return db.ListWorkflowChildrenParams{WorkspaceID: def.WorkspaceID, ParentIssueID: def.ID}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd server && go test ./internal/workflow/ -run TestExpand -count=1`
Expected: FAIL to compile (`Expand` undefined).

- [ ] **Step 3: Implement**

```go
// server/internal/workflow/expand.go
package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const claimStaleAfter = 2 * time.Minute

// ExpandCandidates expands every eligible definition issue.
func (e *Engine) ExpandCandidates(ctx context.Context) error {
	cands, err := e.Q.ListWorkflowDefinitionCandidates(ctx, 50)
	if err != nil {
		return fmt.Errorf("list workflow candidates: %w", err)
	}
	var errs []error
	for _, def := range cands {
		if err := e.Expand(ctx, def); err != nil {
			slog.Warn("workflow expand failed", "issue_id", def.ID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func hashDescription(d pgtype.Text) string {
	sum := sha256.Sum256([]byte(d.String))
	return hex.EncodeToString(sum[:8])
}

// Expand claims def, validates its YAML, and creates one backlog step issue
// per node. It is safe to call concurrently and to re-run after a crash.
func (e *Engine) Expand(ctx context.Context, def db.Issue) error {
	hash := hashDescription(def.Description)
	if dm, ok := readDefMeta(def); ok && dm.State == RunInvalid && dm.ErrorHash == hash {
		return nil // unchanged since it was last found invalid
	}
	claim, _ := json.Marshal(DefMeta{State: RunExpanding, ClaimedAt: e.now().UTC().Format(time.RFC3339)})
	claimed, err := e.Q.ClaimWorkflowDefinition(ctx, db.ClaimWorkflowDefinitionParams{
		Value: claim, ID: def.ID, WorkspaceID: def.WorkspaceID,
		StaleBefore: pgtype.Timestamptz{Time: e.now().Add(-claimStaleAfter), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // another instance owns it
	}
	if err != nil {
		return err
	}
	def = claimed

	parsed, errs := Parse(def.Description.String)
	agents := map[string]db.Agent{}
	if len(errs) == 0 {
		list, err := e.Q.ListAgents(ctx, def.WorkspaceID)
		if err != nil {
			return err
		}
		for _, a := range list {
			agents[strings.ToLower(a.Name)] = a
		}
		for _, n := range parsed.Nodes {
			if _, ok := agents[strings.ToLower(n.Agent)]; !ok {
				errs = append(errs, fmt.Sprintf("node %q: unknown agent %q", n.ID, n.Agent))
			}
		}
	}
	if len(errs) > 0 {
		return e.markInvalid(ctx, def, hash, errs)
	}

	// Steps are found by the run id in metadata, never by parent_issue_id, so a
	// step that lost its parent link is still recognised and not duplicated.
	stamped, err := e.Q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidString(def.ID)})
	if err != nil {
		return err
	}
	byNode := map[string]db.Issue{}
	for _, k := range stamped {
		if m, ok := readStepMeta(k); ok {
			byNode[m.Node] = k
		}
	}
	// Orphans: children that were created but crashed before being stamped.
	existing, err := e.Q.ListWorkflowChildren(ctx, db.ListWorkflowChildrenParams{WorkspaceID: def.WorkspaceID, ParentIssueID: def.ID})
	if err != nil {
		return err
	}
	for _, n := range parsed.Nodes {
		agent := agents[strings.ToLower(n.Agent)]
		meta := StepMeta{
			Run: uuidString(def.ID), Node: n.ID, Deps: nonNil(n.DependsOn), Agent: agent.Name,
			AgentID: uuidString(agent.ID), Approval: n.Approval, MaxRetries: n.Retries(), Phase: PhasePending,
		}
		title := fmt.Sprintf("%s · %s", def.Title, n.ID)
		if _, ok := byNode[n.ID]; ok {
			continue
		}
		var orphan *db.Issue
		for i := range existing {
			if existing[i].Title == title {
				if _, has := readStepMeta(existing[i]); !has {
					orphan = &existing[i]
				}
			}
		}
		if orphan != nil {
			if err := e.writeMeta(ctx, *orphan, meta); err != nil {
				return err
			}
			continue
		}
		res, err := e.Issues.Create(ctx, service.IssueCreateParams{
			WorkspaceID:    def.WorkspaceID,
			Title:          title,
			Description:    pgtype.Text{String: n.Prompt, Valid: true},
			Status:         "backlog",
			Priority:       def.Priority,
			AssigneeType:   pgtype.Text{String: "agent", Valid: true},
			AssigneeID:     agent.ID,
			CreatorType:    def.CreatorType,
			CreatorID:      def.CreatorID,
			ParentIssueID:  def.ID,
			ProjectID:      def.ProjectID,
			AllowDuplicate: true,
		}, service.IssueCreateOpts{BroadcastPayload: e.Events.IssuePayload})
		if err != nil {
			return fmt.Errorf("create step %q: %w", n.ID, err)
		}
		if err := e.writeMeta(ctx, res.Issue, meta); err != nil {
			return err
		}
	}

	// Verify parentage before declaring the run started: every step must hang
	// off the definition issue. A mismatch does not stop the run (steps are
	// tracked by run id) but is surfaced so it is visible instead of silent.
	if err := e.warnOnMissingParents(ctx, def); err != nil {
		return err
	}

	prev := def
	if err := e.writeMeta(ctx, def, DefMeta{State: RunRunning, Total: len(parsed.Nodes)}); err != nil {
		return err
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: def.ID, WorkspaceID: def.WorkspaceID, Status: "in_progress"})
	if err != nil {
		return err
	}
	e.Events.IssueUpdated(ctx, prev, updated)
	return nil
}

// warnOnMissingParents comments on def when any step issue of the run is not
// its direct child.
func (e *Engine) warnOnMissingParents(ctx context.Context, def db.Issue) error {
	steps, err := e.Q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidString(def.ID)})
	if err != nil {
		return err
	}
	var bad []string
	for _, s := range steps {
		if s.ParentIssueID != def.ID {
			if m, ok := readStepMeta(s); ok {
				bad = append(bad, m.Node)
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	_, err = e.systemComment(ctx, def, "Warning: step issue(s) not linked to this issue as sub-issues: "+strings.Join(bad, ", ")+". The workflow still tracks them, but they will not show under this issue.")
	return err
}

func (e *Engine) markInvalid(ctx context.Context, def db.Issue, hash string, errs []string) error {
	if err := e.writeMeta(ctx, def, DefMeta{State: RunInvalid, ErrorHash: hash}); err != nil {
		return err
	}
	_, err := e.systemComment(ctx, def, "Workflow not started. Fix these problems, then edit the description:\n- "+strings.Join(errs, "\n- "))
	return err
}

// systemComment inserts a system-authored comment and publishes it. It does
// not wake any agent.
func (e *Engine) systemComment(ctx context.Context, issue db.Issue, text string) (db.Comment, error) {
	row, err := e.Q.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true},
		Content: text, Type: "system",
	})
	if err != nil {
		return db.Comment{}, err
	}
	c := row.Comment()
	e.Events.CommentCreated(ctx, issue, c)
	return c, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
```

Also add to `store.go` (it uses `util` for UUID formatting):

```go
// in store.go imports add: "github.com/multica-ai/multica/server/internal/util"
func uuidString(u pgtype.UUID) string { return util.UUIDToString(u) }
```

- [ ] **Step 4: Run to verify pass**

Run: `cd server && go test ./internal/workflow/ -run TestExpand -count=1 -v`
Expected: PASS. Fix any signature mismatches against the generated code (`go doc ./pkg/db/generated UpdateIssueStatusParams`, `go doc ./pkg/db/generated CreateCommentRow`).

- [ ] **Step 5: Commit**

```bash
git add server/internal/workflow
git commit -m "feat(workflow): expand flow-labelled issues into backlog step issues"
```

---

### Task 6: Applying events, tick, completion

**Files:**
- Create: `server/internal/workflow/engine.go`
- Test: `server/internal/workflow/engine_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–5.
- Produces:
  - `func (e *Engine) Tick(ctx context.Context) error`
  - `func (e *Engine) ApplyEvent(ctx context.Context, step db.Issue, ev Event, trigger pgtype.UUID) (bool, error)`
  - `func (e *Engine) reopenDefinition(ctx context.Context, def db.Issue) error`

- [ ] **Step 1: Write the failing tests**

```go
// server/internal/workflow/engine_test.go
package workflow

import (
	"context"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// expanded returns an env with an expanded two-step flow: plan -> build(approval).
func expanded(t *testing.T) (*env, db.Issue) {
	t.Helper()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Ship", flowDoc("Planner", "Coder"))
	if err := e.engine.Expand(context.Background(), def); err != nil {
		t.Fatal(err)
	}
	return e, def
}

func stepByNode(t *testing.T, e *env, def db.Issue, node string) db.Issue {
	t.Helper()
	kids, err := e.q.ListWorkflowChildren(context.Background(), dbListChildren(def))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kids {
		if m, ok := readStepMeta(k); ok && m.Node == node {
			return k
		}
	}
	t.Fatalf("no step %q", node)
	return db.Issue{}
}

func phaseOf(t *testing.T, e *env, def db.Issue, node string) Phase {
	t.Helper()
	m, _ := readStepMeta(stepByNode(t, e, def, node))
	return m.Phase
}

func setStatus(t *testing.T, e *env, i db.Issue, status string) {
	t.Helper()
	e.fx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, uuidStr(i), status)
}

func TestTickRunsStepsInDependencyOrder(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)

	if err := e.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "plan") != PhaseRunning || phaseOf(t, e, def, "build") != PhasePending {
		t.Fatal("tick 1: only plan should be running")
	}
	plan := stepByNode(t, e, def, "plan")
	if plan.Status != "in_progress" || len(e.rec.enqueued) != 1 {
		t.Fatalf("plan status=%s enqueued=%v", plan.Status, e.rec.enqueued)
	}

	if err := e.engine.Tick(ctx); err != nil { // nothing changed: idempotent
		t.Fatal(err)
	}
	if len(e.rec.enqueued) != 1 {
		t.Fatalf("idle tick re-dispatched: %v", e.rec.enqueued)
	}

	setStatus(t, e, plan, "done") // the agent finishes
	if err := e.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "plan") != PhaseDone {
		t.Fatal("plan should be done")
	}
	if err := e.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "build") != PhaseRunning || len(e.rec.enqueued) != 2 {
		t.Fatalf("build should be dispatched after plan: %v", e.rec.enqueued)
	}
}

func TestApprovalGateAcceptCompletesWorkflow(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	_ = e.engine.Tick(ctx)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	_ = e.engine.Tick(ctx)
	_ = e.engine.Tick(ctx)
	build := stepByNode(t, e, def, "build")
	setStatus(t, e, build, "in_review") // agent finished, approval required
	_ = e.engine.Tick(ctx)

	build = stepByNode(t, e, def, "build")
	if m, _ := readStepMeta(build); m.Phase != PhaseBlocked || build.Status != "blocked" {
		t.Fatalf("build phase=%s status=%s, want blocked/blocked", m.Phase, build.Status)
	}
	if ok, err := e.engine.ApplyEvent(ctx, build, EventAccept, pgtypeUUIDZero()); err != nil || !ok {
		t.Fatalf("accept = %v, %v", ok, err)
	}
	_ = e.engine.Tick(ctx)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunDone || got.Status != "done" {
		t.Fatalf("definition state=%s status=%s, want done/done", dm.State, got.Status)
	}
}

func TestRejectRedispatchesWithFeedbackThenFails(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	_ = e.engine.Tick(ctx)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	_ = e.engine.Tick(ctx)
	_ = e.engine.Tick(ctx)
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	_ = e.engine.Tick(ctx)

	comment := commentID(t, e, def)
	build := stepByNode(t, e, def, "build")
	if ok, _ := e.engine.ApplyEvent(ctx, build, EventReject, comment); !ok {
		t.Fatal("first reject should redo")
	}
	last := e.rec.enqueued[len(e.rec.enqueued)-1]
	if !strings.HasPrefix(last, "mention:") {
		t.Fatalf("redo must carry the reject comment as trigger, got %q", last)
	}
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	_ = e.engine.Tick(ctx)
	build = stepByNode(t, e, def, "build")
	if ok, _ := e.engine.ApplyEvent(ctx, build, EventReject, comment); !ok {
		t.Fatal("second reject should be applied")
	}
	if phaseOf(t, e, def, "build") != PhaseFailed {
		t.Fatalf("with max_retries=1 the second reject must fail the step, phase=%s", phaseOf(t, e, def, "build"))
	}
	_ = e.engine.Tick(ctx)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunBlocked || got.Status != "blocked" {
		t.Fatalf("definition state=%s status=%s, want blocked", dm.State, got.Status)
	}
	build = stepByNode(t, e, def, "build")
	if ok, _ := e.engine.ApplyEvent(ctx, build, EventRetry, pgtypeUUIDZero()); !ok {
		t.Fatal("retry should restart a failed step")
	}
	if m, _ := readStepMeta(stepByNode(t, e, def, "build")); m.Phase != PhaseRunning || m.Attempts != 0 {
		t.Fatalf("after retry phase=%s attempts=%d", m.Phase, m.Attempts)
	}
}

func TestDispatchErrorFailsStepWithComment(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	e.rec.failNext = errString("agent is archived")
	_ = e.engine.Tick(ctx)
	if phaseOf(t, e, def, "plan") != PhaseFailed {
		t.Fatalf("plan phase = %s, want failed", phaseOf(t, e, def, "plan"))
	}
	found := false
	for _, c := range e.rec.comments {
		if strings.Contains(c.Content, "agent is archived") {
			found = true
		}
	}
	if !found {
		t.Fatal("the failure reason must be commented on the step")
	}
	_ = e.engine.Tick(ctx)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunBlocked {
		t.Fatalf("workflow state = %s, want blocked", dm.State)
	}
}

func TestDeletedStepBlocksInsteadOfCompleting(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	e.fx.Exec(t, `DELETE FROM issue WHERE id = $1`, uuidStr(stepByNode(t, e, def, "build")))
	_ = e.engine.Tick(ctx)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	_ = e.engine.Tick(ctx)
	got, _ := e.q.GetIssue(ctx, def.ID)
	dm, _ := readDefMeta(got)
	if dm.State != RunBlocked || got.Status == "done" {
		t.Fatalf("a missing step must block the workflow, state=%s status=%s", dm.State, got.Status)
	}
}

func TestStepWithoutParentIsStillTracked(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	plan := stepByNode(t, e, def, "plan")
	e.fx.Exec(t, `UPDATE issue SET parent_issue_id = NULL WHERE id = $1`, uuidStr(plan))

	_ = e.engine.Tick(ctx)
	if phaseOf2(t, e, def, "plan") != PhaseRunning {
		t.Fatal("a step that lost its parent must still be dispatched")
	}
	setStatus(t, e, plan, "done")
	_ = e.engine.Tick(ctx)
	_ = e.engine.Tick(ctx)
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State == RunBlocked {
		t.Fatal("a parentless step must not make the run look broken")
	}
	if phaseOf2(t, e, def, "build") != PhaseRunning {
		t.Fatal("the dependent step should run after the parentless one finishes")
	}
}

// phaseOf2 finds the step by run id, so it works when the parent link is gone.
func phaseOf2(t *testing.T, e *env, def db.Issue, node string) Phase {
	t.Helper()
	steps, err := e.q.ListWorkflowSteps(context.Background(), db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidStr(def)})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if m, ok := readStepMeta(s); ok && m.Node == node {
			return m.Phase
		}
	}
	t.Fatalf("no step %q", node)
	return ""
}

func TestAgentTaskFailureRetriesThenFails(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	_ = e.engine.Tick(ctx)
	plan := stepByNode(t, e, def, "plan")
	m, _ := readStepMeta(plan)
	for i := 0; i < 2; i++ {
		e.fx.Exec(t, `INSERT INTO agent_task_queue (agent_id, issue_id, status) VALUES ($1, $2, 'failed')`, m.AgentID, uuidStr(plan))
		_ = e.engine.Tick(ctx)
	}
	if phaseOf(t, e, def, "plan") != PhaseFailed {
		t.Fatalf("plan phase = %s, want failed after retries are exhausted", phaseOf(t, e, def, "plan"))
	}
	if len(e.rec.enqueued) != 2 {
		t.Fatalf("expected the initial dispatch plus one retry, got %v", e.rec.enqueued)
	}
}
```

Add to `testenv_test.go`:

```go
type errString string

func (s errString) Error() string { return string(s) }

func pgtypeUUIDZero() pgtype.UUID { return pgtype.UUID{} }

// commentID inserts a real comment on the definition and returns its id,
// standing in for the reviewer's /reject comment.
func commentID(t *testing.T, e *env, def db.Issue) pgtype.UUID {
	t.Helper()
	id := e.fx.Comment(t, uuidStr(def), "/reject please add tests")
	u, _ := util.ParseUUID(id)
	return u
}
```

(`agent_task_queue` requires only `agent_id`, `issue_id` and the defaulted columns in the current schema; if a later migration added NOT NULL columns, extend the INSERT or use `e.fx.Task(t, agentID, dbfx.Cols{"issue_id": ..., "status": "failed"})`.)

- [ ] **Step 2: Run to verify failure**

Run: `cd server && go test ./internal/workflow/ -run 'TestTick|TestApproval|TestReject|TestDispatch|TestDeleted|TestAgentTask' -count=1`
Expected: FAIL to compile (`Tick`, `ApplyEvent` undefined).

- [ ] **Step 3: Implement**

```go
// server/internal/workflow/engine.go
package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Tick expands eligible definitions, then advances every running workflow.
func (e *Engine) Tick(ctx context.Context) error {
	var errs []error
	if err := e.ExpandCandidates(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := e.advanceRuns(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (e *Engine) advanceRuns(ctx context.Context) error {
	defs, err := e.Q.ListRunningWorkflowDefinitions(ctx, 200)
	if err != nil {
		return fmt.Errorf("list running workflows: %w", err)
	}
	var errs []error
	for _, def := range defs {
		if err := e.advance(ctx, def); err != nil {
			slog.Warn("workflow advance failed", "issue_id", def.ID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type stepRow struct {
	issue db.Issue
	meta  StepMeta
}

// loadSteps finds a run's steps by the run id in metadata, not by
// parent_issue_id, so a step whose parent link is lost is still tracked.
func (e *Engine) loadSteps(ctx context.Context, def db.Issue) ([]stepRow, error) {
	steps, err := e.Q.ListWorkflowSteps(ctx, db.ListWorkflowStepsParams{WorkspaceID: def.WorkspaceID, Run: uuidString(def.ID)})
	if err != nil {
		return nil, err
	}
	var rows []stepRow
	for _, k := range steps {
		if m, ok := readStepMeta(k); ok {
			rows = append(rows, stepRow{k, m})
		}
	}
	return rows, nil
}

func (e *Engine) advance(ctx context.Context, def db.Issue) error {
	dm, ok := readDefMeta(def)
	if !ok || dm.State != RunRunning {
		return nil
	}
	steps, err := e.loadSteps(ctx, def)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if err := e.observe(ctx, s, steps); err != nil {
			return err
		}
	}
	steps, err = e.loadSteps(ctx, def)
	if err != nil {
		return err
	}
	metas := make([]StepMeta, len(steps))
	for i, s := range steps {
		metas[i] = s.meta
	}
	switch Evaluate(metas, dm.Total) {
	case OutcomeDone:
		return e.closeDefinition(ctx, def, RunDone, "done", "")
	case OutcomeBlocked:
		return e.closeDefinition(ctx, def, RunBlocked, "blocked", failureSummary(steps))
	case OutcomeBroken:
		return e.closeDefinition(ctx, def, RunBlocked, "blocked",
			"Workflow stopped: a step issue is missing (it may have been deleted). Restore it or recreate the workflow.")
	}
	return nil
}

// observe derives at most one event for a step from the world and applies it.
func (e *Engine) observe(ctx context.Context, s stepRow, all []stepRow) error {
	switch s.meta.Phase {
	case PhasePending:
		done := map[string]bool{}
		for _, o := range all {
			if o.meta.Phase == PhaseDone {
				done[o.meta.Node] = true
			}
		}
		for _, d := range s.meta.Deps {
			if !done[d] {
				return nil
			}
		}
		_, err := e.ApplyEvent(ctx, s.issue, EventDepsMet, pgtype.UUID{})
		return err
	case PhaseRunning:
		if s.issue.Status == "done" || s.issue.Status == "in_review" {
			_, err := e.ApplyEvent(ctx, s.issue, EventAgentFinished, pgtype.UUID{})
			return err
		}
		agentID, perr := util.ParseUUID(s.meta.AgentID)
		if perr != nil {
			return nil
		}
		status, err := e.Q.LatestWorkflowTaskStatus(ctx, db.LatestWorkflowTaskStatusParams{IssueID: s.issue.ID, AgentID: agentID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if status == "failed" {
			_, err := e.ApplyEvent(ctx, s.issue, EventAgentFailed, pgtype.UUID{})
			return err
		}
		return e.reconcileStatus(ctx, s.issue, s.meta.Phase)
	}
	return nil
}

// reconcileStatus puts the issue back on the status its phase maps to when
// something else moved it (for example a crash between status and metadata
// writes). It never changes the phase.
func (e *Engine) reconcileStatus(ctx context.Context, issue db.Issue, phase Phase) error {
	want := PhaseStatus[phase]
	if issue.Status == want {
		return nil
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: want})
	if err != nil {
		return err
	}
	e.Events.IssueUpdated(ctx, issue, updated)
	return nil
}

// ApplyEvent runs one event through the transition table for step. It
// returns false when no rule matches the step's current phase. trigger, when
// valid, is the reviewer's comment passed to the agent on a redo.
func (e *Engine) ApplyEvent(ctx context.Context, step db.Issue, ev Event, trigger pgtype.UUID) (bool, error) {
	fresh, err := e.Q.GetIssue(ctx, step.ID)
	if err != nil {
		return false, err
	}
	step = fresh
	meta, ok := readStepMeta(step)
	if !ok {
		return false, nil
	}
	rule, ok := Next(meta, ev)
	if !ok {
		return false, nil
	}
	next := meta.Apply(rule)

	cur := step
	if want := PhaseStatus[next.Phase]; step.Status != want {
		cur, err = e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: step.ID, WorkspaceID: step.WorkspaceID, Status: want})
		if err != nil {
			return false, err
		}
		e.Events.IssueUpdated(ctx, step, cur)
	}
	if err := e.writeMeta(ctx, cur, next); err != nil {
		return false, err
	}
	for _, a := range rule.Actions {
		switch a {
		case ActionDispatch:
			if err := e.dispatch(ctx, cur, next, pgtype.UUID{}); err != nil {
				return true, e.failDispatch(ctx, cur, next, err)
			}
		case ActionDispatchWithFeedback:
			if err := e.dispatch(ctx, cur, next, trigger); err != nil {
				return true, e.failDispatch(ctx, cur, next, err)
			}
		case ActionRequestReview:
			if _, err := e.systemComment(ctx, cur, "Waiting for review. The workflow creator can reply `/accept` to approve or `/reject <feedback>` to send it back to the agent."); err != nil {
				return true, err
			}
		case ActionCommentFailure:
			if _, err := e.systemComment(ctx, cur, fmt.Sprintf("Step failed after %d attempt(s). Reply `/retry` to run it again.", next.Attempts+1)); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

func (e *Engine) dispatch(ctx context.Context, issue db.Issue, meta StepMeta, trigger pgtype.UUID) error {
	var err error
	if trigger.Valid {
		agentID, perr := util.ParseUUID(meta.AgentID)
		if perr != nil {
			return perr
		}
		_, err = e.Tasks.EnqueueTaskForMention(ctx, issue, agentID, trigger, service.OriginDerived)
	} else {
		_, err = e.Tasks.EnqueueTaskForIssue(ctx, issue)
	}
	if errors.Is(err, service.ErrDuplicatePendingTask) {
		return nil
	}
	return err
}

// failDispatch marks a step failed when its agent task could not be queued
// (for example the agent was archived) and says why on the issue.
func (e *Engine) failDispatch(ctx context.Context, issue db.Issue, meta StepMeta, cause error) error {
	meta.Phase = PhaseFailed
	if err := e.writeMeta(ctx, issue, meta); err != nil {
		return err
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: PhaseStatus[PhaseFailed]})
	if err == nil {
		e.Events.IssueUpdated(ctx, issue, updated)
	}
	_, cerr := e.systemComment(ctx, issue, fmt.Sprintf("Could not start this step: %v. Fix the cause, then reply `/retry`.", cause))
	return errors.Join(err, cerr)
}

func failureSummary(steps []stepRow) string {
	var failed []string
	for _, s := range steps {
		if s.meta.Phase == PhaseFailed {
			failed = append(failed, s.meta.Node)
		}
	}
	sort.Strings(failed)
	return "Workflow blocked: failed step(s): " + strings.Join(failed, ", ") + ". Reply `/retry` here to rerun them."
}

// closeDefinition records the run's end state on the definition issue.
func (e *Engine) closeDefinition(ctx context.Context, def db.Issue, state RunState, status, comment string) error {
	dm, _ := readDefMeta(def)
	dm.State = state
	if err := e.writeMeta(ctx, def, dm); err != nil {
		return err
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: def.ID, WorkspaceID: def.WorkspaceID, Status: status})
	if err != nil {
		return err
	}
	e.Events.IssueUpdated(ctx, def, updated)
	if comment != "" {
		_, err = e.systemComment(ctx, updated, comment)
	}
	return err
}

// reopenDefinition returns a blocked workflow to running after a retry.
func (e *Engine) reopenDefinition(ctx context.Context, def db.Issue) error {
	cur, err := e.Q.GetIssue(ctx, def.ID)
	if err != nil {
		return err
	}
	dm, ok := readDefMeta(cur)
	if !ok || dm.State != RunBlocked {
		return nil
	}
	dm.State = RunRunning
	if err := e.writeMeta(ctx, cur, dm); err != nil {
		return err
	}
	updated, err := e.Q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: cur.ID, WorkspaceID: cur.WorkspaceID, Status: "in_progress"})
	if err != nil {
		return err
	}
	e.Events.IssueUpdated(ctx, cur, updated)
	return nil
}
```

Note on the "missing step" test: `dm.Total` is 2 and only one step remains, so `Evaluate` returns `OutcomeBroken` and the run blocks with the explanatory comment instead of completing.

- [ ] **Step 4: Run to verify pass**

Run: `cd server && go test ./internal/workflow/ -count=1 -v`
Expected: all workflow tests PASS. Common fix-ups: `db.Issue.Priority` type (`string`), `ListAgents` row type (`[]db.Agent`), `service.ErrDuplicatePendingTask` export name (`rg ErrDuplicatePendingTask server/internal/service`).

- [ ] **Step 5: Commit**

```bash
git add server/internal/workflow
git commit -m "feat(workflow): tick engine applying transitions, completion and failure handling"
```

---

### Task 7: Commands (`/accept`, `/reject`, `/retry`), listener, one-line `comment.go` change

**Files:**
- Create: `server/internal/workflow/commands.go`
- Modify: `server/internal/handler/comment.go:1996-2005` (`isNoteComment`)
- Test: `server/internal/workflow/commands_test.go`, `server/internal/handler/comment_note_workflow_test.go`

**Interfaces:**
- Produces:
  - `type Command string` (`CommandAccept|CommandReject|CommandRetry`)
  - `func ParseCommand(content string) (Command, bool)`
  - `func IsCommandToken(token string) bool` (used by `handler.isNoteComment`; case-insensitive exact match of `/accept`, `/reject`, `/retry`)
  - `type CommentEvent struct { ID, IssueID, AuthorType, AuthorID, Content string }`
  - `func (e *Engine) HandleComment(ctx context.Context, c CommentEvent) error`
  - `func RegisterListeners(bus *events.Bus, e *Engine)`

- [ ] **Step 1: Write the failing tests**

```go
// server/internal/workflow/commands_test.go
package workflow

import (
	"context"
	"strings"
	"testing"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in   string
		want Command
		ok   bool
	}{
		{"/accept", CommandAccept, true},
		{"  /ACCEPT looks good", CommandAccept, true},
		{"/reject missing tests\nmore", CommandReject, true},
		{"/retry", CommandRetry, true},
		{"/accepted", "", false},
		{"see /accept", "", false},
		{"/ accept", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := ParseCommand(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseCommand(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if !IsCommandToken("/Reject") || IsCommandToken("/note") {
		t.Error("IsCommandToken mismatch")
	}
}

func cevent(e *env, issue string, authorType, authorID, content string) CommentEvent {
	return CommentEvent{ID: "00000000-0000-0000-0000-000000000001", IssueID: issue, AuthorType: authorType, AuthorID: authorID, Content: content}
}

// blockedBuild drives the flow to build waiting for review.
func blockedBuild(t *testing.T) (*env, db.Issue, db.Issue) {
	e, def := expanded(t)
	ctx := context.Background()
	_ = e.engine.Tick(ctx)
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	_ = e.engine.Tick(ctx)
	_ = e.engine.Tick(ctx)
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	_ = e.engine.Tick(ctx)
	return e, def, stepByNode(t, e, def, "build")
}

func TestAcceptByCreatorCompletesStep(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "build") != PhaseDone {
		t.Fatalf("build phase = %s, want done", phaseOf(t, e, def, "build"))
	}
}

func TestCommandFromNonCreatorIsRefused(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	before := len(e.rec.comments)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", "00000000-0000-0000-0000-0000000000aa", "/accept")); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "build") != PhaseBlocked {
		t.Fatal("a stranger's /accept must not apply")
	}
	if len(e.rec.comments) != before+1 || !strings.Contains(e.rec.comments[before].Content, "creator") {
		t.Fatal("expected a refusal comment mentioning the creator")
	}
}

func TestCommandsOnAgentCreatedDefinitionAreRefusedVisibly(t *testing.T) {
	ctx := context.Background()
	e, def, build := blockedBuild(t)
	agent := e.agent(t, "Creator Bot")
	e.fx.Exec(t, `UPDATE issue SET creator_type = 'agent', creator_id = $2 WHERE id = $1`, uuidStr(def), agent)
	before := len(e.rec.comments)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "build") != PhaseBlocked || len(e.rec.comments) != before+1 {
		t.Fatal("agent-created definitions must refuse commands with a visible comment")
	}
}

func TestRetryOnDefinitionRestartsAllFailedSteps(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	e.rec.failNext = errString("runtime offline")
	_ = e.engine.Tick(ctx) // plan fails at dispatch
	_ = e.engine.Tick(ctx) // workflow blocks
	cur, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(cur); dm.State != RunBlocked {
		t.Fatalf("precondition: state = %s", dm.State)
	}
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(def), "member", e.user, "/retry")); err != nil {
		t.Fatal(err)
	}
	if phaseOf(t, e, def, "plan") != PhaseRunning {
		t.Fatalf("plan phase = %s, want running after /retry", phaseOf(t, e, def, "plan"))
	}
	cur, _ = e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(cur); dm.State != RunRunning || cur.Status != "in_progress" {
		t.Fatalf("definition state=%s status=%s, want running/in_progress", dm.State, cur.Status)
	}
}

func TestCommandOnUnrelatedIssueIsSilent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	plain := e.flowIssue(t, "Just an issue", "no yaml")
	before := len(e.rec.comments)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(plain), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	if len(e.rec.comments) != before {
		t.Fatal("a command on a non-workflow issue must be ignored silently")
	}
}

func TestAcceptOnRunningStepExplainsWhy(t *testing.T) {
	ctx := context.Background()
	e, def := expanded(t)
	_ = e.engine.Tick(ctx)
	plan := stepByNode(t, e, def, "plan")
	before := len(e.rec.comments)
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(plan), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	if len(e.rec.comments) != before+1 || !strings.Contains(e.rec.comments[before].Content, "not waiting") {
		t.Fatal("expected a 'not waiting for review' comment")
	}
}
```

(`db` import: add `db "github.com/multica-ai/multica/server/pkg/db/generated"` to this test file.)

```go
// server/internal/handler/comment_note_workflow_test.go
package handler

import "testing"

func TestWorkflowCommandsSuppressAgentTriggering(t *testing.T) {
	for _, c := range []string{"/accept", "/REJECT needs work", "  /retry"} {
		if !isNoteComment(c) {
			t.Errorf("isNoteComment(%q) = false, want true so agents are not woken", c)
		}
	}
	for _, c := range []string{"/accepted", "please /accept", "/notes", "hello"} {
		if isNoteComment(c) {
			t.Errorf("isNoteComment(%q) = true, want false", c)
		}
	}
	if !isNoteComment("/note hi") {
		t.Error("existing /note behaviour must be unchanged")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd server && go test ./internal/workflow/ -run 'TestParseCommand|TestAccept|TestCommand|TestRetry' -count=1; go test ./internal/handler/ -run TestWorkflowCommandsSuppress -count=1`
Expected: workflow tests FAIL to compile; handler test FAILS on `/accept`.

- [ ] **Step 3: Implement `commands.go`**

```go
// server/internal/workflow/commands.go
package workflow

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type Command string

const (
	CommandAccept Command = "accept"
	CommandReject Command = "reject"
	CommandRetry  Command = "retry"
)

func firstToken(content string) string {
	t := strings.TrimLeft(content, " \t\r\n")
	if i := strings.IndexFunc(t, unicode.IsSpace); i >= 0 {
		return t[:i]
	}
	return t
}

// IsCommandToken reports whether token is one of the workflow commands
// (case-insensitive). The handler uses it to keep these comments from waking
// agents, the same way /note does.
func IsCommandToken(token string) bool {
	_, ok := commandFromToken(token)
	return ok
}

func commandFromToken(token string) (Command, bool) {
	switch strings.ToLower(token) {
	case "/accept":
		return CommandAccept, true
	case "/reject":
		return CommandReject, true
	case "/retry":
		return CommandRetry, true
	}
	return "", false
}

// ParseCommand recognises a command only as the comment's first token.
func ParseCommand(content string) (Command, bool) {
	return commandFromToken(firstToken(content))
}

// CommentEvent is the subset of a comment the command handler needs.
type CommentEvent struct {
	ID, IssueID, AuthorType, AuthorID, Content string
}

// HandleComment applies a /accept, /reject or /retry comment. Comments that
// are not commands, or are not on a workflow issue, are ignored silently.
func (e *Engine) HandleComment(ctx context.Context, c CommentEvent) error {
	cmd, ok := ParseCommand(c.Content)
	if !ok {
		return nil
	}
	issueID, err := util.ParseUUID(c.IssueID)
	if err != nil {
		return nil
	}
	issue, err := e.Q.GetIssue(ctx, issueID)
	if err != nil {
		return nil
	}
	stepMeta, isStep := readStepMeta(issue)
	_, isDef := readDefMeta(issue)
	if !isStep && !isDef {
		return nil
	}

	def := issue
	if isStep {
		def, err = e.Q.GetIssue(ctx, issue.ParentIssueID)
		if err != nil {
			return nil
		}
	}
	if !isCreator(def, c) {
		_, err := e.systemComment(ctx, issue, "Only the workflow creator can use `/"+string(cmd)+"` here. This workflow was created by "+creatorLabel(def)+".")
		return err
	}
	commentUUID, _ := util.ParseUUID(c.ID)

	switch {
	case isStep && (cmd == CommandAccept || cmd == CommandReject):
		ev := EventAccept
		if cmd == CommandReject {
			ev = EventReject
		}
		applied, err := e.ApplyEvent(ctx, issue, ev, commentUUID)
		if err != nil {
			return err
		}
		if !applied {
			_, err = e.systemComment(ctx, issue, "This step is not waiting for review (phase: "+string(stepMeta.Phase)+").")
		}
		return err
	case isStep && cmd == CommandRetry:
		applied, err := e.ApplyEvent(ctx, issue, EventRetry, commentUUID)
		if err != nil {
			return err
		}
		if !applied {
			_, err = e.systemComment(ctx, issue, "Only a failed step can be retried (phase: "+string(stepMeta.Phase)+").")
			return err
		}
		return e.reopenDefinition(ctx, def)
	case isDef && cmd == CommandRetry:
		steps, err := e.loadSteps(ctx, def)
		if err != nil {
			return err
		}
		retried := 0
		for _, s := range steps {
			if s.meta.Phase != PhaseFailed {
				continue
			}
			if applied, err := e.ApplyEvent(ctx, s.issue, EventRetry, commentUUID); err != nil {
				return err
			} else if applied {
				retried++
			}
		}
		if retried == 0 {
			_, err = e.systemComment(ctx, issue, "No failed steps to retry.")
			return err
		}
		return e.reopenDefinition(ctx, def)
	default:
		_, err := e.systemComment(ctx, issue, "`/"+string(cmd)+"` applies to a step issue. Open the step and comment there.")
		return err
	}
}

func isCreator(def db.Issue, c CommentEvent) bool {
	return c.AuthorType == "member" && def.CreatorType == "member" && util.UUIDToString(def.CreatorID) == c.AuthorID
}

func creatorLabel(def db.Issue) string {
	if def.CreatorType == "member" {
		return "another member"
	}
	return "an agent, so workflow commands are unavailable; recreate it as a member"
}

// RegisterListeners subscribes to comment:created and hands command comments
// to the engine off the publishing goroutine.
func RegisterListeners(bus *events.Bus, e *Engine) {
	bus.Subscribe(protocol.EventCommentCreated, func(ev events.Event) {
		c, ok := decodeCommentEvent(ev)
		if !ok {
			return
		}
		if _, isCmd := ParseCommand(c.Content); !isCmd {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := e.HandleComment(ctx, c); err != nil {
				slog.Warn("workflow command failed", "issue_id", c.IssueID, "error", err)
			}
		}()
	})
}

func decodeCommentEvent(ev events.Event) (CommentEvent, bool) {
	payload, ok := ev.Payload.(map[string]any)
	if !ok {
		return CommentEvent{}, false
	}
	raw, err := json.Marshal(payload["comment"])
	if err != nil {
		return CommentEvent{}, false
	}
	var c CommentEvent
	var wire struct {
		ID         string `json:"id"`
		IssueID    string `json:"issue_id"`
		AuthorType string `json:"author_type"`
		AuthorID   string `json:"author_id"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || wire.IssueID == "" {
		return CommentEvent{}, false
	}
	c = CommentEvent{ID: wire.ID, IssueID: wire.IssueID, AuthorType: wire.AuthorType, AuthorID: wire.AuthorID, Content: wire.Content}
	return c, true
}
```

- [ ] **Step 4: The one-line change in `comment.go`**

Edit `isNoteComment` in `server/internal/handler/comment.go` (currently ends with `return strings.EqualFold(firstToken, noteCommentPrefix)`):

```go
	return strings.EqualFold(firstToken, noteCommentPrefix) || workflow.IsCommandToken(firstToken)
```

and add to the file's import block: `"github.com/multica-ai/multica/server/internal/workflow"`. Extend the function's doc comment with one sentence: `The workflow engine's /accept, /reject and /retry commands are treated the same way so they never wake an agent.`

This is the single edit to existing handler logic. All four existing `isNoteComment` call sites (`comment.go:2011`, `:2677`, `:3969`, `daemon.go:4492`) pick it up.

- [ ] **Step 5: Run to verify pass**

Run: `cd server && go build ./... && go test ./internal/workflow/ -count=1 && go test ./internal/handler/ -run 'TestWorkflowCommandsSuppress|TestWorkflowEvents|Note' -count=1`
Expected: PASS. If `workflow` ever imports `handler`, the build fails with an import cycle; the plan avoids that by design (events are decoded from JSON, not `handler.CommentResponse`).

- [ ] **Step 6: Commit**

```bash
git add server/internal/workflow server/internal/handler/comment.go server/internal/handler/comment_note_workflow_test.go
git commit -m "feat(workflow): /accept, /reject and /retry commands that do not wake agents"
```

---

### Task 8: Scheduler job and server wiring

**Files:**
- Create: `server/internal/scheduler/jobs_workflow.go`
- Modify: `server/cmd/server/main.go` (near the `registerAutopilotListeners(bus, autopilotSvc)` call at ~line 728 and the `schedulerMgr.Register(...)` calls at ~lines 818-840)
- Test: `server/internal/scheduler/jobs_workflow_test.go`

**Interfaces:**
- Produces: `func WorkflowTickJob(t WorkflowTicker) JobSpec` and `type WorkflowTicker interface{ Tick(context.Context) error }`.
- Consumes: `*workflow.Engine` (`Tick`), `h.WorkflowEvents()`, `h.IssueService`, `taskSvc`, `queries`, `bus`.

- [ ] **Step 1: Write the failing test**

```go
// server/internal/scheduler/jobs_workflow_test.go
package scheduler

import (
	"context"
	"errors"
	"testing"
)

type fakeTicker struct {
	calls int
	err   error
}

func (f *fakeTicker) Tick(context.Context) error { f.calls++; return f.err }

func TestWorkflowTickJobDelegatesToTick(t *testing.T) {
	ft := &fakeTicker{}
	spec := WorkflowTickJob(ft)
	if spec.Name != "workflow_tick" {
		t.Fatalf("name = %q", spec.Name)
	}
	if _, err := spec.Handler(context.Background(), HandlerInput{}); err != nil || ft.calls != 1 {
		t.Fatalf("handler err=%v calls=%d", err, ft.calls)
	}
	ft.err = errors.New("boom")
	if _, err := spec.Handler(context.Background(), HandlerInput{}); err == nil {
		t.Fatal("handler must surface the tick error")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd server && go test ./internal/scheduler/ -run TestWorkflowTickJob -count=1`
Expected: FAIL to compile.

- [ ] **Step 3: Implement the job**

```go
// server/internal/scheduler/jobs_workflow.go
package scheduler

import (
	"context"
	"time"
)

type WorkflowTicker interface{ Tick(context.Context) error }

// WorkflowTickJob advances YAML-defined workflows. All state lives in issue
// metadata, so a missed bucket only delays the next step; nothing is lost.
func WorkflowTickJob(t WorkflowTicker) JobSpec {
	return JobSpec{
		Name: "workflow_tick", Cadence: 15 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 45 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, t.Tick(ctx)
		},
	}
}
```

- [ ] **Step 4: Wire into `main.go`**

After `registerAutopilotListeners(bus, autopilotSvc)`:

```go
	workflowEngine := &workflow.Engine{Q: queries, Issues: h.IssueService, Tasks: taskSvc, Events: h.WorkflowEvents()}
	workflow.RegisterListeners(bus, workflowEngine)
```

Next to the other `schedulerMgr.Register` calls:

```go
	if err := schedulerMgr.Register(scheduler.WorkflowTickJob(workflowEngine)); err != nil {
		slog.Warn("scheduler: failed to register workflow_tick job", "error", err)
	}
```

Add the import `"github.com/multica-ai/multica/server/internal/workflow"`. Use the variable that main already uses for `*db.Queries` (the same one passed to `scheduler.AutopilotScheduleDispatchJob(pool, queries, autopilotSvc)`); if `h.IssueService` has a different field name, `rg -n "IssueService" server/internal/handler/handler.go`.

- [ ] **Step 5: Verify the build and the scheduler tests**

Run: `cd server && go build ./... && go vet ./internal/workflow/... ./internal/scheduler/... ./internal/handler/... ./cmd/server/... && go test ./internal/scheduler/ -run TestWorkflowTickJob -count=1`
Expected: PASS, no vet findings.

- [ ] **Step 6: Commit**

```bash
git add server/internal/scheduler/jobs_workflow.go server/internal/scheduler/jobs_workflow_test.go server/cmd/server/main.go
git commit -m "feat(workflow): run the engine from the DB-backed scheduler"
```

---

### Task 9: End-to-end check and documentation

**Files:**
- Create: `server/internal/workflow/e2e_test.go`
- Modify: `docs/superpowers/specs/2026-10-06-workflow-engine-design.md` (append "Implementation notes" listing the eight deviations from this plan's header)

- [ ] **Step 1: Write the end-to-end test**

```go
// server/internal/workflow/e2e_test.go
package workflow

import (
	"context"
	"testing"
)

// The whole story in one test: author moves a labelled issue to todo, steps
// run in order, the approval step waits, /accept finishes it, and the
// definition issue completes.
func TestWorkflowEndToEnd(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.agent(t, "Planner")
	e.agent(t, "Coder")
	def := e.flowIssue(t, "Ship feature", flowDoc("Planner", "Coder"))

	tick := func() {
		t.Helper()
		if err := e.engine.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	tick() // expand + dispatch plan
	if phaseOf(t, e, def, "plan") != PhaseRunning {
		t.Fatal("plan should be running")
	}
	setStatus(t, e, stepByNode(t, e, def, "plan"), "done")
	tick()
	tick() // build becomes ready, then dispatched
	if phaseOf(t, e, def, "build") != PhaseRunning {
		t.Fatal("build should be running")
	}
	setStatus(t, e, stepByNode(t, e, def, "build"), "in_review")
	tick()
	if phaseOf(t, e, def, "build") != PhaseBlocked {
		t.Fatal("build should wait for review")
	}
	build := stepByNode(t, e, def, "build")
	if err := e.engine.HandleComment(ctx, cevent(e, uuidStr(build), "member", e.user, "/accept")); err != nil {
		t.Fatal(err)
	}
	tick()
	got, _ := e.q.GetIssue(ctx, def.ID)
	if dm, _ := readDefMeta(got); dm.State != RunDone || got.Status != "done" {
		t.Fatalf("definition state=%s status=%s, want done/done", dm.State, got.Status)
	}
}
```

- [ ] **Step 2: Run the full verification**

Run each from the repo root and report what actually ran:

```bash
make up
cd server && go build ./... && go vet ./internal/workflow/... ./internal/scheduler/... ./internal/handler/... ./cmd/server/...
cd server && go test ./internal/workflow/... ./internal/scheduler/... -count=1
cd server && go test ./internal/handler/ -run 'WorkflowEvents|WorkflowCommands|Note|Metadata' -count=1
git diff --check
```

Expected: everything passes. Then run `make test` once for the Go backend to confirm the `isNoteComment` change and `main.go` wiring did not break unrelated tests; report any failures with their output rather than skipping them.

- [ ] **Step 3: Update the spec with the implementation notes and commit**

Append to the end of the spec file a section `## 13. Implementation notes` containing the eight bullets from this plan's "Deviations from the spec" header (verbatim), then:

```bash
git add docs/superpowers/specs/2026-10-06-workflow-engine-design.md server/internal/workflow/e2e_test.go
git commit -m "test(workflow): end-to-end workflow run and spec implementation notes"
```
