package sandboxbroker

import "context"

// DispatchOnce runs one active-dispatch round, as Run does on each tick.
func (p *Pool) DispatchOnce(ctx context.Context) { p.dispatchOnce(ctx) }
