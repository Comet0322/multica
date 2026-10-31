package sandboxbroker

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Placement is the unit that decides which tasks share a pod.
type Placement string

const (
	// PlacementTask gives every task its own pod, one at a time.
	PlacementTask Placement = "task"
	// PlacementIssue binds a pod to one issue, or to one chat conversation for
	// tasks that have no issue, and runs that unit's tasks side by side in it.
	PlacementIssue Placement = "issue"
)

// DefaultPodConcurrency is the daemon's own concurrency limit, used for a
// shared pod when none is configured.
const DefaultPodConcurrency = 20

// DefaultPlacementWait stays well under the 90 seconds after which the server
// hands a claimed but unstarted task to the next claimer.
const DefaultPlacementWait = 30 * time.Second

func (c *PoolConfig) normalizePlacement() error {
	switch c.Placement {
	case "":
		c.Placement = PlacementTask
	case PlacementTask, PlacementIssue:
	default:
		return fmt.Errorf("unknown placement %q (want %q or %q)", c.Placement, PlacementTask, PlacementIssue)
	}
	if c.PodConcurrency < 0 {
		return fmt.Errorf("pod concurrency must not be negative")
	}
	switch {
	case c.Placement == PlacementTask:
		c.PodConcurrency = 1
	case c.PodConcurrency == 0:
		c.PodConcurrency = DefaultPodConcurrency
	}
	if c.PlacementWait < 0 {
		return fmt.Errorf("placement wait must not be negative")
	}
	if c.PlacementWait == 0 {
		c.PlacementWait = DefaultPlacementWait
	}
	return nil
}

// parkedTask is a task the manager claimed upstream and has not yet handed to
// a pod. It sits in a pod's queue once assigned, or in the waiting list.
type parkedTask struct {
	raw       []byte
	ref       claimedTaskRef
	key       string
	llmKey    *taskKey
	claimedAt time.Time
}

// placementKey names the unit a task belongs to. Tasks with the same key may
// share a pod; tasks with different keys never do.
func (p *Pool) placementKey(ref claimedTaskRef) string {
	if p.cfg.Placement == PlacementIssue {
		switch {
		case ref.IssueID != "":
			return "issue:" + ref.IssueID
		case ref.ChatSessionID != "":
			return "chat:" + ref.ChatSessionID
		}
	}
	return "task:" + ref.ID
}

// freeLocked reports whether the pod has a slot for another task.
func (p *Pool) freeLocked(pd *pod) bool {
	return len(pd.leases)+len(pd.queue) < p.cfg.PodConcurrency
}

// canClaimLocked reports whether the polling pod may have a task claimed for
// it: an idle pod, or a pod bound to a key with a free slot. Nothing is
// claimed while claimed tasks are still waiting for a pod.
func (p *Pool) canClaimLocked(pd *pod) bool {
	if pd.claiming || len(p.waiting) > 0 {
		return false
	}
	switch pd.State {
	case PodIdle:
		return true
	case PodLeased:
		return p.freeLocked(pd)
	}
	return false
}

// podsByIPLocked lists the registered pods in a stable order, the preferred
// one first.
func (p *Pool) podsByIPLocked(prefer *pod) []*pod {
	pods := make([]*pod, 0, len(p.pods))
	for _, pd := range p.pods {
		pods = append(pods, pd)
	}
	sort.Slice(pods, func(i, j int) bool {
		if (pods[i] == prefer) != (pods[j] == prefer) {
			return pods[i] == prefer
		}
		return pods[i].IP < pods[j].IP
	})
	return pods
}

// placeLocked picks the pod for a task with the given key: a pod already bound
// to the key that has a free slot, else an idle pod. Nil means none can take it
// now. A bound pod never takes another key.
func (p *Pool) placeLocked(prefer *pod, key string) *pod {
	pods := p.podsByIPLocked(prefer)
	for _, pd := range pods {
		if pd.State == PodLeased && pd.boundKey == key && p.freeLocked(pd) {
			return pd
		}
	}
	for _, pd := range pods {
		if pd.State == PodIdle && pd.boundKey == "" {
			return pd
		}
	}
	return nil
}

// assignLocked reserves a slot on the pod for the task and binds the pod to
// the task's key. The lease starts when the pod picks the task up.
func (p *Pool) assignLocked(pd *pod, t *parkedTask) {
	pd.queue = append(pd.queue, t)
	pd.boundKey = t.key
	pd.State = PodLeased
}

// takeQueuedLocked hands the pod the oldest task assigned to it and starts its
// lease. Nil when none is assigned.
func (p *Pool) takeQueuedLocked(pd *pod) *parkedTask {
	if len(pd.queue) == 0 {
		return nil
	}
	t := pd.queue[0]
	pd.queue = pd.queue[1:]
	p.startLeaseLocked(pd, t.ref.ID, t.ref.initiator()).key = t.llmKey
	return t
}

// rebalanceLocked takes tasks back from lost pods and from pods that left them
// unpicked too long, places the waiting tasks that now fit (the polling pod
// first) and returns those that waited past the limit. Callers hold p.mu.
func (p *Pool) rebalanceLocked(polling *pod) (expired []*parkedTask) {
	now := p.now()
	for _, pd := range p.pods {
		if len(pd.queue) == 0 {
			continue
		}
		keep := pd.queue[:0]
		for _, t := range pd.queue {
			if pd.State == PodLost || (pd != polling && now.Sub(t.claimedAt) > p.cfg.PlacementWait) {
				p.waiting = append(p.waiting, t)
				continue
			}
			keep = append(keep, t)
		}
		pd.queue = keep
		if pd.State == PodLeased && len(pd.leases) == 0 && len(pd.queue) == 0 {
			pd.State, pd.boundKey = PodIdle, ""
		}
	}
	sort.SliceStable(p.waiting, func(i, j int) bool { return p.waiting[i].claimedAt.Before(p.waiting[j].claimedAt) })
	still := p.waiting[:0]
	for _, t := range p.waiting {
		if target := p.placeLocked(polling, t.key); target != nil {
			p.assignLocked(target, t)
			continue
		}
		if now.Sub(t.claimedAt) > p.cfg.PlacementWait {
			expired = append(expired, t)
			continue
		}
		still = append(still, t)
	}
	p.waiting = still
	return expired
}

// failParked reports tasks that found no pod in time as failed.
func (p *Pool) failParked(ctx context.Context, tasks []*parkedTask) {
	const why = "no sandbox pod could take the task in time"
	for _, t := range tasks {
		p.revokeKey(t.llmKey, t.ref.ID)
		p.auditDenied(t.ref, why)
		p.failUpstream(ctx, t.ref.ID, why)
	}
}
