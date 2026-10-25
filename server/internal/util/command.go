package util

import (
	"context"
	"os/exec"
)

// Command is exec.Command with the argument vector appended after the
// executable is resolved, so callers can pass a runtime-built argv.
func Command(name string, args ...string) *exec.Cmd {
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- daemon launches configured CLIs by design; argv is never shell-interpreted
	cmd := exec.Command(name)
	cmd.Args = append(cmd.Args, args...)
	return cmd
}

// CommandContext is exec.CommandContext with the same argv handling as Command.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- daemon launches configured CLIs by design; argv is never shell-interpreted
	cmd := exec.CommandContext(ctx, name)
	cmd.Args = append(cmd.Args, args...)
	return cmd
}
