package daemonbridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// run drives one task from claim to delivered outcome.
type run struct {
	b *Bridge
	e *Entry
	// job is rebuilt from the claim on every start, so the journal never has
	// to carry the rendered brief.
	job Job
}

func newRun(b *Bridge, e *Entry) *run { return &run{b: b, e: e} }

func (r *run) log() *slog.Logger {
	return r.b.log.With("task", r.e.Task.ID)
}

func (r *run) save() error { return r.b.journal.Save(r.e) }

func (r *run) run(ctx context.Context) {
	log := r.log()
	if r.e.Phase == PhaseFinished {
		r.deliver(ctx)
		return
	}
	if r.e.Phase == PhaseClaimed {
		if !r.begin(ctx) {
			return
		}
	} else {
		// The backend already holds the run, so the job is not rebuilt: skill
		// bundles can only be resolved while the task is preparing. Only the
		// task's tool endpoint, which lives in this process, is reopened.
		r.reopenTools()
	}

	runCtx, cancel := context.WithTimeout(ctx, r.b.cfg.TaskTimeout)
	defer cancel()
	out, ok := r.pump(runCtx)
	if !ok {
		if ctx.Err() != nil {
			log.Info("bridge stopping; run stays journaled for the next process")
			return
		}
		out = &Outcome{Status: OutcomeFailed, Error: "task exceeded " + r.b.cfg.TaskTimeout.String(), FailureReason: string(taskfailure.ReasonTimeout)}
		_ = r.b.backend.Interrupt(ctx, *r.e.Handle)
	}
	r.finish(ctx, out)
}

func (r *run) reopenTools() {
	if ts, ok := r.b.cfg.Tools.(TaskScoped); ok {
		ts.Open(r.e.Task, classify(r.e.Task))
	}
}

func (r *run) prepareJob(ctx context.Context) error {
	job, err := BuildJob(ctx, r.e.Task, r.b.backend.Capabilities(), r.b.client)
	if err != nil {
		return err
	}
	if tools := r.b.cfg.Tools; tools != nil {
		job.Tools = tools.Specs(r.e.Task, job.Kind)
		if bn, ok := tools.(BriefNoter); ok {
			job.System = bn.BriefNote(job.Kind) + "\n" + job.System
		}
		if ts, ok := tools.(TaskScoped); ok {
			job.MCP = ts.Open(r.e.Task, job.Kind)
		}
	}
	r.job = job
	return nil
}

// begin starts the task on the server and the backend. It reports false when
// the task ended here (failed or no longer ours).
func (r *run) begin(ctx context.Context) bool {
	log := r.log()
	if err := r.prepareJob(ctx); err != nil {
		r.finish(ctx, failure(err))
		return false
	}
	supplements, err := r.b.client.StartTask(ctx, r.e.Task, r.b.startCapabilities()...)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		// The server did not accept the start, so it still owns the task; its
		// sweeper requeues what stays dispatched.
		log.Warn("start refused; dropping task", "error", err)
		_ = r.b.journal.Remove(r.e.Task.ID)
		return false
	}
	r.e.Supplements = supplements

	h, err := r.startBackend(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		r.finish(ctx, failure(err))
		return false
	}
	r.e.Handle = &h
	r.e.Phase = PhaseRunning
	if err := r.save(); err != nil {
		log.Error("journal save failed after backend start", "error", err)
	}
	if h.SessionID != "" {
		if err := r.b.client.PinTaskSession(ctx, r.e.Task.ID, h.SessionID, ""); err != nil {
			log.Warn("pin session", "error", err)
		}
	}
	return true
}

var startBackoffs = []time.Duration{time.Second, 3 * time.Second, 10 * time.Second}

// startBackend retries transient errors and fails fast on a refusal.
func (r *run) startBackend(ctx context.Context) (Handle, error) {
	var err error
	for i := 0; ; i++ {
		var h Handle
		h, err = r.b.backend.Start(ctx, r.job)
		if err == nil {
			return h, nil
		}
		var rej *RejectedError
		if errors.As(err, &rej) || errors.Is(err, ErrUnsupported) || i >= len(startBackoffs) {
			return Handle{}, err
		}
		select {
		case <-ctx.Done():
			return Handle{}, ctx.Err()
		case <-time.After(startBackoffs[i]):
		}
	}
}

// pump follows the run until it ends. ok is false when ctx ended first.
func (r *run) pump(ctx context.Context) (out *Outcome, ok bool) {
	log := r.log()
	b := r.b
	h := *r.e.Handle
	events := time.NewTicker(b.cfg.EventInterval)
	status := time.NewTicker(b.cfg.StatusInterval)
	defer events.Stop()
	defer status.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return nil, false
		case <-status.C:
			if out := r.checkServerState(ctx, h); out != nil {
				return out, true
			}
		case <-events.C:
			done, err := r.drain(ctx, h)
			switch {
			case err == nil:
				failures = 0
			case errors.Is(err, ErrGone):
				return &Outcome{Status: OutcomeFailed, Error: "the backend lost the run", FailureReason: string(taskfailure.ReasonRuntimeRecovery)}, true
			case ctx.Err() != nil:
				return nil, false
			default:
				failures++
				log.Warn("reading backend events", "error", err, "consecutive", failures)
				if failures >= 20 {
					return &Outcome{Status: OutcomeFailed, Error: "backend unreachable: " + err.Error(), FailureReason: string(taskfailure.ReasonRuntimeOffline)}, true
				}
			}
			if done != nil {
				return done, true
			}
		}
	}
}

// checkServerState stops the run when the server cancelled or finished it, and
// forwards supplements.
func (r *run) checkServerState(ctx context.Context, h Handle) *Outcome {
	st, err := r.b.client.GetTaskStatus(ctx, r.e.Task.ID)
	if err != nil {
		return nil
	}
	switch st {
	case "cancelled", "failed", "completed":
		if err := r.b.backend.Interrupt(ctx, h); err != nil {
			r.log().Warn("interrupt", "error", err)
		}
		return &Outcome{Status: OutcomeCancelled}
	}
	if r.e.Supplements {
		r.forwardSupplement(ctx, h)
	}
	return nil
}

func (r *run) forwardSupplement(ctx context.Context, h Handle) {
	s, err := r.b.client.ClaimTaskSupplement(ctx, r.e.Task.ID)
	if err != nil || s == nil {
		return
	}
	sendErr := r.b.backend.Send(ctx, h, s.Content)
	text := ""
	if sendErr != nil {
		text = sendErr.Error()
	}
	if err := r.b.client.AckTaskSupplement(ctx, r.e.Task.ID, s.CommentID, sendErr == nil, text); err != nil {
		r.log().Warn("ack supplement", "error", err)
	}
}

// drain reads pending events, delivers them, and moves the cursor only after
// the server accepted them, so a crash re-sends rather than loses.
func (r *run) drain(ctx context.Context, h Handle) (*Outcome, error) {
	batch, err := r.b.backend.Events(ctx, h, r.e.Cursor)
	if err != nil {
		return nil, err
	}
	var (
		msgs []daemon.TaskMessageData
		done *Outcome
		seen = map[string]bool{}
	)
	for _, id := range r.e.Reported {
		seen[id] = true
	}
	for _, ev := range batch.Events {
		switch ev.Kind {
		case EventUsage:
			if !seen[ev.ID] {
				r.e.Usage = append(r.e.Usage, ev.Usage...)
				r.e.Reported = append(r.e.Reported, ev.ID)
			}
		case EventToolRequest:
			r.answerTool(ctx, h, ev)
		case EventDone:
			done = ev.Done
		default:
			if m, ok := r.message(ev); ok {
				msgs = append(msgs, m)
			}
		}
	}
	if len(msgs) > 0 {
		if err := r.b.client.ReportTaskMessages(ctx, r.e.Task.ID, msgs); err != nil {
			return nil, fmt.Errorf("report messages: %w", err)
		}
	}
	r.e.Cursor = batch.Cursor
	if err := r.save(); err != nil {
		r.log().Warn("journal save", "error", err)
	}
	return done, nil
}

func (r *run) message(ev Event) (daemon.TaskMessageData, bool) {
	m := daemon.TaskMessageData{CallID: ev.CallID, Seq: r.e.NextSeq, CreatedAt: time.Now().UTC()}
	switch ev.Kind {
	case EventText, EventThinking, EventError:
		if strings.TrimSpace(ev.Text) == "" {
			return m, false
		}
		m.Type, m.Content = string(ev.Kind), ev.Text
	case EventToolUse:
		m.Type, m.Tool, m.Input = "tool_use", ev.Tool, ev.Input
	case EventToolResult:
		m.Type, m.Tool, m.Output = "tool_result", ev.Tool, ev.Output
	default:
		return m, false
	}
	r.e.NextSeq++
	return m, true
}

func (r *run) answerTool(ctx context.Context, h Handle, ev Event) {
	res := ToolResult{IsError: true, Output: "no tool runner is configured"}
	switch {
	case ev.Request == nil:
		res.Output = "malformed tool request"
	case r.b.cfg.Tools != nil:
		res = r.b.cfg.Tools.Run(ctx, r.e.Task, *ev.Request)
	}
	if err := r.b.backend.ToolResult(ctx, h, ev.CallID, res); err != nil {
		r.log().Warn("return tool result", "call", ev.CallID, "error", err)
	}
}

// finish records the outcome durably, then delivers it.
func (r *run) finish(ctx context.Context, out *Outcome) {
	r.e.Phase = PhaseFinished
	r.e.Outcome = out
	if err := r.save(); err != nil {
		r.log().Error("journal save of outcome failed", "error", err)
	}
	r.deliver(ctx)
}

func (r *run) deliver(ctx context.Context) {
	log := r.log()
	c := r.b.client
	out := r.e.Outcome
	if out == nil {
		out = &Outcome{Status: OutcomeFailed, Error: "outcome lost", FailureReason: string(taskfailure.ReasonRuntimeRecovery)}
	}
	if !r.e.UsageSent && len(r.e.Usage) > 0 && out.Status != OutcomeCancelled {
		if err := c.ReportTaskUsage(ctx, r.e.Task.ID, mergeUsage(r.e.Usage)); err != nil {
			log.Warn("report usage", "error", err)
		}
		r.e.UsageSent = true
	}
	session := out.SessionID
	if session == "" && r.e.Handle != nil {
		session = r.e.Handle.SessionID
	}
	var err error
	switch out.Status {
	case OutcomeCompleted:
		err = c.CompleteTask(ctx, r.e.Task.ID, out.Output, "", session, "", false, out.RetiredSessionID, "")
	case OutcomeCancelled:
		err = c.AckTaskCancelled(ctx, r.e.Task.ID, daemon.TaskCancelAck{})
	default:
		msg := out.Error
		if msg == "" {
			msg = "the backend run failed"
		}
		err = c.FailTask(ctx, r.e.Task.ID, msg, session, "", "", out.FailureReason, false, out.RetiredSessionID, "")
	}
	if err != nil {
		if ctx.Err() == nil {
			// Keep the entry: the next process delivers it.
			log.Error("deliver outcome", "status", out.Status, "error", err)
		}
		return
	}
	if r.e.Handle != nil {
		if err := r.b.backend.Release(ctx, *r.e.Handle); err != nil {
			log.Warn("release", "error", err)
		}
	}
	if ts, ok := r.b.cfg.Tools.(TaskScoped); ok {
		ts.Close(r.e.Task.ID)
	}
	if err := r.b.journal.Remove(r.e.Task.ID); err != nil {
		log.Warn("journal remove", "error", err)
	}
}

func failure(err error) *Outcome {
	var rej *RejectedError
	if errors.As(err, &rej) {
		return &Outcome{Status: OutcomeFailed, Error: rej.Message, FailureReason: rej.FailureReason}
	}
	return &Outcome{Status: OutcomeFailed, Error: err.Error(), FailureReason: string(taskfailure.ReasonEnvironmentPrepareFailed)}
}

func mergeUsage(in []Usage) []daemon.TaskUsageEntry {
	type key struct{ p, m string }
	sum := map[key]*daemon.TaskUsageEntry{}
	var order []key
	for _, u := range in {
		k := key{u.Provider, u.Model}
		e := sum[k]
		if e == nil {
			e = &daemon.TaskUsageEntry{Provider: u.Provider, Model: u.Model}
			sum[k] = e
			order = append(order, k)
		}
		e.InputTokens += u.InputTokens
		e.OutputTokens += u.OutputTokens
		e.CacheReadTokens += u.CacheReadTokens
		e.CacheWriteTokens += u.CacheWriteTokens
		e.CostUSDTicks += u.CostUSDTicks
	}
	out := make([]daemon.TaskUsageEntry, 0, len(order))
	for _, k := range order {
		out = append(out, *sum[k])
	}
	return out
}
