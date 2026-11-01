// sandbox-broker runs the sandbox pool manager: one Multica runtime per
// workspace of a bot account, served from a fixed pool of pods running the
// stock Multica daemon. See deploy/sandbox/POOL.md.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	settings, err := loadPool(os.Getenv)
	if err == nil {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		err = runPool(ctx, log, settings)
	}
	if err != nil {
		log.Error("sandbox pool manager stopped", "error", err)
		os.Exit(1)
	}
}
