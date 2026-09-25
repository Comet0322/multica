// Command multica-k8s-controller claims Multica tasks and runs each one in an
// isolated sandbox Pod. It authenticates to the server as a daemon and serves
// the relay that sandbox Pods use instead of holding any daemon credential.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/k8sruntime/controller"
	"github.com/multica-ai/multica/server/internal/k8sruntime/kube"
	"github.com/multica-ai/multica/server/internal/k8sruntime/relay"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("controller failed", "error", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// durationEnv parses an optional duration such as "90s"; unset means the
// controller's default.
func durationEnv(key string) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a duration like 90s: %q", key, v)
	}
	return d, nil
}

func list(key string) []string {
	var out []string
	for _, s := range strings.Split(os.Getenv(key), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func run(logger *slog.Logger) error {
	serverURL := env("MULTICA_SERVER_URL", "")
	token := env("MULTICA_CONTROLLER_TOKEN", "") // daemon credential; never given to Pods
	relayURL := env("MULTICA_K8S_RELAY_URL", "")
	image := env("MULTICA_K8S_RUNNER_IMAGE", "")
	for name, v := range map[string]string{
		"MULTICA_SERVER_URL": serverURL, "MULTICA_CONTROLLER_TOKEN": token,
		"MULTICA_K8S_RELAY_URL": relayURL, "MULTICA_K8S_RUNNER_IMAGE": image,
	} {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	workspaces, providers := list("MULTICA_K8S_WORKSPACES"), list("MULTICA_K8S_PROVIDERS")
	if len(workspaces) == 0 || len(providers) == 0 {
		return errors.New("MULTICA_K8S_WORKSPACES and MULTICA_K8S_PROVIDERS are required")
	}
	hostname, _ := os.Hostname()
	daemonID := env("MULTICA_DAEMON_ID", "k8s-"+hostname)
	maxPods, _ := strconv.Atoi(env("MULTICA_K8S_MAX_PODS", "4"))
	pendingTimeout, err := durationEnv("MULTICA_K8S_PENDING_TIMEOUT")
	if err != nil {
		return err
	}
	maxRun, err := durationEnv("MULTICA_K8S_MAX_RUN_DURATION")
	if err != nil {
		return err
	}
	heartbeat, err := durationEnv("MULTICA_K8S_HEARTBEAT_INTERVAL")
	if err != nil {
		return err
	}

	base, err := daemon.NormalizeServerBaseURL(serverURL)
	if err != nil {
		return err
	}
	client := daemon.NewClient(base)
	client.SetToken(token)
	client.SetVersion(env("MULTICA_CLI_VERSION", "k8s-controller"))

	upstream, err := url.Parse(base)
	if err != nil {
		return err
	}
	rl, err := relay.New(upstream, token)
	if err != nil {
		return err
	}

	drv, err := kube.NewInCluster(kube.Config{
		Namespace:          env("MULTICA_K8S_NAMESPACE", ""),
		DaemonID:           daemonID,
		Image:              image,
		PullPolicy:         env("MULTICA_K8S_PULL_POLICY", ""),
		RelayURL:           relayURL,
		RuntimeClassName:   env("MULTICA_K8S_RUNTIME_CLASS", ""),
		CPURequest:         env("MULTICA_K8S_CPU_REQUEST", ""),
		CPULimit:           env("MULTICA_K8S_CPU_LIMIT", ""),
		MemoryRequest:      env("MULTICA_K8S_MEMORY_REQUEST", ""),
		MemoryLimit:        env("MULTICA_K8S_MEMORY_LIMIT", ""),
		WorkspaceSizeLimit: env("MULTICA_K8S_WORKSPACE_SIZE", ""),
		EnvFromSecrets:     list("MULTICA_K8S_ENV_SECRETS"),
	})
	if err != nil {
		return err
	}

	c, err := controller.New(controller.Config{
		DaemonID:   daemonID,
		DeviceName: env("MULTICA_DAEMON_DEVICE_NAME", hostname),
		CLIVersion: env("MULTICA_CLI_VERSION", "k8s-controller"),
		Workspaces: workspaces,
		Providers:  providers,
		MaxPods:    maxPods,

		HeartbeatInterval: heartbeat,
		PendingTimeout:    pendingTimeout,
		MaxRunDuration:    maxRun,
		Models:            list("MULTICA_K8S_MODELS"),
	}, client, rl, drv, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	srv := &http.Server{Addr: env("MULTICA_K8S_RELAY_ADDR", ":8081"), Handler: rl, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	go func() {
		logger.Info("relay listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("relay stopped", "error", err)
			stop()
		}
	}()

	return c.Run(ctx)
}
