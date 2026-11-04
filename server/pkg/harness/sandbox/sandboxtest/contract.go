// Package sandboxtest holds the behaviour every sandbox.Sandbox must have, as a
// test any implementation can run. The tools above the interface rely on all of it.
package sandboxtest

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
)

// Contract runs the shared checks. root is a directory the sandbox may write.
func Contract(t *testing.T, sb sandbox.Sandbox, root string) {
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

	bin := []byte{0, 1, 2, 255, 254, '\n', 0}
	if err := sb.Put(ctx, root+"/bin.dat", bin, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := sb.Get(ctx, root+"/bin.dat"); err != nil || string(got) != string(bin) {
		t.Errorf("binary round trip = %v, %v", got, err)
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
	res, err = sb.Exec(ctx, sandbox.ExecRequest{Command: "sleep 30", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || time.Since(start) > 15*time.Second {
		t.Errorf("timeout: %+v after %v", res, time.Since(start))
	}

	// Killing a command that ran too long must leave the sandbox usable.
	res, err = sb.Exec(ctx, sandbox.ExecRequest{Command: "echo alive"})
	if err != nil || strings.TrimSpace(res.Stdout) != "alive" {
		t.Errorf("sandbox unusable after a timeout: %+v, %v", res, err)
	}
}
