//go:build linux

package podsupervisor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	sysvIPCDir = "/proc/sysvipc"
	mqueueDir  = "/dev/mqueue"
	ipcRmid    = 0
)

// sysvIDs returns the ids in a /proc/sysvipc table: a header line, then one
// object per line with the key first and the id second.
func sysvIDs(r io.Reader) []int {
	var ids []int
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		if id, err := strconv.Atoi(f[1]); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// cleanIPC removes every System V shared memory segment, message queue and
// semaphore set, and every POSIX message queue. They belong to the IPC
// namespace, not to a process, so they outlive the processes that made them and
// would carry data from one task to the next. Call it only where the container
// has its own IPC namespace: it removes whatever the namespace holds.
func cleanIPC() error {
	var errs []error
	for _, t := range []struct {
		table string
		nr    uintptr
	}{{"shm", unix.SYS_SHMCTL}, {"msg", unix.SYS_MSGCTL}, {"sem", unix.SYS_SEMCTL}} {
		f, err := os.Open(filepath.Join(sysvIPCDir, t.table))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		ids := sysvIDs(f)
		f.Close()
		for _, id := range ids {
			// shmctl and msgctl take (id, cmd); semctl takes (id, semnum, cmd).
			a1, a2 := uintptr(ipcRmid), uintptr(0)
			if t.table == "sem" {
				a1, a2 = 0, ipcRmid
			}
			_, _, errno := unix.Syscall(t.nr, uintptr(id), a1, a2)
			// EINVAL and EIDRM: already gone.
			if errno != 0 && errno != syscall.EINVAL && errno != syscall.EIDRM {
				errs = append(errs, fmt.Errorf("remove %s %d: %w", t.table, id, errno))
			}
		}
	}
	entries, err := os.ReadDir(mqueueDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(mqueueDir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
