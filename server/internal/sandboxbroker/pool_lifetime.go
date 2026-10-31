package sandboxbroker

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Defaults for the lifetime of a shared pod.
const (
	// DefaultPodIdleTimeout releases a shared pod that has had no running task
	// for this long.
	DefaultPodIdleTimeout = 15 * time.Minute
	// DefaultPodMaxLifetime stops a shared pod from taking new tasks this long
	// after it was bound.
	DefaultPodMaxLifetime = 2 * time.Hour
	// DefaultPodDrainGrace is how long tasks that are still running may finish
	// once the pod reached its maximum lifetime.
	DefaultPodDrainGrace = 10 * time.Minute
)

func (c *PoolConfig) normalizeLifetime() error {
	if c.PodIdleTimeout < 0 || c.PodMaxLifetime < 0 || c.PodDrainGrace < 0 {
		return fmt.Errorf("pod idle timeout, maximum lifetime and drain grace must not be negative")
	}
	if c.PodIdleTimeout == 0 {
		c.PodIdleTimeout = DefaultPodIdleTimeout
	}
	if c.PodMaxLifetime == 0 {
		c.PodMaxLifetime = DefaultPodMaxLifetime
	}
	if c.PodDrainGrace == 0 {
		c.PodDrainGrace = DefaultPodDrainGrace
	}
	return nil
}

// sharedLocked reports whether the pod is bound to an issue or chat key and so
// keeps that binding between tasks. A pod bound to a single task is released
// the moment the task ends.
func (p *Pool) sharedLocked(pd *pod) bool {
	return pd.boundKey != "" && !strings.HasPrefix(pd.boundKey, "task:")
}

// forcedTask is a task cut off by its pod's expiry.
type forcedTask struct {
	ws *workspace
	id string
}

// expireSharedLocked releases shared pods whose time is up. A pod with no
// running task that has been idle too long is reset. A pod past its maximum
// lifetime stops taking tasks (it is draining), is reset once its tasks end,
// and has them failed when they outlast the grace period. Callers hold p.mu.
func (p *Pool) expireSharedLocked() {
	now := p.now()
	for _, pd := range p.pods {
		if pd.State != PodLeased || p.pods[pd.IP] != pd || pd.boundAt.IsZero() || !p.sharedLocked(pd) {
			continue
		}
		if !pd.draining && now.Sub(pd.boundAt) >= p.cfg.PodMaxLifetime {
			pd.draining = true
			// Tasks assigned but not picked up never started; place them again.
			p.waiting = append(p.waiting, pd.queue...)
			pd.queue = nil
			p.log.Info("pod reached its maximum lifetime; no new tasks", "pod_ip", pd.IP, "bound_since", pd.boundAt)
		}
		switch {
		case pd.draining && len(pd.leases) == 0:
			p.log.Info("pod released: maximum lifetime reached", "pod_ip", pd.IP)
			p.startResetLocked(pd)
		case pd.draining && now.Sub(pd.boundAt) >= p.cfg.PodMaxLifetime+p.cfg.PodDrainGrace:
			p.log.Warn("pod released: tasks outlasted the grace after the maximum lifetime", "pod_ip", pd.IP, "tasks", len(pd.leases))
			for _, l := range pd.leases {
				p.forced = append(p.forced, forcedTask{ws: l.ws, id: l.TaskID})
				p.endLeaseLocked(l, "pod lifetime exceeded")
			}
		case !pd.draining && p.idleLocked(pd) && !pd.claiming && now.Sub(pd.idleSince) >= p.cfg.PodIdleTimeout:
			p.log.Info("pod released: idle timeout", "pod_ip", pd.IP, "idle_since", pd.idleSince)
			p.startResetLocked(pd)
		}
	}
}

// idleLocked reports whether a bound pod has no task running or assigned, so
// it is kept only for the next task of its key.
func (p *Pool) idleLocked(pd *pod) bool {
	return pd.State == PodLeased && len(pd.leases) == 0 && len(pd.queue) == 0 && !pd.idleSince.IsZero()
}

// releaseIdleBoundLocked resets one idle bound pod so a waiting task of
// another key does not starve until the idle timeout. It does nothing while
// another pod is already being reset, whose return will serve the task.
func (p *Pool) releaseIdleBoundLocked() {
	pods := p.podsByIPLocked(nil)
	for _, pd := range pods {
		if pd.State == PodResetting {
			return
		}
	}
	for _, pd := range pods {
		if p.idleLocked(pd) && !pd.claiming {
			p.log.Info("idle bound pod released for a waiting task", "pod_ip", pd.IP)
			p.startResetLocked(pd)
			return
		}
	}
}

// reportForced fails upstream the tasks that expireSharedLocked cut off.
// Reporting needs the network, so callers invoke it without holding p.mu.
func (p *Pool) reportForced(ctx context.Context) {
	p.mu.Lock()
	ids := p.forced
	p.forced = nil
	p.mu.Unlock()
	for _, t := range ids {
		p.failUpstream(ctx, t.ws, t.id, "the pod reached its maximum lifetime before the task finished")
	}
}
