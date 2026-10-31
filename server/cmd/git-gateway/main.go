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
//	GIT_GATEWAY_BRANCH_PREFIX     start of every task's branch namespace, default "agent/"
//	GIT_GATEWAY_MIN_READ_LEVEL    lowest GitLab access level that may clone (10 guest, 20 reporter,
//	                              30 developer, 40 maintainer, 50 owner, or the name), default 20
//	GIT_GATEWAY_MIN_PUSH_LEVEL    lowest access level that may push or open a merge request, default 30
//	GIT_GATEWAY_MR_TARGETS        comma list of branches a merge request may target;
//	                              default is each project's default branch
//	GIT_GATEWAY_MR_LABEL          label put on merge requests the gateway opens, default created-by-agent
//	GIT_GATEWAY_TASK_URL          link template for the merge request description;
//	                              {task_id} and {workspace_id} are replaced
//	GIT_GATEWAY_CACHE_TTL         access cache lifetime, default 10m
//	GIT_GATEWAY_LOOKUP_TIMEOUT    GitLab API timeout, default 10s
//
// The MCP tools are served at /mcp on the same listener, so pods reach them at
// <gateway base URL>/mcp (SANDBOX_GIT_GATEWAY_MCP_URL on the pool manager).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
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
	srv := &http.Server{Addr: listen, Handler: handler(g), ReadHeaderTimeout: 10 * time.Second}
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

// mcpPath is where the MCP tools are mounted. Git paths always continue past
// the project into ".git/...", so the exact path cannot collide with a project.
const mcpPath = "/mcp"

// handler serves git smart HTTP and, at mcpPath, the MCP tools.
func handler(g *gitgateway.Gateway) http.Handler {
	mcp := g.MCPHandler()
	// Not a ServeMux: it would redirect unclean git paths.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mcpPath {
			mcp.ServeHTTP(w, r)
			return
		}
		g.ServeHTTP(w, r)
	})
}

// accessLevels are GitLab's project access levels.
var accessLevels = map[string]int{"guest": 10, "reporter": 20, "developer": 30, "maintainer": 40, "owner": 50}

func parseAccessLevel(raw string) (int, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if n, ok := accessLevels[raw]; ok {
		return n, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	for _, level := range accessLevels {
		if level == n {
			return n, true
		}
	}
	return 0, false
}

// checkTaskURL accepts an http(s) template whose only placeholders are
// {task_id} and {workspace_id}.
func checkTaskURL(tmpl string) bool {
	filled := strings.NewReplacer("{task_id}", "t", "{workspace_id}", "w").Replace(tmpl)
	if strings.ContainsAny(filled, "{}") {
		return false
	}
	u, err := url.Parse(filled)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil
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
	level := func(name string) int {
		raw := getenv(name)
		if raw == "" {
			return 0
		}
		n, ok := parseAccessLevel(raw)
		if !ok {
			errs = append(errs, name+" must be a GitLab access level: 10, 20, 30, 40, 50 or guest, reporter, developer, maintainer, owner")
		}
		return n
	}
	cfg.BranchPrefix = strings.TrimSpace(getenv("GIT_GATEWAY_BRANCH_PREFIX"))
	if p := cfg.BranchPrefix; p != "" && (strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.ContainsAny(p, " \t~^:?*[\\")) {
		errs = append(errs, "GIT_GATEWAY_BRANCH_PREFIX must be a valid branch name prefix such as agent/")
	}
	cfg.MinReadLevel = level("GIT_GATEWAY_MIN_READ_LEVEL")
	cfg.MinPushLevel = level("GIT_GATEWAY_MIN_PUSH_LEVEL")
	if cfg.MinReadLevel != 0 && cfg.MinPushLevel != 0 && cfg.MinPushLevel < cfg.MinReadLevel {
		errs = append(errs, "GIT_GATEWAY_MIN_PUSH_LEVEL must not be lower than GIT_GATEWAY_MIN_READ_LEVEL")
	}
	for _, branch := range strings.Split(getenv("GIT_GATEWAY_MR_TARGETS"), ",") {
		if branch = strings.TrimSpace(branch); branch != "" {
			cfg.MRTargets = append(cfg.MRTargets, branch)
		}
	}
	cfg.MRLabel = strings.TrimSpace(getenv("GIT_GATEWAY_MR_LABEL"))
	if strings.Contains(cfg.MRLabel, ",") {
		errs = append(errs, "GIT_GATEWAY_MR_LABEL must be a single label without commas")
	}
	cfg.TaskURL = strings.TrimSpace(getenv("GIT_GATEWAY_TASK_URL"))
	if cfg.TaskURL != "" && !checkTaskURL(cfg.TaskURL) {
		errs = append(errs, "GIT_GATEWAY_TASK_URL must be an http(s) URL template using only {task_id} and {workspace_id}")
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
