package extworkflow

import (
	"strings"
	"testing"
)

func TestAgentIssueRefusal(t *testing.T) {
	parent := IssueGuard{ActiveParent: true}
	child := IssueGuard{ActiveChild: true}
	own := IssueGuard{ActiveChild: true, OwnStepTask: true}
	cases := []struct {
		name   string
		guard  IssueGuard
		change IssueChange
		want   string // substring of the refusal; "" means allowed
	}{
		{"plain issue", IssueGuard{}, IssueChange{Status: "cancelled", Assignee: true, Parent: true, Delete: true}, ""},
		{"nothing guarded changes", parent, IssueChange{}, ""},
		{"parent status", parent, IssueChange{Status: "done"}, "parent of an active workflow run"},
		{"parent assignee", parent, IssueChange{Assignee: true}, "parent of an active workflow run"},
		{"parent parent", parent, IssueChange{Parent: true}, "parent of an active workflow run"},
		{"parent delete", parent, IssueChange{Delete: true}, "parent of an active workflow run"},
		{"other step status", child, IssueChange{Status: "done"}, "only the status of its own step"},
		{"other step delete", child, IssueChange{Delete: true}, "only the status of its own step"},
		{"own step done", own, IssueChange{Status: "done"}, ""},
		{"own step in progress", own, IssueChange{Status: "in_progress"}, ""},
		{"own step cancel", own, IssueChange{Status: "cancelled"}, "skip the step"},
		{"own step assignee", own, IssueChange{Assignee: true}, "only its status"},
		{"own step parent", own, IssueChange{Parent: true}, "only its status"},
		{"own step delete", own, IssueChange{Delete: true}, "only its status"},
		{"own step status and assignee", own, IssueChange{Status: "done", Assignee: true}, "only its status"},
	}
	for _, tc := range cases {
		got := AgentIssueRefusal(tc.guard, tc.change)
		if tc.want == "" {
			if got != "" {
				t.Errorf("%s: refused: %s", tc.name, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: refusal %q lacks %q", tc.name, got, tc.want)
		}
	}
}

func TestAgentCommentRefusal(t *testing.T) {
	cases := []struct {
		name        string
		role, kind  string
		onRunParent bool
		want        string // substring of the refusal; "" means allowed
	}{
		{"review on parent", RoleSupervisor, KindReview, true, "step-issue"},
		{"failure on parent", RoleSupervisor, KindFailure, true, "decision block"},
		{"rewind request on parent", RoleSupervisor, KindRewindRequest, true, "records the run's progress"},
		{"review on the step", RoleSupervisor, KindReview, false, ""},
		{"summary on parent", RoleSupervisor, KindSummary, true, ""},
		{"conversation on parent", RoleSupervisor, KindConversation, true, ""},
		{"step agent on parent", RoleStep, KindStep, true, ""},
		{"not a workflow task", "", "", true, ""},
	}
	for _, c := range cases {
		got := AgentCommentRefusal(c.role, c.kind, c.onRunParent, "step-issue")
		if (got == "") != (c.want == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: refusal = %q, want %q", c.name, got, c.want)
		}
	}
	if got := AgentCommentRefusal(RoleSupervisor, KindReview, true, ""); got == "" || strings.Contains(got, "()") {
		t.Errorf("without a step issue: refusal = %q", got)
	}
}

func TestIsDecisionTurn(t *testing.T) {
	for _, kind := range []string{KindReview, KindFailure, KindRewindRequest} {
		if !IsDecisionTurn(RoleSupervisor, kind) {
			t.Errorf("%s is a decision turn", kind)
		}
	}
	for _, c := range [][2]string{{RoleSupervisor, KindSummary}, {RoleSupervisor, KindConversation}, {RoleStep, KindStep}, {"", KindReview}} {
		if IsDecisionTurn(c[0], c[1]) {
			t.Errorf("%v is not a decision turn", c)
		}
	}
}
