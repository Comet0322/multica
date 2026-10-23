//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func tryLockDaemonProfile(path string) (*os.File, bool, error) {
	// #nosec G304 -- path derives from operator config or a daemon-owned directory
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}
