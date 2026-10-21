package runtimecontroller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// States reported to the server.
const (
	StateRunning     = "running"
	StateWaking      = "waking"
	StateSuspended   = "suspended"
	StateUnavailable = "unavailable"
)

// Reconciler drives every managed runtime's Sandbox toward what the work queue
// needs: running while there is work (or recent work), suspended when idle,
// woken when a task queues, removed when the runtime is deleted.
type Reconciler struct {
	Cfg    Config
	Server Server
	Kube   Kube
	LLM    LLMKeys // nil disables per-Sandbox LLM keys
	Now    func() time.Time
	Log    *slog.Logger

	mu           sync.Mutex
	lastBusy     map[string]time.Time // by Sandbox name
	notOnlineFor map[string]time.Time // since when a Running Sandbox has not been online
	missing      map[string]int       // consecutive reconciles a Sandbox's runtime was absent
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Run reconciles until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.Cfg.ReconcileInterval)
	defer t.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.log().Warn("reconcile failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Reconcile runs one pass. A failure for one runtime is logged and does not
// stop the others; its state is simply not reported, so the server's TTL turns
// it unavailable if the failure persists.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	runtimes, err := r.Server.ManagedRuntimes(ctx)
	if err != nil {
		return fmt.Errorf("list managed runtimes: %w", err)
	}

	known := make(map[string]bool, len(runtimes))
	var reports []StateReport
	for _, rt := range runtimes {
		name, err := ObjectName(rt.DaemonID)
		if err != nil {
			r.log().Error("skipping runtime", "daemon_id", rt.DaemonID, "error", err)
			continue
		}
		known[name] = true
		state, err := r.reconcileRuntime(ctx, name, rt)
		if err != nil {
			r.log().Warn("reconcile runtime failed", "daemon_id", rt.DaemonID, "error", err)
			continue
		}
		reports = append(reports, StateReport{WorkspaceID: rt.WorkspaceID, DaemonID: rt.DaemonID, State: state})
	}
	if err := r.Server.ReportStates(ctx, reports); err != nil {
		r.log().Warn("report states failed", "error", err)
	}
	return r.collectOrphans(ctx, known)
}

func (r *Reconciler) reconcileRuntime(ctx context.Context, name string, rt Runtime) (string, error) {
	now := r.now()
	sb, err := r.Kube.GetSandbox(ctx, name)
	if err != nil {
		return "", err
	}
	busy := rt.QueuedTasks > 0 || rt.ActiveTasks > 0

	r.mu.Lock()
	if r.lastBusy == nil {
		r.lastBusy = map[string]time.Time{}
		r.notOnlineFor = map[string]time.Time{}
	}
	if _, ok := r.lastBusy[name]; !ok || busy {
		// First sight counts as busy: a restarted controller never suspends a
		// Sandbox on the strength of having forgotten when it last had work.
		r.lastBusy[name] = now
	}
	idleFor := now.Sub(r.lastBusy[name])
	r.mu.Unlock()

	if sb == nil {
		if err := r.createSandbox(ctx, name, rt); err != nil {
			return "", err
		}
		r.markNotOnline(name, now)
		return StateWaking, nil
	}

	switch sb.Mode {
	case ModeSuspended:
		if rt.QueuedTasks == 0 {
			return StateSuspended, nil
		}
		// Waking: the token may have expired while suspended, so always issue a
		// fresh one before the pod starts.
		if err := r.issueToken(ctx, name, rt); err != nil {
			return "", err
		}
		if err := r.Kube.SetMode(ctx, name, ModeRunning); err != nil {
			return "", err
		}
		r.markNotOnline(name, now)
		r.log().Info("waking sandbox", "daemon_id", rt.DaemonID, "queued", rt.QueuedTasks)
		return StateWaking, nil

	default: // Running
		if err := r.rotateTokenIfDue(ctx, name, rt); err != nil {
			r.log().Warn("token rotation failed", "daemon_id", rt.DaemonID, "error", err)
		}
		if !busy && idleFor >= r.Cfg.IdleSuspendAfter {
			if err := r.Kube.SetMode(ctx, name, ModeSuspended); err != nil {
				return "", err
			}
			r.log().Info("suspending idle sandbox", "daemon_id", rt.DaemonID, "idle", idleFor.Round(time.Second))
			return StateSuspended, nil
		}
		if rt.Status == "online" {
			r.mu.Lock()
			delete(r.notOnlineFor, name)
			r.mu.Unlock()
			return StateRunning, nil
		}
		r.mu.Lock()
		since, ok := r.notOnlineFor[name]
		if !ok {
			since = now
			r.notOnlineFor[name] = now
		}
		r.mu.Unlock()
		if now.Sub(since) >= r.Cfg.StuckAfter {
			r.log().Warn("sandbox running but daemon not online; marking unavailable",
				"daemon_id", rt.DaemonID, "for", now.Sub(since).Round(time.Second))
			return StateUnavailable, nil
		}
		return StateWaking, nil
	}
}

func (r *Reconciler) markNotOnline(name string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notOnlineFor == nil {
		r.notOnlineFor = map[string]time.Time{}
	}
	r.notOnlineFor[name] = now
}

func (r *Reconciler) createSandbox(ctx context.Context, name string, rt Runtime) error {
	llm := false
	if r.LLM != nil {
		if err := r.ensureLLMKey(ctx, name, rt); err != nil {
			return fmt.Errorf("llm key: %w", err)
		}
		llm = true
	}
	if err := r.issueToken(ctx, name, rt); err != nil {
		return err
	}
	if err := r.Kube.CreateSandbox(ctx, SandboxSpec{Name: name, WorkspaceID: rt.WorkspaceID, DaemonID: rt.DaemonID, LLM: llm}); err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	r.log().Info("created sandbox", "daemon_id", rt.DaemonID, "sandbox", name)
	return nil
}

// issueToken mints a daemon token and writes it to the token Secret that the
// pod mounts as a file.
func (r *Reconciler) issueToken(ctx context.Context, name string, rt Runtime) error {
	tok, err := r.Server.IssueDaemonToken(ctx, rt.WorkspaceID, rt.DaemonID, r.Cfg.TokenTTL)
	if err != nil {
		return fmt.Errorf("issue daemon token: %w", err)
	}
	return r.Kube.PutSecret(ctx, tokenSecretName(name), r.Cfg.labels(rt.WorkspaceID, rt.DaemonID),
		map[string]string{AnnotationTokenExpiresAt: tok.ExpiresAt.UTC().Format(time.RFC3339)},
		map[string]string{tokenSecretKey: tok.Value})
}

// rotateTokenIfDue replaces the token once half its lifetime is gone. A missing
// or unreadable expiry counts as due.
func (r *Reconciler) rotateTokenIfDue(ctx context.Context, name string, rt Runtime) error {
	sec, err := r.Kube.GetSecret(ctx, tokenSecretName(name))
	if err != nil {
		return err
	}
	if sec != nil {
		if exp, err := time.Parse(time.RFC3339, sec.Annotations[AnnotationTokenExpiresAt]); err == nil {
			if exp.Sub(r.now()) > r.Cfg.TokenTTL/2 {
				return nil
			}
		}
	}
	r.log().Info("rotating daemon token", "daemon_id", rt.DaemonID)
	return r.issueToken(ctx, name, rt)
}

// ensureLLMKey creates the Sandbox's LLM Secret once; the key lives for the
// life of the Sandbox and is revoked when it is deleted.
func (r *Reconciler) ensureLLMKey(ctx context.Context, name string, rt Runtime) error {
	if sec, err := r.Kube.GetSecret(ctx, llmSecretName(name)); err != nil {
		return err
	} else if sec != nil && sec.Data[llmSecretKey] != "" {
		return nil
	}
	key, err := r.LLM.Generate(ctx, LLMKeyRequest{Alias: rt.DaemonID, WorkspaceID: rt.WorkspaceID, DaemonID: rt.DaemonID, RuntimeID: rt.ID})
	if err != nil {
		return err
	}
	base := r.Cfg.LiteLLMURL
	data := map[string]string{
		llmSecretKey:           key, // kept for revocation
		"ANTHROPIC_BASE_URL":   base,
		"ANTHROPIC_AUTH_TOKEN": key,
		"OPENAI_BASE_URL":      base + "/v1",
		"OPENAI_API_KEY":       key,
	}
	if err := r.Kube.PutSecret(ctx, llmSecretName(name), r.Cfg.labels(rt.WorkspaceID, rt.DaemonID), nil, data); err != nil {
		// Do not leave a minted key that nothing records.
		if derr := r.LLM.Delete(ctx, key); derr != nil {
			err = errors.Join(err, derr)
		}
		return err
	}
	return nil
}

// collectOrphans removes Sandboxes whose runtime has been gone for several
// consecutive reconciles: revoke tokens, revoke the LLM key, delete Secrets
// and the Sandbox. A runtime that is merely missing from one list is not
// enough, so a transient server error never deletes a workdir.
func (r *Reconciler) collectOrphans(ctx context.Context, known map[string]bool) error {
	boxes, err := r.Kube.ListManagedSandboxes(ctx)
	if err != nil {
		return fmt.Errorf("list sandboxes: %w", err)
	}
	r.mu.Lock()
	if r.missing == nil {
		r.missing = map[string]int{}
	}
	seen := map[string]bool{}
	var due []SandboxInfo
	for _, b := range boxes {
		seen[b.Name] = true
		if known[b.Name] {
			delete(r.missing, b.Name)
			continue
		}
		r.missing[b.Name]++
		if r.missing[b.Name] >= r.Cfg.MissingGrace {
			due = append(due, b)
		}
	}
	for name := range r.missing {
		if !seen[name] {
			delete(r.missing, name)
		}
	}
	r.mu.Unlock()

	var errs []error
	for _, b := range due {
		if err := r.deleteSandbox(ctx, b); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", b.Name, err))
			continue
		}
		r.mu.Lock()
		delete(r.missing, b.Name)
		delete(r.lastBusy, b.Name)
		delete(r.notOnlineFor, b.Name)
		r.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (r *Reconciler) deleteSandbox(ctx context.Context, b SandboxInfo) error {
	ws, daemon := b.Labels[LabelWorkspaceID], b.Labels[LabelDaemonID]
	r.log().Info("deleting sandbox of removed runtime", "sandbox", b.Name, "daemon_id", daemon)
	if ws != "" && daemon != "" {
		if err := r.Server.RevokeDaemonTokens(ctx, ws, daemon); err != nil {
			return fmt.Errorf("revoke tokens: %w", err)
		}
	}
	if r.LLM != nil {
		if sec, err := r.Kube.GetSecret(ctx, llmSecretName(b.Name)); err != nil {
			return err
		} else if sec != nil && sec.Data[llmSecretKey] != "" {
			if err := r.LLM.Delete(ctx, sec.Data[llmSecretKey]); err != nil {
				return fmt.Errorf("revoke llm key: %w", err)
			}
		}
	}
	if err := r.Kube.DeleteSandbox(ctx, b.Name); err != nil {
		return err
	}
	if err := r.Kube.DeleteSecret(ctx, tokenSecretName(b.Name)); err != nil {
		return err
	}
	return r.Kube.DeleteSecret(ctx, llmSecretName(b.Name))
}
