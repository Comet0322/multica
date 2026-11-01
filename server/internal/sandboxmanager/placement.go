package sandboxmanager

// claimedTaskRef is the part of a claimed task the pool reads: which task it
// is, where it comes from and who started it.
type claimedTaskRef struct {
	ID             string `json:"id"`
	AgentID        string `json:"agent_id"`
	RuntimeID      string `json:"runtime_id"`
	WorkspaceID    string `json:"workspace_id"`
	IssueID        string `json:"issue_id"`
	ChatSessionID  string `json:"chat_session_id"`
	AutopilotRunID string `json:"autopilot_run_id"`
	InitiatorEmail string `json:"initiator_email"`
}
