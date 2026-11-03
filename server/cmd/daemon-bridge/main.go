// Command daemon-bridge registers a runtime with a Multica server over the
// daemon protocol and runs claimed tasks on a pluggable backend.
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/multica-ai/multica/server/internal/daemonbridge"
	"github.com/multica-ai/multica/server/internal/daemonbridge/oma"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "daemon-bridge:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		server  = flag.String("server", env("MULTICA_SERVER_URL", ""), "Multica server URL")
		pat     = flag.String("token", "", "personal access token (default: $MULTICA_TOKEN)")
		id      = flag.String("daemon-id", env("MULTICA_BRIDGE_ID", ""), "stable daemon ID")
		state   = flag.String("state-dir", env("MULTICA_BRIDGE_STATE", ""), "journal directory")
		cliPath = flag.String("multica", env("MULTICA_BRIDGE_CLI", "multica"), "multica CLI used as the task tool")
		backend = flag.String("backend", env("MULTICA_BRIDGE_BACKEND", ""), "backend: oma")
		wsList  = flag.String("workspaces", "", "comma-separated workspace IDs (default: all)")
		health  = flag.String("health", "", "address for /healthz, for example :19514")
		conc    = flag.Int("concurrency", 4, "tasks run at once")
		mcpAddr = flag.String("mcp-listen", "", "address for the per-task MCP tool server, for example :19515 (empty disables it)")
		mcpURL  = flag.String("mcp-url", "", "URL the engine uses to reach --mcp-listen (default http://127.0.0.1<port>)")
	)
	flag.Parse()
	if *pat == "" {
		*pat = os.Getenv("MULTICA_TOKEN")
	}
	if *server == "" || *pat == "" || *id == "" {
		return fmt.Errorf("--server, --token (or $MULTICA_TOKEN) and --daemon-id are required")
	}
	if *state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		*state = filepath.Join(home, ".multica", "bridge", *id)
	}
	be, err := newBackend(*backend)
	if err != nil {
		return err
	}
	cli := &daemonbridge.CLITool{Binary: *cliPath, ServerURL: *server}
	cfg := daemonbridge.Config{
		ServerURL: *server, PAT: *pat, DaemonID: *id, StateDir: *state,
		MaxConcurrent: *conc, CLIVersion: "daemon-bridge", Tools: cli,
	}
	var toolSrv *http.Server
	if *mcpAddr != "" {
		secret, err := stateSecret(*state)
		if err != nil {
			return err
		}
		url := *mcpURL
		if url == "" {
			url = "http://" + *mcpAddr
			if strings.HasPrefix(*mcpAddr, ":") {
				url = "http://127.0.0.1" + *mcpAddr
			}
		}
		schema, err := daemonbridge.LoadSchema(context.Background(), *cliPath)
		if err != nil {
			return fmt.Errorf("load tool schema: %w", err)
		}
		tools := daemonbridge.NewToolServer(cli, url, schema)
		tools.Secret = secret
		mux := http.NewServeMux()
		mux.Handle("/mcp", tools)
		toolSrv = &http.Server{Addr: *mcpAddr, Handler: mux}
		cfg.Tools = tools
	}
	if *wsList != "" {
		cfg.Workspaces = strings.Split(*wsList, ",")
	}
	br, err := daemonbridge.New(cfg, be)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *health != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
		srv := &http.Server{Addr: *health, Handler: mux}
		go func() { _ = srv.ListenAndServe() }()
		go func() { <-ctx.Done(); _ = srv.Close() }()
	}
	if toolSrv != nil {
		go func() {
			if err := toolSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("tool server", "error", err)
				stop()
			}
		}()
		go func() { <-ctx.Done(); _ = toolSrv.Close() }()
	}
	slog.Info("daemon-bridge started", "server", *server, "daemon_id", *id, "backend", *backend)
	return br.Run(ctx)
}

func newBackend(name string) (daemonbridge.Backend, error) {
	switch name {
	case "oma":
		return oma.New(oma.Config{
			BaseURL:       os.Getenv("OMA_URL"),
			Auth:          os.Getenv("OMA_API_KEY"),
			EnvironmentID: os.Getenv("OMA_ENVIRONMENT_ID"),
			Model:         os.Getenv("OMA_MODEL"),
		})
	case "":
		return nil, fmt.Errorf("--backend is required (available: oma)")
	default:
		return nil, fmt.Errorf("unknown backend %q (available: oma)", name)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// stateSecret returns the bridge's persistent secret, creating it on first use.
func stateSecret(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "tool-secret")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, os.WriteFile(path, b, 0o600)
}
