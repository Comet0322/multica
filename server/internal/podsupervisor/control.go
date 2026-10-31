package podsupervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const commandReset = "reset"

// Command is the body the control URL returns to a poll.
type Command struct {
	ID      string `json:"id"`
	Command string `json:"command"`
}

// Report is what the supervisor POSTs back once a command is finished.
type Report struct {
	ID     string `json:"id"`
	Status string `json:"status"` // "done" or "failed"
	Error  string `json:"error,omitempty"`
}

// poll fetches commands until ctx ends. A reset is handed to the main loop,
// which owns the daemon; the outcome is then reported back.
func (s *Supervisor) poll(ctx context.Context) {
	var last *Report // outcome of the most recently handled command
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cmd, err := s.fetchCommand(ctx)
		if err != nil {
			s.log.Warn("poll control url failed", "error", err)
			continue
		}
		if cmd == nil || cmd.Command != commandReset || cmd.ID == "" {
			continue
		}
		if last == nil || last.ID != cmd.ID {
			report := Report{ID: cmd.ID, Status: "done"}
			if err := s.requestReset(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				s.log.Error("reset failed", "id", cmd.ID, "error", err)
				report.Status, report.Error = "failed", err.Error()
			}
			last = &report
		}
		if err := s.sendReport(ctx, *last); err != nil {
			s.log.Warn("report reset outcome failed, will retry", "id", last.ID, "error", err)
		}
	}
}

func (s *Supervisor) requestReset(ctx context.Context) error {
	req := resetRequest{reply: make(chan error, 1)}
	select {
	case s.resets <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Supervisor) fetchCommand(ctx context.Context) (*Command, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.ControlURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("control url returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var cmd Command
	if err := json.Unmarshal(body, &cmd); err != nil {
		return nil, fmt.Errorf("decode command: %w", err)
	}
	return &cmd, nil
}

func (s *Supervisor) sendReport(ctx context.Context, r Report) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.ControlURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return errors.New("control url rejected report: " + resp.Status)
	}
	return nil
}
