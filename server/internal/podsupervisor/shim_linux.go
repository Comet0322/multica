//go:build linux

package podsupervisor

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// RunShim is the whole program when this binary runs under the name of an
// agent CLI. It makes the supervisor run the real CLI, as the agent user, with
// the shim's own arguments, working directory, environment and stdio, and
// returns the exit status the CLI had. To the daemon that started it, the shim
// is the agent: it can signal it, wait for it and kill its process group.
func RunShim(name string, args []string) int {
	sock := os.Getenv(socketEnv)
	if sock == "" {
		fmt.Fprintf(os.Stderr, "%s: %s is not set; this is not a pod\n", name, socketEnv)
		return 126
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 126
	}
	// The agent user shares the task's files through the group. Directories
	// and files the daemon created before it started the agent are opened to
	// the group here, as the daemon's own user, which owns them.
	shareWithGroup(dir)
	shareCheckouts(dir)

	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: "@" + sock, Net: "unix"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: connect to the pod supervisor: %v\n", name, err)
		return 126
	}
	defer conn.Close()

	fds, closeFds := stdioDescriptors()
	_, _, err = conn.WriteMsgUnix([]byte{0}, syscall.UnixRights(fds...), nil)
	closeFds()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: send stdio: %v\n", name, err)
		return 126
	}
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	if err := enc.Encode(launchRequest{Name: name, Args: args, Dir: dir, Env: os.Environ()}); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 126
	}
	var reply launchReply
	if err := dec.Decode(&reply); err != nil {
		fmt.Fprintf(os.Stderr, "%s: the pod supervisor refused the launch\n", name)
		return 126
	}
	if reply.Error != "" {
		fmt.Fprintf(os.Stderr, "%s: %s\n", name, reply.Error)
		if reply.Code != 0 {
			return reply.Code
		}
		return 126
	}

	// The agent asks the daemon for its checkout after it has started, and git
	// makes the worktree's files private to the daemon; keep opening them for as
	// long as the agent runs.
	stopSweep := make(chan struct{})
	defer close(stopSweep)
	go func() {
		t := time.NewTicker(checkoutSweepEvery)
		defer t.Stop()
		for {
			select {
			case <-stopSweep:
				return
			case <-t.C:
				shareCheckouts(dir)
			}
		}
	}()

	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for s := range sigs {
			if n, ok := s.(syscall.Signal); ok {
				_ = enc.Encode(launchControl{Signal: int(n)})
			}
		}
	}()

	if err := dec.Decode(&reply); err != nil {
		fmt.Fprintf(os.Stderr, "%s: lost the pod supervisor\n", name)
		return 1
	}
	if reply.Error != "" {
		fmt.Fprintf(os.Stderr, "%s: %s\n", name, reply.Error)
		return 1
	}
	if reply.Signal > 0 {
		// Die by the same signal, as the agent did.
		signal.Reset()
		_ = syscall.Kill(os.Getpid(), syscall.Signal(reply.Signal))
		time.Sleep(100 * time.Millisecond)
		return 128 + reply.Signal
	}
	return reply.Code
}

// stdioDescriptors returns descriptors for stdin, stdout and stderr; one that
// is closed is replaced by /dev/null.
func stdioDescriptors() ([]int, func()) {
	fds := []int{0, 1, 2}
	var opened []*os.File
	for i, fd := range fds {
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) == nil {
			continue
		}
		if f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
			opened = append(opened, f)
			fds[i] = int(f.Fd())
		}
	}
	return fds, func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}
}

// checkoutSweepEvery is how often the shim re-opens the checkouts below the
// task directory while the agent runs. The sweep only touches the small git
// directories of worktrees, never the working files.
const checkoutSweepEvery = 200 * time.Millisecond

// shareCheckouts opens the git directory of every worktree below root to the
// agent's group.
func shareCheckouts(root string) {
	for _, gitDir := range worktreeGitDirs(root) {
		shareWithGroup(gitDir)
	}
}

// worktreeGitDirs returns the git directories that the checkouts below root
// (git worktrees: a .git file reading "gitdir: <path>") keep in their bare
// repository cache, outside root. Git creates files there, config.worktree
// among them, readable by the daemon only, and git fails for the agent without
// them. Only the worktree's own directory is returned, never the cache itself.
func worktreeGitDirs(root string) []string {
	var dirs []string
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil
	}
	defer r.Close()
	_ = fs.WalkDir(r.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if strings.Count(rel, "/") >= 2 || (d.Name() == ".git" && rel != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != ".git" || !d.Type().IsRegular() {
			return nil
		}
		raw, err := r.ReadFile(rel)
		if err != nil || len(raw) > 4096 {
			return nil
		}
		if target, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: "); ok && filepath.IsAbs(target) {
			dirs = append(dirs, filepath.Clean(target))
		}
		return nil
	})
	return dirs
}

// shareWithGroup gives the owning group read and write access (and search on
// directories) to everything below root that the caller owns. Symbolic links
// are never followed, and .git directories are left alone: git creates their
// content with the umask and they can be large.
func shareWithGroup(root string) {
	uid := os.Getuid()
	r, err := os.OpenRoot(root)
	if err != nil {
		return
	}
	defer r.Close()
	_ = fs.WalkDir(r.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" && rel != "." {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid {
			return nil
		}
		want := fs.FileMode(0o060)
		if info.IsDir() {
			want = 0o070
		}
		if info.Mode().Perm()&want != want {
			_ = r.Chmod(rel, info.Mode().Perm()|want)
		}
		return nil
	})
}
