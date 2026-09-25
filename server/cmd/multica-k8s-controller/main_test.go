package main

import (
	"reflect"
	"testing"
	"time"
)

func TestEveryDocumentedSettingReachesTheControllerConfig(t *testing.T) {
	for k, v := range map[string]string{
		"MULTICA_K8S_PENDING_TIMEOUT":    "90s",
		"MULTICA_K8S_MAX_RUN_DURATION":   "2h",
		"MULTICA_K8S_HEARTBEAT_INTERVAL": "5s",
		"MULTICA_K8S_MODELS":             "a=Model A, b",
		"MULTICA_K8S_MODELS_URL":         "https://gw.example/",
		"MULTICA_K8S_MODELS_API_KEY":     "k",
		"MULTICA_K8S_DEFAULT_MODEL":      "b",
		"MULTICA_DAEMON_DEVICE_NAME":     "dev",
		"MULTICA_CLI_VERSION":            "9.9",
	} {
		t.Setenv(k, v)
	}
	cfg, err := controllerConfig("d1", "host", []string{"ws"}, []string{"claude"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PendingTimeout != 90*time.Second || cfg.MaxRunDuration != 2*time.Hour || cfg.HeartbeatInterval != 5*time.Second {
		t.Errorf("durations = %v %v %v", cfg.PendingTimeout, cfg.MaxRunDuration, cfg.HeartbeatInterval)
	}
	if !reflect.DeepEqual(cfg.Models, []string{"a=Model A", "b"}) {
		t.Errorf("Models = %q", cfg.Models)
	}
	if cfg.ModelsURL != "https://gw.example/" || cfg.ModelsAPIKey != "k" || cfg.DefaultModel != "b" {
		t.Errorf("model scan settings = %q %q %q", cfg.ModelsURL, cfg.ModelsAPIKey, cfg.DefaultModel)
	}
	if cfg.DaemonID != "d1" || cfg.MaxPods != 7 || cfg.DeviceName != "dev" || cfg.CLIVersion != "9.9" {
		t.Errorf("identity settings = %+v", cfg)
	}
}

func TestBadDurationIsRejected(t *testing.T) {
	t.Setenv("MULTICA_K8S_PENDING_TIMEOUT", "soon")
	if _, err := controllerConfig("d", "h", []string{"w"}, []string{"claude"}, 1); err == nil {
		t.Fatal("an unparseable duration must be an error, not silently ignored")
	}
}
