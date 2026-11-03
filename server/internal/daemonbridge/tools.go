package daemonbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
)

// CLIToolName is the tool a run uses to act on Multica.
const CLIToolName = "multica"

// CLITool lets a run use the real multica CLI as the task's agent. The brief
// the daemon renders already documents every command, so running the actual
// binary gives the run the full surface (issues, comments, status, projects,
// labels, autopilots, squads, attachments, ...) without the bridge
// reimplementing any of it, and keeps behaviour identical to a local daemon.
//
// The binary runs with the task-scoped token only, in a throwaway directory
// and config root, and the commands that manage the local install or its
// credentials are refused.
type CLITool struct {
	// Binary is the multica executable.
	Binary string
	// ServerURL is the Multica server the CLI talks to.
	ServerURL string
	// Timeout bounds one invocation; zero means five minutes.
	Timeout time.Duration
	// MaxOutput caps the output returned to the run; zero means 64 KiB.
	MaxOutput int
}

var cliDenied = map[string]bool{
	"daemon": true, "auth": true, "login": true, "setup": true,
	"config": true, "update": true, "runtime": true,
}

var cliDeniedFlags = []string{"--token", "--server-url", "--profile", "--config"}

type cliArgs struct {
	Args  []string `json:"args"`
	Stdin string   `json:"stdin,omitempty"`
}

// Specs implements ToolRunner.
func (t *CLITool) Specs(daemon.Task, Kind) []ToolSpec {
	return []ToolSpec{{
		Name: CLIToolName,
		Description: "Run the multica CLI as this task's agent. Pass the arguments after `multica`, " +
			"for example [\"issue\",\"get\",\"<id>\",\"--output\",\"json\"]. Use stdin for long text " +
			"flags that read from standard input.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"args":{"type":"array","items":{"type":"string"},"description":"arguments after multica"},` +
			`"stdin":{"type":"string","description":"text piped to standard input"}},"required":["args"]}`),
	}}
}

// Run implements ToolRunner.
func (t *CLITool) Run(ctx context.Context, task daemon.Task, req ToolRequest) ToolResult {
	if req.Name != CLIToolName {
		return errResult("unknown tool %q", req.Name)
	}
	var in cliArgs
	if err := json.Unmarshal(req.Args, &in); err != nil {
		return errResult("arguments must be {\"args\": [...]}: %v", err)
	}
	if err := vetArgs(in.Args); err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}

	work, err := os.MkdirTemp("", "multica-tool-")
	if err != nil {
		return errResult("cannot create a working directory: %v", err)
	}
	defer os.RemoveAll(work)
	cfgRoot, tmp := work+"/config", work+"/tmp"
	for _, d := range []string{cfgRoot, tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return errResult("cannot create %s: %v", d, err)
		}
	}
	taskEnv, err := daemon.CLIEnv(task, t.ServerURL, cfgRoot, tmp)
	if err != nil {
		return errResult("%v", err)
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.Binary, in.Args...)
	cmd.Dir = work
	cmd.Env = append(baseEnv(work), flatten(taskEnv)...)
	if in.Stdin != "" {
		cmd.Stdin = strings.NewReader(in.Stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()

	text := t.clip(out.String())
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return ToolResult{IsError: true, Output: text + "\n[timed out after " + timeout.String() + "]"}
	case runErr != nil:
		return ToolResult{IsError: true, Output: text + "\n[" + runErr.Error() + "]"}
	}
	return ToolResult{Output: text}
}

func vetArgs(args []string) error {
	if len(args) == 0 {
		return errors.New("args is empty")
	}
	for _, a := range args {
		for _, f := range cliDeniedFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return fmt.Errorf("%s is not allowed: the task's credentials are fixed", f)
			}
		}
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if cliDenied[a] {
			return fmt.Errorf("`multica %s` is not available to task agents", a)
		}
		break
	}
	return nil
}

func (t *CLITool) clip(s string) string {
	max := t.MaxOutput
	if max <= 0 {
		max = 64 << 10
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n[output truncated: %d bytes dropped]", len(s)-max)
}

func baseEnv(home string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C.UTF-8"}
}

func flatten(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func errResult(format string, args ...any) ToolResult {
	return ToolResult{IsError: true, Output: fmt.Sprintf(format, args...)}
}
