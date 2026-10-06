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
