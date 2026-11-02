//go:build linux

package podsupervisor

import (
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const prSetChildSubreaper = 36

// becomeSubreaper makes orphaned descendants reparent to this process even
// when it is not PID 1, so the same code path works in a container and in
// tests.
func becomeSubreaper() error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

type procInfo struct {
	pid, ppid int
	zombie    bool
}

// listProcs reads every process from /proc. Entries that vanish mid-scan are
// skipped.
func listProcs() []procInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []procInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// The command name may contain spaces and parentheses; the fixed
		// fields start after the last ")".
		s := string(raw)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(s[i+1:])
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		out = append(out, procInfo{pid: pid, ppid: ppid, zombie: fields[0] == "Z"})
	}
	return out
}

// descendants returns every process below root, including orphans that were
// reparented to root and processes that called setsid.
func descendants(root int) []procInfo {
	all := listProcs()
	children := make(map[int][]procInfo)
	for _, p := range all {
		children[p.ppid] = append(children[p.ppid], p)
	}
	var out []procInfo
	queue := []int{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			out = append(out, c)
			queue = append(queue, c.pid)
		}
	}
	return out
}

func killProc(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// reapProc collects an exited child so it does not linger as a zombie. It
// fails harmlessly for processes that are not our children.
func reapProc(pid int) {
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
}

// setSharedUmask makes the processes started from here create files that the
// pod's shared group can read and write, and nobody else can touch: the daemon
// and the agents run as different users that share only that group.
func setSharedUmask() { syscall.Umask(0o007) }

// setPrivateUmask keeps the files of a single-user pod to that user.
func setPrivateUmask() { syscall.Umask(0o077) }

const prSetDumpable = 4

// denyPeerAccess clears the dumpable flag of this process. Processes of the
// same user then cannot ptrace it or read its /proc entries (they become
// root's). The flag is reset by execve, so the daemon and agents started from
// here are ordinary processes.
func denyPeerAccess() error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// ignoreStraySignals makes the signals that would end a Go program, other than
// the SIGINT and SIGTERM the container runtime sends, do nothing, so a task
// cannot stop its pod's supervisor by signalling PID 1. (SIGKILL and SIGSTOP
// from inside a pid namespace are dropped by the kernel for its init.)
func ignoreStraySignals() {
	signal.Ignore(syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGABRT, syscall.SIGUSR1,
		syscall.SIGUSR2, syscall.SIGPIPE, syscall.SIGALRM, syscall.SIGVTALRM, syscall.SIGPROF,
		syscall.SIGXCPU, syscall.SIGXFSZ, syscall.SIGIO, syscall.SIGPWR)
}

// reserveThreads starts n OS threads now and keeps them. A task that fills the
// container's process limit (a fork bomb) leaves the supervisor unable to start
// a thread, and the Go runtime then aborts the whole program; with idle threads
// already in hand the supervisor never needs a new one.
func reserveThreads(n int) {
	var started, release sync.WaitGroup
	release.Add(1)
	started.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			// Locked goroutines each hold a thread of their own; unlocking
			// before exit returns the thread to the runtime's idle pool.
			runtime.LockOSThread()
			started.Done()
			release.Wait()
			runtime.UnlockOSThread()
		}()
	}
	started.Wait()
	release.Done()
}
