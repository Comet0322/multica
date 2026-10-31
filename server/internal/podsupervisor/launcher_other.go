//go:build !linux

package podsupervisor

import (
	"errors"
	"log/slog"
	"syscall"
)

const socketEnv = "POD_AGENT_SOCKET"

type launcherConfig struct {
	DaemonUID uint32
	Cred      *syscall.Credential
	Home      string
	TmpDir    string
	ShimDir   string
	RealPath  []string
}

type launcher struct {
	launcherConfig
	socket string
}

func newLauncher(launcherConfig, *slog.Logger) (*launcher, error) {
	return nil, errors.New("the user split requires Linux")
}

func (*launcher) listen() error { return nil }
func (*launcher) shutdown()     {}
func (*launcher) allow(bool)    {}
func (*launcher) owns(int) bool { return false }

// RunShim is not supported off Linux.
func RunShim(name string, args []string) int { return 126 }
