//go:build linux

package podsupervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Runner is the agent container's PID 1: it starts the agent CLIs for the
// daemon and puts the container back into a clean state when the supervisor
// closes it.
type Runner struct {
	cfg RunnerConfig
	log *slog.Logger
	l   *launcher
	// ops serializes close and open.
	ops sync.Mutex
}

// NewRunner validates cfg.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	l := newLauncher(launcherConfig{
		DaemonUID: cfg.DaemonUID, Socket: cfg.Socket, Home: cfg.Home, TmpDir: cfg.TmpDir,
		ShimDir: cfg.ShimDir, RealPath: realPath(pathOf(cfg.Env), cfg.ShimDir),
	}, cfg.Logger)
	return &Runner{cfg: cfg, log: cfg.Logger, l: l}, nil
}

// Run blocks until ctx ends, then kills every process in the container. The
// runner starts closed: no agent may start until the supervisor opens it.
func (r *Runner) Run(ctx context.Context) error {
	if err := becomeSubreaper(); err != nil {
		return fmt.Errorf("become subreaper: %w", err)
	}
	setSharedUmask()
	// A restarted runner has lost its agents, and the volume outlived the
	// container; the shared areas belong to the daemon's side until it closes
	// the runner.
	if err := r.cleanHome(); err != nil {
		return fmt.Errorf("prepare home: %w", err)
	}
	if err := r.l.listen(); err != nil {
		return fmt.Errorf("listen for agent launches: %w", err)
	}
	defer r.l.shutdown()
	ctl, err := net.Listen("unix", "@"+r.cfg.Socket+controlSuffix)
	if err != nil {
		return fmt.Errorf("listen for control: %w", err)
	}
	defer ctl.Close()
	go func() {
		for {
			c, err := ctl.Accept()
			if err != nil {
				return
			}
			go r.handle(c.(*net.UnixConn))
		}
	}()

	reap := time.NewTicker(zombieReapInterval)
	defer reap.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := killDescendants(); err != nil {
				r.log.Error("kill descendants on shutdown", "error", err)
			}
			return nil
		case <-reap.C:
			reapZombies(r.l.owns)
		}
	}
}

func (r *Runner) handle(conn *net.UnixConn) {
	defer conn.Close()
	if uid, err := peerUID(conn); err != nil || uid != r.cfg.DaemonUID {
		return
	}
	var req runnerRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	reply := runnerReply{}
	switch req.Op {
	case opClose:
		if err := r.close(); err != nil {
			r.log.Error("close failed", "error", err)
			reply.Error = err.Error()
		}
	case opOpen:
		r.ops.Lock()
		r.l.allow(true)
		r.ops.Unlock()
		reply.Open = true
	case opStatus:
		reply.Open = r.l.isOpen()
	default:
		reply.Error = "unknown operation"
	}
	_ = json.NewEncoder(conn).Encode(reply)
}

// close stops the agents and removes what they left. Emptying the shared areas
// is best effort: a directory the daemon user made and the agent cannot open
// is the supervisor's to empty, and it checks that they are empty. The agent's
// own HOME is the runner's alone, so its failure is the close's.
func (r *Runner) close() error {
	r.ops.Lock()
	defer r.ops.Unlock()
	// No agent may start between now and the next open; one that did would
	// outlive the cleaning.
	r.l.allow(false)
	if err := killDescendants(); err != nil {
		return fmt.Errorf("kill processes: %w", err)
	}
	for _, dir := range []string{r.cfg.WorkRoot, r.cfg.TmpDir, r.cfg.ShmDir} {
		if dir == "" {
			continue
		}
		if err := emptyDir(dir); err != nil {
			r.log.Debug("left files of the daemon user for the supervisor", "dir", dir, "error", err)
		}
	}
	if err := r.cleanHome(); err != nil {
		return fmt.Errorf("clean home: %w", err)
	}
	return nil
}

func (r *Runner) cleanHome() error {
	if err := emptyDir(r.cfg.Home); err != nil {
		return err
	}
	return restoreTemplate(r.cfg.HomeTemplate, r.cfg.Home)
}
