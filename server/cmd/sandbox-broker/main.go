// sandbox-broker exposes a managed Multica runtime whose tasks run in
// sandboxes executing the stock Multica daemon. See deploy/sandbox/README.md.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxbroker"
	"github.com/multica-ai/multica/server/internal/sandboxprovider"
)

// sandboxEnvPrefix marks broker environment variables forwarded to every
// sandbox with the prefix removed, e.g. SANDBOX_ENV_ANTHROPIC_BASE_URL. Empty
// values are skipped so an unset compose variable does not override defaults.
const sandboxEnvPrefix = "SANDBOX_ENV_"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, provider, err := load(os.Getenv, os.Environ())
	if err != nil {
		log.Error("invalid sandbox broker configuration", "error", err)
		os.Exit(1)
	}
	b, err := sandboxbroker.New(cfg, provider, log)
	if err != nil {
		log.Error("invalid sandbox broker configuration", "error", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := b.Run(ctx); err != nil {
		log.ErrorContext(ctx, "sandbox broker stopped", "error", err)
		os.Exit(1)
	}
}

func load(getenv func(string) string, environ []string) (sandboxbroker.Config, sandboxprovider.Provider, error) {
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
	number := func(name string) float64 {
		raw := getenv(name)
		if raw == "" {
			return 0
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			errs = append(errs, name+" must be a number")
		}
		return n
	}
	list := func(name string) []string {
		var out []string
		for _, item := range strings.Split(getenv(name), ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
		return out
	}

	cfg := sandboxbroker.Config{
		MulticaURL:     getenv("MULTICA_SERVER_URL"),
		MulticaToken:   getenv("MULTICA_TOKEN"),
		WorkspaceID:    getenv("MULTICA_WORKSPACE_ID"),
		BrokerID:       getenv("SANDBOX_BROKER_ID"),
		Name:           getenv("SANDBOX_BROKER_NAME"),
		ListenAddr:     getenv("SANDBOX_BROKER_LISTEN"),
		SandboxURL:     getenv("SANDBOX_BROKER_URL"),
		Image:          getenv("SANDBOX_IMAGE"),
		Providers:      list("SANDBOX_PROVIDERS"),
		StateDir:       getenv("SANDBOX_BROKER_STATE_DIR"),
		MaxConcurrency: int(number("SANDBOX_MAX_CONCURRENCY")),
		IdleTimeout:    duration("SANDBOX_IDLE_TIMEOUT"),
		StartTimeout:   duration("SANDBOX_START_TIMEOUT"),
		Isolation:      sandboxbroker.Isolation(getenv("SANDBOX_ISOLATION")),
		CleanupGrace:   duration("SANDBOX_CLEANUP_GRACE"),
		VolumeTTL:      duration("SANDBOX_VOLUME_TTL"),
		PollInterval:   duration("SANDBOX_BROKER_POLL_INTERVAL"),
		SandboxEnv:     map[string]string{},
	}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if key, ok := strings.CutPrefix(name, sandboxEnvPrefix); ok && key != "" && value != "" {
			cfg.SandboxEnv[key] = value
		}
	}

	var provider sandboxprovider.Provider
	switch kind := getenv("SANDBOX_PROVIDER"); kind {
	case "", "docker":
		instance := getenv("SANDBOX_DOCKER_INSTANCE")
		if instance == "" {
			instance = "default"
		}
		d, err := sandboxprovider.NewDocker(sandboxprovider.DockerConfig{
			Instance:   instance,
			SocketPath: getenv("DOCKER_SOCKET"),
			Network:    getenv("SANDBOX_DOCKER_NETWORK"),
			ExtraHosts: list("SANDBOX_DOCKER_EXTRA_HOSTS"),
			Runtime:    getenv("SANDBOX_DOCKER_RUNTIME"),
			MemoryMB:   int64(number("SANDBOX_MEMORY_MB")),
			CPUs:       number("SANDBOX_CPUS"),
		})
		if err != nil {
			errs = append(errs, err.Error())
		}
		provider = d
	default:
		errs = append(errs, fmt.Sprintf("unknown SANDBOX_PROVIDER %q", kind))
	}
	if len(errs) > 0 {
		return cfg, nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return cfg, provider, nil
}
