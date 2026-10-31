package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
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
// runs. Every variable is described in deploy/sandbox/POOL.md.
type poolSettings struct {
	workspacesFile string
	// workspacesGroupRead lets the workspace file be group-readable (a Kubernetes Secret volume).
	workspacesGroupRead bool
	podListen           string
	adminListen         string
	// gatewayListen, when set, is a listener serving only the git gateway's
	// verification endpoints.
	gatewayListen string
	// auditFile is where audit records are appended; "-" means standard output
	// and empty disables auditing.
	auditFile string
	config    sandboxbroker.PoolConfig
}

// secretValue reads a secret from NAME or, when NAME_FILE is set, from that
// file. Setting both is refused. Errors name the variable, never the value.
func secretValue(getenv func(string) string, name string) (string, error) {
	value, path := getenv(name), getenv(name+"_FILE")
	if value != "" && path != "" {
		return "", fmt.Errorf("set only one of %s and %s_FILE", name, name)
	}
	if path == "" {
		return strings.TrimSpace(value), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: cannot read %s", name, path)
	}
	return strings.TrimSpace(string(raw)), nil
}

// envPairs parses "NAME=VALUE,NAME=VALUE".
func envPairs(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		name, value, ok := strings.Cut(item, "=")
		if name = strings.TrimSpace(name); !ok || name == "" {
			return nil, fmt.Errorf("%q is not NAME=VALUE", item)
		}
		out[name] = strings.TrimSpace(value)
	}
	return out, nil
}

func loadPool(getenv func(string) string) (poolSettings, error) {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	duration := func(name string) time.Duration {
		raw := getenv(name)
		if raw == "" {
			return 0
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			fail("%s must be a duration such as 10m", name)
		} else if d < 0 {
			fail("%s must not be negative", name)
		}
		return d
	}
	whole := func(name string) int {
		raw := getenv(name)
		if raw == "" {
			return 0
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			fail("%s must be a whole number, 0 or more", name)
			return 0
		}
		return n
	}
	decimal := func(name string) float64 {
		raw := getenv(name)
		if raw == "" {
			return 0
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			fail("%s must be a number, 0 or more", name)
			return 0
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
	envMap := func(name string) map[string]string {
		m, err := envPairs(getenv(name))
		if err != nil {
			fail("%s: %v", name, err)
		}
		if len(m) == 0 {
			return nil
		}
		return m
	}
	secret := func(name string) string {
		v, err := secretValue(getenv, name)
		if err != nil {
			fail("%s", err)
		}
		return v
	}

	s := poolSettings{
		workspacesFile: getenv("SANDBOX_WORKSPACES_FILE"),
		podListen:      getenv("SANDBOX_BROKER_LISTEN"),
		adminListen:    getenv("SANDBOX_POOL_ADMIN_LISTEN"),
		gatewayListen:  getenv("SANDBOX_POOL_GATEWAY_LISTEN"),
		auditFile:      getenv("SANDBOX_AUDIT_FILE"),
		config: sandboxbroker.PoolConfig{
			MulticaURL:       getenv("MULTICA_SERVER_URL"),
			Name:             getenv("SANDBOX_BROKER_NAME"),
			ExpectedSize:     whole("SANDBOX_POOL_SIZE"),
			Providers:        list("SANDBOX_PROVIDERS"),
			DrainTimeout:     duration("SANDBOX_POOL_DRAIN_TIMEOUT"),
			HeartbeatTimeout: duration("SANDBOX_POOL_HEARTBEAT_TIMEOUT"),
			ResetTimeout:     duration("SANDBOX_POOL_RESET_TIMEOUT"),
			Placement:        sandboxbroker.Placement(getenv("SANDBOX_PLACEMENT")),
			PodConcurrency:   whole("SANDBOX_POD_CONCURRENCY"),
			PlacementWait:    duration("SANDBOX_PLACEMENT_WAIT"),
			PodIdleTimeout:   duration("SANDBOX_POD_IDLE_TIMEOUT"),
			PodMaxLifetime:   duration("SANDBOX_POD_MAX_LIFETIME"),
			PodDrainGrace:    duration("SANDBOX_POD_DRAIN_GRACE"),
		},
	}
	switch getenv("SANDBOX_WORKSPACES_FILE_GROUP_READ") {
	case "":
	case "true":
		s.workspacesGroupRead = true
		s.config.WorkspaceFileOptions = []sandboxbroker.LoadOption{sandboxbroker.AllowGroupRead()}
	case "false":
	default:
		fail("SANDBOX_WORKSPACES_FILE_GROUP_READ must be true or false")
	}
	switch s.config.Placement {
	case "", sandboxbroker.PlacementTask, sandboxbroker.PlacementIssue, sandboxbroker.PlacementChat:
	default:
		fail("SANDBOX_PLACEMENT must be task, issue or chat")
	}

	// LiteLLM is on when SANDBOX_LITELLM_URL is set; its other variables
	// without it are a mistake, not something to ignore silently.
	llmEnv := envMap("SANDBOX_LITELLM_ENV")
	llm := &sandboxbroker.LiteLLMConfig{
		URL:         getenv("SANDBOX_LITELLM_URL"),
		PodURL:      getenv("SANDBOX_LITELLM_POD_URL"),
		MasterKey:   secret("SANDBOX_LITELLM_MASTER_KEY"),
		Models:      list("SANDBOX_LITELLM_MODELS"),
		MaxBudget:   decimal("SANDBOX_LITELLM_MAX_BUDGET"),
		RPMLimit:    whole("SANDBOX_LITELLM_RPM_LIMIT"),
		TaskTimeout: duration("SANDBOX_LITELLM_TASK_TIMEOUT"),
		Margin:      duration("SANDBOX_LITELLM_MARGIN"),
		Env:         llmEnv,
	}
	if llm.URL != "" {
		if llm.MasterKey == "" {
			fail("SANDBOX_LITELLM_MASTER_KEY or SANDBOX_LITELLM_MASTER_KEY_FILE is required with SANDBOX_LITELLM_URL")
		}
		if len(llm.Models) == 0 {
			fail("SANDBOX_LITELLM_MODELS must list at least one model")
		}
		if llm.MaxBudget <= 0 {
			fail("SANDBOX_LITELLM_MAX_BUDGET must be set above zero with SANDBOX_LITELLM_URL (the cap on one task's key)")
		}
		if llm.RPMLimit <= 0 {
			fail("SANDBOX_LITELLM_RPM_LIMIT must be set above zero with SANDBOX_LITELLM_URL (the requests per minute of one task's key)")
		}
		s.config.LiteLLM = llm
	} else if llm.PodURL != "" || llm.MasterKey != "" || len(llm.Models) > 0 || llm.MaxBudget != 0 || llm.RPMLimit != 0 ||
		llm.TaskTimeout != 0 || llm.Margin != 0 || len(llmEnv) > 0 || getenv("SANDBOX_LITELLM_MASTER_KEY_FILE") != "" {
		fail("SANDBOX_LITELLM_* settings need SANDBOX_LITELLM_URL")
	}

	// The git gateway is on when SANDBOX_GIT_GATEWAY_URL, the address pods use
	// for it, is set.
	gitEnv := envMap("SANDBOX_GIT_GATEWAY_ENV")
	gitMCP := strings.TrimSpace(getenv("SANDBOX_GIT_GATEWAY_MCP_URL"))
	if gitURL := getenv("SANDBOX_GIT_GATEWAY_URL"); gitURL != "" {
		if u, err := url.Parse(gitMCP); gitMCP != "" && (err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "") {
			fail("SANDBOX_GIT_GATEWAY_MCP_URL must be an http(s) URL such as http://git-gateway:8081/mcp")
		}
		s.config.Git = &sandboxbroker.GitGatewayConfig{URL: gitURL, MCPURL: gitMCP, Env: gitEnv}
	} else if len(gitEnv) > 0 || gitMCP != "" {
		fail("SANDBOX_GIT_GATEWAY_ENV and SANDBOX_GIT_GATEWAY_MCP_URL need SANDBOX_GIT_GATEWAY_URL")
	}

	if s.gatewayListen != "" && s.config.Git == nil {
		fail("SANDBOX_POOL_GATEWAY_LISTEN needs SANDBOX_GIT_GATEWAY_URL")
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
	workspaces, err := sandboxbroker.LoadWorkspaces(s.workspacesFile, s.config.WorkspaceFileOptions...)
	if err != nil {
		return err
	}
	s.config.Workspaces = workspaces
	if s.auditFile != "" {
		w, closeAudit, err := openAudit(s.auditFile)
		if err != nil {
			return err
		}
		defer closeAudit()
		s.config.AuditLog = w
	}
	// Range and URL checks of the pool itself run here, before anything
	// listens.
	pool, err := sandboxbroker.NewPool(s.config, log)
	if err != nil {
		return fmt.Errorf("invalid pool configuration: %w", err)
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
	var gatewayLn net.Listener
	if s.gatewayListen != "" {
		if gatewayLn, err = net.Listen("tcp", s.gatewayListen); err != nil {
			return fmt.Errorf("listen for the git gateway: %w", err)
		}
		defer gatewayLn.Close()
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	return servePool(ctx, log, pool, s.workspacesFile, podLn, adminLn, gatewayLn, hup)
}

// openAudit opens the audit log for appending. "-" is standard output. The
// file is private to the manager's user: it names people and tasks.
func openAudit(path string) (io.Writer, func(), error) {
	if path == "-" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("SANDBOX_AUDIT_FILE: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}

// servePool serves the pod and admin APIs and applies the workspace file each
// time reload fires. A reload that fails is logged and changes nothing.
//
// The admin API also carries the git gateway's verification endpoints when
// the pool has a git gateway configured (pool.AdminHandler mounts them). That
// listener is loopback-only by default, so a gateway in another pod uses
// gatewayLn instead: a listener (nil for none) that serves just those two
// endpoints (pool.GatewayHandler), which the network policy can open to the
// gateway alone.
func servePool(ctx context.Context, log *slog.Logger, pool *sandboxbroker.Pool, path string, podLn, adminLn, gatewayLn net.Listener, reload <-chan os.Signal) error {
	servers := []*http.Server{
		{Handler: pool.PodHandler(), ReadHeaderTimeout: 10 * time.Second},
		{Handler: pool.AdminHandler(), ReadHeaderTimeout: 10 * time.Second},
	}
	listeners := []net.Listener{podLn, adminLn}
	if gatewayLn != nil {
		servers = append(servers, &http.Server{Handler: pool.GatewayHandler(), ReadHeaderTimeout: 10 * time.Second})
		listeners = append(listeners, gatewayLn)
	}
	serveErr := make(chan error, len(servers))
	for i, ln := range listeners {
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
