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
