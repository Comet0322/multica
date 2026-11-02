// pod-supervisor is the PID 1 of a pooled sandbox pod, which is one container
// and no root. It runs the Multica daemon and resets the pod on command. See
// package podsupervisor.
//
// Usage:
//
//	pod-supervisor [flags] -- daemon-command [args...]
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/podsupervisor"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	run, err := parse(os.Args[1:], os.Environ(), log)
	if err != nil {
		log.Error("invalid pod supervisor configuration", "error", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		log.ErrorContext(ctx, "pod supervisor stopped", "error", err)
		os.Exit(1)
	}
}

// parse reads the flags and returns the function that runs the supervisor.
func parse(args, environ []string, log *slog.Logger) (func(context.Context) error, error) {
	var (
		sup     podsupervisor.Config
		pollGap time.Duration
	)
	fs := flag.NewFlagSet("pod-supervisor", flag.ContinueOnError)
	fs.BoolVar(&sup.CleanIPC, "clean-ipc", false, "remove System V and POSIX IPC objects on every reset (own IPC namespace only)")
	fs.StringVar(&sup.WorkRoot, "work-root", "", "working root, emptied on reset")
	fs.StringVar(&sup.TmpDir, "tmpdir", "", "TMPDIR, emptied on reset")
	fs.StringVar(&sup.ControlURL, "control-url", "", "URL polled for reset commands")
	fs.DurationVar(&pollGap, "poll-interval", 2*time.Second, "control URL poll interval")
	fs.StringVar(&sup.DaemonHome, "daemon-home", "", "HOME of the daemon and the agents, rebuilt on reset")
	fs.StringVar(&sup.DaemonHomeTemplate, "daemon-home-template", "", "read-only template of HOME")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() == 0 {
		return nil, fmt.Errorf("daemon command required after --")
	}
	sup.PollInterval = pollGap
	sup.DaemonCmd = fs.Args()
	sup.Env, sup.Logger = environ, log
	s, err := podsupervisor.New(sup)
	if err != nil {
		return nil, err
	}
	return s.Run, nil
}
