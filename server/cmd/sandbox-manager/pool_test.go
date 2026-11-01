package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxmanager"
)

const (
	testWorkspaceA = "aaaaaaaa-2222-4333-8444-555555555555"
	testWorkspaceB = "bbbbbbbb-2222-4333-8444-555555555555"
)

// fakeMultica lists the workspaces of the account whose PAT is "mul_pat_pool".
type fakeMultica struct {
	*httptest.Server
	mu  sync.Mutex
	ids []string
}

func newFakeMultica(t *testing.T, ids ...string) *fakeMultica {
	t.Helper()
	f := &fakeMultica{ids: ids}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer mul_pat_pool" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/daemon/workspaces" {
			http.NotFound(w, r)
			return
		}
		list := []map[string]any{}
		for _, id := range f.ids {
			list = append(list, map[string]any{"id": id})
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeMultica) set(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = ids
}

func TestLoadPoolReadsTheEnvironment(t *testing.T) {
	env := map[string]string{
		"SANDBOX_POOL_TOKEN":              "mul_pat_pool",
		"MULTICA_SERVER_URL":              "http://multica:8080",
		"SANDBOX_POOL_SIZE":               "4",
		"SANDBOX_PROVIDERS":               "claude, codex",
		"SANDBOX_POOL_DRAIN_TIMEOUT":      "5m",
		"SANDBOX_POOL_ADMIN_LISTEN":       "127.0.0.1:9000",
		"SANDBOX_POOL_MAX_LEASES":         "6",
		"SANDBOX_POOL_SYNC_INTERVAL":      "45s",
		"SANDBOX_POOL_RUNTIME_VISIBILITY": "private",
	}
	s, err := loadPool(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	c := s.config
	if c.Ticket != "mul_pat_pool" || s.podListen != ":8790" || s.adminListen != "127.0.0.1:9000" ||
		c.ExpectedSize != 4 || len(c.Providers) != 2 || c.DrainTimeout != 5*time.Minute ||
		c.MaxLeases != 6 || c.SyncInterval != 45*time.Second || c.RuntimeVisibility != "private" {
		t.Fatalf("unexpected settings: %+v", s)
	}
	env["SANDBOX_POOL_SIZE"] = "many"
	if _, err := loadPool(func(k string) string { return env[k] }); err == nil {
		t.Fatal("a bad pool size must be refused")
	}
}

// servedBy starts servePool for a pool synced once, as runPool does, and
// returns the workspaces the admin listener reports.
func servedBy(t *testing.T, up *fakeMultica, syncInterval time.Duration, hup chan os.Signal) func() []string {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := sandboxmanager.NewPool(sandboxmanager.PoolConfig{
		MulticaURL: up.URL, Ticket: "mul_pat_pool", SyncInterval: syncInterval,
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Sync(context.Background()); err != nil {
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
	done := make(chan error, 1)
	go func() { done <- servePool(ctx, log, pool, podLn, adminLn, nil, hup) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return func() []string {
		t.Helper()
		resp, err := http.Get("http://" + adminLn.Addr().String() + "/v1/inventory")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var inv sandboxmanager.Inventory
		if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, w := range inv.Workspaces {
			ids = append(ids, w.WorkspaceID)
		}
		return ids
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSIGHUPSyncsWorkspacesAtOnce(t *testing.T) {
	up := newFakeMultica(t, testWorkspaceA)
	hup := make(chan os.Signal)
	// An hour between syncs: only the signal can make the change appear.
	served := servedBy(t, up, time.Hour, hup)
	if got := served(); !slices.Equal(got, []string{testWorkspaceA}) {
		t.Fatalf("serving %v", got)
	}

	up.set(testWorkspaceA, testWorkspaceB)
	time.Sleep(100 * time.Millisecond)
	if got := served(); len(got) != 1 {
		t.Fatalf("the change appeared without a signal: %v", got)
	}
	hup <- syscall.SIGHUP
	eventually(t, "the signal to add B", func() bool { return len(served()) == 2 })
}

func TestWorkspacesFollowTheSyncInterval(t *testing.T) {
	up := newFakeMultica(t, testWorkspaceA)
	served := servedBy(t, up, 20*time.Millisecond, make(chan os.Signal))
	up.set(testWorkspaceA, testWorkspaceB)
	eventually(t, "the interval to add B", func() bool { return len(served()) == 2 })
}
