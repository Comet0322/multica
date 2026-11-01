//go:build linux

package podsupervisor

import (
	"os"
	"strconv"
	"strings"
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
