package main

import (
	"strings"
	"testing"
)

func TestManagedDaemonRefusesBackgroundStart(t *testing.T) {
	t.Setenv("MULTICA_DAEMON_MANAGED", "true")
	cmd := daemonStartCmd
	if err := cmd.Flags().Set("foreground", "false"); err != nil {
		t.Fatal(err)
	}
	err := runDaemonStart(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--foreground") {
		// requireHumanLocalCommand may also refuse in some test envs; accept only our message.
		t.Fatalf("expected a --foreground requirement, got %v", err)
	}
}
