package sandboxbroker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/sandboxprovider"
)

// Isolation decides which tasks share a sandbox, and with it a filesystem,
// processes and agent session store.
type Isolation string

const (
	// IsolationIssue gives each agent one sandbox per issue (or chat session,
	// or autopilot run), matching the scope in which Multica resumes an
	// agent's session and workdir.
	IsolationIssue Isolation = "issue"
	// IsolationTask gives every task a fresh sandbox, deleted when it ends.
	IsolationTask Isolation = "task"
	// IsolationAgent shares one sandbox across all of an agent's tasks, like a
	// local daemon. Tasks of the same agent can see each other's files.
	IsolationAgent Isolation = "agent"
)

type scopeKind string

const (
	scopeAgent     scopeKind = "agent"
	scopeIssue     scopeKind = "issue"
	scopeChat      scopeKind = "chat"
	scopeAutopilot scopeKind = "autopilot"
	scopeTask      scopeKind = "task"
)

type claimedTaskRef struct {
	ID             string `json:"id"`
	AgentID        string `json:"agent_id"`
	RuntimeID      string `json:"runtime_id"`
	WorkspaceID    string `json:"workspace_id"`
	IssueID        string `json:"issue_id"`
	ChatSessionID  string `json:"chat_session_id"`
	AutopilotRunID string `json:"autopilot_run_id"`
}

// placement is the sandbox a task runs in and the scope that sandbox serves,
// which decides when its volume may be deleted.
type placement struct {
	Key     string
	AgentID string
	Kind    scopeKind
	ScopeID string
}

func place(isolation Isolation, ref claimedTaskRef) (placement, error) {
	agent := strings.ToLower(strings.TrimSpace(ref.AgentID))
	if agent == "" {
		return placement{}, fmt.Errorf("claimed task has no agent")
	}
	p := placement{AgentID: agent}
	switch isolation {
	case IsolationAgent:
		p.Kind, p.ScopeID = scopeAgent, agent
		p.Key = "agent-" + agent
		if !sandboxprovider.ValidKey(p.Key) {
			return placement{}, fmt.Errorf("invalid agent ID")
		}
		return p, nil
	case IsolationTask:
		p.Kind, p.ScopeID = scopeTask, ref.ID
	default:
		switch {
		case ref.IssueID != "":
			p.Kind, p.ScopeID = scopeIssue, ref.IssueID
		case ref.ChatSessionID != "":
			p.Kind, p.ScopeID = scopeChat, ref.ChatSessionID
		case ref.AutopilotRunID != "":
			p.Kind, p.ScopeID = scopeAutopilot, ref.AutopilotRunID
		default:
			// Nothing to resume across, e.g. quick-create tasks.
			p.Kind, p.ScopeID = scopeTask, ref.ID
		}
	}
	if p.ScopeID == "" {
		return placement{}, fmt.Errorf("claimed task has no ID")
	}
	sum := sha256.Sum256([]byte(agent + "\x00" + string(p.Kind) + "\x00" + p.ScopeID))
	p.Key = string(p.Kind) + "-" + hex.EncodeToString(sum[:12])
	return p, nil
}
