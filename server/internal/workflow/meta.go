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
