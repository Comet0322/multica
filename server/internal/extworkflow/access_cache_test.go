package extworkflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

type countingAccess struct {
	calls int
	ok    bool
	err   error
}

func (c *countingAccess) CanInvokeAgent(context.Context, pgtype.UUID, string, pgtype.UUID, pgtype.UUID) (bool, error) {
	c.calls++
	return c.ok, c.err
}

func TestAccessCacheTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	acc := &countingAccess{ok: true}
	e := NewEngine(Deps{Access: acc, Now: func() time.Time { return now }})
	ws, actor, agent := util.MustParseUUID("00000000-0000-0000-0000-000000000001"), util.MustParseUUID("00000000-0000-0000-0000-000000000002"), util.MustParseUUID("00000000-0000-0000-0000-000000000003")
	check := func() bool {
		ok, err := e.canInvoke(context.Background(), ws, "member", actor, agent)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !check() || !check() || acc.calls != 1 {
		t.Fatalf("within the TTL: calls = %d, want 1", acc.calls)
	}
	now = now.Add(defaultAccessCacheTTL + time.Second)
	acc.ok = false
	if check() || acc.calls != 2 {
		t.Fatalf("after the TTL: calls = %d, want a fresh check", acc.calls)
	}
	if len(e.cache.entries) != 1 {
		t.Fatalf("expired entries not pruned: %d", len(e.cache.entries))
	}
}

func TestAccessCacheDoesNotCacheErrors(t *testing.T) {
	acc := &countingAccess{err: errors.New("db down")}
	e := NewEngine(Deps{Access: acc})
	id := util.MustParseUUID("00000000-0000-0000-0000-000000000001")
	for i := 0; i < 2; i++ {
		if _, err := e.canInvoke(context.Background(), id, "member", id, id); err == nil {
			t.Fatal("want error")
		}
	}
	if acc.calls != 2 {
		t.Fatalf("calls = %d, want 2", acc.calls)
	}
	acc.err, acc.ok = nil, true
	if ok, err := e.canInvoke(context.Background(), id, "member", id, id); err != nil || !ok || acc.calls != 3 {
		t.Fatalf("recovered: ok=%v err=%v calls=%d", ok, err, acc.calls)
	}
}
