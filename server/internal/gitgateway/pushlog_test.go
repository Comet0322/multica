package gitgateway_test

import (
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

func TestPushAllowedFromDeveloperUpward(t *testing.T) {
	cases := map[string]struct {
		level int
		want  int
	}{
		"guest":      {10, 0},
		"reporter":   {gitgateway.LevelReporter, 0},
		"developer":  {gitgateway.LevelDeveloper, 1},
		"maintainer": {40, 1},
		"owner":      {50, 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.gitlab.addUser("alice@example.com", 7, "active", c.level)
			tok := h.issue("t1", "alice@example.com")
			h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/x"))
			if n := len(h.gitlab.pushes()); n != c.want {
				t.Fatalf("level %d: %d pushes reached GitLab, want %d", c.level, n, c.want)
			}
		})
	}
}

func TestSuccessfulPushIsRecordedForTheTask(t *testing.T) {
	h, tok := pushHarness(t)
	if got := h.gw.PushedBranches("t1", project); len(got) != 0 {
		t.Fatalf("before any push: %v", got)
	}
	h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/b", "refs/heads/agent/t1/a/nested"))
	want := []string{"agent/t1/a/nested", "agent/t1/b"}
	if got := h.gw.PushedBranches("t1", project); !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
}

func TestDeletedBranchIsNoLongerRecorded(t *testing.T) {
	h, tok := pushHarness(t)
	h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/x"))
	del := pkt(oldSHA+" "+zeroSHA+" refs/heads/agent/t1/x\x00report-status side-band-64k\n") + "0000"
	h.do("POST", pushPath, tok, del)
	if got := h.gw.PushedBranches("t1", project); len(got) != 0 {
		t.Fatalf("deleted branch still recorded: %v", got)
	}
}

func TestOnlyAcceptedRefsAreRecorded(t *testing.T) {
	t.Run("ref rejected by GitLab", func(t *testing.T) {
		h, tok := pushHarness(t)
		h.gitlab.setPushOutcome(0, "refs/heads/agent/t1/bad")
		h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/good", "refs/heads/agent/t1/bad"))
		if got := h.gw.PushedBranches("t1", project); !reflect.DeepEqual(got, []string{"agent/t1/good"}) {
			t.Fatalf("recorded %v, want only the accepted ref", got)
		}
	})
	t.Run("GitLab error", func(t *testing.T) {
		h, tok := pushHarness(t)
		h.gitlab.setPushOutcome(500, "")
		h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/x"))
		if got := h.gw.PushedBranches("t1", project); len(got) != 0 {
			t.Fatalf("failed push recorded: %v", got)
		}
	})
	t.Run("refused by the gateway: outside namespace", func(t *testing.T) {
		h, tok := pushHarness(t)
		h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/ok", "refs/heads/main"))
		if got := h.gw.PushedBranches("t1", project); len(got) != 0 {
			t.Fatalf("refused push recorded: %v", got)
		}
	})
	t.Run("refused by the gateway: below Developer", func(t *testing.T) {
		h, tok := pushHarness(t)
		h.gitlab.setLevel(7, gitgateway.LevelReporter)
		h.do("POST", pushPath, tok, pushBody("PACK", "refs/heads/agent/t1/x"))
		if got := h.gw.PushedBranches("t1", project); len(got) != 0 {
			t.Fatalf("refused push recorded: %v", got)
		}
	})
}

func TestPushRecordBelongsToOneTask(t *testing.T) {
	h, tok1 := pushHarness(t)
	tok2 := h.issue("t2", "alice@example.com")
	h.do("POST", pushPath, tok1, pushBody("PACK", "refs/heads/agent/t1/x"))
	h.do("POST", pushPath, tok2, pushBody("PACK", "refs/heads/agent/t2/y"))

	if got := h.gw.PushedBranches("t1", project); !reflect.DeepEqual(got, []string{"agent/t1/x"}) {
		t.Fatalf("t1 sees %v", got)
	}
	if got := h.gw.PushedBranches("t2", project); !reflect.DeepEqual(got, []string{"agent/t2/y"}) {
		t.Fatalf("t2 sees %v", got)
	}
	if got := h.gw.PushedBranches("t3", project); len(got) != 0 {
		t.Fatalf("an unrelated task sees %v", got)
	}
	if got := h.gw.PushedBranches("t1", "group/other"); len(got) != 0 {
		t.Fatalf("another project sees %v", got)
	}
}
