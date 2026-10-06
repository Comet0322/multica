package workflow

// Workflow state lives in issue.metadata as flat primitive keys prefixed with
// wf_. The platform's metadata contract is a flat map of strings, numbers and
// booleans (enforced by the metadata API and by the frontend issue schema), so
// nested objects, arrays and nulls are never written.
//
// A step issue is recognised by KeyRun, a definition issue by KeyState.
const (
	// Step keys.
	KeyRun          = "wf_run"
	KeyNode         = "wf_node"
	KeyDeps         = "wf_deps" // node ids joined by a comma; "" when none
	KeyAgent        = "wf_agent"
	KeyAgentID      = "wf_agent_id"
	KeyApproval     = "wf_approval"
	KeyMaxRetries   = "wf_max_retries"
	KeyAttempts     = "wf_attempts"
	KeyPhase        = "wf_phase"
	KeyDispatchedAt = "wf_dispatched_at" // RFC3339Nano; "" when never dispatched

	// Definition keys.
	KeyState     = "wf_state"
	KeyErrorHash = "wf_error_hash"
	KeyClaimedAt = "wf_claimed_at"
	KeyTotal     = "wf_total"
)

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
	// RunStopped: the definition issue was closed by a person mid-run.
	RunStopped RunState = "stopped"
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
	// DispatchedAt (RFC3339Nano UTC) stamps the latest dispatch generation; only
	// agent tasks created at or after it count for this step.
	DispatchedAt string `json:"dispatched_at,omitempty"`
}

// DefMeta is the metadata of a definition issue.
type DefMeta struct {
	State     RunState `json:"state"`
	ErrorHash string   `json:"error_hash,omitempty"`
	ClaimedAt string   `json:"claimed_at,omitempty"`
	Total     int      `json:"total,omitempty"`
}
