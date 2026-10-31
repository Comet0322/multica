package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxbroker"
)

// poolSettings is the pool manager's command-line environment. The manager is
// selected by SANDBOX_WORKSPACES_FILE; without it the single-workspace broker
// runs.
type poolSettings struct {
	workspacesFile string
	podListen      string
	adminListen    string
	config         sandboxbroker.PoolConfig
}

func loadPool(getenv func(string) string) (poolSettings, error) {
	var errs []string
	duration := func(name string) time.Duration {
		raw := getenv(name)
		if raw == "" {
			return 0
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			errs = append(errs, name+" must be a duration such as 10m")
		}
		return d
	}
	size := 0
	if raw := getenv("SANDBOX_POOL_SIZE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			errs = append(errs, "SANDBOX_POOL_SIZE must be a whole number")
		}
		size = n
	}
	var providers []string
	for _, item := range strings.Split(getenv("SANDBOX_PROVIDERS"), ",") {
		if item = strings.TrimSpace(item); item != "" {
			providers = append(providers, item)
		}
	}
	s := poolSettings{
		workspacesFile: getenv("SANDBOX_WORKSPACES_FILE"),
		podListen:      getenv("SANDBOX_BROKER_LISTEN"),
		adminListen:    getenv("SANDBOX_POOL_ADMIN_LISTEN"),
		config: sandboxbroker.PoolConfig{
			MulticaURL:   getenv("MULTICA_SERVER_URL"),
			Name:         getenv("SANDBOX_BROKER_NAME"),
			ExpectedSize: size,
			Providers:    providers,
			DrainTimeout: duration("SANDBOX_POOL_DRAIN_TIMEOUT"),
		},
	}
	if s.podListen == "" {
		s.podListen = ":8790"
	}
	if s.adminListen == "" {
		// The admin interface is not for pods; stay on loopback by default.
		s.adminListen = "127.0.0.1:8791"
	}
	if len(errs) > 0 {
		return s, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return s, nil
}

// runPool starts the pool manager and reloads its workspace file on SIGHUP
// until ctx ends.
func runPool(ctx context.Context, log *slog.Logger, s poolSettings) error {
	workspaces, err := sandboxbroker.LoadWorkspaces(s.workspacesFile)
	if err != nil {
		return err
	}
	s.config.Workspaces = workspaces
	pool, err := sandboxbroker.NewPool(s.config, log)
	if err != nil {
		return err
	}
	podLn, err := net.Listen("tcp", s.podListen)
	if err != nil {
		return fmt.Errorf("listen for pods: %w", err)
	}
	defer podLn.Close()
	adminLn, err := net.Listen("tcp", s.adminListen)
	if err != nil {
		return fmt.Errorf("listen for administrators: %w", err)
	}
	defer adminLn.Close()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	return servePool(ctx, log, pool, s.workspacesFile, podLn, adminLn, hup)
}

// servePool serves the pod and admin APIs and applies the workspace file each
// time reload fires. A reload that fails is logged and changes nothing.
func servePool(ctx context.Context, log *slog.Logger, pool *sandboxbroker.Pool, path string, podLn, adminLn net.Listener, reload <-chan os.Signal) error {
	servers := []*http.Server{
		{Handler: pool.PodHandler(), ReadHeaderTimeout: 10 * time.Second},
		{Handler: pool.AdminHandler(), ReadHeaderTimeout: 10 * time.Second},
	}
	serveErr := make(chan error, 2)
	for i, ln := range []net.Listener{podLn, adminLn} {
		go func() { serveErr <- servers[i].Serve(ln) }()
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(shutdownCtx)
		}
	}()
	log.InfoContext(ctx, "sandbox pool manager started", "pod_listen", podLn.Addr().String(), "admin_listen", adminLn.Addr().String())
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serveErr:
			return fmt.Errorf("pool API stopped: %w", err)
		case <-reload:
			log.InfoContext(ctx, "reloading workspace configuration")
			_, _ = pool.ReloadFile(ctx, path)
		}
	}
}
