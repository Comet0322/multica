package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfig_ManagedRequiresTokenFileAndDisablesLocalFeatures(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	t.Setenv("MULTICA_DAEMON_AUTO_UPDATE", "true")
	t.Setenv("MULTICA_DAEMON_AUTO_RELOAD", "true")
	base := Overrides{ServerURL: "http://localhost:0", WorkspacesRoot: t.TempDir(), Managed: true}

	if _, err := LoadConfig(base); err == nil || !strings.Contains(err.Error(), "token file") {
		t.Fatalf("managed without token file: err=%v", err)
	}

	base.TokenFile = filepath.Join(t.TempDir(), "token")
	cfg, err := LoadConfig(base)
	if err != nil {
		t.Fatalf("LoadConfig managed: %v", err)
	}
	if !cfg.Managed || cfg.TokenFile != base.TokenFile {
		t.Fatalf("managed=%v tokenFile=%q", cfg.Managed, cfg.TokenFile)
	}
	if cfg.AutoUpdateEnabled || cfg.AutoReloadEnabled {
		t.Fatalf("managed must force auto-update/reload off even when env enables them: update=%v reload=%v", cfg.AutoUpdateEnabled, cfg.AutoReloadEnabled)
	}
	if cfg.DrainTimeout != DefaultManagedDrainTimeout {
		t.Fatalf("DrainTimeout=%s", cfg.DrainTimeout)
	}

	t.Setenv("MULTICA_DAEMON_DRAIN_TIMEOUT", "45s")
	cfg, _ = LoadConfig(base)
	if cfg.DrainTimeout != 45*time.Second {
		t.Fatalf("DrainTimeout=%s want 45s", cfg.DrainTimeout)
	}
}

func TestLoadConfig_ManagedFromEnv(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	t.Setenv("MULTICA_DAEMON_MANAGED", "true")
	t.Setenv("MULTICA_DAEMON_TOKEN_FILE", "/run/secrets/mdt")
	cfg, err := LoadConfig(Overrides{ServerURL: "http://localhost:0", WorkspacesRoot: t.TempDir()})
	if err != nil || !cfg.Managed || cfg.TokenFile != "/run/secrets/mdt" {
		t.Fatalf("cfg.Managed=%v TokenFile=%q err=%v", cfg.Managed, cfg.TokenFile, err)
	}
}

func TestLoadTokenFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	writeToken(t, good, "mdt_abc123")
	if tok, err := loadTokenFile(good); err != nil || tok != "mdt_abc123" {
		t.Fatalf("good token: %q %v", tok, err)
	}
	for name, content := range map[string]string{"pat": "mul_abc", "task": "mat_abc", "empty": ""} {
		p := filepath.Join(dir, name)
		writeToken(t, p, content)
		if _, err := loadTokenFile(p); err == nil {
			t.Errorf("%s token must be rejected", name)
		}
	}
	if _, err := loadTokenFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file must error")
	}
}

func TestResolveAuthManagedUsesTokenFileNotCLIConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.multica/config.json at all
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "mdt_first")
	d := &Daemon{cfg: Config{Managed: true, TokenFile: path}, logger: discardLogger(), client: NewClient("http://localhost:0")}
	if err := d.resolveAuth(); err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if d.client.Token() != "mdt_first" {
		t.Fatalf("token=%q", d.client.Token())
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".multica")); err == nil {
		t.Fatal("managed mode must not write a CLI config")
	}
}

func TestTokenFileLoopRotatesAndKeepsTokenOnBadRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "mdt_one")
	d := &Daemon{cfg: Config{Managed: true, TokenFile: path, TokenFilePollInterval: 10 * time.Millisecond}, logger: discardLogger(), client: NewClient("http://localhost:0")}
	if err := d.resolveAuth(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.tokenFileLoop(ctx)

	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if d.client.Token() == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("token=%q want %q", d.client.Token(), want)
	}
	writeToken(t, path, "mdt_two")
	waitFor("mdt_two")

	writeToken(t, path, "garbage") // invalid content must not replace a good token
	time.Sleep(60 * time.Millisecond)
	if d.client.Token() != "mdt_two" {
		t.Fatalf("token replaced by invalid file content: %q", d.client.Token())
	}
	os.Remove(path)
	time.Sleep(60 * time.Millisecond)
	if d.client.Token() != "mdt_two" {
		t.Fatalf("token cleared by missing file: %q", d.client.Token())
	}
}

func TestClientTokenSwapIsRaceFree(t *testing.T) {
	c := NewClient("http://localhost:0")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.SetToken("mdt_x")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = c.Token()
			}
		}()
	}
	wg.Wait()
}

func newDrainDaemon(timeout time.Duration) (*Daemon, context.CancelFunc, context.Context) {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	d := &Daemon{cfg: Config{Managed: true, DrainTimeout: timeout}, logger: discardLogger(), rootCtx: rootCtx}
	return d, rootCancel, rootCtx
}

func TestDrainWaitsForRunningTaskThenCancels(t *testing.T) {
	d, rootCancel, rootCtx := newDrainDaemon(5 * time.Second)
	sigCtx, signal := context.WithCancel(context.Background())
	d.activeTasks.Store(1)
	go d.drainOnShutdown(sigCtx, rootCancel)

	signal()
	time.Sleep(700 * time.Millisecond)
	if !d.draining.Load() {
		t.Fatal("daemon must be draining after the signal")
	}
	if rootCtx.Err() != nil {
		t.Fatal("daemon context cancelled while a task was still running")
	}
	d.activeTasks.Store(0)
	select {
	case <-rootCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("daemon not cancelled after the last task finished")
	}
}

func TestDrainTimeoutCancelsRunningTasks(t *testing.T) {
	d, rootCancel, rootCtx := newDrainDaemon(300 * time.Millisecond)
	sigCtx, signal := context.WithCancel(context.Background())
	d.activeTasks.Store(1)
	go d.drainOnShutdown(sigCtx, rootCancel)
	signal()
	select {
	case <-rootCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("drain timeout did not cancel the daemon")
	}
}

func TestDrainWithNoTasksCancelsImmediately(t *testing.T) {
	d, rootCancel, rootCtx := newDrainDaemon(time.Minute)
	sigCtx, signal := context.WithCancel(context.Background())
	go d.drainOnShutdown(sigCtx, rootCancel)
	signal()
	select {
	case <-rootCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("idle daemon should stop at once")
	}
}
