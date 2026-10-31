// Package gitgateway is the central service that holds the GitLab admin token
// and lets sandbox pods reach git only through it. A request is allowed when
// the human who started the task has enough access in GitLab and the request
// is on the gateway's small allow list; everything else is refused.
//
// This file covers task identity: the manager issues one opaque token per
// task, and the gateway asks the manager who the token belongs to.
package gitgateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Identity is what the manager knows about the task behind a gateway token.
type Identity struct {
	TaskID      string `json:"task_id"`
	WorkspaceID string `json:"workspace_id"`
	// Initiator is the email of the human the task runs for.
	Initiator string `json:"initiator"`
}

// ErrUnknownToken means the token was never issued or its task has ended.
var ErrUnknownToken = errors.New("gitgateway: unknown or revoked token")

// Resolver is the gateway's view of the manager. Any error other than
// ErrUnknownToken means the manager could not answer, and the gateway refuses.
type Resolver interface {
	Resolve(ctx context.Context, token string) (Identity, error)
}

// TokenRegistry is the manager-side table of live gateway tokens. The manager
// issues a token when it hands a task to a pod and revokes it when the lease
// ends. Tokens are opaque random strings that mean nothing outside the
// gateway.
type TokenRegistry struct {
	mu     sync.Mutex
	tokens map[string]Identity
	byTask map[string]string
	// onRevoke holds the functions to call after a token is revoked.
	onRevoke []func(taskID string)
}

// maxReplyBytes bounds how much of the manager's reply the gateway reads.
const maxReplyBytes = 1 << 20

func NewTokenRegistry() *TokenRegistry {
	return &TokenRegistry{tokens: map[string]Identity{}, byTask: map[string]string{}}
}

// Issue returns a fresh token for the task. A task keeps a single live token:
// issuing again revokes the earlier one.
func (r *TokenRegistry) Issue(id Identity) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gitgateway: generate token: %w", err)
	}
	token := "gwt_" + base64.RawURLEncoding.EncodeToString(raw)
	id.Initiator = strings.ToLower(strings.TrimSpace(id.Initiator))
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.byTask[id.TaskID]; ok {
		delete(r.tokens, old)
	}
	r.tokens[token] = id
	r.byTask[id.TaskID] = token
	return token, nil
}

// Revoke ends the task's token and tells the revoke hooks. It is safe to call
// for unknown tasks.
func (r *TokenRegistry) Revoke(taskID string) {
	r.mu.Lock()
	token, ok := r.byTask[taskID]
	if ok {
		delete(r.tokens, token)
		delete(r.byTask, taskID)
	}
	hooks := r.onRevoke
	r.mu.Unlock()
	if ok {
		for _, hook := range hooks {
			hook(taskID)
		}
	}
}

// OnRevoke registers a function called with the task ID whenever Revoke ends
// a live token, so state kept per task (the gateway's record of pushed
// branches) is dropped with it. Issuing a replacement token does not call it.
func (r *TokenRegistry) OnRevoke(hook func(taskID string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRevoke = append(r.onRevoke, hook)
}

// Resolve implements Resolver for in-process use.
func (r *TokenRegistry) Resolve(_ context.Context, token string) (Identity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.tokens[token]
	if !ok {
		return Identity{}, ErrUnknownToken
	}
	return id, nil
}

// VerifyHandler serves the endpoint the gateway calls: POST with
// {"token": "..."} answers 200 with the Identity, or 404 for a token that is
// not live. Expose it to the gateway only.
func (r *TokenRegistry) VerifyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		id, err := r.Resolve(req.Context(), body.Token)
		if err != nil {
			http.Error(w, "unknown token", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(id)
	})
}

// HTTPResolver asks a manager's VerifyHandler who a token belongs to.
type HTTPResolver struct {
	URL    string
	Client *http.Client
}

func (h HTTPResolver) Resolve(ctx context.Context, token string) (Identity, error) {
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	payload, _ := json.Marshal(map[string]string{"token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(payload))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Identity{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Identity{}, ErrUnknownToken
	case resp.StatusCode != http.StatusOK:
		return Identity{}, fmt.Errorf("gitgateway: manager answered %d", resp.StatusCode)
	}
	var id Identity
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReplyBytes)).Decode(&id); err != nil {
		return Identity{}, fmt.Errorf("gitgateway: manager reply: %w", err)
	}
	if id.TaskID == "" {
		return Identity{}, errors.New("gitgateway: manager reply has no task")
	}
	return id, nil
}
