// Package podsupervisor holds the two programs of a pooled sandbox pod, which
// has two containers of one image and no root anywhere.
//
// The Supervisor is the daemon container's PID 1. It runs the Multica daemon,
// restarts it when it crashes, and on a reset command puts the pod back into a
// clean state without restarting the containers: it stops the daemon, has the
// Runner close, kills every descendant, empties the shared writable areas
// (work root, TMPDIR, shared memory) and the daemon's home, rebuilds the
// daemon's home from the read-only template baked into the image, and starts
// the daemon again with a fresh environment.
//
// The Runner is the agent container's PID 1. It starts the agent CLIs for the
// daemon and cleans the agent's home. See runner.go for how the two meet.
//
// # Daemon and agents
//
// The stock daemon starts agents as itself and cannot be changed, so its PATH
// starts with Config.ShimDir, which holds one hard link of the binary per
// agent CLI name. A shim sends its argv, working directory, environment and
// stdio to the Runner, which runs the real CLI in the agent container. The
// daemon container's secrets (the daemon's home, its processes) are not
// visible there: the volumes are not mounted and the process namespace is not
// shared. See deploy/sandbox/SANDBOX.md, which also lists what the split does
// not protect against.
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
//
// # Single container
//
// With Config.Single the pod is one container and one user: the
// daemon starts the agent CLIs itself, and HOME (Config.DaemonHome, rebuilt
// from Config.DaemonHomeTemplate), TMPDIR, the work root and shared memory are
// all one area that every reset empties. There is no runner, so the agents
// are not isolated from the daemon (same user: they can see and trace it, and
// read its HOME). What keeps tasks apart is that nothing outlives a reset and
// that the supervisor cannot be reached from the agents:
//
//   - the supervisor sets PR_SET_DUMPABLE to 0, so processes of the same user
//     can neither ptrace it nor read /proc/<pid>/{mem,environ,fd,...};
//   - as PID 1 it ignores SIGKILL and SIGSTOP from inside the pod (the
//     kernel drops them) and ignores the other signals that would end it
//     except SIGINT and SIGTERM, which only restart the container (it empties
//     the same areas at start);
//   - a reset kills every process of the pod before it touches a file.
package podsupervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	// defaultRunnerTimeout is how long the supervisor waits for the runner to
	// come up; the two containers start independently.
	defaultRunnerTimeout = 2 * time.Minute
	// runnerCheckEvery is how many reap ticks lie between two checks that the
	// runner is still open.
	runnerCheckEvery = 5
	// resetAttempts is how often a failing reset is tried before the
	// supervisor gives up and exits; resetRetryDelay is the first pause
	// between tries, doubled each time.
	resetAttempts   = 3
	resetRetryDelay = time.Second
	// runnerRetryDelay is the pause between two tries to reach the runner.
	runnerRetryDelay = 500 * time.Millisecond
)

// Config describes the daemon container of one pod, or with Single the only
// container of one (see the package comment).
type Config struct {
	// Single makes the pod one container: no runner, no shims, AgentSocket and
	// ShimDir are not used.
	Single bool
	// DaemonCmd is the command (argv) that starts the daemon.
	DaemonCmd []string
	// Env is the complete base environment of the daemon. The supervisor adds
	// HOME, TMPDIR, PATH and the runner's socket. Nothing else is inherited, so
	// each restart starts from the same clean environment.
	Env []string

	// WorkRoot and TmpDir are shared with the agent container and emptied on
	// reset.
	WorkRoot string
	TmpDir   string
	// ShmDir, when set, is shared with the agent container and emptied on every
	// reset. Mount an emptyDir (medium Memory) on /dev/shm and name it here, or
	// files a task leaves in shared memory survive into the next workspace's
	// task. Empty takes POD_SHM_DIR from Env.
	ShmDir string

	// DaemonHome is HOME of the daemon, mounted in this container only; the
	// daemon's TMPDIR is its tmp subdirectory. Both are emptied on every reset
	// and DaemonHome is rebuilt from DaemonHomeTemplate, which carries the
	// daemon's CLI config.
	DaemonHome         string
	DaemonHomeTemplate string

	// ShimDir holds one hard link of this binary per agent CLI name and starts
	// the daemon's PATH.
	ShimDir string
	// AgentSocket is the abstract socket name of the runner in the agent
	// container.
	AgentSocket string

	ControlURL   string
	PollInterval time.Duration
	RestartDelay time.Duration
	// UnknownTimeout is how long the control URL may keep answering 404
	// before the supervisor exits so the container restarts. Zero means
	// defaultUnknownTimeout.
	UnknownTimeout time.Duration
	// RunnerTimeout is how long the runner may stay unreachable when the
	// supervisor starts, before it exits so the container restarts. Zero means
	// defaultRunnerTimeout.
	RunnerTimeout time.Duration
	HTTPClient    *http.Client
	Logger        *slog.Logger
}

// Supervisor runs the daemon and serves reset commands.
type Supervisor struct {
	cfg    Config
	log    *slog.Logger
	runner runnerClient
	// single is a pod of one container: no runner, no shims, and the daemon and
	// the agents share one user, HOME and TMPDIR.
	single bool
	// realPath is where the real agent CLIs are: the PATH without the shims.
	realPath []string
	resets   chan resetRequest
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
	required := map[string]string{"work root": cfg.WorkRoot, "tmpdir": cfg.TmpDir, "control url": cfg.ControlURL,
		"daemon home": cfg.DaemonHome}
	if !cfg.Single {
		required["shim dir"], required["agent socket"] = cfg.ShimDir, cfg.AgentSocket
	}
	for name, v := range required {
		if v == "" {
			errs = append(errs, name+" is required")
		}
	}
	if cfg.DaemonHome != "" && (cfg.DaemonHome == cfg.TmpDir || cfg.DaemonHome == cfg.WorkRoot || within(cfg.TmpDir, cfg.DaemonHome) || within(cfg.WorkRoot, cfg.DaemonHome) || within(cfg.DaemonHome, cfg.TmpDir) || within(cfg.DaemonHome, cfg.WorkRoot)) {
		errs = append(errs, "daemon home must be a volume of its own")
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
	if cfg.RunnerTimeout <= 0 {
		cfg.RunnerTimeout = defaultRunnerTimeout
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
	return &Supervisor{
		cfg: cfg, log: cfg.Logger, runner: runnerClient{socket: cfg.AgentSocket},
		single:   cfg.Single,
		realPath: realPath(pathOf(cfg.Env), cfg.ShimDir),
		resets:   make(chan resetRequest), giveUp: make(chan error, 1),
	}, nil
}

// Run blocks until ctx is cancelled, then stops the daemon and every
// descendant. It returns an error only if the pod cannot be brought up at all.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := becomeSubreaper(); err != nil {
		return fmt.Errorf("become subreaper: %w", err)
	}
	if s.single {
		// The agents run as this user and could otherwise read this process's
		// memory and environment or trace it; it must outlive every task.
		if err := denyPeerAccess(); err != nil {
			return fmt.Errorf("make the supervisor undumpable: %w", err)
		}
		setPrivateUmask()
		ignoreStraySignals()
	} else {
		// The daemon and the agents share the task files through their common group.
		setSharedUmask()
	}
	if err := s.makeDirs(); err != nil {
		return fmt.Errorf("prepare directories: %w", err)
	}
	// A runner that outlived a restart of this container may still hold agents
	// of the old daemon; close it before the new daemon starts. It also removes
	// what the agents left behind in their own name, which this side cannot
	// open, so the shared areas are cleaned only afterwards.
	if err := s.closeRunner(ctx, s.cfg.RunnerTimeout); err != nil {
		return err
	}
	if err := s.cleanDirs(); err != nil {
		return fmt.Errorf("clean directories: %w", err)
	}
	// The server learns of the restart (and of this address) before the
	// daemon calls it.
	s.sendEvent(ctx, eventStarted)
	go s.poll(ctx)

	var (
		cmd     *child
		exited  chan error
		restart <-chan time.Time
	)
	start := func() {
		c, ch, err := s.startDaemon(ctx)
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
	ticks := 0
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
			reapZombies(func(pid int) bool { return pid == daemon })
			if ticks++; ticks%runnerCheckEvery == 0 && cmd != nil {
				s.keepRunnerOpen(ctx)
			}
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

// closeRunner closes the runner, waiting up to wait for it to be reachable.
func (s *Supervisor) closeRunner(ctx context.Context, wait time.Duration) error {
	if s.single {
		return nil
	}
	deadline := time.Now().Add(wait)
	for {
		callCtx, cancel := context.WithTimeout(ctx, runnerCallTimeout)
		_, err := s.runner.call(callCtx, opClose)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(runnerRetryDelay):
		}
	}
}

// keepRunnerOpen reopens a runner that restarted (it starts closed) while the
// daemon is running.
func (s *Supervisor) keepRunnerOpen(ctx context.Context) {
	if s.single {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultControlClient)
	defer cancel()
	reply, err := s.runner.call(callCtx, opStatus)
	if err != nil {
		s.log.Warn("agent runner unreachable", "error", err)
		return
	}
	if !reply.Open {
		s.log.Warn("agent runner was closed while the daemon runs; opening it")
		if _, err := s.runner.call(callCtx, opOpen); err != nil {
			s.log.Error("open the agent runner", "error", err)
		}
	}
}

// makeDirs makes sure the shared writable areas exist.
func (s *Supervisor) makeDirs() error {
	for _, dir := range []string{s.cfg.WorkRoot, s.cfg.TmpDir, s.cfg.DaemonHome} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	return nil
}

func (s *Supervisor) cleanDirs() error {
	for _, dir := range []string{s.cfg.WorkRoot, s.cfg.TmpDir} {
		if err := emptyDir(dir); err != nil {
			return err
		}
	}
	if s.cfg.ShmDir != "" {
		if err := emptyDir(s.cfg.ShmDir); err != nil {
			return fmt.Errorf("empty shared memory: %w", err)
		}
	}
	return s.prepareDaemonHome()
}

// prepareDaemonHome rebuilds the daemon's private HOME and TMPDIR.
func (s *Supervisor) prepareDaemonHome() error {
	if err := emptyDir(s.cfg.DaemonHome); err != nil {
		return err
	}
	if !s.single {
		if err := os.Mkdir(s.daemonTmp(), 0o700); err != nil {
			return err
		}
	}
	return restoreTemplate(s.cfg.DaemonHomeTemplate, s.cfg.DaemonHome)
}

func (s *Supervisor) daemonTmp() string { return filepath.Join(s.cfg.DaemonHome, "tmp") }

// resetWithRetry runs resetEnvironment until it succeeds, resetAttempts times
// at most, pausing longer after each failure.
func (s *Supervisor) resetWithRetry(ctx context.Context) error {
	delay := resetRetryDelay
	for attempt := 1; ; attempt++ {
		err := s.resetEnvironment(ctx)
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

// resetEnvironment returns the pod to its initial state: the runner kills the
// agents and empties what it owns, then this side kills every descendant and
// empties the rest. The daemon must already be stopped. The runner stays
// closed until the daemon is started again.
func (s *Supervisor) resetEnvironment(ctx context.Context) error {
	if err := s.closeRunner(ctx, 0); err != nil {
		return err
	}
	if err := killDescendants(); err != nil {
		return fmt.Errorf("kill processes: %w", err)
	}
	if err := s.cleanDirs(); err != nil {
		return fmt.Errorf("clean directories: %w", err)
	}
	return nil
}

func (s *Supervisor) startDaemon(ctx context.Context) (*child, chan error, error) {
	if !s.single {
		// Agents may start only once the daemon runs.
		callCtx, cancel := context.WithTimeout(ctx, defaultControlClient)
		_, err := s.runner.call(callCtx, opOpen)
		cancel()
		if err != nil {
			return nil, nil, err
		}
	}
	// A process started without a standard input gets none; the daemon reads
	// nothing from it.
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, nil, err
	}
	defer null.Close()
	attr := &os.ProcAttr{
		Dir:   s.cfg.WorkRoot,
		Env:   s.daemonEnv(),
		Files: []*os.File{null, os.Stdout, os.Stderr},
	}
	cmd, err := startChild(&spec{program: s.cfg.DaemonCmd[0], argv0: s.cfg.DaemonCmd[0], args: s.cfg.DaemonCmd[1:], attr: attr})
	if err != nil {
		return nil, nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return cmd, exited, nil
}

func (s *Supervisor) daemonEnv() []string {
	if s.single {
		skip := []string{"HOME=", "TMPDIR="}
		env := make([]string, 0, len(s.cfg.Env)+2)
		for _, kv := range s.cfg.Env {
			if !hasAnyPrefix(kv, skip) {
				env = append(env, kv)
			}
		}
		return append(env, "HOME="+s.cfg.DaemonHome, "TMPDIR="+s.cfg.TmpDir)
	}
	skip := []string{"HOME=", "TMPDIR=", "PATH=", socketEnv + "=", daemonTmpEnv + "="}
	env := make([]string, 0, len(s.cfg.Env)+5)
	for _, kv := range s.cfg.Env {
		if !hasAnyPrefix(kv, skip) {
			env = append(env, kv)
		}
	}
	// The shims come first so the daemon finds them instead of the CLIs.
	return append(env, "HOME="+s.cfg.DaemonHome, "TMPDIR="+s.daemonTmp(),
		"PATH="+s.cfg.ShimDir+":"+strings.Join(s.realPath, ":"),
		socketEnv+"="+s.cfg.AgentSocket, daemonTmpEnv+"="+s.daemonTmp())
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
