package gitgateway_test

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

func TestRevokingTheTaskTokenForgetsItsPushedBranches(t *testing.T) {
	h, tok := pushHarness(t)
	h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/x"))
	other := h.issue("t2", "alice@example.com")
	h.do("POST", pushPath, other, pushBody("PACK", "refs/heads/agent/t2/y"))

	h.tokens.Revoke("t1")

	if got := h.gw.PushedBranches("t1", project); len(got) != 0 {
		t.Fatalf("revoked task still has %v", got)
	}
	if got := h.gw.PushedBranches("t2", project); !reflect.DeepEqual(got, []string{"agent/t2/y"}) {
		t.Fatalf("revoking t1 touched t2: %v", got)
	}
}

func TestIssuingAgainDoesNotForgetTheTasksBranches(t *testing.T) {
	h, tok := pushHarness(t)
	h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/x"))
	h.issue("t1", "alice@example.com") // replaces the token, the task goes on
	if got := h.gw.PushedBranches("t1", project); len(got) != 1 {
		t.Fatalf("a re-issued token forgot the branches: %v", got)
	}
}

func TestForgetTaskDropsItsBranches(t *testing.T) {
	// For a gateway in its own process, which is not told about revocations.
	h, tok := pushHarness(t)
	h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/x"))
	h.gw.ForgetTask("t1")
	if got := h.gw.PushedBranches("t1", project); len(got) != 0 {
		t.Fatalf("forgotten task still has %v", got)
	}
}

func TestPushRecordsOfEndedTasksAgeOut(t *testing.T) {
	// A gateway in its own process is never told a token was revoked, so the
	// record is bounded by age as well.
	h := newHarness(t, func(c *gitgateway.Config) { c.PushLogTTL = time.Hour })
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelDeveloper)
	old := h.issue("t1", "alice@example.com")
	h.do("POST", pushPath, old, pushBody("PACK", "refs/heads/agent/t1/x"))

	h.clock.Advance(2 * time.Hour)
	fresh := h.issue("t2", "alice@example.com")
	h.do("POST", pushPath, fresh, pushBody("PACK", "refs/heads/agent/t2/y"))

	if got := h.gw.PushedBranches("t1", project); len(got) != 0 {
		t.Fatalf("a record older than the TTL survived: %v", got)
	}
	if got := h.gw.PushedBranches("t2", project); len(got) != 1 {
		t.Fatalf("a fresh record was dropped: %v", got)
	}
}

func TestPushLogHoldsAtMostAFixedNumberOfTasks(t *testing.T) {
	h := newHarness(t, func(c *gitgateway.Config) { c.PushLogMaxTasks = 3 })
	h.gitlab.addUser("alice@example.com", 7, "active", gitgateway.LevelDeveloper)
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("t%d", i)
		tok := h.issue(id, "alice@example.com")
		h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/"+id+"/x"))
		h.clock.Advance(time.Second)
	}
	var kept int
	for i := 1; i <= 5; i++ {
		if len(h.gw.PushedBranches(fmt.Sprintf("t%d", i), project)) > 0 {
			kept++
		}
	}
	if kept != 3 || len(h.gw.PushedBranches("t5", project)) == 0 || len(h.gw.PushedBranches("t1", project)) != 0 {
		t.Fatalf("want the 3 newest tasks kept, kept %d", kept)
	}
}
