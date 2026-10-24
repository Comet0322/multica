package cli

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/selfexec"
)

// knownBrewPrefixes lists the install roots Homebrew uses on each platform.
// Order is irrelevant — the prefixes do not nest.
var knownBrewPrefixes = []string{"/opt/homebrew", "/usr/local", "/home/linuxbrew/.linuxbrew"}

// MatchKnownBrewPrefix returns the Homebrew prefix whose Cellar contains path,
// or "" if path is not under a known Cellar. It is the offline equivalent of
// `brew --prefix`: callers reach for it when `brew --prefix` is unavailable
// (brew not on PATH) but the binary's path still betrays its install root.
func MatchKnownBrewPrefix(path string) string {
	for _, prefix := range knownBrewPrefixes {
		if strings.HasPrefix(path, prefix+"/Cellar/") {
			return prefix
		}
	}
	return ""
}

// IsBrewInstall checks whether the running multica binary was installed via Homebrew.
// The check itself lives in brewInstallForPath (stable_exe.go) so it stays
// one implementation with the stable-path resolver that depends on it.
func IsBrewInstall() bool {
	exePath, err := selfexec.Resolve()
	if err != nil {
		return false
	}
	return brewInstallForPath(exePath)
}

// GetBrewPrefix returns the Homebrew prefix by running `brew --prefix`, or empty string.
// Bounded: a wedged brew (broken shellenv, unreachable network home) must not
// park the caller — the daemon's reload loop reaches here periodically.
func GetBrewPrefix() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "brew", "--prefix")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
