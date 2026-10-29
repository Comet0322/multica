package sandboxbroker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type taskState string

const (
	// taskQueued: claimed upstream, waiting for its sandbox daemon to claim it.
	taskQueued taskState = "queued"
	// taskDelivered: handed to the sandbox daemon, which has not started it.
	taskDelivered taskState = "delivered"
	// taskStarted: the sandbox daemon reported /start; the daemon owns it now.
	taskStarted taskState = "started"
)

// The journal lets a restarted broker keep serving running sandboxes and
// queued claims. It holds claim payloads (which contain task-scoped tokens)
// and sandbox credentials, so it is written 0600 and belongs on private
// persistent storage.
type journal struct {
	Binding string `json:"binding"`
	// Identity is what the image's daemon presented on registration. The
	// broker replays it upstream so the server builds claims for the exact
	// daemon version and capabilities that will execute them.
	Identity     *identity                `json:"identity,omitempty"`
	Registration json.RawMessage          `json:"registration,omitempty"`
	Runtimes     []runtimeRef             `json:"runtimes,omitempty"`
	Tasks        map[string]*task         `json:"tasks"`
	Sandboxes    map[string]*sandboxState `json:"sandboxes"`
}

type identity struct {
	Image    string            `json:"image"`
	Register json.RawMessage   `json:"register"`
	Headers  map[string]string `json:"headers"`
}

type runtimeRef struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

type task struct {
	ID          string          `json:"id"`
	Key         string          `json:"key"`
	RuntimeID   string          `json:"runtime_id"`
	Claim       json.RawMessage `json:"claim"`
	State       taskState       `json:"state"`
	ClaimedAt   time.Time       `json:"claimed_at"`
	LeaseAt     time.Time       `json:"lease_at,omitempty"`
	CheckedAt   time.Time       `json:"checked_at,omitempty"`
	FailureNote string          `json:"failure_note,omitempty"`
}

type sandboxState struct {
	Key   string `json:"key"`
	Token string `json:"token"`
	// Running is the broker's intent; the provider reports the actual state.
	Running   bool      `json:"running"`
	IdleSince time.Time `json:"idle_since"`
	// RequestedAt is when the broker last asked the provider to start it.
	RequestedAt time.Time `json:"requested_at,omitempty"`
	// Image is what the broker last started the sandbox with.
	Image string `json:"image,omitempty"`
	// The scope this sandbox serves decides when it is deleted.
	AgentID   string    `json:"agent_id,omitempty"`
	ScopeKind scopeKind `json:"scope_kind,omitempty"`
	ScopeID   string    `json:"scope_id,omitempty"`
	// LastUsed is when a task was last claimed for or released by it.
	LastUsed         time.Time `json:"last_used,omitempty"`
	CleanupCheckedAt time.Time `json:"cleanup_checked_at,omitempty"`
}

func newJournal(binding string) *journal {
	return &journal{Binding: binding, Tasks: map[string]*task{}, Sandboxes: map[string]*sandboxState{}}
}

func loadJournal(path, binding string) (*journal, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return newJournal(binding), nil
	}
	if err != nil {
		return nil, err
	}
	var j journal
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("decode broker journal: %w", err)
	}
	if j.Binding != binding {
		return nil, fmt.Errorf("broker journal belongs to another workspace or server; use a separate state directory")
	}
	if j.Tasks == nil {
		j.Tasks = map[string]*task{}
	}
	if j.Sandboxes == nil {
		j.Sandboxes = map[string]*sandboxState{}
	}
	return &j, nil
}

func saveJournal(path string, j *journal) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".journal-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
