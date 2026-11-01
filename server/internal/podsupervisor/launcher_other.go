//go:build !linux

package podsupervisor

import (
	"context"
	"errors"
)

// Runner is not supported off Linux.
type Runner struct{}

// NewRunner is not supported off Linux.
func NewRunner(RunnerConfig) (*Runner, error) {
	return nil, errors.New("the agent runner requires Linux")
}

// Run is not supported off Linux.
func (*Runner) Run(context.Context) error { return errors.New("the agent runner requires Linux") }

// RunShim is not supported off Linux.
func RunShim(name string, args []string) int { return 126 }
