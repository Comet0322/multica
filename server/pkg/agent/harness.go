package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness/agentsvc"
)

// harnessBackend hands the task to the agent service and relays what comes
// back. It starts no agent CLI and holds no model key, git credential or
// sandbox: those belong to the service, which runs the harness loop and gives
// the model a sandbox that offers only "run a command" and "move a file".
//
// It is configured through Config.Env:
//
//	HARNESS_AGENT_URL    base URL of the agent service (required)
//	HARNESS_AGENT_TOKEN  its bearer token (required)
//
// and the MULTICA_* values the daemon already computes for an agent's task,
// which are passed on so the service can act on the task's behalf.
type harnessBackend struct {
	cfg Config
}

// harnessMaxSeedBytes bounds what is sent from the task's prepared working
// directory.
const harnessMaxSeedBytes = 32 << 20

func (b *harnessBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	env := b.cfg.Env
	if env["HARNESS_AGENT_URL"] == "" || env["HARNESS_AGENT_TOKEN"] == "" {
		return nil, errors.New("harness: HARNESS_AGENT_URL and HARNESS_AGENT_TOKEN are not set")
	}
	req := agentsvc.RunRequest{
		Prompt: prompt, SystemPrompt: harnessBrief(opts), Model: opts.Model, MaxTurns: opts.MaxTurns,
		TimeoutSec: int((opts.Timeout + time.Second - 1) / time.Second), Env: map[string]string{},
	}
	for _, k := range agentsvc.TaskEnvKeys {
		if v := env[k]; v != "" {
			req.Env[k] = v
		}
	}
	if opts.Cwd != "" {
		files, err := seedFiles(opts.Cwd)
		if err != nil {
			return nil, err
		}
		req.Files = files
	}

	client := &agentsvc.Client{BaseURL: env["HARNESS_AGENT_URL"], Token: env["HARNESS_AGENT_TOKEN"]}
	runCtx, cancel := runContext(ctx, opts.Timeout)
	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)
	start := time.Now()

	go func() {
		defer cancel()
		res, err := client.Run(runCtx, req, func(e agentsvc.Event) {
			switch e.Type {
			case agentsvc.EventMessage:
				if e.Message != nil {
					trySend(msgCh, harnessMessage(e.Message))
				}
			case agentsvc.EventHeartbeat:
				// Counts as activity for the daemon's idle watchdog, shows nothing.
				trySend(msgCh, Message{Type: MessageStatus})
			}
		})
		close(msgCh)

		out := Result{Usage: map[string]TokenUsage{}, DurationMs: time.Since(start).Milliseconds()}
		switch {
		case err != nil:
			out.Status, out.Error = "failed", err.Error()
			switch {
			case errors.Is(runCtx.Err(), context.DeadlineExceeded):
				out.Status = "timeout"
			case ctx.Err() != nil:
				out.Status = "cancelled"
			}
		default:
			out.Status, out.Output, out.Error, out.DurationMs = res.Status, res.Output, res.Error, res.DurationMs
			for name, u := range res.Usage {
				out.Usage[name] = TokenUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens}
			}
		}
		resCh <- out
		close(resCh)
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

func harnessMessage(m *agentsvc.Message) Message {
	switch m.Type {
	case "text":
		return Message{Type: MessageText, Content: m.Content}
	case "tool-use":
		return Message{Type: MessageToolUse, Tool: m.Tool, CallID: m.CallID, Input: m.Input}
	case "tool-result":
		return Message{Type: MessageToolResult, Tool: m.Tool, CallID: m.CallID, Output: m.Output}
	default:
		return Message{Type: MessageError, Content: m.Content}
	}
}

// harnessBrief is the Multica runtime brief. The daemon writes it into the
// workdir as CLAUDE.md or AGENTS.md for CLI agents; here it is read back and
// sent to the service, because there is no CLI to discover the file.
func harnessBrief(opts ExecOptions) string {
	parts := []string{}
	if opts.SystemPrompt != "" {
		parts = append(parts, opts.SystemPrompt)
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		if opts.Cwd == "" {
			break
		}
		if b, err := os.ReadFile(filepath.Join(opts.Cwd, name)); err == nil && len(b) > 0 {
			parts = append(parts, string(b))
			break
		}
	}
	return strings.Join(parts, "\n\n")
}

// seedFiles reads the prepared working directory (the runtime brief, skill
// files, anything the daemon placed there) to be placed in the sandbox. Nothing
// flows back: the sandbox is disposable and results leave through the Multica
// tools.
func seedFiles(src string) ([]agentsvc.File, error) {
	var files []agentsvc.File
	var total int64
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		if total += info.Size(); total > harnessMaxSeedBytes {
			return fmt.Errorf("harness: working directory is larger than %d MB", harnessMaxSeedBytes>>20)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		files = append(files, agentsvc.File{Path: filepath.ToSlash(rel), Mode: uint32(info.Mode().Perm()), Data: data})
		return nil
	})
	return files, err
}
