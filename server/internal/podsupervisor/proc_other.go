//go:build !linux

package podsupervisor

import "errors"

type procInfo struct {
	pid, ppid int
	zombie    bool
}

func becomeSubreaper() error { return errors.New("pod supervisor requires Linux") }

func listProcs() []procInfo { return nil }

func descendants(int) []procInfo { return nil }

func killProc(int) {}

func reapProc(int) {}

func setSharedUmask() {}

func setPrivateUmask() {}

func denyPeerAccess() error { return nil }

func ignoreStraySignals() {}
