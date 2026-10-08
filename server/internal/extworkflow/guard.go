package extworkflow

import "github.com/multica-ai/multica/server/internal/issuestatus"

// The agent write guard (spec §6.5). The engine reads a run's state from its
// issues: a step child moved to done or in_review finishes the step, a
// cancelled or deleted child skips it, and a cancelled, reassigned or deleted
// parent cancels the run. An agent with ordinary issue powers could therefore
// steer a run around the decision protocol, so the HTTP layer refuses agent
// writes to those fields while the run is active. People are not guarded, and
// the engine writes through queries, not the handlers.

// IssueGuard is what the guard knows about one issue and the acting task.
type IssueGuard struct {
	// ActiveParent: the issue is the parent of a running or waiting_human run.
	ActiveParent bool
	// ActiveChild: the issue is a step's child issue in such a run.
	ActiveChild bool
	// OwnStepTask: the acting task is a live step task of that child's step.
	OwnStepTask bool
}

// IssueChange is what an agent's write would change on the issue.
type IssueChange struct {
	// Status is the new status's effective behavior, "" when unchanged.
	Status   string
	Assignee bool
	Parent   bool
	Delete   bool
}

// Any reports whether the write touches a guarded field.
func (c IssueChange) Any() bool {
	return c.Status != "" || c.Assignee || c.Parent || c.Delete
}

// AgentIssueRefusal returns why an agent may not make change, or "" when it
// may. The message tells the agent what to do instead.
func AgentIssueRefusal(g IssueGuard, c IssueChange) string {
	if !c.Any() {
		return ""
	}
	switch {
	case g.ActiveParent:
		return "this issue is the parent of an active workflow run, and the workflow engine sets its status: agents cannot change its status, assignee or parent issue, or delete it. Comment on the issue instead; a person can change or cancel the run."
	case !g.ActiveChild:
		return ""
	case !g.OwnStepTask:
		return "this issue is a step of an active workflow run, and the workflow engine runs it: an agent may change only the status of its own step's issue, while it works on that step. A supervisor decides with a decision block in a comment; anything else needs a person."
	case c.Assignee || c.Parent || c.Delete:
		return "this issue is your workflow step: you may change only its status, moving it to done (or in_review) when you finish. Its assignee and parent issue belong to the workflow engine, and it cannot be deleted."
	case c.Status == issuestatus.Cancelled:
		return "this issue is your workflow step: cancelling it would skip the step, which only the supervisor or a person may decide. Move it to done (or in_review) when you finish, or use request-rewind if an upstream result makes the step infeasible."
	}
	return ""
}

// IsDecisionTurn reports whether a task with this role and kind is a
// supervisor turn that answers with a decision block on a step's child issue.
func IsDecisionTurn(role, kind string) bool {
	if role != RoleSupervisor {
		return false
	}
	switch kind {
	case KindReview, KindFailure, KindRewindRequest:
		return true
	}
	return false
}

// AgentCommentRefusal returns why an agent's task (role, kind) may not comment
// on an issue, or "" when it may. onRunParent: the issue is the parent of the
// task's run. A decision turn's only comment is its decision block on the
// step's issue; the engine records the run's progress on the parent, so a
// note there only notifies the run's trigger member again. stepIssue is the
// step's issue id, "" when unknown.
func AgentCommentRefusal(role, kind string, onRunParent bool, stepIssue string) string {
	if !onRunParent || !IsDecisionTurn(role, kind) {
		return ""
	}
	where := "the step's issue"
	if stepIssue != "" {
		where += " (" + stepIssue + ")"
	}
	return "this issue is the parent of the workflow run you are deciding on, and the workflow engine records the run's progress here: do not comment on it in this turn. Post your decision block as one comment on " + where + " instead."
}
