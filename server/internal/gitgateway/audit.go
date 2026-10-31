package gitgateway

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

const (
	AuditAllow = "allow"
	AuditDeny  = "deny"
)

// AuditRecord is one line of the gateway audit log. It carries no credentials:
// neither the task token nor the GitLab admin token is copied into it.
type AuditRecord struct {
	Time        time.Time `json:"time"`
	Action      string    `json:"action"`
	Decision    string    `json:"decision"`
	Reason      string    `json:"reason"`
	TaskID      string    `json:"task_id,omitempty"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	Initiator   string    `json:"initiator,omitempty"`
	Project     string    `json:"project,omitempty"`
	// Refs lists the refs a push asked to update.
	Refs []string `json:"refs,omitempty"`
}

type auditLog struct {
	mu  sync.Mutex
	w   io.Writer
	err func(error)
}

func (a *auditLog) write(rec AuditRecord) {
	if a == nil || a.w == nil {
		return
	}
	line, err := json.Marshal(rec)
	if err == nil {
		a.mu.Lock()
		_, err = a.w.Write(append(line, '\n'))
		a.mu.Unlock()
	}
	if err != nil && a.err != nil {
		a.err(err)
	}
}
