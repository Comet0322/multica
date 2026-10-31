// Package podsupervisor is the PID 1 of a pooled sandbox pod. It runs the
// Multica daemon as a non-root user, restarts it when it crashes, and on a
// reset command puts the pod back into a clean state without restarting the
// container: every descendant process is killed, the three writable areas
// (work root, HOME, TMPDIR) are emptied, HOME is rebuilt from the read-only
// template baked into the image, and the daemon is started again with a fresh
// environment.
//
// # Daemon and agent users
//
// With Config.ShimDir set, the daemon and the agents run as different non-root
// users, although the daemon starts agents as itself and cannot be changed:
// see Config.ShimDir and deploy/sandbox/POOL.md, which also lists what the
// split does not protect against.
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
// "done" is sent only after the daemon has been started again. A reset that
// fails is retried a few times first; if it still fails, "failed" is reported
// and the supervisor exits with an error, so the kubelet restarts the
// container, whose start cleans the writable areas again. The server should
// keep returning a command until its outcome has been reported; a command
// whose id was already handled is not executed twice, its stored outcome is
// only re-sent.
//
// The supervisor also reports events, with no command id, so the server can
// tell a daemon that restarted from one that kept running:
//
//	{"event": "started"}        the supervisor (so the container) started
//	{"event": "daemon-exited"}  the daemon exited by itself and is restarted
//
// "started" is sent before the daemon is first started. "daemon-exited" is
// sent before the daemon is started again, but a manager that cannot be
// reached does not hold the restart up for long.
//
// A control URL that keeps answering 404 (the server does not know the pod:
// its daemon never registered, or the server dropped it) for
// Config.UnknownTimeout makes the supervisor exit too.
package podsupervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	defaultPollInterval  = 2 * time.Second
	defaultRestartDelay  = time.Second
	zombieReapInterval   = time.Second
	daemonStopTimeout    = 10 * time.Second
	defaultControlClient = 10 * time.Second
	// defaultUnknownTimeout is how long the control URL may answer 404
	// before the supervisor exits.
	defaultUnknownTimeout = 5 * time.Minute
	// resetAttempts is how often a failing reset is tried before the
	// supervisor gives up and exits; resetRetryDelay is the first pause
	// between tries, doubled each time.
	resetAttempts   = 3
	resetRetryDelay = time.Second
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
	// Home is HOME of the agent processes (and of the daemon when ShimDir is
	// empty).
	Home   string
	TmpDir string
	// HomeTemplate is copied into Home on every reset. Empty means an empty
	// HOME.
	HomeTemplate string

	// ShimDir switches on the daemon/agent user split. The stock daemon starts
	// every agent CLI as its own user, so the supervisor cannot give the agent
	// another user by itself. Instead the daemon's PATH starts with ShimDir,
	// which holds one hard link of this binary per agent CLI name ("claude",
	// "codex", ...). A shim sends its argv, working directory, environment and
	// stdio to the supervisor over a socket only the daemon user may use, and
	// the supervisor runs the real CLI, found on the remaining PATH, as
	// AgentUser. The daemon then runs as DaemonUser with its own HOME
	// (DaemonHome), which the agent user cannot read.
	ShimDir string
	// AgentUser is the user agents run as. Required when the supervisor runs
	// as root and ShimDir is set; it must differ from DaemonUser.
	AgentUser *User
	// DaemonHome is HOME of the daemon (mode 0700, owned by DaemonUser), and
	// must be below TmpDir: that area is made sticky and world-writable so the
	// agent user cannot rename it. DaemonHomeTemplate is copied into it on
	// every reset; it carries the daemon's CLI config.
	DaemonHome         string
	DaemonHomeTemplate string

	// ShmDir, when set, is emptied on every reset and kept world-writable
	// with the sticky bit, like /tmp. Mount an emptyDir (medium Memory) on
	// /dev/shm and name it here, or files a task leaves in shared memory
	// survive into the next workspace's task. Empty takes POD_SHM_DIR from
	// Env.
	ShmDir string

	ControlURL   string
	PollInterval time.Duration
	RestartDelay time.Duration
	// UnknownTimeout is how long the control URL may keep answering 404
	// before the supervisor exits so the container restarts. Zero means
	// defaultUnknownTimeout.
	UnknownTimeout time.Duration
	HTTPClient     *http.Client
	Logger         *slog.Logger
}

// Supervisor runs the daemon and serves reset commands.
type Supervisor struct {
	cfg        Config
	log        *slog.Logger
	owner      *owner // the daemon user
	agentOwner *owner // the agent user; the daemon user unless ShimDir is set
	launcher   *launcher
	resets     chan resetRequest
	// giveUp makes Run return with the error: the pod cannot be brought back
	// without restarting the container.
	giveUp chan error
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
	if cfg.ShimDir != "" {
		switch {
		case cfg.DaemonHome == "":
			errs = append(errs, "daemon home is required with a shim dir")
		case !within(cfg.TmpDir, cfg.DaemonHome):
			errs = append(errs, "daemon home must be below the tmpdir")
		}
		if os.Geteuid() == 0 {
			switch {
			case cfg.AgentUser == nil || cfg.AgentUser.UID == 0:
				errs = append(errs, "agents must run as a non-root user")
			case cfg.DaemonUser != nil && cfg.AgentUser.UID == cfg.DaemonUser.UID:
				errs = append(errs, "agent user must differ from the daemon user")
			}
		}
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
	if cfg.UnknownTimeout <= 0 {
		cfg.UnknownTimeout = defaultUnknownTimeout
	}
	if cfg.ShmDir == "" {
		cfg.ShmDir = envValue(cfg.Env, "POD_SHM_DIR")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultControlClient}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Supervisor{cfg: cfg, log: cfg.Logger, resets: make(chan resetRequest), giveUp: make(chan error, 1)}
	if cfg.DaemonUser != nil && os.Geteuid() == 0 {
		s.owner = &owner{uid: int(cfg.DaemonUser.UID), gid: int(cfg.DaemonUser.GID)}
	}
	s.agentOwner = s.owner
	if cfg.ShimDir != "" {
		var cred *syscall.Credential
		if cfg.AgentUser != nil && os.Geteuid() == 0 {
			s.agentOwner = &owner{uid: int(cfg.AgentUser.UID), gid: int(cfg.AgentUser.GID)}
			cred = &syscall.Credential{Uid: cfg.AgentUser.UID, Gid: cfg.AgentUser.GID, Groups: []uint32{cfg.AgentUser.GID}}
		}
		daemonUID := uint32(os.Geteuid())
		if cfg.DaemonUser != nil && os.Geteuid() == 0 {
			daemonUID = cfg.DaemonUser.UID
		}
		l, err := newLauncher(launcherConfig{
			DaemonUID: daemonUID, Cred: cred, Home: cfg.Home, TmpDir: cfg.TmpDir, DaemonTmp: filepath.Join(cfg.DaemonHome, "tmp"),
			ShimDir: cfg.ShimDir, RealPath: realPath(pathOf(cfg.Env), cfg.ShimDir),
		}, cfg.Logger)
		if err != nil {
			return nil, err
		}
		s.launcher = l
	}
	return s, nil
}

// Run blocks until ctx is cancelled, then stops the daemon and every
// descendant. It returns an error only if the pod cannot be brought up at all.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := becomeSubreaper(); err != nil {
		return fmt.Errorf("become subreaper: %w", err)
	}
	if s.launcher != nil {
		// Agent and daemon share the task files through their common group.
		setSharedUmask()
		if err := s.launcher.listen(); err != nil {
			return fmt.Errorf("listen for agent launches: %w", err)
		}
		defer s.launcher.shutdown()
	}
	if err := s.prepareDirs(); err != nil {
		return fmt.Errorf("prepare directories: %w", err)
	}
	// The server learns of the restart (and of this address) before the
	// daemon calls it.
	s.sendEvent(ctx, eventStarted)
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
		s.launcher.allow(true)
	}
	stop := func() {
		// No agent may start between the daemon's death and the end of the
		// reset; one that did would outlive the clean-up.
		s.launcher.allow(false)
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
			// The tasks the daemon ran are gone with it; the server must not
			// take the new daemon for the old one still running them.
			restart = s.afterEvent(ctx, eventDaemonExited, s.cfg.RestartDelay)
		case err := <-s.giveUp:
			restart = nil
			stop()
			if kerr := killDescendants(); kerr != nil {
				s.log.Error("kill descendants before exiting", "error", kerr)
			}
			return err
		case <-restart:
			start()
		case <-reap.C:
			daemon := 0
			if cmd != nil {
				daemon = cmd.Process.Pid
			}
			reapZombies(func(pid int) bool { return pid == daemon || s.launcher.owns(pid) })
		case req := <-s.resets:
			restart = nil
			stop()
			err := s.resetWithRetry(ctx)
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
		if err := s.dirOwner(dir).chown(dir); err != nil {
			return err
		}
	}
	return s.cleanDirs()
}

// dirOwner is who owns a writable area. With the user split, HOME belongs to
// the agent user, and TMPDIR to root with the sticky bit so that the daemon's
// private directory inside it cannot be renamed or removed by the agent user.
func (s *Supervisor) dirOwner(dir string) *owner {
	if s.cfg.ShimDir == "" {
		return s.owner
	}
	switch dir {
	case s.cfg.Home:
		return s.agentOwner
	case s.cfg.TmpDir:
		if os.Geteuid() == 0 {
			return &owner{}
		}
		return nil
	}
	return s.owner
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
	if s.cfg.ShmDir != "" {
		if err := emptyDir(s.cfg.ShmDir); err != nil {
			return fmt.Errorf("empty shared memory: %w", err)
		}
		// Every user may create files there, nobody may remove another's.
		if err := os.Chmod(s.cfg.ShmDir, os.ModeSticky|0o777); err != nil {
			return err
		}
	}
	if s.cfg.ShimDir != "" {
		if err := os.Chmod(s.cfg.TmpDir, os.ModeSticky|0o777); err != nil {
			return err
		}
	}
	if err := restoreTemplate(s.cfg.HomeTemplate, s.cfg.Home, s.agentOwner); err != nil {
		return err
	}
	return s.prepareDaemonHome()
}

// prepareDaemonHome rebuilds the daemon's private HOME (user split only).
func (s *Supervisor) prepareDaemonHome() error {
	if s.cfg.ShimDir == "" {
		return nil
	}
	for _, dir := range []string{s.cfg.DaemonHome, s.daemonTmp()} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		if err := s.owner.chown(dir); err != nil {
			return err
		}
	}
	return restoreTemplate(s.cfg.DaemonHomeTemplate, s.cfg.DaemonHome, s.owner)
}

func (s *Supervisor) daemonTmp() string { return filepath.Join(s.cfg.DaemonHome, "tmp") }

// resetWithRetry runs resetEnvironment until it succeeds, resetAttempts times
// at most, pausing longer after each failure.
func (s *Supervisor) resetWithRetry(ctx context.Context) error {
	delay := resetRetryDelay
	for attempt := 1; ; attempt++ {
		err := s.resetEnvironment()
		if err == nil || attempt >= resetAttempts {
			return err
		}
		s.log.Warn("reset failed, retrying", "attempt", attempt, "error", err, "delay", delay)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		delay *= 2
	}
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
	home, tmp := s.cfg.Home, s.cfg.TmpDir
	skip := []string{"HOME=", "TMPDIR="}
	if s.cfg.ShimDir != "" {
		home, tmp = s.cfg.DaemonHome, s.daemonTmp()
		skip = append(skip, "PATH=", socketEnv+"=")
	}
	env := make([]string, 0, len(s.cfg.Env)+4)
	for _, kv := range s.cfg.Env {
		if !hasAnyPrefix(kv, skip) {
			env = append(env, kv)
		}
	}
	env = append(env, "HOME="+home, "TMPDIR="+tmp)
	if s.cfg.ShimDir != "" {
		// The shims come first so the daemon finds them instead of the CLIs.
		env = append(env, "PATH="+s.cfg.ShimDir+":"+strings.Join(s.launcher.RealPath, ":"), socketEnv+"="+s.launcher.socket)
	}
	return env
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// envValue returns the value of name in env, or "".
func envValue(env []string, name string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// pathOf returns the PATH of env, or a default one.
func pathOf(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			return v
		}
	}
	return "/usr/local/bin:/usr/bin:/bin"
}

// realPath is the PATH without the shim directory: where the real agent CLIs
// are.
func realPath(path, shimDir string) []string {
	var out []string
	for _, d := range filepath.SplitList(path) {
		if d != "" && filepath.Clean(d) != filepath.Clean(shimDir) {
			out = append(out, d)
		}
	}
	return out
}

// within reports whether path is strictly below dir.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../")
}
