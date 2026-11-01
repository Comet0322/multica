package podsupervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

// A pod has two containers of the same image. The supervisor, in the daemon
// container, runs the daemon. The runner, in the agent container, runs the
// agent CLIs the daemon asks for through the shims. The containers run as
// different users, have separate process namespaces and mount different
// volumes: the daemon's home is mounted in the daemon container only, the
// agent's in the agent container only. The pod shares one network namespace,
// so the two meet on abstract unix sockets:
//
//   - <socket>: shims to runner, to start an agent (launcher_linux.go).
//   - <socket>-ctl: supervisor to runner, to open and close the runner.
//
// Both sockets accept only the daemon's user, checked with SO_PEERCRED.
const (
	// socketEnv names the environment variable that tells a shim where the
	// runner listens.
	socketEnv = "POD_AGENT_SOCKET"
	// daemonTmpEnv names the daemon's private TMPDIR for its shims, which read
	// the files the daemon hands to an agent from there.
	daemonTmpEnv = "POD_DAEMON_TMP"
	// controlSuffix is added to the socket name to get the control socket.
	controlSuffix = "-ctl"
)

// Operations on the control socket.
const (
	// opClose makes the runner refuse new agents, kill every process in its
	// container and empty its HOME and the shared writable areas, as far as its
	// own files go. The daemon must be stopped first.
	opClose = "close"
	// opOpen lets agents start again.
	opOpen = "open"
	// opStatus reports whether agents may start.
	opStatus = "status"
)

type runnerRequest struct {
	Op string `json:"op"`
}

type runnerReply struct {
	Error string `json:"error,omitempty"`
	Open  bool   `json:"open,omitempty"`
}

// RunnerConfig describes the agent container.
type RunnerConfig struct {
	// Socket is the abstract socket name (without the leading "@").
	Socket string
	// DaemonUID is the only user that may start agents or control the runner.
	DaemonUID uint32

	// WorkRoot, TmpDir and ShmDir are shared with the daemon container. On
	// close the runner empties what it can of them: it owns the files the
	// agents made, the daemon container cannot open every directory they made.
	WorkRoot string
	TmpDir   string
	ShmDir   string // optional
	// Home is the agent's HOME, mounted in this container only. It is rebuilt
	// from HomeTemplate (optional) at start and on every close.
	Home         string
	HomeTemplate string

	// ShimDir is left out of the agents' PATH.
	ShimDir string
	// Env is the environment whose PATH finds the real agent CLIs.
	Env []string

	Logger *slog.Logger
}

func (c RunnerConfig) validate() error {
	var errs []string
	for name, v := range map[string]string{"socket": c.Socket, "work root": c.WorkRoot, "home": c.Home, "tmpdir": c.TmpDir, "shim dir": c.ShimDir} {
		if v == "" {
			errs = append(errs, name+" is required")
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// runnerClient is the supervisor's side of the control socket.
type runnerClient struct {
	socket string
}

// call performs one operation. The runner may be slow to answer a close: it
// kills processes and empties directories.
func (c runnerClient) call(ctx context.Context, op string) (runnerReply, error) {
	var reply runnerReply
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "unix", "@"+c.socket+controlSuffix)
	if err != nil {
		return reply, fmt.Errorf("agent runner: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(runnerCallTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := json.NewEncoder(conn).Encode(runnerRequest{Op: op}); err != nil {
		return reply, fmt.Errorf("agent runner: %w", err)
	}
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return reply, fmt.Errorf("agent runner: no answer to %s: %w", op, err)
	}
	if reply.Error != "" {
		return reply, fmt.Errorf("agent runner: %s: %s", op, reply.Error)
	}
	return reply, nil
}

// runnerCallTimeout bounds one control call, a close with its directory
// cleaning included.
const runnerCallTimeout = 60 * time.Second
