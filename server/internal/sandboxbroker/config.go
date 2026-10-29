// Package sandboxbroker exposes one hidden-infrastructure Multica runtime and
// executes its tasks in sandboxes that run the stock Multica daemon.
//
// Upstream, the broker is an ordinary daemon: it registers runtimes with
// runtime_mode "cloud", heartbeats, claims tasks and keeps their prepare lease
// alive. Downstream, it impersonates the Multica server for the daemon inside
// each sandbox: register/heartbeat/claim are answered locally, task-scoped
// calls are forwarded with the broker's credential after an ownership check,
// and everything else under /api/ passes through untouched so agents' task
// tokens keep working. The daemon code is never modified; every supported
// endpoint is listed in proxy.go so protocol drift surfaces as a logged 403.
package sandboxbroker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Config struct {
	MulticaURL   string
	MulticaToken string
	WorkspaceID  string
	// BrokerID is the upstream daemon_id. It must stay stable across restarts
	// or the server registers a new runtime row.
	BrokerID string
	// Name is the machine and runtime name members see.
	Name string
	// ListenAddr serves the sandbox-facing API.
	ListenAddr string
	// SandboxURL is how sandboxes reach ListenAddr.
	SandboxURL string
	Image      string
	// Providers limits which agent CLIs detected in the image become upstream
	// runtimes. Empty exposes every detected provider.
	Providers []string
	StateDir  string
	// MaxConcurrency bounds tasks claimed but not finished across sandboxes.
	MaxConcurrency int
	// IdleTimeout stops a sandbox's compute once it has had no task this long.
	IdleTimeout time.Duration
	// StartTimeout fails tasks whose sandbox never starts their run.
	StartTimeout time.Duration
	// SandboxEnv is added to every sandbox, e.g. LLM gateway settings.
	SandboxEnv map[string]string

	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	LeaseInterval     time.Duration
}

func (c *Config) normalize() error {
	if _, err := uuid.Parse(c.WorkspaceID); err != nil {
		return fmt.Errorf("workspace ID must be a UUID")
	}
	c.WorkspaceID = strings.ToLower(c.WorkspaceID)
	if err := checkBaseURL(c.MulticaURL); err != nil {
		return fmt.Errorf("Multica URL: %w", err)
	}
	if err := checkBaseURL(c.SandboxURL); err != nil {
		return fmt.Errorf("sandbox URL: %w", err)
	}
	c.MulticaURL = strings.TrimRight(c.MulticaURL, "/")
	c.SandboxURL = strings.TrimRight(c.SandboxURL, "/")
	if strings.TrimSpace(c.MulticaToken) == "" {
		return fmt.Errorf("Multica token is required")
	}
	if c.Image == "" {
		return fmt.Errorf("sandbox image is required")
	}
	if c.StateDir == "" {
		return fmt.Errorf("state directory is required")
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":8790"
	}
	if c.Name == "" {
		c.Name = "Managed Sandbox"
	}
	if c.BrokerID == "" {
		sum := sha256.Sum256([]byte(c.MulticaURL + "\x00" + c.WorkspaceID))
		c.BrokerID = "sandbox-broker-" + hex.EncodeToString(sum[:])[:24]
	}
	if c.MaxConcurrency == 0 {
		c.MaxConcurrency = 4
	}
	if c.MaxConcurrency < 1 {
		return fmt.Errorf("max concurrency must be positive")
	}
	defaults := []struct {
		d   *time.Duration
		def time.Duration
	}{
		{&c.IdleTimeout, 10 * time.Minute},
		{&c.StartTimeout, 10 * time.Minute},
		{&c.PollInterval, 3 * time.Second},
		{&c.HeartbeatInterval, 15 * time.Second},
		// The server's prepare lease lasts 45s.
		{&c.LeaseInterval, 15 * time.Second},
	}
	for _, d := range defaults {
		if *d.d == 0 {
			*d.d = d.def
		}
		if *d.d < time.Millisecond {
			return fmt.Errorf("durations must be positive")
		}
	}
	return nil
}

func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be an http(s) base URL")
	}
	return nil
}
