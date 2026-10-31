package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxbroker"
)

const (
	testWorkspaceA = "aaaaaaaa-2222-4333-8444-555555555555"
	testWorkspaceB = "bbbbbbbb-2222-4333-8444-555555555555"
)

func TestLoadPoolReadsTheEnvironment(t *testing.T) {
	env := map[string]string{
		"SANDBOX_WORKSPACES_FILE":    "/etc/pool/workspaces.json",
		"MULTICA_SERVER_URL":         "http://multica:8080",
		"SANDBOX_POOL_SIZE":          "4",
		"SANDBOX_PROVIDERS":          "claude, codex",
		"SANDBOX_POOL_DRAIN_TIMEOUT": "5m",
		"SANDBOX_POOL_ADMIN_LISTEN":  "127.0.0.1:9000",
	}
	s, err := loadPool(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if s.workspacesFile != "/etc/pool/workspaces.json" || s.podListen != ":8790" || s.adminListen != "127.0.0.1:9000" ||
		s.config.ExpectedSize != 4 || len(s.config.Providers) != 2 || s.config.DrainTimeout != 5*time.Minute {
		t.Fatalf("unexpected settings: %+v", s)
	}
	env["SANDBOX_POOL_SIZE"] = "many"
	if _, err := loadPool(func(k string) string { return env[k] }); err == nil {
		t.Fatal("a bad pool size must be refused")
	}
}

func TestSIGHUPStyleReloadChangesTheServedWorkspaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.json")
	write := func(mode os.FileMode, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(id, pat string) string { return `{"workspace_id":"` + id + `","pat":"` + pat + `"}` }
	write(0o600, `{"workspaces":[`+entry(testWorkspaceA, "pat-a")+`]}`)

	workspaces, err := sandboxbroker.LoadWorkspaces(path)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := sandboxbroker.NewPool(sandboxbroker.PoolConfig{MulticaURL: "http://127.0.0.1:1", Workspaces: workspaces}, log)
	if err != nil {
		t.Fatal(err)
	}
	podLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hup := make(chan os.Signal)
	done := make(chan error, 1)
	go func() { done <- servePool(ctx, log, pool, path, podLn, adminLn, nil, hup) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	served := func() []string {
		t.Helper()
		resp, err := http.Get("http://" + adminLn.Addr().String() + "/v1/inventory")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var inv sandboxbroker.Inventory
		if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, w := range inv.Workspaces {
			ids = append(ids, w.WorkspaceID)
		}
		return ids
	}
	if got := served(); len(got) != 1 || got[0] != testWorkspaceA {
		t.Fatalf("serving %v", got)
	}

	// A file readable by others is refused and the old configuration stays.
	write(0o644, `{"workspaces":[`+entry(testWorkspaceA, "pat-a")+`,`+entry(testWorkspaceB, "pat-b")+`]}`)
	hup <- syscall.SIGHUP
	hup <- syscall.SIGHUP // the loop takes the second only after finishing the first
	if got := served(); len(got) != 1 {
		t.Fatalf("a refused file changed the configuration: %v", got)
	}

	write(0o600, `{"workspaces":[`+entry(testWorkspaceA, "pat-a")+`,`+entry(testWorkspaceB, "pat-b")+`]}`)
	hup <- syscall.SIGHUP
	hup <- syscall.SIGHUP
	if got := served(); len(got) != 2 {
		t.Fatalf("the reload did not add B: %v", got)
	}
}
