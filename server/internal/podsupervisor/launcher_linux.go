//go:build linux

package podsupervisor

import (
	"crypto/rand"
	"encoding/hex"
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

// socketEnv names the environment variable that tells a shim where the
// supervisor listens.
const socketEnv = "POD_AGENT_SOCKET"

// Wire protocol between a shim and the supervisor, over one stream socket:
//
//  1. shim -> supervisor: one byte carrying the shim's stdin, stdout and
//     stderr as SCM_RIGHTS.
//  2. shim -> supervisor: a launchRequest as JSON.
//  3. supervisor -> shim: launchReply{Started: true}, or launchReply{Error}.
//  4. shim -> supervisor, any time: launchControl{Signal} to signal the
//     agent's process group. Closing the socket kills the agent.
//  5. supervisor -> shim: launchReply{Exited: true, Code or Signal}.
type launchRequest struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
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
	// Cred is the user agents run as; nil keeps the supervisor's own (tests).
	Cred     *syscall.Credential
	Home     string
	TmpDir   string
	ShimDir  string
	RealPath []string
}

// launcher runs agent CLIs on behalf of the daemon, as another user.
type launcher struct {
	launcherConfig
	log    *slog.Logger
	socket string // abstract socket name, random per pod start

	ln net.Listener
	mu sync.Mutex
	// open is false while the pod is being reset: no agent may start then.
	open bool
	pids map[int]struct{}
}

func newLauncher(cfg launcherConfig, log *slog.Logger) (*launcher, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &launcher{launcherConfig: cfg, log: log, socket: "multica-pod-agents-" + hex.EncodeToString(b[:]),
		pids: map[int]struct{}{}}, nil
}

func (l *launcher) listen() error {
	if l == nil {
		return nil
	}
	ln, err := net.Listen("unix", "@"+l.socket)
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
			f.Close()
		}
	}()
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	var req launchRequest
	if err := dec.Decode(&req); err != nil {
		return
	}
	cmd, err := l.command(req, files)
	if err == nil {
		err = l.start(cmd)
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

func (l *launcher) command(req launchRequest, files []*os.File) (*exec.Cmd, error) {
	name := req.Name
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return nil, fmt.Errorf("invalid agent name %q", name)
	}
	path, err := l.lookup(name)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(req.Dir) {
		return nil, fmt.Errorf("working directory %q is not absolute", req.Dir)
	}
	cmd := exec.Command(path, req.Args...)
	cmd.Args[0] = name
	cmd.Dir = req.Dir
	cmd.Env = l.agentEnv(req.Env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = files[0], files[1], files[2]
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: l.Cred}
	return cmd, nil
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
		case hasAnyPrefix(kv, []string{"HOME=", "TMPDIR=", socketEnv + "="}):
		case strings.HasPrefix(kv, "PATH="):
			path = strings.Join(realPath(strings.TrimPrefix(kv, "PATH="), l.ShimDir), ":")
		default:
			env = append(env, kv)
		}
	}
	return append(env, "HOME="+l.Home, "TMPDIR="+l.TmpDir, "PATH="+path)
}

func (l *launcher) start(cmd *exec.Cmd) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.open {
		return errors.New("the pod is not accepting agent launches (resetting)")
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	l.pids[cmd.Process.Pid] = struct{}{}
	return nil
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
			f.Close()
		}
		return nil, fmt.Errorf("got %d descriptors, want %d", len(files), n)
	}
	return files, nil
}
