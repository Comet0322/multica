package sandbox_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
)

// contract is what every Sandbox must do; the tools above it rely on all of it.
func contract(t *testing.T, sb sandbox.Sandbox, root string) {
	t.Helper()
	ctx := context.Background()

	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: "echo out; echo err >&2; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Errorf("exec = %+v, want exit 3, stdout out, stderr err", res)
	}

	res, err = sb.Exec(ctx, sandbox.ExecRequest{Command: "echo $GREETING", Env: map[string]string{"GREETING": "hi"}})
	if err != nil || strings.TrimSpace(res.Stdout) != "hi" {
		t.Errorf("env not passed: %+v, %v", res, err)
	}

	if err := sb.Put(ctx, root+"/a/b/c.txt", []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err := sb.Get(ctx, root+"/a/b/c.txt")
	if err != nil || string(got) != "hello" {
		t.Errorf("get after put = %q, %v", got, err)
	}
	res, err = sb.Exec(ctx, sandbox.ExecRequest{Command: "cat a/b/c.txt", Cwd: root})
	if err != nil || res.Stdout != "hello" {
		t.Errorf("cwd not honoured: %+v, %v", res, err)
	}

	if _, err := sb.Get(ctx, root+"/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: err = %v, want fs.ErrNotExist", err)
	}

	// A path with shell metacharacters is a file name, not code.
	odd := root + "/it's $(touch pwned) `x`.txt"
	if err := sb.Put(ctx, odd, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Get(ctx, root+"/pwned"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a path was executed as code: %v", err)
	}

	start := time.Now()
	res, err = sb.Exec(ctx, sandbox.ExecRequest{Command: "sleep 30", Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || time.Since(start) > 10*time.Second {
		t.Errorf("timeout: %+v after %v", res, time.Since(start))
	}
}

func TestLocalSandbox(t *testing.T) {
	dir := t.TempDir()
	sb, err := sandbox.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	contract(t, sb, "work")

	if err := sb.Put(context.Background(), "../escape", []byte("x"), 0o644); err == nil {
		t.Error("a path outside the root was written")
	}
}

// TestDockerSandbox runs the same contract in a container, and checks the one
// property that makes it a sandbox: no network. It needs HARNESS_DOCKER_IMAGE,
// an image with sh and sleep.
func TestDockerSandbox(t *testing.T) {
	image := os.Getenv("HARNESS_DOCKER_IMAGE")
	if image == "" {
		t.Skip("HARNESS_DOCKER_IMAGE not set")
	}
	ctx := context.Background()
	sb, err := sandbox.NewDocker(ctx, sandbox.DockerOptions{Image: image, MemoryMB: 256, Pids: 256})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close(context.Background()) })
	contract(t, sb, "/tmp/work")

	// `none` still leaves lo and, on some kernels, a tunl0 that is down and has no
	// route. What matters is that no interface besides lo is up.
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: `for i in $(ls /sys/class/net); do [ "$i" = lo ] && continue; echo "$i $(cat /sys/class/net/$i/operstate)"; done`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, " up") {
		t.Errorf("sandbox has a network interface that is up:\n%s", res.Stdout)
	}
}
