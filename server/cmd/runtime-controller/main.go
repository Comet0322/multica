// Command runtime-controller manages the Kubernetes Sandboxes behind managed
// runtimes. See internal/runtimecontroller.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/multica-ai/multica/server/internal/runtimecontroller"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("runtime-controller exited", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := runtimecontroller.LoadConfig()
	if err != nil {
		return err
	}
	restCfg, err := kubeConfig()
	if err != nil {
		return err
	}
	core, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return err
	}

	rec := &runtimecontroller.Reconciler{
		Cfg:    cfg,
		Server: runtimecontroller.NewHTTPServer(cfg.ServerURL, cfg.ControllerSecret),
		Kube:   &runtimecontroller.KubeClient{Cfg: cfg, Dynamic: dyn, Core: core},
		Log:    log,
	}
	if cfg.LiteLLMAdminKey != "" {
		rec.LLM = runtimecontroller.NewLiteLLM(cfg)
	} else {
		log.Warn("LITELLM_ADMIN_KEY not set: Sandboxes get no LLM credentials")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go serveHealth(ctx, cfg.HealthAddr, log)

	if !cfg.LeaderElect {
		rec.Run(ctx)
		return nil
	}
	return runLeaderElected(ctx, cfg, core, rec, log)
}

func kubeConfig() (*rest.Config, error) {
	if path := os.Getenv("KUBECONFIG"); path != "" {
		// Outside a cluster the context must be named: a controller must never
		// act on whichever cluster happens to be current.
		ctxName := os.Getenv("KUBE_CONTEXT")
		if ctxName == "" {
			return nil, errors.New("KUBE_CONTEXT is required when KUBECONFIG is set")
		}
		return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: path},
			&clientcmd.ConfigOverrides{CurrentContext: ctxName},
		).ClientConfig()
	}
	return rest.InClusterConfig()
}

func runLeaderElected(ctx context.Context, cfg runtimecontroller.Config, core kubernetes.Interface, rec *runtimecontroller.Reconciler, log *slog.Logger) error {
	id, _ := os.Hostname()
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: cfg.LeaseName, Namespace: cfg.Namespace},
		Client:     core.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: id},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   30 * time.Second,
		RenewDeadline:   20 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				log.Info("became leader", "identity", id)
				rec.Run(ctx)
			},
			OnStoppedLeading: func() { log.Info("stopped leading", "identity", id) },
		},
	})
	return nil
}

func serveHealth(ctx context.Context, addr string, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("health server stopped", "error", err)
	}
}
