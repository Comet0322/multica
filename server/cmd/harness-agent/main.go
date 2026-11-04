// Command harness-agent is the agent service (see pkg/harness/agentsvc): daemons
// send it tasks, it runs the model loop and gives the model a sandbox obtained
// from the sandbox API. It holds the model key and the git credential; the
// sandbox and the daemon hold neither.
//
// Configuration, all from the environment:
//
//	HARNESS_AGENT_TOKEN        bearer token daemons must present (required)
//	HARNESS_AGENT_ADDR         listen address (default :8100)
//	HARNESS_SANDBOX_API_URL    the sandbox API (required)
//	HARNESS_SANDBOX_API_TOKEN  its token (required)
//	HARNESS_SANDBOX_IMAGE      image to ask for; empty uses the sandbox API's default
//	HARNESS_MODEL, HARNESS_MODEL_ID, HARNESS_ANTHROPIC_API_KEY, HARNESS_ANTHROPIC_BASE_URL,
//	HARNESS_MOCK_SCRIPT        the model (see agentsvc.ModelFactory)
//	HARNESS_GIT_ALLOWED_REPOS  comma-separated repository URLs; enables git_clone and deliver
//	HARNESS_GIT_TOKEN, HARNESS_GIT_USER  https credential for those repositories
//	HARNESS_MULTICA_URL        Multica server the tools call; empty uses the one each task names
//	HARNESS_MAX_RUNS           concurrent runs (default 20)
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/pkg/harness/agentsvc"
	"github.com/multica-ai/multica/server/pkg/harness/sandbox"
)

func main() {
	if err := run(); err != nil {
		slog.Error("harness-agent", "error", err)
		os.Exit(1)
	}
}

func run() error {
	token, sbURL, sbToken := os.Getenv("HARNESS_AGENT_TOKEN"), os.Getenv("HARNESS_SANDBOX_API_URL"), os.Getenv("HARNESS_SANDBOX_API_TOKEN")
	if token == "" || sbURL == "" || sbToken == "" {
		return errors.New("HARNESS_AGENT_TOKEN, HARNESS_SANDBOX_API_URL and HARNESS_SANDBOX_API_TOKEN are required")
	}
	newModel, err := agentsvc.ModelFactory(os.Getenv)
	if err != nil {
		return err
	}
	image := os.Getenv("HARNESS_SANDBOX_IMAGE")

	maxRuns := 20
	if n, err := strconv.Atoi(os.Getenv("HARNESS_MAX_RUNS")); err == nil && n > 0 {
		maxRuns = n
	}
	var repos []string
	for _, r := range strings.Split(os.Getenv("HARNESS_GIT_ALLOWED_REPOS"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			repos = append(repos, r)
		}
	}

	svc, err := agentsvc.New(agentsvc.Config{
		Token:    token,
		NewModel: newModel,
		NewSandbox: func(ctx context.Context) (sandbox.Sandbox, string, error) {
			sb, err := sandbox.NewRemote(ctx, sandbox.RemoteConfig{
				BaseURL: sbURL, Token: sbToken,
				Options: sandbox.CreateOptions{Image: image, MemoryMB: 2048, Pids: 1024},
			})
			return sb, "/work", err
		},
		GitRepos: repos, GitUser: os.Getenv("HARNESS_GIT_USER"), GitToken: os.Getenv("HARNESS_GIT_TOKEN"),
		MulticaURL: os.Getenv("HARNESS_MULTICA_URL"),
		MaxRuns:    maxRuns,
	})
	if err != nil {
		return err
	}

	addr := os.Getenv("HARNESS_AGENT_ADDR")
	if addr == "" {
		addr = ":8100"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// No WriteTimeout: a run streams for as long as it lasts.
	httpSrv := &http.Server{Addr: addr, Handler: svc, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	slog.Info("harness-agent listening", "addr", addr, "sandbox_api", sbURL, "git_repos", len(repos))
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
