// git-gateway is the only service that holds the GitLab administrator token.
// Sandbox pods clone and fetch through it with a per-task token, and it allows
// each request only if the task's human initiator has access in GitLab.
//
// Configuration (environment):
//
//	GIT_GATEWAY_LISTEN            listen address, default :8081
//	GIT_GATEWAY_GITLAB_URL        GitLab base URL (required)
//	GIT_GATEWAY_GITLAB_TOKEN      GitLab admin token, or
//	GIT_GATEWAY_GITLAB_TOKEN_FILE a file holding it
//	GIT_GATEWAY_VERIFY_URL        manager endpoint that resolves task tokens (required)
//	GIT_GATEWAY_POD_VERIFY_URL    manager endpoint that maps a pod address to its leased tasks;
//	                              when set, token-less requests from a leased pod may read (never write)
//	GIT_GATEWAY_AUDIT_FILE       JSON Lines audit log; default stdout
//	GIT_GATEWAY_CACHE_TTL         access cache lifetime, default 10m
//	GIT_GATEWAY_LOOKUP_TIMEOUT    GitLab API timeout, default 10s
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/gitgateway"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, listen, err := load(os.Getenv)
	if err != nil {
		log.Error("invalid git gateway configuration", "error", err)
		os.Exit(2)
	}
	cfg.Logger = log
	g, err := gitgateway.New(cfg)
	if err != nil {
		log.Error("invalid git gateway configuration", "error", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	srv := &http.Server{Addr: listen, Handler: g, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("git gateway listening", "addr", listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("git gateway stopped", "error", err)
		os.Exit(1)
	}
}

func load(getenv func(string) string) (gitgateway.Config, string, error) {
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
	cfg := gitgateway.Config{
		GitLabURL:     getenv("GIT_GATEWAY_GITLAB_URL"),
		AdminToken:    strings.TrimSpace(getenv("GIT_GATEWAY_GITLAB_TOKEN")),
		CacheTTL:      duration("GIT_GATEWAY_CACHE_TTL"),
		LookupTimeout: duration("GIT_GATEWAY_LOOKUP_TIMEOUT"),
		Audit:         os.Stdout,
	}
	if path := getenv("GIT_GATEWAY_GITLAB_TOKEN_FILE"); path != "" && cfg.AdminToken == "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, "GIT_GATEWAY_GITLAB_TOKEN_FILE is unreadable")
		}
		cfg.AdminToken = strings.TrimSpace(string(raw))
	}
	if cfg.AdminToken == "" {
		errs = append(errs, "GIT_GATEWAY_GITLAB_TOKEN or GIT_GATEWAY_GITLAB_TOKEN_FILE is required")
	}
	if cfg.GitLabURL == "" {
		errs = append(errs, "GIT_GATEWAY_GITLAB_URL is required")
	}
	verify := getenv("GIT_GATEWAY_VERIFY_URL")
	if verify == "" {
		errs = append(errs, "GIT_GATEWAY_VERIFY_URL is required")
	}
	cfg.Resolver = gitgateway.HTTPResolver{URL: verify}
	if podVerify := getenv("GIT_GATEWAY_POD_VERIFY_URL"); podVerify != "" {
		cfg.PodResolver = gitgateway.HTTPPodResolver{URL: podVerify}
	}
	if path := getenv("GIT_GATEWAY_AUDIT_FILE"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			errs = append(errs, "GIT_GATEWAY_AUDIT_FILE cannot be opened")
		} else {
			cfg.Audit = io.Writer(f)
		}
	}
	listen := getenv("GIT_GATEWAY_LISTEN")
	if listen == "" {
		listen = ":8081"
	}
	if len(errs) > 0 {
		return cfg, listen, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return cfg, listen, nil
}
