package extworkflow

import "fmt"

// This file is the pure state machine of a workflow run (spec §5.4–§5.6). It
// performs no I/O: the engine loads a RunState, calls Next, executes the
// returned effects and persists the returned state in one transaction.

type StepStatus string

const (
	StepPending            StepStatus = "pending"
	StepRunning            StepStatus = "running"
	StepAwaitingSupervisor StepStatus = "awaiting_supervisor"
	StepAwaitingHuman      StepStatus = "awaiting_human"
	StepDone               StepStatus = "done"
	StepSkipped            StepStatus = "skipped"
	StepFailed             StepStatus = "failed"
	StepCancelled          StepStatus = "cancelled"
)

// Terminal reports whether the step can no longer change within its run.
func (s StepStatus) Terminal() bool {
	switch s {
	case StepDone, StepSkipped, StepFailed, StepCancelled:
		return true
	}
	return false
}

// Awaiting reports whether the step waits for a decision ("awaiting_*").
func (s StepStatus) Awaiting() bool {
	return s == StepAwaitingSupervisor || s == StepAwaitingHuman
}

// Settled reports whether the step satisfies its dependents.
func (s StepStatus) Settled() bool { return s == StepDone || s == StepSkipped }

type PendingReason string

const (
	PendingReview        PendingReason = "review"
	PendingFailure       PendingReason = "failure"
	PendingRewindRequest PendingReason = "rewind_request"
)

type RunStatus string

const (
	RunRunning      RunStatus = "running"
	RunWaitingHuman RunStatus = "waiting_human"
	RunDone         RunStatus = "done"
	RunFailed       RunStatus = "failed"
	RunCancelled    RunStatus = "cancelled"
)

func (s RunStatus) Active() bool { return s == RunRunning || s == RunWaitingHuman }

type DecisionAction string

const (
	ActionApprove       DecisionAction = "approve"
	ActionRedo          DecisionAction = "redo"
	ActionRetry         DecisionAction = "retry"
	ActionSkip          DecisionAction = "skip"
	ActionRewind        DecisionAction = "rewind"
	ActionEscalate      DecisionAction = "escalate"
	ActionAbort         DecisionAction = "abort"
	ActionRequestRewind DecisionAction = "request-rewind"
)

type Decision struct {
	Action   DecisionAction
	Step     string // node key (parent-issue comments)
	To       string // node key
	Reason   string
	Feedback string
}

// Task roles and kinds stamped on agent_task_queue (spec §6.1).
const (
	RoleStep       = "step"
	RoleSupervisor = "supervisor"

	KindStep          = "step"
	KindReview        = "review"
	KindFailure       = "failure"
	KindRewindRequest = "rewind_request"
	KindSummary       = "summary"
	KindConversation  = "conversation"
)

// MaxSupervisorWakes bounds the supervisor tasks spent on one pending
// decision: the first wake, one re-wake after a silent turn, then a person.
const MaxSupervisorWakes = 2

// Timeline kinds written to ext_workflow_run_event.kind.
const (
	RunEventRunStarted      = "run_started"
	RunEventStepStarted     = "step_started"
	RunEventStepFinished    = "step_finished"
	RunEventStepFailed      = "step_failed"
	RunEventDecision        = "decision"
	RunEventRewind          = "rewind"
	RunEventRewindRequested = "rewind_requested"
	RunEventEscalated       = "escalated"
	RunEventRunFinished     = "run_finished"
	RunEventRunCancelled    = "run_cancelled"
	RunEventProtocolError   = "protocol_error"
)

// Milestones are the only system comments the engine posts on the parent.
const (
	MilestoneRunStarted   = "run_started"
	MilestoneEscalated    = "escalated"
	MilestoneRunFinished  = "run_finished"
	MilestoneRunFailed    = "run_failed"
	MilestoneRunCancelled = "run_cancelled"
)

// StepState is the engine-owned part of one ext_workflow_run_step row.
type StepState struct {
	Key              string
	Status           StepStatus
	Attempts         int
	PendingReason    PendingReason
	LastFeedback     string
	EscalationReason string
	SupervisorWakes  int
}

// RunState is everything Next decides on. SupervisorBusy, SupervisorFor and
// SummaryRequested are derived by the engine from in-flight tasks.
type RunState struct {
	Def              Definition
	Status           RunStatus
	RewindsUsed      int
	Steps            map[string]StepState
	SupervisorBusy   bool            // a review/failure/rewind_request/summary task is in flight
	SupervisorFor    map[string]bool // steps with an in-flight supervisor task
	SummaryRequested bool
}

// NewRunState is a fresh run: every step pending.
func NewRunState(def Definition) RunState {
	rs := RunState{Def: def, Status: RunRunning, Steps: map[string]StepState{}, SupervisorFor: map[string]bool{}}
	for _, n := range def.Nodes {
		rs.Steps[n.Key] = StepState{Key: n.Key, Status: StepPending}
	}
	return rs
}

func (rs RunState) Clone() RunState {
	out := rs
	out.Steps = make(map[string]StepState, len(rs.Steps))
	for k, v := range rs.Steps {
		out.Steps[k] = v
	}
	out.SupervisorFor = make(map[string]bool, len(rs.SupervisorFor))
	for k, v := range rs.SupervisorFor {
		if v {
			out.SupervisorFor[k] = true
		}
	}
	return out
}

// AllSettled reports whether every step is done or skipped.
func (rs RunState) AllSettled() bool {
	for _, n := range rs.Def.Nodes {
		if !rs.Steps[n.Key].Status.Settled() {
			return false
		}
	}
	return true
}

func (rs RunState) depsSettled(n Node) bool {
	for _, dep := range n.DependsOn {
		if !rs.Steps[dep].Status.Settled() {
			return false
		}
	}
	return true
}

type EventKind string

const (
	EvTick                 EventKind = "tick"                   // run-level: only schedule
	EvStepFinished         EventKind = "step_finished"          // child moved to done/in_review
	EvStepFailed           EventKind = "step_failed"            // Reason: agent outcome
	EvDecision             EventKind = "decision"               // Decision, from supervisor, step agent (request-rewind) or person
	EvSupervisorNoDecision EventKind = "supervisor_no_decision" // the focus step's supervisor task ended undecided
	EvChildCancelled       EventKind = "child_cancelled"        // a person cancelled (or deleted) the child
	EvSummaryEnded         EventKind = "summary_ended"          // run-level: the summary task ended
	EvCancelRun            EventKind = "cancel_run"             // run-level: Reason
)

type Event struct {
	Kind     EventKind
	Reason   string
	Decision Decision
	// Detail is merged into the recorded timeline payload (task id, error).
	Detail map[string]any
}

type EffectKind string

const (
	EffEnqueueStep       EffectKind = "enqueue_step"       // Step
	EffEnqueueSupervisor EffectKind = "enqueue_supervisor" // Step ("" = summary), SupervisorKind
	EffSetChildStatus    EffectKind = "set_child_status"   // Step, IssueStatus
	EffSetParentStatus   EffectKind = "set_parent_status"  // IssueStatus
	EffCancelStepWork    EffectKind = "cancel_step_work"   // Step: its step tasks and its queued supervisor tasks
	EffCancelRunTasks    EffectKind = "cancel_run_tasks"   // every in-flight task of the run
	EffEscalate          EffectKind = "escalate"           // Step, Reason: inbox item + milestone
	EffMilestone         EffectKind = "milestone"          // Milestone, Reason
	EffRecordEvent       EffectKind = "record_event"       // RunEvent, Step, Payload
)

type Effect struct {
	Kind           EffectKind
	Step           string
	IssueStatus    string
	SupervisorKind string
	Reason         string
	Milestone      string
	RunEvent       string
	Payload        map[string]any
	// Engine marks effects the scheduler produced; the engine records them
	// with the engine as actor rather than the event's actor.
	Engine bool
}

// Next applies ev to the step named key ("" for run-level events) and then
// schedules the run. It never mutates rs; on error it returns rs unchanged.
func Next(rs RunState, key string, ev Event) (RunState, []Effect, error) {
	if !rs.Status.Active() {
		if ev.Kind == EvDecision {
			return rs, nil, fmt.Errorf("%w: the run is %s", ErrStatusMismatch, rs.Status)
		}
		return rs, nil, ErrNoTransition
	}
	out := rs.Clone()
	var effs []Effect
	var err error
	switch ev.Kind {
	case EvTick:
	case EvCancelRun:
		return out, endRun(&out, RunCancelled, "", ev.Reason), nil
	case EvSummaryEnded:
		if !out.SummaryRequested || !out.AllSettled() {
			return rs, nil, ErrNoTransition
		}
		out.Status = RunDone
		out.SupervisorBusy = false
		return out, []Effect{
			{Kind: EffSetParentStatus, IssueStatus: "in_review"},
			record(RunEventRunFinished, "", map[string]any{"outcome": string(RunDone)}),
			{Kind: EffMilestone, Milestone: MilestoneRunFinished},
		}, nil
	case EvDecision:
		if ev.Decision.Action == ActionAbort {
			if effs, err = abortRun(&out, key, ev.Decision); err != nil {
				return rs, nil, err
			}
			return out, effs, nil
		}
		effs, err = stepDecision(&out, key, ev.Decision)
	case EvStepFinished, EvStepFailed, EvSupervisorNoDecision, EvChildCancelled:
		effs, err = stepObservation(&out, key, ev)
	default:
		return rs, nil, fmt.Errorf("%w: unknown event %q", ErrNoTransition, ev.Kind)
	}
	if err != nil {
		return rs, nil, err
	}
	return out, append(effs, schedule(&out)...), nil
}

func stepObservation(out *RunState, key string, ev Event) ([]Effect, error) {
	st, ok := out.Steps[key]
	if !ok {
		return nil, ErrNoTransition
	}
	node, _ := out.Def.NodeByKey(key)
	switch ev.Kind {
	case EvStepFinished:
		if st.Status != StepRunning {
			return nil, ErrNoTransition
		}
		payload := withDetail(map[string]any{"attempt": st.Attempts, "review": node.RequiresReview}, ev.Detail)
		if node.RequiresReview {
			out.Steps[key] = awaitSupervisor(st, PendingReview)
			return []Effect{record(RunEventStepFinished, key, payload)}, nil
		}
		st.Status = StepDone
		clearWait(&st)
		out.Steps[key] = st
		return []Effect{
			{Kind: EffSetChildStatus, Step: key, IssueStatus: "done"},
			record(RunEventStepFinished, key, payload),
		}, nil
	case EvStepFailed:
		if st.Status != StepRunning {
			return nil, ErrNoTransition
		}
		out.Steps[key] = awaitSupervisor(st, PendingFailure)
		payload := withDetail(map[string]any{"reason": ev.Reason, "attempt": st.Attempts}, ev.Detail)
		return []Effect{record(RunEventStepFailed, key, payload)}, nil
	case EvSupervisorNoDecision:
		if st.Status != StepAwaitingSupervisor {
			return nil, ErrNoTransition
		}
		if st.SupervisorWakes >= MaxSupervisorWakes {
			st.Status = StepAwaitingHuman
			st.EscalationReason = "The supervisor did not reach a decision: " + ev.Reason
			out.Steps[key] = st
			return []Effect{
				record(RunEventEscalated, key, withDetail(map[string]any{"reason": st.EscalationReason, "auto": true}, ev.Detail)),
				{Kind: EffEscalate, Step: key, Reason: st.EscalationReason},
			}, nil
		}
		return []Effect{record(RunEventProtocolError, key,
			withDetail(map[string]any{"reason": NoDecisionReason, "detail": ev.Reason, "wake": st.SupervisorWakes}, ev.Detail))}, nil
	case EvChildCancelled:
		if st.Status.Terminal() {
			return nil, ErrNoTransition
		}
		st.Status = StepSkipped
		clearWait(&st)
		out.Steps[key] = st
		return []Effect{
			{Kind: EffCancelStepWork, Step: key},
			record(RunEventDecision, key, withDetail(map[string]any{"action": string(ActionSkip), "source": "child_cancelled"}, ev.Detail)),
		}, nil
	}
	return nil, ErrNoTransition
}

func stepDecision(out *RunState, key string, d Decision) ([]Effect, error) {
	if d.Action == ActionRequestRewind {
		return requestRewind(out, key, d)
	}
	st, ok := out.Steps[key]
	if !ok {
		return nil, fmt.Errorf("%w: unknown step %q", ErrIllegalDecision, key)
	}
	if !st.Status.Awaiting() {
		return nil, fmt.Errorf("%w: step %q is %s", ErrStatusMismatch, key, st.Status)
	}
	node, _ := out.Def.NodeByKey(key)
	rec := record(RunEventDecision, key, decisionPayload(d))
	switch d.Action {
	case ActionApprove:
		st.Status = StepDone
		clearWait(&st)
		out.Steps[key] = st
		return []Effect{rec, {Kind: EffCancelStepWork, Step: key}, {Kind: EffSetChildStatus, Step: key, IssueStatus: "done"}}, nil
	case ActionRedo, ActionRetry:
		if d.Action == ActionRedo && d.Feedback == "" {
			return nil, fmt.Errorf("%w: redo needs feedback", ErrIllegalDecision)
		}
		if st.Attempts >= node.MaxAttempts {
			return nil, fmt.Errorf("%w: step %q used %d of %d attempts", ErrIllegalDecision, key, st.Attempts, node.MaxAttempts)
		}
		if d.Action == ActionRedo {
			st.LastFeedback = d.Feedback
		}
		effs := []Effect{rec, {Kind: EffCancelStepWork, Step: key}}
		return append(effs, dispatch(out, key, st, false)...), nil
	case ActionSkip:
		st.Status = StepSkipped
		clearWait(&st)
		out.Steps[key] = st
		return []Effect{rec, {Kind: EffCancelStepWork, Step: key}, {Kind: EffSetChildStatus, Step: key, IssueStatus: "cancelled"}}, nil
	case ActionRewind:
		if d.To == "" || d.Feedback == "" {
			return nil, fmt.Errorf("%w: rewind needs to and feedback", ErrIllegalDecision)
		}
		if d.To != key && !Ancestors(out.Def, key)[d.To] {
			return nil, fmt.Errorf("%w: %q is not %q or upstream of it", ErrIllegalDecision, d.To, key)
		}
		if out.RewindsUsed >= out.Def.MaxRewinds {
			return nil, fmt.Errorf("%w: the rewind budget (%d) is spent", ErrIllegalDecision, out.Def.MaxRewinds)
		}
		return append([]Effect{rec}, applyRewind(out, key, d.To, d.Feedback)...), nil
	case ActionEscalate:
		if d.Reason == "" {
			return nil, fmt.Errorf("%w: escalate needs a reason", ErrIllegalDecision)
		}
		if st.Status != StepAwaitingSupervisor {
			return nil, fmt.Errorf("%w: only the supervisor's turn can be escalated", ErrIllegalDecision)
		}
		st.Status = StepAwaitingHuman
		st.EscalationReason = d.Reason
		out.Steps[key] = st
		return []Effect{
			rec,
			record(RunEventEscalated, key, map[string]any{"reason": d.Reason}),
			{Kind: EffEscalate, Step: key, Reason: d.Reason},
		}, nil
	}
	return nil, fmt.Errorf("%w: unknown action %q", ErrIllegalDecision, d.Action)
}

func requestRewind(out *RunState, key string, d Decision) ([]Effect, error) {
	st, ok := out.Steps[key]
	if !ok {
		return nil, fmt.Errorf("%w: unknown step %q", ErrIllegalDecision, key)
	}
	if st.Status != StepRunning {
		return nil, fmt.Errorf("%w: step %q is %s", ErrStatusMismatch, key, st.Status)
	}
	if d.Reason == "" {
		return nil, fmt.Errorf("%w: request-rewind needs a reason", ErrIllegalDecision)
	}
	if d.To != "" && d.To != key && !Ancestors(out.Def, key)[d.To] {
		return nil, fmt.Errorf("%w: %q is not %q or upstream of it", ErrIllegalDecision, d.To, key)
	}
	payload := map[string]any{"to": d.To, "reason": d.Reason}
	if out.RewindsUsed >= out.Def.MaxRewinds {
		payload["budget_exhausted"] = true
		out.Steps[key] = awaitSupervisor(st, PendingFailure)
	} else {
		out.Steps[key] = awaitSupervisor(st, PendingRewindRequest)
	}
	return []Effect{record(RunEventRewindRequested, key, payload)}, nil
}

// applyRewind resets R = {to} ∪ (descendants of to that are not pending)
// (spec §5.5). Child issues are reused: they go back to backlog.
func applyRewind(out *RunState, from, to, feedback string) []Effect {
	out.RewindsUsed++
	desc := Descendants(out.Def, to)
	reset := []string{}
	var effs []Effect
	for _, n := range out.Def.Nodes {
		st := out.Steps[n.Key]
		if n.Key != to && !(desc[n.Key] && st.Status != StepPending) {
			continue
		}
		st.Status = StepPending
		st.Attempts = 0
		clearWait(&st)
		st.LastFeedback = ""
		if n.Key == to {
			st.LastFeedback = feedback
		}
		out.Steps[n.Key] = st
		reset = append(reset, n.Key)
		effs = append(effs,
			Effect{Kind: EffCancelStepWork, Step: n.Key},
			Effect{Kind: EffSetChildStatus, Step: n.Key, IssueStatus: "backlog"})
	}
	rec := record(RunEventRewind, from, map[string]any{"from": from, "to": to, "reset": reset, "rewinds_used": out.RewindsUsed})
	return append([]Effect{rec}, effs...)
}

func abortRun(out *RunState, key string, d Decision) ([]Effect, error) {
	if d.Reason == "" {
		return nil, fmt.Errorf("%w: abort needs a reason", ErrIllegalDecision)
	}
	if key != "" {
		st, ok := out.Steps[key]
		if !ok {
			return nil, fmt.Errorf("%w: unknown step %q", ErrIllegalDecision, key)
		}
		if st.Status.Terminal() {
			return nil, fmt.Errorf("%w: step %q is %s", ErrStatusMismatch, key, st.Status)
		}
	}
	effs := []Effect{record(RunEventDecision, key, decisionPayload(d))}
	return append(effs, endRun(out, RunFailed, key, d.Reason)...), nil
}

// endRun closes the run: open steps are cancelled (the aborted focus step
// fails), their children are cancelled and every in-flight task stops.
func endRun(out *RunState, status RunStatus, focus, reason string) []Effect {
	effs := []Effect{{Kind: EffCancelRunTasks}}
	for _, n := range out.Def.Nodes {
		st := out.Steps[n.Key]
		if st.Status.Terminal() {
			continue
		}
		st.Status = StepCancelled
		if status == RunFailed && n.Key == focus {
			st.Status = StepFailed
		}
		clearWait(&st)
		out.Steps[n.Key] = st
		effs = append(effs, Effect{Kind: EffSetChildStatus, Step: n.Key, IssueStatus: "cancelled"})
	}
	out.Status = status
	out.SupervisorBusy = false
	out.SupervisorFor = map[string]bool{}
	if status == RunFailed {
		return append(effs,
			Effect{Kind: EffSetParentStatus, IssueStatus: "blocked"},
			record(RunEventRunFinished, "", map[string]any{"outcome": string(RunFailed), "reason": reason}),
			Effect{Kind: EffMilestone, Milestone: MilestoneRunFailed, Reason: reason})
	}
	return append(effs,
		record(RunEventRunCancelled, "", map[string]any{"reason": reason}),
		Effect{Kind: EffMilestone, Milestone: MilestoneRunCancelled, Reason: reason})
}

// schedule starts ready steps, wakes the supervisor for at most one pending
// decision, requests the summary once everything settled, and derives
// waiting_human. Its effects are the engine's own.
func schedule(out *RunState) []Effect {
	if !out.Status.Active() {
		return nil
	}
	var effs []Effect
	for _, n := range out.Def.Nodes {
		st := out.Steps[n.Key]
		if st.Status != StepPending || !out.depsSettled(n) {
			continue
		}
		effs = append(effs, dispatch(out, n.Key, st, true)...)
	}
	if !out.SupervisorBusy {
		for _, n := range out.Def.Nodes {
			st := out.Steps[n.Key]
			if st.Status != StepAwaitingSupervisor || out.SupervisorFor[n.Key] || st.SupervisorWakes >= MaxSupervisorWakes {
				continue
			}
			st.SupervisorWakes++
			out.Steps[n.Key] = st
			out.SupervisorFor[n.Key] = true
			out.SupervisorBusy = true
			effs = append(effs, engine(Effect{Kind: EffEnqueueSupervisor, Step: n.Key, SupervisorKind: string(st.PendingReason)}))
			break
		}
	}
	if !out.SupervisorBusy && !out.SummaryRequested && out.AllSettled() {
		out.SummaryRequested = true
		out.SupervisorBusy = true
		effs = append(effs, engine(Effect{Kind: EffEnqueueSupervisor, SupervisorKind: KindSummary}))
	}
	out.Status = RunRunning
	for _, st := range out.Steps {
		if st.Status == StepAwaitingHuman {
			out.Status = RunWaitingHuman
			break
		}
	}
	return effs
}

// dispatch starts the next attempt of a step. Every dispatch, first or
// repeated (redo, retry, a rewound step rescheduled), records step_started
// with its attempt; the event is the engine's, as the scheduler's are.
// byEngine marks the status change and the enqueue as the engine's too.
func dispatch(out *RunState, key string, st StepState, byEngine bool) []Effect {
	st.Status = StepRunning
	st.Attempts++
	clearWait(&st)
	out.Steps[key] = st
	child := Effect{Kind: EffSetChildStatus, Step: key, IssueStatus: "in_progress"}
	enqueue := Effect{Kind: EffEnqueueStep, Step: key}
	if byEngine {
		child, enqueue = engine(child), engine(enqueue)
	}
	return []Effect{child, enqueue, engine(record(RunEventStepStarted, key, map[string]any{"attempt": st.Attempts}))}
}

func awaitSupervisor(st StepState, reason PendingReason) StepState {
	st.Status = StepAwaitingSupervisor
	st.PendingReason = reason
	st.EscalationReason = ""
	st.SupervisorWakes = 0
	return st
}

func clearWait(st *StepState) {
	st.PendingReason = ""
	st.EscalationReason = ""
	st.SupervisorWakes = 0
}

func engine(e Effect) Effect {
	e.Engine = true
	return e
}

func record(kind, step string, payload map[string]any) Effect {
	if payload == nil {
		payload = map[string]any{}
	}
	return Effect{Kind: EffRecordEvent, RunEvent: kind, Step: step, Payload: payload}
}

func withDetail(p, detail map[string]any) map[string]any {
	for k, v := range detail {
		if _, taken := p[k]; !taken {
			p[k] = v
		}
	}
	return p
}

func decisionPayload(d Decision) map[string]any {
	p := map[string]any{"action": string(d.Action)}
	if d.To != "" {
		p["to"] = d.To
	}
	if d.Reason != "" {
		p["reason"] = d.Reason
	}
	if d.Feedback != "" {
		p["feedback"] = d.Feedback
	}
	return p
}
