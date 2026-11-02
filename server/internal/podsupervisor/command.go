package podsupervisor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// spec describes one program to start: the supervisor's daemon, or an agent
// CLI on behalf of the daemon.
type spec struct {
	program string   // resolved against PATH when it has no slash
	argv0   string   // what the program sees as its own name
	args    []string // arguments after argv0
	attr    *os.ProcAttr
}

// child is a started program.
type child struct {
	Process *os.Process
}

// startChild starts the program a spec describes. The supervisor starts only
// programs its own configuration names (the daemon command and the agent CLIs
// found on the pod's PATH), never anything a task supplies. It still resolves
// the program to an absolute path and refuses anything that is not an
// executable regular file, so a typo or a directory fails here with a clear
// message instead of at exec time.
func startChild(sp *spec) (*child, error) {
	resolved, err := exec.LookPath(sp.program)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("%s is not an executable file", abs)
	}
	p, err := os.StartProcess(abs, append([]string{sp.argv0}, sp.args...), sp.attr)
	if err != nil {
		return nil, err
	}
	return &child{Process: p}, nil
}

// Wait waits for the program to end. Like exec.Cmd.Wait it returns nil for a
// clean exit and an *exec.ExitError otherwise.
func (c *child) Wait() error {
	state, err := c.Process.Wait()
	if err != nil {
		return err
	}
	if !state.Success() {
		return &exec.ExitError{ProcessState: state}
	}
	return nil
}
