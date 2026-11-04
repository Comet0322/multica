// Package agentsvc is the agent service: the harness behind an HTTP API, so the
// daemon hands it a task and gets the run back as a stream, and the model key,
// the git credential and the sandbox API address live in this one service
// instead of in every daemon.
//
//	POST   /v1/runs        body RunRequest; the response is NDJSON, one Event per line
//	DELETE /v1/runs/{id}   cancels a run (so does closing the connection)
//	GET    /healthz
//
// A run's lifetime is its request: there is nothing to poll and nothing to
// resume. The stream is "started", then "message" and "heartbeat" events, then
// exactly one "result". A stream that ends without a result means the service
// died, and the client reports the run as failed.
package agentsvc

// File is one file placed in the sandbox before the run starts.
type File struct {
	// Path is relative to the sandbox working directory.
	Path string `json:"path"`
	Mode uint32 `json:"mode,omitempty"`
	Data []byte `json:"data"`
}

// RunRequest is one task.
type RunRequest struct {
	Prompt string `json:"prompt"`
	// SystemPrompt is the Multica runtime brief; the service appends its own
	// description of the environment after it.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Model names the model to use; empty means the service's default.
	Model      string `json:"model,omitempty"`
	MaxTurns   int    `json:"max_turns,omitempty"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
	// Env is the task's identity: only the MULTICA_* keys in TaskEnvKeys are
	// read, and everything else is ignored.
	Env   map[string]string `json:"env,omitempty"`
	Files []File            `json:"files,omitempty"`
}

// TaskEnvKeys are the values a daemon computes for a task and passes through.
var TaskEnvKeys = []string{"MULTICA_TOKEN", "MULTICA_SERVER_URL", "MULTICA_WORKSPACE_ID", "MULTICA_AGENT_ID", "MULTICA_TASK_ID"}

// Event is one line of the response stream.
type Event struct {
	Type    string   `json:"type"` // started | message | heartbeat | result
	RunID   string   `json:"run_id,omitempty"`
	Message *Message `json:"message,omitempty"`
	Result  *Result  `json:"result,omitempty"`
}

// Event types.
const (
	EventStarted   = "started"
	EventMessage   = "message"
	EventHeartbeat = "heartbeat"
	EventResult    = "result"
)

// Message is something the agent said or did: type is text, tool-use,
// tool-result or error.
type Message struct {
	Type    string         `json:"type"`
	Content string         `json:"content,omitempty"`
	Tool    string         `json:"tool,omitempty"`
	CallID  string         `json:"call_id,omitempty"`
	Input   map[string]any `json:"input,omitempty"`
	Output  string         `json:"output,omitempty"`
}

// Usage counts one model's tokens.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// Result ends a run. Status is completed, failed, timeout or cancelled.
type Result struct {
	Status     string           `json:"status"`
	Output     string           `json:"output,omitempty"`
	Error      string           `json:"error,omitempty"`
	DurationMs int64            `json:"duration_ms"`
	Usage      map[string]Usage `json:"usage,omitempty"`
}

// WireError is the body of a refused request.
type WireError struct {
	Error string `json:"error"`
}
