// Package runtimecontroller runs the platform-side lifecycle of managed
// runtimes: one Kubernetes Sandbox per managed runtime, woken when work is
// queued, suspended when idle, with a rotating daemon token and a per-Sandbox
// LLM key. It talks to the Multica server only through the internal API
// (/internal/v1) and never reads the database.
package runtimecontroller

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the controller's configuration, read from the environment.
type Config struct {
	// Server side.
	ServerURL        string // where the controller reaches the Multica server
	ControllerSecret string // MULTICA_CONTROLLER_SECRET bearer

	// Sandbox pods.
	Namespace        string
	PodServerURL     string // where Sandbox daemons reach the server (defaults to ServerURL)
	Image            string
	ImagePullPolicy  string
	RuntimeClassName string
	PVCSize          string
	CPURequest       string
	CPULimit         string
	MemoryRequest    string
	MemoryLimit      string
	WorkspacesRoot   string

	// Behaviour.
	ReconcileInterval time.Duration
	IdleSuspendAfter  time.Duration
	TokenTTL          time.Duration // daemon token lifetime; rotated at half
	StuckAfter        time.Duration // running but not online this long => unavailable
	DrainTimeout      time.Duration // daemon drain; pod grace period is this plus slack
	MissingGrace      int           // reconciles a runtime must be absent before its Sandbox is deleted

	// LiteLLM (Plan A). LiteLLMAdminKey empty disables key minting.
	LiteLLMURL      string
	LiteLLMAdminKey string
	LiteLLMModels   []string
	LiteLLMBudget   float64
	LiteLLMRPM      int

	LeaderElect bool
	LeaseName   string
	HealthAddr  string
}

// LoadConfig reads the configuration and validates what cannot be defaulted.
func LoadConfig() (Config, error) {
	c := Config{
		ServerURL:         strings.TrimRight(os.Getenv("MULTICA_SERVER_URL"), "/"),
		ControllerSecret:  os.Getenv("MULTICA_CONTROLLER_SECRET"),
		Namespace:         env("CONTROLLER_NAMESPACE", "default"),
		Image:             os.Getenv("SANDBOX_IMAGE"),
		ImagePullPolicy:   env("SANDBOX_IMAGE_PULL_POLICY", "IfNotPresent"),
		RuntimeClassName:  os.Getenv("SANDBOX_RUNTIME_CLASS"),
		PVCSize:           env("SANDBOX_PVC_SIZE", "5Gi"),
		CPURequest:        env("SANDBOX_CPU_REQUEST", "250m"),
		CPULimit:          env("SANDBOX_CPU_LIMIT", "2"),
		MemoryRequest:     env("SANDBOX_MEMORY_REQUEST", "512Mi"),
		MemoryLimit:       env("SANDBOX_MEMORY_LIMIT", "4Gi"),
		WorkspacesRoot:    env("SANDBOX_WORKSPACES_ROOT", "/data/workspaces"),
		LiteLLMURL:        strings.TrimRight(os.Getenv("LITELLM_URL"), "/"),
		LiteLLMAdminKey:   os.Getenv("LITELLM_ADMIN_KEY"),
		LiteLLMModels:     splitCSV(os.Getenv("LITELLM_MODELS")),
		LeaseName:         env("CONTROLLER_LEASE_NAME", "multica-runtime-controller"),
		HealthAddr:        env("CONTROLLER_HEALTH_ADDR", ":8081"),
		LeaderElect:       envBool("CONTROLLER_LEADER_ELECT", true),
		ReconcileInterval: 5 * time.Second,
		IdleSuspendAfter:  15 * time.Minute,
		TokenTTL:          24 * time.Hour,
		StuckAfter:        5 * time.Minute,
		DrainTimeout:      10 * time.Minute,
		MissingGrace:      3,
	}
	c.PodServerURL = env("SANDBOX_SERVER_URL", c.ServerURL)
	var err error
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{
		{"RECONCILE_INTERVAL", &c.ReconcileInterval},
		{"IDLE_SUSPEND_AFTER", &c.IdleSuspendAfter},
		{"TOKEN_TTL", &c.TokenTTL},
		{"STUCK_AFTER", &c.StuckAfter},
		{"MULTICA_DAEMON_DRAIN_TIMEOUT", &c.DrainTimeout},
	} {
		if v := os.Getenv(d.key); v != "" {
			if *d.dst, err = time.ParseDuration(v); err != nil {
				return Config{}, fmt.Errorf("%s: %w", d.key, err)
			}
		}
	}
	if v := os.Getenv("LITELLM_MAX_BUDGET"); v != "" {
		if c.LiteLLMBudget, err = strconv.ParseFloat(v, 64); err != nil {
			return Config{}, fmt.Errorf("LITELLM_MAX_BUDGET: %w", err)
		}
	}
	if v := os.Getenv("LITELLM_RPM"); v != "" {
		if c.LiteLLMRPM, err = strconv.Atoi(v); err != nil {
			return Config{}, fmt.Errorf("LITELLM_RPM: %w", err)
		}
	}
	if v := os.Getenv("MISSING_GRACE"); v != "" {
		if c.MissingGrace, err = strconv.Atoi(v); err != nil {
			return Config{}, fmt.Errorf("MISSING_GRACE: %w", err)
		}
	}
	return c, c.Validate()
}

// Validate rejects configurations that cannot work.
func (c Config) Validate() error {
	switch {
	case c.ServerURL == "":
		return fmt.Errorf("MULTICA_SERVER_URL is required")
	case c.ControllerSecret == "":
		return fmt.Errorf("MULTICA_CONTROLLER_SECRET is required")
	case c.Image == "":
		return fmt.Errorf("SANDBOX_IMAGE is required")
	case c.TokenTTL < time.Minute || c.TokenTTL > 24*time.Hour:
		return fmt.Errorf("TOKEN_TTL must be between 1m and 24h (the server caps daemon tokens at 24h)")
	case c.ReconcileInterval <= 0:
		return fmt.Errorf("RECONCILE_INTERVAL must be positive")
	case c.LiteLLMAdminKey != "" && c.LiteLLMURL == "":
		return fmt.Errorf("LITELLM_URL is required when LITELLM_ADMIN_KEY is set")
	}
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return fallback
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
