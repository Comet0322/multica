// Package sandbox is the harness's only view of the machine that does the work.
//
// The harness (model loop, tools, credentials) runs on the host. The sandbox is
// its hands: it can run a command and move files, and nothing else. It cannot
// call back, listen, or reach the network, so nothing in this interface may
// assume it can. Every other harness package depends on Sandbox and on nothing
// below it, which is what lets the sandbox be swapped (a local directory in
// tests, a container in development, a vendor API later) without touching the
// tools or the loop.
package sandbox

import (
	"context"
	"io/fs"
	"time"
)

// ExecRequest is one command to run inside the sandbox.
type ExecRequest struct {
	// Command is interpreted by the sandbox's shell, so pipes and redirects work.
	Command string
	// Cwd is the working directory; empty means the sandbox's default.
	Cwd string
	// Env is added to the sandbox's environment for this command only.
	Env map[string]string
	// Timeout bounds the command; zero means the sandbox's own limit.
	Timeout time.Duration
}

// ExecResult is the outcome of a command that ran to an end, whatever its exit
// status. A command that could not be run at all is an error from Exec instead.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TimedOut is set when Timeout expired and the command was killed.
	TimedOut bool
}

// Sandbox is a single isolated execution environment, used by one task.
type Sandbox interface {
	// Exec runs a command and waits for it to finish.
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
	// Put writes a file, creating its parent directories.
	Put(ctx context.Context, path string, data []byte, mode fs.FileMode) error
	// Get reads a file. A missing file is an error satisfying errors.Is(err, fs.ErrNotExist).
	Get(ctx context.Context, path string) ([]byte, error)
	// Close destroys the sandbox and everything in it.
	Close(ctx context.Context) error
}
