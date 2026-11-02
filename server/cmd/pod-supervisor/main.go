// pod-supervisor is the PID 1 of both containers of a pooled sandbox pod. As
// the supervisor (the default) it runs the Multica daemon and resets the pod on
// command; as the runner (-role runner) it starts the agent CLIs for the daemon.
// Neither needs root. See package podsupervisor.
//
// Usage:
//
//	pod-supervisor [flags] -- daemon-command [args...]
//	pod-supervisor -role runner [flags]
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/podsupervisor"
)

const (
	roleSupervisor = "supervisor"
	roleRunner     = "runner"
)

func main() {
	// Linked under the name of an agent CLI, this binary is the shim that
	// hands the launch to the runner (see podsupervisor.Config.ShimDir).
	if name := filepath.Base(os.Args[0]); name != "pod-supervisor" {
		os.Exit(podsupervisor.RunShim(name, os.Args[1:]))
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	role, run, err := parse(os.Args[1:], os.Environ(), log)
	if err != nil {
		log.Error("invalid pod "+role+" configuration", "error", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		log.ErrorContext(ctx, "pod "+role+" stopped", "error", err)
		os.Exit(1)
	}
}

// parse reads the flags and returns the role and the function that runs it.
func parse(args, environ []string, log *slog.Logger) (string, func(context.Context) error, error) {
	var (
		role    string
		sup     podsupervisor.Config
		run     podsupervisor.RunnerConfig
		daemon  int
		pollGap time.Duration
	)
	fs := flag.NewFlagSet("pod-supervisor", flag.ContinueOnError)
	fs.StringVar(&role, "role", roleSupervisor, "supervisor (daemon container) or runner (agent container)")
	fs.BoolVar(&sup.Single, "single", false, "supervisor: the pod is this one container (no runner, no shims)")
	fs.StringVar(&sup.AgentSocket, "agent-socket", "", "abstract socket name of the agent runner, shared by both roles")
	fs.StringVar(&sup.WorkRoot, "work-root", "", "working root shared by both containers, emptied on reset")
	fs.StringVar(&sup.TmpDir, "tmpdir", "", "TMPDIR shared by both containers, emptied on reset")
	fs.StringVar(&sup.ShimDir, "shim-dir", "", "directory of agent shims")
	fs.IntVar(&daemon, "daemon-uid", -1, "runner: the only user that may start agents (the daemon's)")
	fs.StringVar(&run.Home, "home", "", "runner: agent HOME, rebuilt from the template on reset")
	fs.StringVar(&run.HomeTemplate, "home-template", "", "runner: read-only HOME template")
	fs.StringVar(&sup.ControlURL, "control-url", "", "supervisor: URL polled for reset commands")
	fs.DurationVar(&pollGap, "poll-interval", 2*time.Second, "supervisor: control URL poll interval")
	fs.StringVar(&sup.DaemonHome, "daemon-home", "", "supervisor: daemon HOME, a volume of the daemon container only")
	fs.StringVar(&sup.DaemonHomeTemplate, "daemon-home-template", "", "supervisor: read-only template of the daemon HOME")
	if err := fs.Parse(args); err != nil {
		return roleSupervisor, nil, err
	}
	switch role {
	case roleRunner:
		uid, ok := podsupervisor.ID32(daemon)
		if !ok {
			return role, nil, fmt.Errorf("-daemon-uid %d is missing or out of range", daemon)
		}
		run.Socket, run.DaemonUID = sup.AgentSocket, uid
		run.WorkRoot, run.TmpDir, run.ShimDir = sup.WorkRoot, sup.TmpDir, sup.ShimDir
		run.ShmDir = envValue(environ, "POD_SHM_DIR")
		run.Env, run.Logger = environ, log
		r, err := podsupervisor.NewRunner(run)
		if err != nil {
			return role, nil, err
		}
		return role, r.Run, nil
	case roleSupervisor:
		if fs.NArg() == 0 {
			return role, nil, fmt.Errorf("daemon command required after --")
		}
		sup.PollInterval = pollGap
		sup.DaemonCmd = fs.Args()
		sup.Env, sup.Logger = environ, log
		s, err := podsupervisor.New(sup)
		if err != nil {
			return role, nil, err
		}
		return role, s.Run, nil
	}
	return roleSupervisor, nil, fmt.Errorf("unknown role %q", role)
}

// envValue returns the value of name in environ, or "".
func envValue(environ []string, name string) string {
	for _, kv := range environ {
		if len(kv) > len(name) && kv[:len(name)] == name && kv[len(name)] == '=' {
			return kv[len(name)+1:]
		}
	}
	return ""
}
