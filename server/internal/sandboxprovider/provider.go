// Package sandboxprovider starts and stops the isolated environments that run
// a stock Multica daemon on behalf of the sandbox broker.
//
// Every operation is addressed by a caller-chosen key and is idempotent, so the
// broker can repeat calls after a crash without tracking provider IDs. A
// sandbox owns one persistent volume that outlives its compute: Stop releases
// compute and keeps the volume, Delete removes both.
//
// Implementations may be asynchronous (Kubernetes reconciles in the
// background), so callers must poll Get rather than assume Ensure returns a
// running sandbox.
package sandboxprovider

import (
	"context"
	"errors"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusStopped Status = "stopped"
	StatusFailed  Status = "failed"
	StatusMissing Status = "missing"
)

// ErrInvalidKey rejects keys a provider cannot map to a resource name.
var ErrInvalidKey = errors.New("sandbox key must be 1-63 characters of [a-z0-9-] and start with a letter or digit")

type Spec struct {
	Key   string
	Image string
	// Env is passed to the sandbox's entrypoint. It carries the broker URL and
	// the sandbox-scoped broker credential, never the broker's own Multica token.
	Env map[string]string
	// Labels are informational metadata added next to the provider's own.
	Labels map[string]string
}

type Sandbox struct {
	Key    string
	Status Status
	// Reason explains StatusFailed (exit code, image pull error, ...).
	Reason string
	// Image is the image the existing compute was created from; empty when
	// there is no compute.
	Image string
}

type Provider interface {
	// Ensure creates the sandbox, or starts it when it exists but is stopped.
	// Compute created from a different image is replaced; the volume is kept.
	Ensure(ctx context.Context, spec Spec) (Sandbox, error)
	// Get reports StatusMissing when no compute exists for key.
	Get(ctx context.Context, key string) (Sandbox, error)
	Stop(ctx context.Context, key string) error
	Delete(ctx context.Context, key string) error
	// List returns every sandbox this provider instance manages.
	List(ctx context.Context) ([]Sandbox, error)
}

// ValidKey reports whether key is usable as a DNS-1123 label, the strictest
// naming rule among supported providers.
func ValidKey(key string) bool {
	if len(key) == 0 || len(key) > 63 {
		return false
	}
	for i, c := range key {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(key)-1:
		default:
			return false
		}
	}
	return true
}
