// Package podsupervisor is the PID 1 of a pooled sandbox pod. It runs the
// Multica daemon as a non-root user, restarts it when it crashes, and on a
// reset command puts the pod back into a clean state without restarting the
// container: every descendant process is killed, the three writable areas
// (work root, HOME, TMPDIR) are emptied, HOME is rebuilt from the read-only
// template baked into the image, and the daemon is started again with a fresh
// environment.
//
// # Control channel
//
// The supervisor polls Config.ControlURL with GET every PollInterval. The
// response is JSON, or an empty body / 204 when there is nothing to do:
//
//	{"id": "r-42", "command": "reset"}
//
// Unknown commands are ignored. For "reset" the supervisor performs the reset
// and then POSTs the outcome to the same URL:
//
//	{"id": "r-42", "status": "done"}
//	{"id": "r-42", "status": "failed", "error": "remove /tmp/x: ..."}
//
// "done" is sent only after the daemon has been started again. On "failed" the
// daemon is deliberately left stopped; a later reset command retries. The
// server should keep returning a command until its outcome has been reported;
// a command whose id was already handled is not executed twice, its stored
// outcome is only re-sent.
package podsupervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultPollInterval  = 2 * time.Second
	defaultRestartDelay  = time.Second
	zombieReapInterval   = time.Second
	daemonStopTimeout    = 10 * time.Second
	defaultControlClient = 10 * time.Second
)

// User is the non-root identity the daemon runs as.
type User struct {
	UID, GID uint32
}

// Config describes one pod.
type Config struct {
	// DaemonCmd is the command (argv) that starts the daemon.
	DaemonCmd []string
	// DaemonUser is required when the supervisor runs as root. When the
	// supervisor already runs as a non-root user it may be nil.
	DaemonUser *User
	// Env is the complete base environment of the daemon. The supervisor adds
	// HOME and TMPDIR. Nothing else is inherited, so each restart starts from
	// the same clean environment.
	Env []string

	WorkRoot string
	Home     string
	TmpDir   string
	// HomeTemplate is copied into Home on every reset. Empty means an empty
	// HOME.
	HomeTemplate string

	ControlURL   string
	PollInterval time.Duration
	RestartDelay time.Duration
	HTTPClient   *http.Client
	Logger       *slog.Logger
}

// Supervisor runs the daemon and serves reset commands.
type Supervisor struct {
	cfg    Config
	log    *slog.Logger
	owner  *owner
	resets chan resetRequest
}

type resetRequest struct {
	reply chan error
}

// New validates cfg.
func New(cfg Config) (*Supervisor, error) {
	var errs []string
	if len(cfg.DaemonCmd) == 0 {
		errs = append(errs, "daemon command is required")
	}
	for name, v := range map[string]string{"work root": cfg.WorkRoot, "home": cfg.Home, "tmpdir": cfg.TmpDir, "control url": cfg.ControlURL} {
		if v == "" {
			errs = append(errs, name+" is required")
		}
	}
	if os.Geteuid() == 0 && (cfg.DaemonUser == nil || cfg.DaemonUser.UID == 0) {
		errs = append(errs, "daemon must run as a non-root user")
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.RestartDelay <= 0 {
		cfg.RestartDelay = defaultRestartDelay
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultControlClient}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Supervisor{cfg: cfg, log: cfg.Logger, resets: make(chan resetRequest)}
	if cfg.DaemonUser != nil && os.Geteuid() == 0 {
		s.owner = &owner{uid: int(cfg.DaemonUser.UID), gid: int(cfg.DaemonUser.GID)}
	}
	return s, nil
}

// Run blocks until ctx is cancelled, then stops the daemon and every
// descendant. It returns an error only if the pod cannot be brought up at all.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := becomeSubreaper(); err != nil {
		return fmt.Errorf("become subreaper: %w", err)
	}
	if err := s.prepareDirs(); err != nil {
		return fmt.Errorf("prepare directories: %w", err)
	}
	go s.poll(ctx)

	var (
		cmd     *exec.Cmd
		exited  chan error
		restart <-chan time.Time
	)
	start := func() {
		c, ch, err := s.startDaemon()
		if err != nil {
			s.log.Error("start daemon failed", "error", err)
			restart = time.After(s.cfg.RestartDelay)
			return
		}
		cmd, exited, restart = c, ch, nil
	}
	stop := func() {
		if cmd == nil {
			return
		}
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(daemonStopTimeout):
			s.log.Error("daemon did not exit after SIGKILL")
		}
		cmd, exited = nil, nil
	}

	start()
	reap := time.NewTicker(zombieReapInterval)
	defer reap.Stop()
	for {
		select {
		case <-ctx.Done():
			restart = nil
			stop()
			if err := killDescendants(); err != nil {
				s.log.Error("kill descendants on shutdown", "error", err)
			}
			return nil
		case err := <-exited:
			s.log.Warn("daemon exited, restarting", "error", err, "delay", s.cfg.RestartDelay)
			cmd, exited = nil, nil
			restart = time.After(s.cfg.RestartDelay)
		case <-restart:
			start()
		case <-reap.C:
			keep := 0
			if cmd != nil {
				keep = cmd.Process.Pid
			}
			reapZombies(keep)
		case req := <-s.resets:
			restart = nil
			stop()
			err := s.resetEnvironment()
			if err == nil {
				start()
				if cmd == nil {
					restart = nil
					err = errors.New("daemon failed to start after reset")
				}
			}
			req.reply <- err
		}
	}
}

// prepareDirs makes sure the three writable areas exist, are clean and are
// owned by the daemon user, and restores HOME from the template.
func (s *Supervisor) prepareDirs() error {
	for _, dir := range s.dirs() {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := s.owner.chown(dir); err != nil {
			return err
		}
	}
	return s.cleanDirs()
}

func (s *Supervisor) dirs() []string {
	return []string{s.cfg.WorkRoot, s.cfg.Home, s.cfg.TmpDir}
}

func (s *Supervisor) cleanDirs() error {
	for _, dir := range s.dirs() {
		if err := emptyDir(dir); err != nil {
			return err
		}
	}
	return restoreTemplate(s.cfg.HomeTemplate, s.cfg.Home, s.owner)
}

// resetEnvironment kills every descendant and returns the pod to its initial
// state. The daemon must already be stopped.
func (s *Supervisor) resetEnvironment() error {
	if err := killDescendants(); err != nil {
		return fmt.Errorf("kill processes: %w", err)
	}
	if err := s.cleanDirs(); err != nil {
		return fmt.Errorf("clean directories: %w", err)
	}
	return nil
}

func (s *Supervisor) startDaemon() (*exec.Cmd, chan error, error) {
	cmd := exec.Command(s.cfg.DaemonCmd[0], s.cfg.DaemonCmd[1:]...)
	cmd.Dir = s.cfg.WorkRoot
	cmd.Env = s.daemonEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if s.cfg.DaemonUser != nil && os.Geteuid() == 0 {
		cmd.SysProcAttr = credentialAttr(s.cfg.DaemonUser.UID, s.cfg.DaemonUser.GID)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return cmd, exited, nil
}

func (s *Supervisor) daemonEnv() []string {
	env := make([]string, 0, len(s.cfg.Env)+2)
	for _, kv := range s.cfg.Env {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "TMPDIR=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+s.cfg.Home, "TMPDIR="+s.cfg.TmpDir)
}
