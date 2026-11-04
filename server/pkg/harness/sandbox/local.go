package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Local is a Sandbox backed by a directory on the host. It isolates nothing; it
// exists so the tools and the loop can be tested without a container runtime.
// Paths are taken relative to the directory, and an absolute path is treated as
// relative to it too, so a test cannot write outside it by accident.
type Local struct {
	root string
}

// NewLocal returns a Local sandbox rooted at dir, which must exist.
func NewLocal(dir string) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("local sandbox root %q is not a directory", dir)
	}
	return &Local{root: abs}, nil
}

func (l *Local) resolve(p string) (string, error) {
	full := filepath.Join(l.root, filepath.FromSlash(p))
	if full != l.root && !strings.HasPrefix(full, l.root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the sandbox", p)
	}
	return full, nil
}

func (l *Local) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	dir, err := l.resolve(req.Cwd)
	if err != nil {
		return ExecResult{}, err
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", req.Command)
	cmd.Dir = dir
	// A killed shell leaves its children holding the output pipes; without a
	// bound, Wait would block until they exit on their own.
	cmd.WaitDelay = time.Second
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exit):
		res.ExitCode = exit.ExitCode()
		res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	default:
		return ExecResult{}, runErr
	}
	return res, nil
}

func (l *Local) Put(_ context.Context, path string, data []byte, mode fs.FileMode) error {
	full, err := l.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, data, mode)
}

func (l *Local) Get(_ context.Context, path string) ([]byte, error) {
	full, err := l.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

func (l *Local) Close(context.Context) error { return nil }
