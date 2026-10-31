package sandboxbroker_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/sandboxbroker"
)

// A Kubernetes Secret volume is delivered group-readable (fsGroup) and owned
// by root, so the owner-only rule cannot hold there. The operator opts in to
// group read, and nothing wider.
func TestWorkspaceConfigFileGroupRead(t *testing.T) {
	t.Parallel()
	body := `{"workspaces":[{"workspace_id":"` + wsA + `","pat":"` + patA + `"}]}`
	load := func(mode os.FileMode, opts ...sandboxbroker.LoadOption) error {
		path := filepath.Join(t.TempDir(), "workspaces.json")
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := sandboxbroker.LoadWorkspaces(path, opts...)
		return err
	}
	if err := load(0o440, sandboxbroker.AllowGroupRead()); err != nil {
		t.Fatalf("group-readable file refused despite the opt-in: %v", err)
	}
	if err := load(0o440); err == nil {
		t.Fatal("group-readable file accepted without the opt-in")
	}
	for _, mode := range []os.FileMode{0o460, 0o404, 0o444, 0o660} {
		if err := load(mode, sandboxbroker.AllowGroupRead()); err == nil {
			t.Fatalf("mode %o must stay refused with the opt-in", mode)
		}
	}
}
