package sandbox_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
	"github.com/multica-ai/multica/server/pkg/harness/sandbox/sandboxtest"
)

func TestLocalSandbox(t *testing.T) {
	sb, err := sandbox.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sandboxtest.Contract(t, sb, "work")

	if err := sb.Put(context.Background(), "../escape", []byte("x"), 0o644); err == nil {
		t.Error("a path outside the root was written")
	}
}

func TestSweepRemovesOnlyLabelledContainers(t *testing.T) {
	image := os.Getenv("HARNESS_DOCKER_IMAGE")
	if image == "" {
		t.Skip("HARNESS_DOCKER_IMAGE not set")
	}
	ctx := context.Background()
	mine, err := sandbox.NewDocker(ctx, sandbox.DockerOptions{Image: image, Labels: map[string]string{"harness.test": "sweep-a"}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := sandbox.NewDocker(ctx, sandbox.DockerOptions{Image: image, Labels: map[string]string{"harness.test": "sweep-b"}})
	if err != nil {
		_ = mine.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close(context.Background()); _ = mine.Close(context.Background()) })

	if n, err := sandbox.SweepDocker(ctx, "harness.test", "sweep-a"); err != nil || n != 1 {
		t.Fatalf("swept %d, %v; want 1", n, err)
	}
	if _, err := mine.Exec(ctx, sandbox.ExecRequest{Command: "true"}); err == nil {
		t.Error("the swept container still answers")
	}
	if res, err := other.Exec(ctx, sandbox.ExecRequest{Command: "true"}); err != nil || res.ExitCode != 0 {
		t.Errorf("a container with another label was removed: %v", err)
	}
	if n, err := sandbox.SweepDocker(ctx, "harness.test", "sweep-a"); err != nil || n != 0 {
		t.Errorf("second sweep: %d, %v", n, err)
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
	sandboxtest.Contract(t, sb, "/tmp/work")

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
