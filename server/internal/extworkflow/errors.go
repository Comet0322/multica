package extworkflow

import "errors"

// Sentinels shared by the engine, the handlers and the comment protocol.
// Handlers map ErrStatusMismatch to 409, ErrIllegalDecision to 422 and
// ErrForbidden to 403.
var (
	ErrEngineDisabled        = errors.New("workflow engine is disabled")
	ErrWorkflowNotFound      = errors.New("workflow not found")
	ErrWorkflowArchived      = errors.New("workflow is archived")
	ErrNoNodes               = errors.New("workflow has no nodes")
	ErrSupervisorUnavailable = errors.New("workflow supervisor agent is unavailable")
	ErrAgentNotInvokable     = errors.New("agent may not be invoked by this actor")
	ErrStatusMismatch        = errors.New("workflow step status does not allow this")
	ErrIllegalDecision       = errors.New("illegal workflow decision")
	ErrForbidden             = errors.New("not allowed to act on this workflow run")
	ErrRunNotFound           = errors.New("workflow run not found")
	// ErrNoTransition reports an observation that does not apply to the
	// current state (a stale task event, a repeated child event). The engine
	// treats it as a tick, never as a failure.
	ErrNoTransition = errors.New("event does not apply in the current state")
)

// AssignError is a refused workflow assignment, carrying the HTTP status the
// assignment endpoints answer with.
type AssignError struct {
	Status  int
	Code    string
	Message string
	Err     error
}

func (e *AssignError) Error() string { return e.Message }
func (e *AssignError) Unwrap() error { return e.Err }
