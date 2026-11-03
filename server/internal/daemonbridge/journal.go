package daemonbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/multica-ai/multica/server/internal/daemon"
)

// Phase is how far a task got. A task is journaled before each step so a
// restarted bridge resumes it instead of orphaning it on the server.
type Phase string

const (
	PhaseClaimed  Phase = "claimed"  // claimed, /start not yet acknowledged
	PhaseRunning  Phase = "running"  // the backend run exists
	PhaseFinished Phase = "finished" // outcome known, not yet delivered
)

// Entry is the durable record of one task. It holds the task-scoped token, so
// the file is written 0600 and removed once the outcome is delivered.
type Entry struct {
	Task    daemon.Task `json:"task"`
	Phase   Phase       `json:"phase"`
	Handle  *Handle     `json:"handle,omitempty"`
	Cursor  string      `json:"cursor,omitempty"`
	NextSeq int         `json:"next_seq"`
	// Reported are event IDs already sent as usage, so a replay does not count
	// tokens twice.
	Reported []string `json:"reported,omitempty"`
	// Usage is the run's accumulated usage, sent once before the outcome.
	Usage     []Usage  `json:"usage,omitempty"`
	UsageSent bool     `json:"usage_sent,omitempty"`
	Outcome   *Outcome `json:"outcome,omitempty"`
	// Supplements is whether the server accepted mid-run input for this task.
	Supplements bool `json:"supplements,omitempty"`
}

// Journal stores entries as one file per task.
type Journal struct{ dir string }

// OpenJournal creates dir if needed.
func OpenJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Journal{dir: dir}, nil
}

func (j *Journal) path(taskID string) (string, error) {
	if taskID == "" || strings.ContainsAny(taskID, `/\.`) {
		return "", fmt.Errorf("invalid task id %q", taskID)
	}
	return filepath.Join(j.dir, taskID+".json"), nil
}

// Save writes the entry atomically.
func (j *Journal) Save(e *Entry) error {
	p, err := j.path(e.Task.ID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(j.dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Rename(name, p); err != nil {
		return err
	}
	if d, err := os.Open(j.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Remove deletes the entry; a missing one is fine.
func (j *Journal) Remove(taskID string) error {
	p, err := j.path(taskID)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Load returns every entry, ordered by task ID. Unreadable files are skipped
// and reported in errs.
func (j *Journal) Load() (entries []*Entry, errs []error) {
	files, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, []error{err}
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(j.dir, f.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var e Entry
		if err := json.Unmarshal(data, &e); err != nil || e.Task.ID == "" {
			errs = append(errs, fmt.Errorf("journal %s: unreadable", f.Name()))
			continue
		}
		entries = append(entries, &e)
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Task.ID < entries[b].Task.ID })
	return entries, errs
}
