package sandboxmanager

import (
	"context"
	"encoding/json"
)

// issueTaskKey issues the claimed task's LiteLLM key and returns the task body
// with it injected. The key is revoked again if injection fails.
func (p *Pool) issueTaskKey(ctx context.Context, raw json.RawMessage, ref claimedTaskRef) (json.RawMessage, *taskKey, error) {
	key, err := p.llm.issue(ctx, ref, ref.initiator())
	if err != nil {
		return nil, nil, err
	}
	out, err := p.llm.inject(raw, key)
	if err != nil {
		p.revokeKey(&key, ref.ID)
		return nil, nil, err
	}
	return out, &key, nil
}

// revokeKey deletes a task's key in the background. A nil key is ignored.
func (p *Pool) revokeKey(key *taskKey, taskID string) {
	if key == nil || p.llm == nil {
		return
	}
	p.llm.revokeAsync(*key, func(err error) {
		p.log.Error("revoke task key; it expires on its own", "task_id", taskID, "error", err)
	})
}
