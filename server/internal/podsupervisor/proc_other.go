//go:build !linux

package podsupervisor

import (
	"errors"
	"syscall"
)

type procInfo struct {
	pid, ppid int
	zombie    bool
}

func becomeSubreaper() error { return errors.New("pod supervisor requires Linux") }

func descendants(int) []procInfo { return nil }

func killProc(int) {}

func reapProc(int) {}

func credentialAttr(uid, gid uint32) *syscall.SysProcAttr { return nil }

func setSharedUmask() {}
