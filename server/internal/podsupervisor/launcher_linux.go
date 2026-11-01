//go:build linux

package podsupervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Wire protocol between a shim and the runner, over one stream socket:
//
//  1. shim -> runner: one byte carrying the shim's stdin, stdout and stderr
//     as SCM_RIGHTS.
//  2. shim -> runner: a launchRequest as JSON.
//  3. runner -> shim: launchReply{Started: true}, or launchReply{Error}.
//  4. shim -> runner, any time: launchControl{Signal} to signal the agent's
//     process group. Closing the socket kills the agent.
//  5. runner -> shim: launchReply{Exited: true, Code or Signal}.
type launchRequest struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
	// Files are the contents of files the daemon named in Args that live in
	// its private TMPDIR, which the agent's container cannot see (see
	// argFile).
	Files []argFile `json:"files,omitempty"`
}

// argFile is a file the daemon passed to the agent by path. The daemon keeps
// such files, the task's MCP configuration among them, in its private home.
// The shim reads them there and the runner writes a copy the agent can open.
type argFile struct {
	Index  int    `json:"index"`  // position in Args
	Prefix string `json:"prefix"` // "--flag=" when the argument was a flag with a value
	Name   string `json:"name"`   // base name of the original
	Data   []byte `json:"data"`
}

type launchControl struct {
	Signal int `json:"signal"`
}

type launchReply struct {
	Started bool   `json:"started,omitempty"`
	Error   string `json:"error,omitempty"`
	// Code is the exit status the shim should use for an Error (127: no such
	// CLI).
	Code   int  `json:"code,omitempty"`
	Exited bool `json:"exited,omitempty"`
	Signal int  `json:"signal,omitempty"`
}

type launcherConfig struct {
	// DaemonUID is the only user allowed to launch agents.
	DaemonUID uint32
	// Socket is the abstract socket name the shims connect to.
	Socket   string
	Home     string
	TmpDir   string
	ShimDir  string
	RealPath []string
}

// launcher runs agent CLIs on behalf of the daemon, in the runner's container.
type launcher struct {
	launcherConfig
	log *slog.Logger

	ln net.Listener
	mu sync.Mutex
	// open is false while the pod is being reset: no agent may start then.
	open bool
	pids map[int]struct{}
}

func newLauncher(cfg launcherConfig, log *slog.Logger) *launcher {
	if log == nil {
		log = slog.Default()
	}
	return &launcher{launcherConfig: cfg, log: log, pids: map[int]struct{}{}}
}

func (l *launcher) listen() error {
	if l == nil {
		return nil
	}
	ln, err := net.Listen("unix", "@"+l.Socket)
	if err != nil {
		return err
	}
	l.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go l.handle(c.(*net.UnixConn))
		}
	}()
	return nil
}

func (l *launcher) shutdown() {
	if l == nil {
		return
	}
	l.allow(false)
	if l.ln != nil {
		_ = l.ln.Close()
	}
}

// allow opens or closes the launcher for new agents.
func (l *launcher) allow(open bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.open = open
	l.mu.Unlock()
}

// isOpen reports whether agents may start.
func (l *launcher) isOpen() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open
}

// owns reports whether pid is an agent the launcher waits for itself.
func (l *launcher) owns(pid int) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.pids[pid]
	return ok
}

func (l *launcher) handle(conn *net.UnixConn) {
	defer conn.Close()
	// Only the daemon's user may launch. Anything else in the pod, the agents
	// included, is turned away before it can send a byte of request.
	if uid, err := peerUID(conn); err != nil || uid != l.DaemonUID {
		return
	}
	files, err := receiveFiles(conn, 3)
	if err != nil {
		return
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	var req launchRequest
	if err := dec.Decode(&req); err != nil {
		return
	}
	sp, cleanup, err := l.command(req, files)
	defer cleanup()
	var cmd *child
	if err == nil {
		cmd, err = l.start(sp)
	}
	if err != nil {
		code := 126
		if errors.Is(err, exec.ErrNotFound) {
			code = 127
		}
		_ = enc.Encode(launchReply{Error: err.Error(), Code: code})
		return
	}
	pid := cmd.Process.Pid
	if enc.Encode(launchReply{Started: true}) != nil {
		l.kill(pid)
	}

	go func() {
		for {
			var c launchControl
			if dec.Decode(&c) != nil {
				// The shim is gone (killed by the daemon, or crashed): the
				// agent must not outlive it.
				l.kill(pid)
				return
			}
			if c.Signal > 0 && c.Signal < 32 {
				l.signal(pid, syscall.Signal(c.Signal))
			}
		}
	}()

	waitErr := cmd.Wait()
	l.mu.Lock()
	delete(l.pids, pid)
	l.mu.Unlock()
	// Like the daemon does for a process it started: what the agent left in
	// its process group does not outlive it.
	_ = syscall.Kill(-pid, syscall.SIGKILL)

	reply := launchReply{Exited: true}
	var ee *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			reply.Signal = int(ws.Signal())
		} else {
			reply.Code = ee.ExitCode()
		}
	default:
		reply = launchReply{Error: waitErr.Error(), Code: 1}
	}
	_ = enc.Encode(reply)
}

func (l *launcher) command(req launchRequest, files []*os.File) (*spec, func(), error) {
	noop := func() {}
	name := req.Name
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return nil, noop, fmt.Errorf("invalid agent name %q", name)
	}
	path, err := l.lookup(name)
	if err != nil {
		return nil, noop, err
	}
	if !filepath.IsAbs(req.Dir) {
		return nil, noop, fmt.Errorf("working directory %q is not absolute", req.Dir)
	}
	args, cleanup := l.writeArgFiles(req.Args, req.Files)
	return &spec{
		program: path,
		argv0:   name,
		args:    args,
		attr: &os.ProcAttr{
			Dir:   req.Dir,
			Env:   l.agentEnv(req.Env),
			Files: []*os.File{files[0], files[1], files[2]},
			Sys:   &syscall.SysProcAttr{Setpgid: true},
		},
	}, cleanup, nil
}

// maxArgFile bounds a file copied for the agent; these are small configuration
// files (the MCP configuration of a task).
const maxArgFile = 1 << 20

// writeArgFiles replaces each argument the shim sent a file for with the path
// of a copy in the agent's TMPDIR, and returns the arguments and a function
// that removes the copies once the agent has ended. Each copy goes into a fresh
// directory through an os.Root on it, so a name cannot lead out of it.
func (l *launcher) writeArgFiles(args []string, files []argFile) ([]string, func()) {
	var dirs []string
	cleanup := func() {
		for _, d := range dirs {
			_ = os.RemoveAll(d)
		}
	}
	out := append([]string(nil), args...)
	for _, f := range files {
		if f.Index < 0 || f.Index >= len(out) || len(f.Data) > maxArgFile ||
			f.Name == "" || f.Name == "." || f.Name == ".." || strings.ContainsAny(f.Name, "/\x00") {
			continue
		}
		dir, err := os.MkdirTemp(l.TmpDir, "agent-arg-")
		if err != nil {
			continue
		}
		dirs = append(dirs, dir)
		dst, err := os.OpenRoot(dir)
		if err != nil {
			continue
		}
		err = dst.WriteFile(f.Name, f.Data, 0o600)
		_ = dst.Close()
		if err != nil {
			continue
		}
		out[f.Index] = f.Prefix + filepath.Join(dir, f.Name)
	}
	return out, cleanup
}

// lookup finds the real CLI on the PATH without the shim directory.
func (l *launcher) lookup(name string) (string, error) {
	for _, dir := range l.RealPath {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
}

// agentEnv is the daemon's environment for the agent (it carries the task's
// own variables) with the pod's agent HOME and TMPDIR, and a PATH that does
// not lead back into the shims.
func (l *launcher) agentEnv(in []string) []string {
	env := make([]string, 0, len(in)+3)
	path := strings.Join(l.RealPath, ":")
	for _, kv := range in {
		switch {
		case hasAnyPrefix(kv, []string{"HOME=", "TMPDIR=", socketEnv + "=", daemonTmpEnv + "="}):
		case strings.HasPrefix(kv, "PATH="):
			path = strings.Join(realPath(strings.TrimPrefix(kv, "PATH="), l.ShimDir), ":")
		default:
			env = append(env, kv)
		}
	}
	return append(env, "HOME="+l.Home, "TMPDIR="+l.TmpDir, "PATH="+path)
}

func (l *launcher) start(sp *spec) (*child, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.open {
		return nil, errors.New("the pod is not accepting agent launches (resetting)")
	}
	c, err := startChild(sp)
	if err != nil {
		return nil, err
	}
	l.pids[c.Process.Pid] = struct{}{}
	return c, nil
}

func (l *launcher) signal(pid int, sig syscall.Signal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.pids[pid]; ok {
		_ = syscall.Kill(-pid, sig)
	}
}

// kill ends the agent, its process group and everything below it.
func (l *launcher) kill(pid int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.pids[pid]; !ok {
		return
	}
	for _, p := range descendants(pid) {
		killProc(p.pid)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func peerUID(conn *net.UnixConn) (uint32, error) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var serr error
	if err := rc.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return cred.Uid, nil
}

// receiveFiles reads the single byte that carries n descriptors. The
// descriptors are close-on-exec from the start, so a process the supervisor
// forks meanwhile cannot inherit them.
func receiveFiles(conn *net.UnixConn, n int) ([]*os.File, error) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	buf, oob := make([]byte, 1), make([]byte, syscall.CmsgSpace(4*n))
	var oobn int
	var rerr error
	if err := rc.Read(func(fd uintptr) bool {
		_, oobn, _, _, rerr = syscall.Recvmsg(int(fd), buf, oob, syscall.MSG_CMSG_CLOEXEC)
		return rerr != syscall.EAGAIN
	}); err != nil {
		return nil, err
	}
	if rerr != nil {
		return nil, rerr
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	var fds []int
	for i := range msgs {
		got, err := syscall.ParseUnixRights(&msgs[i])
		if err != nil {
			return nil, err
		}
		fds = append(fds, got...)
	}
	files := make([]*os.File, len(fds))
	for i, fd := range fds {
		files[i] = os.NewFile(uintptr(fd), "agent-stdio")
	}
	if len(files) != n {
		for _, f := range files {
			_ = f.Close()
		}
		return nil, fmt.Errorf("got %d descriptors, want %d", len(files), n)
	}
	return files, nil
}
