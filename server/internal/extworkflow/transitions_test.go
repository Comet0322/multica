package extworkflow

import (
	"errors"
	"reflect"
	"testing"
)

// testDef is the run every table case starts from:
//
//	spec ──► backend (review, max 2) ──► qa
//	  └────► docs
func testDef() Definition {
	return Definition{
		SupervisorAgentID: "00000000-0000-0000-0000-0000000000aa",
		MaxRewinds:        2,
		Nodes: []Node{
			{Key: "spec", Title: "Spec", AgentID: "00000000-0000-0000-0000-000000000001", MaxAttempts: 3},
			{Key: "backend", Title: "Backend", AgentID: "00000000-0000-0000-0000-000000000002", RequiresReview: true, MaxAttempts: 2, DependsOn: []string{"spec"}},
			{Key: "qa", Title: "QA", AgentID: "00000000-0000-0000-0000-000000000003", MaxAttempts: 3, DependsOn: []string{"backend"}},
			{Key: "docs", Title: "Docs", AgentID: "00000000-0000-0000-0000-000000000004", MaxAttempts: 3, DependsOn: []string{"spec"}},
		},
	}
}

func state(mods ...func(*RunState)) RunState {
	rs := NewRunState(testDef())
	for _, m := range mods {
		m(&rs)
	}
	return rs
}

func step(key string, status StepStatus, attempts int, opts ...func(*StepState)) func(*RunState) {
	return func(rs *RunState) {
		st := rs.Steps[key]
		st.Status = status
		st.Attempts = attempts
		for _, o := range opts {
			o(&st)
		}
		rs.Steps[key] = st
	}
}

func because(r PendingReason) func(*StepState) { return func(s *StepState) { s.PendingReason = r } }
func woken(n int) func(*StepState)             { return func(s *StepState) { s.SupervisorWakes = n } }
func supervising(key string) func(*RunState) {
	return func(rs *RunState) { rs.SupervisorFor[key] = true; rs.SupervisorBusy = true }
}
func busy(rs *RunState)                     { rs.SupervisorBusy = true }
func rewinds(n int) func(*RunState)         { return func(rs *RunState) { rs.RewindsUsed = n } }
func summaryRequested(rs *RunState)         { rs.SummaryRequested = true }
func runStatus(s RunStatus) func(*RunState) { return func(rs *RunState) { rs.Status = s } }
func decision(d Decision) Event             { return Event{Kind: EvDecision, Decision: d} }

func sig(e Effect) string {
	switch e.Kind {
	case EffEnqueueStep:
		return "enqueue_step:" + e.Step
	case EffEnqueueSupervisor:
		return "enqueue_supervisor:" + e.Step + "/" + e.SupervisorKind
	case EffSetChildStatus:
		return "child:" + e.Step + "=" + e.IssueStatus
	case EffSetParentStatus:
		return "parent=" + e.IssueStatus
	case EffCancelStepWork:
		return "cancel_step:" + e.Step
	case EffCancelRunTasks:
		return "cancel_run"
	case EffEscalate:
		return "escalate:" + e.Step
	case EffMilestone:
		return "milestone:" + e.Milestone
	case EffRecordEvent:
		return "event:" + e.RunEvent + "@" + e.Step
	}
	return "unknown:" + string(e.Kind)
}

func sigs(effs []Effect) []string {
	out := []string{}
	for _, e := range effs {
		out = append(out, sig(e))
	}
	return out
}

func wantStep(t *testing.T, rs RunState, key string, status StepStatus, attempts int) StepState {
	t.Helper()
	st := rs.Steps[key]
	if st.Status != status || st.Attempts != attempts {
		t.Fatalf("step %s = %s/%d, want %s/%d", key, st.Status, st.Attempts, status, attempts)
	}
	return st
}

func payloadOf(t *testing.T, effs []Effect, runEvent string) map[string]any {
	t.Helper()
	for _, e := range effs {
		if e.Kind == EffRecordEvent && e.RunEvent == runEvent {
			return e.Payload
		}
	}
	t.Fatalf("no %s event in %v", runEvent, sigs(effs))
	return nil
}

func TestNext(t *testing.T) {
	cases := []struct {
		name    string
		state   RunState
		key     string
		event   Event
		wantErr error
		want    []string
		check   func(t *testing.T, got RunState, effs []Effect)
	}{
		{
			name:  "tick starts the roots",
			state: state(),
			event: Event{Kind: EvTick},
			want:  []string{"child:spec=in_progress", "enqueue_step:spec", "event:step_started@spec"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "spec", StepRunning, 1)
				wantStep(t, got, "backend", StepPending, 0)
			},
		},
		{
			name:  "idle tick does nothing",
			state: state(step("spec", StepRunning, 1)),
			event: Event{Kind: EvTick},
			want:  []string{},
		},
		{
			name:  "finishing a step without review starts its dependents",
			state: state(step("spec", StepRunning, 1)),
			key:   "spec",
			event: Event{Kind: EvStepFinished},
			want: []string{
				"child:spec=done", "event:step_finished@spec",
				"child:backend=in_progress", "enqueue_step:backend", "event:step_started@backend",
				"child:docs=in_progress", "enqueue_step:docs", "event:step_started@docs",
			},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "spec", StepDone, 1)
				wantStep(t, got, "backend", StepRunning, 1)
				wantStep(t, got, "qa", StepPending, 0)
			},
		},
		{
			name:  "finishing a review step wakes the supervisor",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepRunning, 1)),
			key:   "backend",
			event: Event{Kind: EvStepFinished},
			want:  []string{"event:step_finished@backend", "enqueue_supervisor:backend/review"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				st := wantStep(t, got, "backend", StepAwaitingSupervisor, 1)
				if st.PendingReason != PendingReview || st.SupervisorWakes != 1 || !got.SupervisorBusy || !got.SupervisorFor["backend"] {
					t.Fatalf("backend = %+v busy=%v for=%v", st, got.SupervisorBusy, got.SupervisorFor)
				}
			},
		},
		{
			name:  "a busy supervisor defers the wake",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepRunning, 1), busy),
			key:   "backend",
			event: Event{Kind: EvStepFinished},
			want:  []string{"event:step_finished@backend"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				if st := wantStep(t, got, "backend", StepAwaitingSupervisor, 1); st.SupervisorWakes != 0 {
					t.Fatalf("wakes = %d, want 0 while the supervisor is busy", st.SupervisorWakes)
				}
			},
		},
		{
			name:    "finishing a pending step is not a transition",
			state:   state(),
			key:     "backend",
			event:   Event{Kind: EvStepFinished},
			wantErr: ErrNoTransition,
		},
		{
			name:  "a failed step goes to the supervisor",
			state: state(step("spec", StepRunning, 1)),
			key:   "spec",
			event: Event{Kind: EvStepFailed, Reason: "agent_error", Detail: map[string]any{"error": "boom"}},
			want:  []string{"event:step_failed@spec", "enqueue_supervisor:spec/failure"},
			check: func(t *testing.T, got RunState, effs []Effect) {
				if st := wantStep(t, got, "spec", StepAwaitingSupervisor, 1); st.PendingReason != PendingFailure {
					t.Fatalf("reason = %s", st.PendingReason)
				}
				p := payloadOf(t, effs, RunEventStepFailed)
				if p["reason"] != "agent_error" || p["error"] != "boom" {
					t.Fatalf("payload = %v", p)
				}
			},
		},
		{
			name:  "request-rewind with budget left asks the supervisor",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepRunning, 1)),
			key:   "backend",
			event: decision(Decision{Action: ActionRequestRewind, To: "spec", Reason: "spec assumed pagination"}),
			want:  []string{"event:rewind_requested@backend", "enqueue_supervisor:backend/rewind_request"},
		},
		{
			name:  "request-rewind with the budget spent becomes a failure",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepRunning, 1), rewinds(2)),
			key:   "backend",
			event: decision(Decision{Action: ActionRequestRewind, To: "spec", Reason: "again"}),
			want:  []string{"event:rewind_requested@backend", "enqueue_supervisor:backend/failure"},
			check: func(t *testing.T, got RunState, effs []Effect) {
				if p := payloadOf(t, effs, RunEventRewindRequested); p["budget_exhausted"] != true {
					t.Fatalf("payload = %v", p)
				}
			},
		},
		{
			name:    "request-rewind to a non-ancestor is illegal",
			state:   state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepRunning, 1)),
			key:     "backend",
			event:   decision(Decision{Action: ActionRequestRewind, To: "docs", Reason: "x"}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:    "request-rewind needs a running step",
			state:   state(step("spec", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview))),
			key:     "backend",
			event:   decision(Decision{Action: ActionRequestRewind, Reason: "x"}),
			wantErr: ErrStatusMismatch,
		},
		{
			name: "approve finishes the step and starts its dependents",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1),
				step("backend", StepAwaitingSupervisor, 1, because(PendingReview), woken(1)), supervising("backend")),
			key:   "backend",
			event: decision(Decision{Action: ActionApprove}),
			want: []string{
				"event:decision@backend", "cancel_step:backend", "child:backend=done",
				"child:qa=in_progress", "enqueue_step:qa", "event:step_started@qa",
			},
		},
		{
			name:    "approve needs a pending decision",
			state:   state(step("spec", StepRunning, 1)),
			key:     "spec",
			event:   decision(Decision{Action: ActionApprove}),
			wantErr: ErrStatusMismatch,
		},
		{
			name:    "redo needs feedback",
			state:   state(step("spec", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview))),
			key:     "backend",
			event:   decision(Decision{Action: ActionRedo}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:  "redo restarts the step with feedback",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview), woken(1))),
			key:   "backend",
			event: decision(Decision{Action: ActionRedo, Feedback: "add tests"}),
			want:  []string{"event:decision@backend", "cancel_step:backend", "child:backend=in_progress", "enqueue_step:backend", "event:step_started@backend"},
			check: func(t *testing.T, got RunState, effs []Effect) {
				st := wantStep(t, got, "backend", StepRunning, 2)
				if st.LastFeedback != "add tests" || st.PendingReason != "" || st.SupervisorWakes != 0 {
					t.Fatalf("backend = %+v", st)
				}
				wantStepStarted(t, effs, 2)
			},
		},
		{
			name:    "redo is refused once attempts are spent",
			state:   state(step("spec", StepDone, 1), step("backend", StepAwaitingSupervisor, 2, because(PendingReview))),
			key:     "backend",
			event:   decision(Decision{Action: ActionRedo, Feedback: "again"}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:  "retry restarts a failed step",
			state: state(step("spec", StepAwaitingSupervisor, 1, because(PendingFailure), woken(1))),
			key:   "spec",
			event: decision(Decision{Action: ActionRetry}),
			want:  []string{"event:decision@spec", "cancel_step:spec", "child:spec=in_progress", "enqueue_step:spec", "event:step_started@spec"},
			check: func(t *testing.T, got RunState, effs []Effect) {
				wantStep(t, got, "spec", StepRunning, 2)
				wantStepStarted(t, effs, 2)
			},
		},
		{
			name:  "a person's retry restarts an escalated step",
			state: state(step("spec", StepAwaitingHuman, 1, because(PendingFailure)), runStatus(RunWaitingHuman)),
			key:   "spec",
			event: decision(Decision{Action: ActionRetry}),
			want:  []string{"event:decision@spec", "cancel_step:spec", "child:spec=in_progress", "enqueue_step:spec", "event:step_started@spec"},
			check: func(t *testing.T, got RunState, effs []Effect) {
				wantStep(t, got, "spec", StepRunning, 2)
				wantStepStarted(t, effs, 2)
				if got.Status != RunRunning {
					t.Fatalf("run = %s, want running", got.Status)
				}
			},
		},
		{
			name:  "skip settles the step and unblocks dependents",
			state: state(step("spec", StepAwaitingHuman, 3, because(PendingFailure)), runStatus(RunWaitingHuman)),
			key:   "spec",
			event: decision(Decision{Action: ActionSkip}),
			want: []string{
				"event:decision@spec", "cancel_step:spec", "child:spec=cancelled",
				"child:backend=in_progress", "enqueue_step:backend", "event:step_started@backend",
				"child:docs=in_progress", "enqueue_step:docs", "event:step_started@docs",
			},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "spec", StepSkipped, 3)
				if got.Status != RunRunning {
					t.Fatalf("run = %s, want running", got.Status)
				}
			},
		},
		{
			name:    "rewind to a non-ancestor is illegal",
			state:   state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingRewindRequest))),
			key:     "backend",
			event:   decision(Decision{Action: ActionRewind, To: "docs", Feedback: "x"}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:    "rewind needs budget",
			state:   state(step("spec", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingRewindRequest)), rewinds(2)),
			key:     "backend",
			event:   decision(Decision{Action: ActionRewind, To: "spec", Feedback: "x"}),
			wantErr: ErrIllegalDecision,
		},
		{
			name: "rewind resets the target and its started descendants",
			state: state(step("spec", StepDone, 1), step("docs", StepRunning, 1),
				step("backend", StepAwaitingSupervisor, 1, because(PendingRewindRequest), woken(1)), supervising("backend")),
			key:   "backend",
			event: decision(Decision{Action: ActionRewind, To: "spec", Feedback: "use cursor pagination"}),
			want: []string{
				"event:decision@backend", "event:rewind@backend",
				"cancel_step:spec", "child:spec=backlog",
				"cancel_step:backend", "child:backend=backlog",
				"cancel_step:docs", "child:docs=backlog",
				"child:spec=in_progress", "enqueue_step:spec", "event:step_started@spec",
			},
			check: func(t *testing.T, got RunState, effs []Effect) {
				if got.RewindsUsed != 1 {
					t.Fatalf("rewinds = %d", got.RewindsUsed)
				}
				if st := wantStep(t, got, "spec", StepRunning, 1); st.LastFeedback != "use cursor pagination" {
					t.Fatalf("spec feedback = %q", st.LastFeedback)
				}
				wantStep(t, got, "backend", StepPending, 0)
				wantStep(t, got, "docs", StepPending, 0)
				wantStep(t, got, "qa", StepPending, 0)
				if p := payloadOf(t, effs, RunEventRewind); !reflect.DeepEqual(p["reset"], []string{"spec", "backend", "docs"}) {
					t.Fatalf("reset = %v", p["reset"])
				}
				wantStepStarted(t, effs, 1)
			},
		},
		{
			name:  "rewind to the step itself reruns only that step",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingHuman, 2, because(PendingReview)), runStatus(RunWaitingHuman)),
			key:   "backend",
			event: decision(Decision{Action: ActionRewind, To: "backend", Feedback: "start over"}),
			want: []string{
				"event:decision@backend", "event:rewind@backend",
				"cancel_step:backend", "child:backend=backlog",
				"child:backend=in_progress", "enqueue_step:backend", "event:step_started@backend",
			},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "backend", StepRunning, 1)
				wantStep(t, got, "docs", StepDone, 1)
				if got.Status != RunRunning {
					t.Fatalf("run = %s", got.Status)
				}
			},
		},
		{
			name:  "escalate hands the step to a person",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview), woken(1)), supervising("backend")),
			key:   "backend",
			event: decision(Decision{Action: ActionEscalate, Reason: "needs product call"}),
			want:  []string{"event:decision@backend", "event:escalated@backend", "escalate:backend"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				if st := wantStep(t, got, "backend", StepAwaitingHuman, 1); st.EscalationReason != "needs product call" {
					t.Fatalf("reason = %q", st.EscalationReason)
				}
				if got.Status != RunWaitingHuman {
					t.Fatalf("run = %s, want waiting_human", got.Status)
				}
			},
		},
		{
			name:    "escalate is only for the supervisor's turn",
			state:   state(step("spec", StepAwaitingHuman, 1, because(PendingFailure)), runStatus(RunWaitingHuman)),
			key:     "spec",
			event:   decision(Decision{Action: ActionEscalate, Reason: "x"}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:  "abort fails the run",
			state: state(step("spec", StepDone, 1), step("backend", StepRunning, 1), step("docs", StepAwaitingHuman, 1, because(PendingFailure)), runStatus(RunWaitingHuman)),
			key:   "backend",
			event: decision(Decision{Action: ActionAbort, Reason: "wrong approach"}),
			want: []string{
				"event:decision@backend", "cancel_run",
				"child:backend=cancelled", "child:qa=cancelled", "child:docs=cancelled",
				"parent=blocked", "event:run_finished@", "milestone:run_failed",
			},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "spec", StepDone, 1)
				wantStep(t, got, "backend", StepFailed, 1)
				wantStep(t, got, "docs", StepCancelled, 1)
				if got.Status != RunFailed {
					t.Fatalf("run = %s", got.Status)
				}
			},
		},
		{
			name:    "abort needs a reason",
			state:   state(step("spec", StepRunning, 1)),
			event:   decision(Decision{Action: ActionAbort}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:  "a silent supervisor is woken once more",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview), woken(1))),
			key:   "backend",
			event: Event{Kind: EvSupervisorNoDecision, Reason: "ended without a decision"},
			want:  []string{"event:protocol_error@backend", "enqueue_supervisor:backend/review"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				if st := got.Steps["backend"]; st.SupervisorWakes != 2 {
					t.Fatalf("wakes = %d, want 2", st.SupervisorWakes)
				}
			},
		},
		{
			name:  "a second silence escalates",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview), woken(2))),
			key:   "backend",
			event: Event{Kind: EvSupervisorNoDecision, Reason: "ended without a decision"},
			want:  []string{"event:escalated@backend", "escalate:backend"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "backend", StepAwaitingHuman, 1)
				if got.Status != RunWaitingHuman {
					t.Fatalf("run = %s", got.Status)
				}
			},
		},
		{
			name:    "supervisor silence on a decided step is not a transition",
			state:   state(step("spec", StepDone, 1)),
			key:     "spec",
			event:   Event{Kind: EvSupervisorNoDecision},
			wantErr: ErrNoTransition,
		},
		{
			name:  "a person cancelling the child skips the step",
			state: state(step("spec", StepRunning, 1)),
			key:   "spec",
			event: Event{Kind: EvChildCancelled},
			want: []string{
				"cancel_step:spec", "event:decision@spec",
				"child:backend=in_progress", "enqueue_step:backend", "event:step_started@backend",
				"child:docs=in_progress", "enqueue_step:docs", "event:step_started@docs",
			},
			check: func(t *testing.T, got RunState, _ []Effect) { wantStep(t, got, "spec", StepSkipped, 1) },
		},
		{
			name:  "the last settled step requests the summary",
			state: state(step("spec", StepDone, 1), step("backend", StepDone, 1), step("docs", StepDone, 1), step("qa", StepRunning, 1)),
			key:   "qa",
			event: Event{Kind: EvStepFinished},
			want:  []string{"child:qa=done", "event:step_finished@qa", "enqueue_supervisor:/summary"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				if !got.SummaryRequested || !got.SupervisorBusy {
					t.Fatalf("summary=%v busy=%v", got.SummaryRequested, got.SupervisorBusy)
				}
			},
		},
		{
			name:  "the summary ending finishes the run",
			state: state(step("spec", StepDone, 1), step("backend", StepDone, 1), step("docs", StepSkipped, 1), step("qa", StepDone, 1), summaryRequested),
			event: Event{Kind: EvSummaryEnded},
			want:  []string{"parent=in_review", "event:run_finished@", "milestone:run_finished"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				if got.Status != RunDone {
					t.Fatalf("run = %s", got.Status)
				}
			},
		},
		{
			name:  "cancelling the run cancels open steps",
			state: state(step("spec", StepRunning, 1)),
			event: Event{Kind: EvCancelRun, Reason: "the parent issue was cancelled"},
			want: []string{
				"cancel_run", "child:spec=cancelled", "child:backend=cancelled", "child:qa=cancelled", "child:docs=cancelled",
				"event:run_cancelled@", "milestone:run_cancelled",
			},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "spec", StepCancelled, 1)
				if got.Status != RunCancelled {
					t.Fatalf("run = %s", got.Status)
				}
			},
		},
		{
			name:    "decisions on a finished run are a status mismatch",
			state:   state(runStatus(RunDone)),
			key:     "spec",
			event:   decision(Decision{Action: ActionApprove}),
			wantErr: ErrStatusMismatch,
		},
		{
			name:    "observations on a finished run are ignored",
			state:   state(runStatus(RunCancelled)),
			key:     "spec",
			event:   Event{Kind: EvStepFinished},
			wantErr: ErrNoTransition,
		},
		{
			name:    "unknown actions are illegal",
			state:   state(step("spec", StepAwaitingHuman, 1)),
			key:     "spec",
			event:   decision(Decision{Action: "promote"}),
			wantErr: ErrIllegalDecision,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.state.Clone()
			got, effs, err := Next(tc.state, tc.key, tc.event)
			if !reflect.DeepEqual(tc.state, before) {
				t.Fatalf("Next mutated its input")
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if !reflect.DeepEqual(got, before) {
					t.Fatalf("a rejected event changed the state")
				}
				return
			}
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if gotSigs := sigs(effs); !reflect.DeepEqual(gotSigs, tc.want) {
				t.Fatalf("effects\n got %v\nwant %v", gotSigs, tc.want)
			}
			if tc.check != nil {
				tc.check(t, got, effs)
			}
		})
	}
}

// wantStepStarted checks the one step_started event: the attempt it
// dispatches, recorded with the engine as actor like every dispatch.
func wantStepStarted(t *testing.T, effs []Effect, attempt int) {
	t.Helper()
	var started []Effect
	for _, e := range effs {
		if e.Kind == EffRecordEvent && e.RunEvent == RunEventStepStarted {
			started = append(started, e)
		}
	}
	if len(started) != 1 {
		t.Fatalf("step_started events = %d in %v, want 1", len(started), sigs(effs))
	}
	if !reflect.DeepEqual(started[0].Payload, map[string]any{"attempt": attempt}) || !started[0].Engine {
		t.Fatalf("step_started = %+v, want attempt %d by the engine", started[0], attempt)
	}
}

func TestScheduleMarksEngineEffects(t *testing.T) {
	_, effs, err := Next(state(step("spec", StepAwaitingHuman, 1)), "spec", decision(Decision{Action: ActionApprove}))
	if err != nil {
		t.Fatal(err)
	}
	// The decision's own three effects carry the decider; the scheduler's
	// follow-up (backend and docs starting) is the engine's.
	if len(effs) != 9 {
		t.Fatalf("effects = %v", sigs(effs))
	}
	for i, e := range effs {
		if want := i >= 3; e.Engine != want {
			t.Fatalf("effect %d %s engine=%v, want %v", i, sig(e), e.Engine, want)
		}
	}
}
