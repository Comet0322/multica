package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadPoolReadsTheTokenFromAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool-token")
	if err := os.WriteFile(path, []byte("mul_pat_from_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"SANDBOX_POOL_TOKEN_FILE": path, "MULTICA_SERVER_URL": "http://multica:8080"}
	s, err := loadPool(envLookup(env))
	if err != nil || s.config.Token != "mul_pat_from_file" {
		t.Fatalf("token from file: %v, %+v", err, s.config.Token)
	}
}

func TestLoadPoolRefusesBadTokenSettings(t *testing.T) {
	cases := map[string]map[string]string{
		"missing":        {"MULTICA_SERVER_URL": "http://multica:8080"},
		"both":           {"SANDBOX_POOL_TOKEN": "mul_pat_x", "SANDBOX_POOL_TOKEN_FILE": "/x", "MULTICA_SERVER_URL": "http://multica:8080"},
		"file not found": {"SANDBOX_POOL_TOKEN_FILE": "/nonexistent/token", "MULTICA_SERVER_URL": "http://multica:8080"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadPool(envLookup(env))
			if err == nil || !strings.Contains(err.Error(), "SANDBOX_POOL_TOKEN") || strings.Contains(err.Error(), "mul_pat_x") {
				t.Fatalf("want an error naming SANDBOX_POOL_TOKEN without the value, got %v", err)
			}
		})
	}
}

func TestLoadPoolRuntimeVisibility(t *testing.T) {
	for _, tc := range []struct{ set, want string }{{"", ""}, {"public", "public"}, {"private", "private"}} {
		env := baseEnv()
		env["SANDBOX_POOL_RUNTIME_VISIBILITY"] = tc.set
		s, err := loadPool(envLookup(env))
		if err != nil || s.config.RuntimeVisibility != tc.want {
			t.Fatalf("visibility %q: %v, got %q", tc.set, err, s.config.RuntimeVisibility)
		}
	}
	for _, bad := range []string{"workspace", "Public", "yes"} {
		env := baseEnv()
		env["SANDBOX_POOL_RUNTIME_VISIBILITY"] = bad
		if _, err := loadPool(envLookup(env)); err == nil || !strings.Contains(err.Error(), "SANDBOX_POOL_RUNTIME_VISIBILITY") {
			t.Fatalf("visibility %q: want an error naming the variable, got %v", bad, err)
		}
	}
}

func TestLoadPoolRefusesBadSyncSettings(t *testing.T) {
	for name, set := range map[string]map[string]string{
		"bad max leases":      {"SANDBOX_POOL_MAX_LEASES": "lots"},
		"negative max leases": {"SANDBOX_POOL_MAX_LEASES": "-1"},
		"bad sync interval":   {"SANDBOX_POOL_SYNC_INTERVAL": "often"},
		"negative interval":   {"SANDBOX_POOL_SYNC_INTERVAL": "-5s"},
	} {
		env := baseEnv()
		for k, v := range set {
			env[k] = v
		}
		_, err := loadPool(envLookup(env))
		var name2 string
		for k := range set {
			name2 = k
		}
		if err == nil || !strings.Contains(err.Error(), name2) {
			t.Errorf("%s: want an error naming %s, got %v", name, name2, err)
		}
	}
}

func TestStartupStopsWhenTheFirstWorkspaceListingFails(t *testing.T) {
	up := newFakeMultica(t, testWorkspaceA)
	env := baseEnv()
	env["SANDBOX_POOL_TOKEN"] = "wrong-token"
	env["MULTICA_SERVER_URL"] = up.URL
	s, err := loadPool(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	s.podListen, s.adminListen = "127.0.0.1:0", "127.0.0.1:0"
	err = runPool(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), s)
	if err == nil || !strings.Contains(err.Error(), "list the account's workspaces") {
		t.Fatalf("a rejected token must stop the manager, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong-token") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

// TestMainSelectsTheModeFromTheToken re-runs this test binary as the
// sandbox-broker command and looks at which mode it reports.
func TestMainSelectsTheModeFromTheToken(t *testing.T) {
	if os.Getenv("SANDBOX_TEST_RUN_MAIN") == "1" {
		main()
		return
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("mul_pat_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(env ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMainSelectsTheModeFromTheToken$")
		// The listing goes to a closed port, so a pool manager stops at once.
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "SANDBOX_TEST_RUN_MAIN=1", "MULTICA_SERVER_URL=http://127.0.0.1:1"}, env...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected a configuration or startup error, output:\n%s", out)
		}
		return string(out)
	}
	if out := run("SANDBOX_POOL_TOKEN=mul_pat_x"); !strings.Contains(out, "sandbox pool manager stopped") || strings.Contains(out, "mul_pat_x") {
		t.Fatalf("SANDBOX_POOL_TOKEN must select the pool manager:\n%s", out)
	}
	if out := run("SANDBOX_POOL_TOKEN_FILE=" + tokenFile); !strings.Contains(out, "sandbox pool manager stopped") {
		t.Fatalf("SANDBOX_POOL_TOKEN_FILE must select the pool manager:\n%s", out)
	}
	// The retired variable no longer selects the pool manager.
	if out := run("SANDBOX_WORKSPACES_FILE=/etc/pool/workspaces.json"); strings.Contains(out, "sandbox pool manager stopped") || !strings.Contains(out, "invalid sandbox broker configuration") {
		t.Fatalf("without a token the single-workspace broker must run:\n%s", out)
	}
	if out := run(); !strings.Contains(out, "invalid sandbox broker configuration") {
		t.Fatalf("without a token the single-workspace broker must run:\n%s", out)
	}
}
