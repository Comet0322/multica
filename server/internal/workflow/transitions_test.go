package workflow

import "testing"

func step(phase Phase, approval bool, attempts, max int) StepMeta {
	return StepMeta{Run: "r", Node: "n", Phase: phase, Approval: approval, Attempts: attempts, MaxRetries: max}
}

func TestNextTable(t *testing.T) {
	cases := []struct {
		name     string
		in       StepMeta
		ev       Event
		wantOK   bool
		to       Phase
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
