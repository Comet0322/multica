// pod-supervisor is the PID 1 of a pooled sandbox pod: it runs the Multica
// daemon and resets the pod on command. See package podsupervisor for the
// control channel.
//
// Usage: pod-supervisor [flags] -- daemon-command [args...]
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

func main() {
	// Linked under the name of an agent CLI, this binary is the shim that
	// hands the launch to the supervisor (see podsupervisor.Config.ShimDir).
	if name := filepath.Base(os.Args[0]); name != "pod-supervisor" {
		os.Exit(podsupervisor.RunShim(name, os.Args[1:]))
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := parse(os.Args[1:], os.Environ())
	if err != nil {
		log.Error("invalid pod supervisor configuration", "error", err)
		os.Exit(2)
	}
	cfg.Logger = log
	s, err := podsupervisor.New(cfg)
	if err != nil {
		log.Error("invalid pod supervisor configuration", "error", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		log.ErrorContext(ctx, "pod supervisor stopped", "error", err)
		os.Exit(1)
	}
}

func parse(args, environ []string) (podsupervisor.Config, error) {
	var cfg podsupervisor.Config
	fs := flag.NewFlagSet("pod-supervisor", flag.ContinueOnError)
	fs.StringVar(&cfg.ControlURL, "control-url", "", "URL polled for reset commands")
	fs.StringVar(&cfg.WorkRoot, "work-root", "", "daemon working root, emptied on reset")
	fs.StringVar(&cfg.Home, "home", "", "daemon HOME, rebuilt from the template on reset")
	fs.StringVar(&cfg.TmpDir, "tmpdir", "", "daemon TMPDIR, emptied on reset")
	fs.StringVar(&cfg.HomeTemplate, "home-template", "", "read-only HOME template")
	fs.DurationVar(&cfg.PollInterval, "poll-interval", 2*time.Second, "control URL poll interval")
	fs.StringVar(&cfg.ShimDir, "shim-dir", "", "directory of agent shims; switches on the daemon/agent user split")
	fs.StringVar(&cfg.DaemonHome, "daemon-home", "", "daemon HOME in split mode, below the tmpdir")
	fs.StringVar(&cfg.DaemonHomeTemplate, "daemon-home-template", "", "read-only template of the daemon HOME in split mode")
	agentUID := fs.Int("agent-uid", -1, "agent user id (split mode, required when running as root)")
	agentGID := fs.Int("agent-gid", -1, "agent group id")
	uid := fs.Int("uid", -1, "daemon user id (required when running as root)")
	gid := fs.Int("gid", -1, "daemon group id")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	var err error
	if cfg.DaemonUser, err = userFromFlags("uid", *uid, *gid); err != nil {
		return cfg, err
	}
	if cfg.AgentUser, err = userFromFlags("agent-uid", *agentUID, *agentGID); err != nil {
		return cfg, err
	}
	if fs.NArg() == 0 {
		return cfg, fmt.Errorf("daemon command required after --")
	}
	cfg.DaemonCmd = fs.Args()
	cfg.Env = environ
	return cfg, nil
}

// userFromFlags turns a uid/gid flag pair into a user. A negative uid means the
// flag was not given; a negative gid defaults to the uid. Ids outside the range
// of a Linux uid_t are refused rather than wrapped.
func userFromFlags(name string, uid, gid int) (*podsupervisor.User, error) {
	if uid < 0 {
		return nil, nil
	}
	if gid < 0 {
		gid = uid
	}
	u, ok := podsupervisor.ID32(uid)
	g, gok := podsupervisor.ID32(gid)
	if !ok || !gok {
		return nil, fmt.Errorf("-%s %d / group %d is out of range", name, uid, gid)
	}
	return &podsupervisor.User{UID: u, GID: g}, nil
}
