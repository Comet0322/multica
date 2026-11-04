// Command sandbox-api serves the sandbox API (see pkg/harness/sandboxapi) in
// front of Docker containers that have no network. It is the only place that
// needs a container runtime; the agent service reaches sandboxes through it.
//
// Configuration, all from the environment:
//
//	SANDBOX_API_TOKEN       bearer token clients must present (required)
//	SANDBOX_API_ADDR        listen address (default :8090)
//	SANDBOX_API_IMAGES      comma-separated images clients may ask for; the first
//	                        is the default (required)
//	SANDBOX_API_INSTANCE    names this instance on its containers so a restart can
//	                        remove the ones a crash left behind (default "default");
//	                        two instances on one Docker host need different names
//	SANDBOX_API_MAX         most sandboxes at once (default 50)
//	SANDBOX_API_IDLE_TTL    unused time before a sandbox is deleted (default 2h)
//	SANDBOX_API_MAX_MEMORY_MB, SANDBOX_API_MAX_PIDS
//	                        per-sandbox ceilings; a client may ask for less
//	                        (defaults 4096 and 1024)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
	"github.com/multica-ai/multica/server/pkg/harness/sandboxapi"
)

const instanceLabel = "multica.sandbox-api"

func main() {
	if err := run(); err != nil {
		slog.Error("sandbox-api", "error", err)
		os.Exit(1)
	}
}

func run() error {
	token := os.Getenv("SANDBOX_API_TOKEN")
	images := splitList(os.Getenv("SANDBOX_API_IMAGES"))
	if token == "" || len(images) == 0 {
		return errors.New("SANDBOX_API_TOKEN and SANDBOX_API_IMAGES are required")
	}
	instance := env("SANDBOX_API_INSTANCE", "default")
	maxMem := envInt("SANDBOX_API_MAX_MEMORY_MB", 4096)
	maxPids := envInt("SANDBOX_API_MAX_PIDS", 1024)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Anything still carrying our label belongs to a previous run that died.
	if n, err := sandbox.SweepDocker(ctx, instanceLabel, instance); err != nil {
		return err
	} else if n > 0 {
		slog.Info("removed sandboxes left by a previous run", "count", n)
	}

	srv, err := sandboxapi.New(sandboxapi.Config{
		Token:        token,
		MaxSandboxes: envInt("SANDBOX_API_MAX", 50),
		IdleTTL:      envDuration("SANDBOX_API_IDLE_TTL", 2*time.Hour),
		New: func(ctx context.Context, o sandbox.CreateOptions) (sandbox.Sandbox, error) {
			image := o.Image
			if image == "" {
				image = images[0]
			}
			if !contains(images, image) {
				return nil, fmt.Errorf("image %q is not allowed", image)
			}
			return sandbox.NewDocker(ctx, sandbox.DockerOptions{
				Image:    image,
				MemoryMB: clamp(o.MemoryMB, maxMem),
				Pids:     clamp(o.Pids, maxPids),
				Labels:   map[string]string{instanceLabel: instance},
			})
		},
	})
	if err != nil {
		return err
	}

	httpSrv := &http.Server{Addr: env("SANDBOX_API_ADDR", ":8090"), Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	reaped := make(chan struct{})
	go func() { srv.Run(ctx, time.Minute); close(reaped) }()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	slog.Info("sandbox-api listening", "addr", httpSrv.Addr, "images", images, "instance", instance)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-reaped // Run deletes every sandbox on its way out
	return nil
}

// clamp returns the ceiling when the client asked for nothing or for more.
func clamp(asked, ceiling int) int {
	if asked <= 0 || asked > ceiling {
		return ceiling
	}
	return asked
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return def
}
