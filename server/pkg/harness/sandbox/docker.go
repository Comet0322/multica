package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Docker is a Sandbox backed by one container that has no network. Every
// operation is a `docker exec` against it, which is the same shape as a vendor
// sandbox API (run a command, move a file), so a second implementation of
// Sandbox against such an API does not change anything above this file.
type Docker struct {
	name string
}

// DockerOptions configures the container.
type DockerOptions struct {
	Image string
	// Name is the container name; empty lets Docker pick one.
	Name string
	// User runs commands as this user (uid[:gid] or a name); empty uses the image's.
	User string
	// MemoryMB and Pids bound the container; zero leaves them unbounded.
	MemoryMB int
	Pids     int
}

// NewDocker starts a container from the image with `--network none` and keeps
// it idle until Close. The image needs `sleep` and a POSIX `sh`.
func NewDocker(ctx context.Context, opts DockerOptions) (*Docker, error) {
	if opts.Image == "" {
		return nil, errors.New("docker sandbox: image is required")
	}
	args := []string{"run", "-d", "--rm", "--network", "none", "--init"}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	if opts.User != "" {
		args = append(args, "--user", opts.User)
	}
	if opts.MemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(opts.MemoryMB)+"m")
	}
	if opts.Pids > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(opts.Pids))
	}
	args = append(args, "--entrypoint", "sleep", opts.Image, "infinity")
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: start: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return &Docker{name: strings.TrimSpace(string(out))}, nil
}

// Name is the container id, for logs and for tests that need to look inside.
func (d *Docker) Name() string { return d.name }

func (d *Docker) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	args := []string{"exec"}
	if req.Cwd != "" {
		args = append(args, "-w", req.Cwd)
	}
	for k, v := range req.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, d.name, "sh", "-c", req.Command)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exit):
		res.ExitCode = exit.ExitCode()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Killing the docker client does not stop the command in the container.
			res.TimedOut = true
			d.killAll()
		}
	default:
		return ExecResult{}, runErr
	}
	return res, nil
}

// killAll stops everything but the container's own idle process after a timeout.
func (d *Docker) killAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "exec", d.name, "sh", "-c",
		"for p in /proc/[0-9]*; do n=${p#/proc/}; [ \"$n\" -gt 1 ] && [ \"$n\" != \"$$\" ] && kill -9 \"$n\" 2>/dev/null; done; true").Run()
}

func (d *Docker) Put(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	// The path is passed as a positional parameter, never spliced into the script.
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", d.name, "sh", "-c",
		`mkdir -p "$(dirname "$1")" && cat > "$1" && chmod "$2" "$1"`, "sh", path, fmt.Sprintf("%o", mode.Perm()))
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker sandbox: put %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *Docker) Get(ctx context.Context, path string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "exec", d.name, "sh", "-c",
		`[ -e "$1" ] || exit 44; cat "$1"`, "sh", path)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 44 {
			return nil, fmt.Errorf("docker sandbox: get %s: %w", path, fs.ErrNotExist)
		}
		return nil, fmt.Errorf("docker sandbox: get %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (d *Docker) Close(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", d.name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker sandbox: remove: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
