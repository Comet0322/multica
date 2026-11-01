package sandboxmanager

import (
	"context"
	"time"
)

// DispatchOnce runs one active-dispatch round, as Run does on each tick.
func (p *Pool) DispatchOnce(ctx context.Context) { p.dispatchOnce(ctx) }

// ReconcileOnce runs one lease reconciliation pass, as the loop does on each
// tick.
func (p *Pool) ReconcileOnce(ctx context.Context) { p.reconcileOnce(ctx) }

// SetNow replaces the manager's clock. Call it before the pool serves anything.
func (p *Pool) SetNow(now func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.now = now
}

// SetLastMarkLost pretends the lost-pod check last ran at t (wall clock), to
// stage a manager that stalled.
func (p *Pool) SetLastMarkLost(t time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastMarkLost = t
}

// Touch runs the lost-pod check, as every request does.
func (p *Pool) Touch() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.markLostLocked()
}
