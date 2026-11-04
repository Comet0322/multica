package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness"
	"github.com/multica-ai/multica/server/pkg/harness/model"
	"github.com/multica-ai/multica/server/pkg/harness/multicatools"
	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
	"github.com/multica-ai/multica/server/pkg/harness/tools"
)

// harnessBackend runs the agent loop on the host and does the work in a sandbox
// that offers only "run a command" and "move a file". Unlike every other
// backend in this package it starts no agent CLI: the model, the tools and the
// credentials all stay here, and the sandbox gets none of them.
//
// It is configured through Config.Env:
//
//	HARNESS_SANDBOX         "docker" (default) or "local" (tests; isolates nothing)
//	HARNESS_SANDBOX_IMAGE   image for the docker sandbox
//	HARNESS_MODEL           "mock" is the only model implemented so far
//	HARNESS_MOCK_SCRIPT     path to the mock's script (see loadMockScript)
//
// and the MULTICA_* values the daemon already computes for an agent's task.
type harnessBackend struct {
	cfg Config
}

const harnessWorkdir = "/work"

// harnessMaxSeedBytes bounds what is copied into the sandbox from the task's
// prepared working directory.
const harnessMaxSeedBytes = 32 << 20

var harnessIssueRE = regexp.MustCompile(`assigned issue ID is: ([0-9a-fA-F-]{36})`)

func (b *harnessBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	env := b.cfg.Env
	mdl, err := harnessModel(env)
	if err != nil {
		return nil, err
	}
	sb, workdir, err := harnessSandbox(ctx, env, opts)
	if err != nil {
		return nil, err
	}
	if opts.Cwd != "" && env["HARNESS_SANDBOX"] != "local" {
		if err := seedSandbox(ctx, sb, opts.Cwd, workdir); err != nil {
			_ = sb.Close(context.WithoutCancel(ctx))
			return nil, err
		}
	}

	reg := tools.NewRegistry(tools.Sandboxed{SB: sb, Workdir: workdir}.Tools()...)
	if m := harnessIssueRE.FindStringSubmatch(prompt); m != nil && env["MULTICA_TOKEN"] != "" {
		client := &multicatools.Client{
			BaseURL: env["MULTICA_SERVER_URL"], Token: env["MULTICA_TOKEN"], WorkspaceID: env["MULTICA_WORKSPACE_ID"],
			AgentID: env["MULTICA_AGENT_ID"], TaskID: env["MULTICA_TASK_ID"],
		}
		for _, t := range multicatools.Tools(client, multicatools.NewScope(m[1])) {
			reg.Add(t)
		}
	}

	runCtx, cancel := runContext(ctx, opts.Timeout)
	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)
	start := time.Now()

	go func() {
		defer cancel()
		out := harness.Run(runCtx, harness.Config{
			Model: mdl, Tools: reg, System: harnessSystemPrompt(opts), MaxTurns: opts.MaxTurns,
			Emit: func(e harness.Event) { trySend(msgCh, harnessMessage(e)) },
		}, prompt)
		close(msgCh)
		// The sandbox is gone before the result is delivered: a task that reports
		// completion must not leave a container behind for a daemon that then exits.
		if err := sb.Close(context.WithoutCancel(ctx)); err != nil {
			b.cfg.Logger.Warn("harness: closing the sandbox failed", "error", err)
		}

		res := Result{Status: out.Status, Output: out.Output, Error: out.Error, DurationMs: time.Since(start).Milliseconds(), Usage: map[string]TokenUsage{}}
		if out.Status == harness.StatusFailed && runCtx.Err() == context.DeadlineExceeded {
			res.Status = "timeout"
		}
		for name, u := range out.Usage {
			res.Usage[name] = TokenUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens}
		}
		resCh <- res
		close(resCh)
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

func harnessMessage(e harness.Event) Message {
	switch e.Kind {
	case harness.EventText:
		return Message{Type: MessageText, Content: e.Text}
	case harness.EventToolUse:
		return Message{Type: MessageToolUse, Tool: e.Tool, CallID: e.CallID, Input: e.Input}
	case harness.EventToolResult:
		return Message{Type: MessageToolResult, Tool: e.Tool, CallID: e.CallID, Output: e.Output}
	default:
		return Message{Type: MessageError, Content: e.Text}
	}
}

// harnessSystemPrompt is the Multica runtime brief. The daemon writes it into
// the workdir as CLAUDE.md or AGENTS.md for CLI agents; here it is read back and
// given to the model directly, because there is no CLI to discover the file.
func harnessSystemPrompt(opts ExecOptions) string {
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

func harnessSandbox(ctx context.Context, env map[string]string, opts ExecOptions) (sandbox.Sandbox, string, error) {
	switch kind := env["HARNESS_SANDBOX"]; kind {
	case "local":
		if opts.Cwd == "" {
			return nil, "", fmt.Errorf("harness: the local sandbox needs a working directory")
		}
		sb, err := sandbox.NewLocal(opts.Cwd)
		return sb, "/", err
	case "", "docker":
		sb, err := sandbox.NewDocker(ctx, sandbox.DockerOptions{Image: env["HARNESS_SANDBOX_IMAGE"], MemoryMB: 2048, Pids: 1024})
		return sb, harnessWorkdir, err
	default:
		return nil, "", fmt.Errorf("harness: unknown sandbox %q", kind)
	}
}

// seedSandbox copies the prepared working directory (the runtime brief, skill
// files, anything the daemon placed there) into the sandbox. Nothing flows
// back: the sandbox is disposable and results leave through the Multica tools.
func seedSandbox(ctx context.Context, sb sandbox.Sandbox, src, dst string) error {
	var total int64
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
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
		return sb.Put(ctx, dst+"/"+filepath.ToSlash(rel), data, info.Mode().Perm())
	})
}

func harnessModel(env map[string]string) (model.Model, error) {
	switch name := env["HARNESS_MODEL"]; name {
	case "mock":
		return loadMockScript(env["HARNESS_MOCK_SCRIPT"])
	case "":
		return nil, fmt.Errorf("harness: HARNESS_MODEL is not set")
	default:
		return nil, fmt.Errorf("harness: model %q is not implemented (only \"mock\")", name)
	}
}

// loadMockScript reads a JSON array of steps: {"say": "text"} ends the run with
// that answer; {"tool": "bash", "input": {...}} calls a tool.
func loadMockScript(path string) (model.Model, error) {
	if path == "" {
		return nil, fmt.Errorf("harness: HARNESS_MOCK_SCRIPT is not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("harness: mock script: %w", err)
	}
	var script []struct {
		Say   string         `json:"say"`
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal(raw, &script); err != nil {
		return nil, fmt.Errorf("harness: mock script: %w", err)
	}
	steps := make([]model.Step, 0, len(script))
	for i, s := range script {
		switch {
		case s.Tool != "":
			steps = append(steps, model.Call(fmt.Sprintf("call-%d", i), s.Tool, s.Input))
		case s.Say != "":
			steps = append(steps, model.Say(s.Say))
		default:
			return nil, fmt.Errorf("harness: mock script step %d has neither say nor tool", i)
		}
	}
	return model.NewMock(steps...), nil
}
