package extworkflow

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// defaultAccessCacheTTL bounds how stale an invoke-gate answer may be. Active
// runs are re-evaluated on every advance and reconcile tick, and the gate costs
// several queries per agent.
const defaultAccessCacheTTL = 30 * time.Second

type accessKey struct {
	workspace pgtype.UUID
	actorType string
	actor     pgtype.UUID
	agent     pgtype.UUID
}

// accessCache memoizes AgentAccess answers briefly. Errors are never cached.
type accessCache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[accessKey]accessEntry
}

type accessEntry struct {
	ok      bool
	expires time.Time
}

func newAccessCache(ttl time.Duration, now func() time.Time) *accessCache {
	if now == nil {
		now = time.Now
	}
	return &accessCache{ttl: ttl, now: now, entries: map[accessKey]accessEntry{}}
}

func (c *accessCache) get(k accessKey) (ok, hit bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, found := c.entries[k]
	if !found || !c.now().Before(ent.expires) {
		return false, false
	}
	return ent.ok, true
}

func (c *accessCache) put(k accessKey, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for key, ent := range c.entries { // prune on insert keeps the map bounded
		if !now.Before(ent.expires) {
			delete(c.entries, key)
		}
	}
	c.entries[k] = accessEntry{ok: ok, expires: now.Add(c.ttl)}
}

// canInvoke is access.CanInvokeAgent through the cache. A non-positive TTL
// disables caching.
func (e *Engine) canInvoke(ctx context.Context, workspaceID pgtype.UUID, actorType string, actorID, agentID pgtype.UUID) (bool, error) {
	if e.cache == nil {
		return e.access.CanInvokeAgent(ctx, workspaceID, actorType, actorID, agentID)
	}
	k := accessKey{workspaceID, actorType, actorID, agentID}
	if ok, hit := e.cache.get(k); hit {
		return ok, nil
	}
	ok, err := e.access.CanInvokeAgent(ctx, workspaceID, actorType, actorID, agentID)
	if err != nil {
		return false, err
	}
	e.cache.put(k, ok)
	return ok, nil
}
