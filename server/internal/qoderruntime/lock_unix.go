//go:build !windows

package qoderruntime

import (
	"fmt"
	"github.com/multica-ai/multica/server/internal/util"
	"golang.org/x/sys/unix"
	"os"
)

func lockState(path string) (*os.File, error) {
	f, err := util.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("bridge state is already locked: %w", err)
	}
	return f, nil
}

func syncStateDir(path string) error {
	d, err := util.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
