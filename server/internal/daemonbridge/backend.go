// Package daemonbridge presents the Multica daemon protocol to a Multica server
// and runs the claimed tasks on a Backend instead of a local agent CLI.
//
// The bridge owns everything the server sees of a daemon: registration,
// heartbeats, claiming, start, streamed messages, usage, cancellation,
// completion and failure. A Backend only executes: it receives a rendered
// brief and tools and reports events. Nothing Multica-specific leaks into it,
// and the task-scoped credential never leaves the bridge.
package daemonbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// Backend runs one task at a time per Handle. Every method may be called again
// after a crash or a lost response, so each must be safe to repeat.
type Backend interface {
	// Capabilities says what the backend can honour. The bridge advertises only
	// what is supported and fails a task cleanly when it needs more.
	Capabilities() Capabilities

	// Start begins the run for job.TaskID, or re-attaches to the run already
	// started for it. It must never create a second run for one task ID.
	Start(ctx context.Context, job Job) (Handle, error)

	// Events returns what happened after cursor ("" is the beginning) and the
	// cursor to continue from. Event IDs must be stable across calls.
	Events(ctx context.Context, h Handle, cursor string) (Batch, error)

	// Send delivers more input to a run that is still going. Only called when
	// Capabilities().Supplement is set.
	Send(ctx context.Context, h Handle, input string) error

	// ToolResult answers an Event of kind EventToolRequest.
	ToolResult(ctx context.Context, h Handle, callID string, result ToolResult) error

	// Interrupt asks a run to stop. It returns once the run has stopped or
	// definitely will not do more work.
	Interrupt(ctx context.Context, h Handle) error

	// Release frees what the run holds. A run that is already gone is not an error.
	Release(ctx context.Context, h Handle) error

	// Models lists the models agents on this backend can use.
	Models(ctx context.Context) ([]Model, error)
}

// Capabilities describes what a Backend supports.
type Capabilities struct {
	// Provider is the runtime type registered with Multica, for example
	// "claude". It selects the agent family the server tailors tasks for.
	Provider string
	// Version is the agent version shown on the runtime.
	Version string

	Supplement   bool // accepts extra input while a run is going
	Resume       bool // can continue a session from an earlier task
	Skills       bool // honours agent skills
	CustomEnv    bool // honours agent custom_env
	CustomArgs   bool // honours agent custom_args
	MCPConfig    bool // honours agent mcp_config
	Model        bool // honours a per-agent model
	Thinking     bool // honours thinking level and service tier
	RemoteMCP    bool // can reach remote MCP servers the task connects
	Attachments  bool // can fetch task attachments
	LocalWorkDir bool // can work in a local directory resource
}

// Kind is the sort of work a task asks for.
type Kind string

const (
	KindIssue     Kind = "issue"
	KindChat      Kind = "chat"
	KindAutopilot Kind = "autopilot"
	KindQuick     Kind = "quick_create"
	KindWakeup    Kind = "wakeup"
	KindSquad     Kind = "squad_leader"
)

// Job is what a Backend is asked to run.
type Job struct {
	TaskID      string
	WorkspaceID string
	AgentID     string
	IssueID     string
	Kind        Kind

	// System is the runtime brief and Prompt the first turn, both rendered by
	// the daemon's own prompt code.
	System string
	Prompt string

	Agent AgentSpec
	// ResumeSessionID is the session of this agent's previous task on the same
	// conversation. The backend may decline it by starting fresh.
	ResumeSessionID string

	Repos     []Repo
	Resources []Resource
	// Tools are the tools the bridge executes for the run.
	Tools []ToolSpec
	// MCP, when set, serves Tools over MCP for this task only. An engine that
	// speaks MCP should connect here instead of delivering EventToolRequest.
	MCP *MCPEndpoint
	// Task is the claimed task with every credential removed.
	Task json.RawMessage
}

// AgentSpec is the agent configuration the claim carried.
type AgentSpec struct {
	ID            string
	Name          string
	Instructions  string
	Model         string
	ThinkingLevel string
	ServiceTier   string
	CustomEnv     map[string]string
	CustomArgs    []string
	MCPConfig     json.RawMessage
	RuntimeConfig json.RawMessage
	Skills        []Skill
}

// Skill is a skill with its files.
type Skill struct {
	ID, Name, Description, Content string
	Files                          []SkillFile
}

// SkillFile is a supporting file of a skill.
type SkillFile struct{ Path, Content string }

// Repo is a repository of the workspace.
type Repo struct{ URL, Description, Ref string }

// Resource is a project resource.
type Resource struct {
	Type  string
	Ref   json.RawMessage
	Label string
}

// Handle identifies a run on the backend. The bridge stores it and passes it
// back unchanged.
type Handle struct {
	// SessionID is the backend's session. It is reported as the task's
	// session so a later task can resume it.
	SessionID string          `json:"session_id"`
	Opaque    json.RawMessage `json:"opaque,omitempty"`
}

// Batch is a page of events.
type Batch struct {
	Events []Event
	Cursor string
}

// EventKind classifies an Event.
type EventKind string

const (
	EventText        EventKind = "text"
	EventThinking    EventKind = "thinking"
	EventToolUse     EventKind = "tool_use"
	EventToolResult  EventKind = "tool_result"
	EventError       EventKind = "error"
	EventUsage       EventKind = "usage"
	EventToolRequest EventKind = "tool_request"
	EventDone        EventKind = "done"
)

// Event is one thing a run did.
type Event struct {
	// ID is unique within the run and stable when the event is read again.
	ID   string
	Kind EventKind

	Text   string         // text, thinking, error
	Tool   string         // tool_use, tool_result
	CallID string         // tool_use, tool_result, tool_request
	Input  map[string]any // tool_use
	Output string         // tool_result

	Usage   []Usage      // usage
	Request *ToolRequest // tool_request
	Done    *Outcome     // done
}

// Usage is token usage for one model.
type Usage struct {
	Provider         string
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSDTicks     int64
}

// ToolRequest asks the bridge to run one of Job.Tools.
type ToolRequest struct {
	Name string
	Args json.RawMessage
}

// ToolResult is the answer to a ToolRequest.
type ToolResult struct {
	Output  string
	IsError bool
}

// ToolSpec describes a tool the bridge offers a run.
type ToolSpec struct {
	Name        string
	Description string
	// InputSchema is a JSON schema for the tool's arguments.
	InputSchema json.RawMessage
}

// OutcomeStatus is how a run ended.
type OutcomeStatus string

const (
	OutcomeCompleted OutcomeStatus = "completed"
	OutcomeFailed    OutcomeStatus = "failed"
	// OutcomeCancelled is a run that stopped because it was told to.
	OutcomeCancelled OutcomeStatus = "cancelled"
)

// Outcome is the end of a run.
type Outcome struct {
	Status OutcomeStatus
	// Output is the final answer. For a completed run the server turns it into
	// a comment when the agent has not commented, as for a local agent.
	Output string
	Error  string
	// FailureReason is a pkg/taskfailure reason; empty lets the server classify.
	FailureReason string
	// SessionID is the session to resume next time; empty keeps Handle.SessionID.
	SessionID string
	// RetiredSessionID is a session the run proved unusable and gave up.
	RetiredSessionID string
}

// Model is a model a backend offers.
type Model struct {
	ID       string
	Label    string
	Provider string
	Default  bool
}

// Errors a Backend returns to steer the bridge.
var (
	// ErrGone means the run no longer exists on the backend.
	ErrGone = errors.New("run no longer exists on the backend")
	// ErrUnsupported means the job needs something the backend cannot do.
	ErrUnsupported = errors.New("unsupported by the backend")
)

// RejectedError is a definite refusal: nothing was started and retrying will
// not help. The task is failed with Reason.
type RejectedError struct {
	Message       string
	FailureReason string
}

func (e *RejectedError) Error() string { return e.Message }

// Rejected builds a RejectedError.
func Rejected(reason taskfailure.Reason, format string, args ...any) error {
	return &RejectedError{Message: fmt.Sprintf(format, args...), FailureReason: string(reason)}
}

// Done builds a finishing event.
func Done(id int, out Outcome) Event {
	return Event{ID: fmt.Sprintf("e%d", id), Kind: EventDone, Done: &out}
}
