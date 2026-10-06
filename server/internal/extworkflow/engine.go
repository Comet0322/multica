package extworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// AgentAccess is the invoke gate (handler.canInvokeAgent), injected so this
// package never imports handler.
type AgentAccess interface {
	CanInvokeAgent(ctx context.Context, workspaceID pgtype.UUID, actorType string, actorID pgtype.UUID, agentID pgtype.UUID) (bool, error)
}

// Publisher sends a workspace event (handler.publish).
type Publisher interface {
	Publish(eventType string, workspaceID string, actorType string, actorID string, payload map[string]any)
}

type Deps struct {
	Pool      *pgxpool.Pool
	Queries   *db.Queries
	Issues    *service.IssueService
	Tasks     *service.TaskService
	Access    AgentAccess
	Publisher Publisher
	Enabled   bool
}

// Engine schedules workflow runs. Every state change goes through advance:
// lock the run row, load it, apply Next, execute the effects and persist, in
// one transaction; notifications go out after the commit.
type Engine struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	issues  *service.IssueService
	tasks   *service.TaskService
	access  AgentAccess
	pub     Publisher
	enabled bool
}

func NewEngine(d Deps) *Engine {
	return &Engine{pool: d.Pool, q: d.Queries, issues: d.Issues, tasks: d.Tasks, access: d.Access, pub: d.Publisher, enabled: d.Enabled}
}

// Enabled reports the MULTICA_WORKFLOW_ENGINE kill switch.
func (e *Engine) Enabled() bool { return e != nil && e.enabled }

// InboxTypeEscalation is the inbox item a person receives on escalation.
const InboxTypeEscalation = "ext_workflow_escalation"

// Actor is who caused an event, as recorded on the timeline.
type Actor struct {
	Type       string // "engine" | "agent" | "member"
	ID         pgtype.UUID
	OnBehalfOf pgtype.UUID // member, when the supervisor acts for a commenter
	// TaskID is the agent task that produced a decision; run-wide and step
	// cancellations spare it so the deciding turn is not cut short.
	TaskID pgtype.UUID
}

// EngineActor is the actor of observations and scheduling.
var EngineActor = Actor{Type: "engine"}

// AdvanceInput is one event for Advance. StepKey is empty for run-level
// events (tick, cancel, summary end, abort without a step).
type AdvanceInput struct {
	StepKey        string
	Event          Event
	Actor          Actor
	ExpectedStatus StepStatus // optional guard: ErrStatusMismatch when the step moved on
}

// RunSnapshot is a run with its steps (in node order), its in-flight tasks and
// the RunState derived from them.
type RunSnapshot struct {
	Run    db.ExtWorkflowRun
	Def    Definition
	Steps  []db.ExtWorkflowRunStep
	ByKey  map[string]db.ExtWorkflowRunStep
	Active []db.AgentTaskQueue
	State  RunState
}

// KeyOf maps a step id to its node key.
func (s *RunSnapshot) KeyOf(stepID pgtype.UUID) (string, bool) {
	for _, st := range s.Steps {
		if st.ID == stepID {
			return st.NodeKey, true
		}
	}
	return "", false
}

// LoadRunSnapshot reads a run's steps and in-flight tasks through q (use the
// transaction holding the run lock when acting on it).
func LoadRunSnapshot(ctx context.Context, q *db.Queries, run db.ExtWorkflowRun) (*RunSnapshot, error) {
	var def Definition
	if err := json.Unmarshal(run.Definition, &def); err != nil {
		return nil, fmt.Errorf("decode run definition: %w", err)
	}
	rows, err := q.ListExtWorkflowRunSteps(ctx, run.ID)
	if err != nil {
		return nil, fmt.Errorf("list run steps: %w", err)
	}
	snap := &RunSnapshot{Run: run, Def: def, ByKey: make(map[string]db.ExtWorkflowRunStep, len(rows))}
	for _, r := range rows {
		snap.ByKey[r.NodeKey] = r
	}
	for _, n := range def.Nodes {
		if r, ok := snap.ByKey[n.Key]; ok {
			snap.Steps = append(snap.Steps, r)
		}
	}
	if snap.Active, err = q.ListActiveExtWorkflowTasksForRun(ctx, run.ID); err != nil {
		return nil, fmt.Errorf("list run tasks: %w", err)
	}
	summary, err := q.HasExtWorkflowSummaryTask(ctx, run.ID)
	if err != nil {
		return nil, fmt.Errorf("check summary task: %w", err)
	}
	st := RunState{
		Def: def, Status: RunStatus(run.Status), RewindsUsed: int(run.RewindsUsed),
		Steps: make(map[string]StepState, len(rows)), SupervisorFor: map[string]bool{}, SummaryRequested: summary,
	}
	for _, r := range snap.Steps {
		st.Steps[r.NodeKey] = StepState{
			Key: r.NodeKey, Status: StepStatus(r.Status), Attempts: int(r.Attempts),
			PendingReason: PendingReason(r.PendingReason.String), LastFeedback: r.LastFeedback.String,
			EscalationReason: r.EscalationReason.String, SupervisorWakes: int(r.SupervisorWakes),
		}
	}
	for _, t := range snap.Active {
		if t.ExtWorkflowRole.String != RoleSupervisor || t.ExtWorkflowKind.String == KindConversation {
			continue
		}
		st.SupervisorBusy = true
		if key, ok := snap.KeyOf(t.ExtWorkflowStepID); ok {
			st.SupervisorFor[key] = true
		}
	}
	snap.State = st
	return snap, nil
}

// LoadRun reads a run outside any transaction (for briefings and API reads).
func (e *Engine) LoadRun(ctx context.Context, runID pgtype.UUID) (*RunSnapshot, error) {
	run, err := e.q.GetExtWorkflowRun(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	return LoadRunSnapshot(ctx, e.q, run)
}

// Advance applies one event to a run: decisions (Part C), cancellation, ticks.
// Decisions answer ErrStatusMismatch / ErrIllegalDecision; observations that
// no longer apply become a tick.
func (e *Engine) Advance(ctx context.Context, runID pgtype.UUID, in AdvanceInput) error {
	if !e.Enabled() {
		return ErrEngineDisabled
	}
	actor := in.Actor
	if actor.Type == "" {
		actor = EngineActor
	}
	return e.advance(ctx, nil, runID, func(context.Context, *db.Queries, *RunSnapshot) ([]stepEvent, error) {
		return []stepEvent{{Key: in.StepKey, Event: in.Event, Actor: actor, Expected: in.ExpectedStatus}}, nil
	})
}

type stepEvent struct {
	Key      string
	Event    Event
	Actor    Actor
	Expected StepStatus
}

// deriveFunc turns the locked run into the events to apply.
type deriveFunc func(ctx context.Context, q *db.Queries, snap *RunSnapshot) ([]stepEvent, error)

// advance is the single entry point (spec §5.1). With ext != nil it runs in
// the caller's transaction and defers notifications to the caller's
// ExtAfterCommit collector.
func (e *Engine) advance(ctx context.Context, ext pgx.Tx, runID pgtype.UUID, derive deriveFunc) error {
	tx := ext
	if tx == nil {
		var err error
		if tx, err = e.pool.Begin(ctx); err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		defer tx.Rollback(ctx)
	}
	q := e.q.WithTx(tx)
	run, err := q.LockExtWorkflowRun(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRunNotFound
	}
	if err != nil {
		return fmt.Errorf("lock run: %w", err)
	}
	snap, err := LoadRunSnapshot(ctx, q, run)
	if err != nil {
		return err
	}
	evs, err := derive(ctx, q, snap)
	if err != nil {
		return err
	}
	out := &outbox{run: run}
	if err := e.apply(ctx, tx, q, snap, evs, out); err != nil {
		return err
	}
	if ext != nil {
		if after := service.ExtAfterCommitFrom(ctx); after != nil {
			flushCtx := context.WithoutCancel(ctx)
			after.Add(func() { e.flush(flushCtx, out) })
		} else {
			e.flush(ctx, out)
		}
		return nil
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	e.flush(ctx, out)
	return nil
}

// apply runs the events through Next, executes the effects and persists the
// resulting state. A run with nothing to do still gets one tick.
func (e *Engine) apply(ctx context.Context, tx pgx.Tx, q *db.Queries, snap *RunSnapshot, evs []stepEvent, out *outbox) error {
	state := snap.State.Clone()
	if len(evs) == 0 {
		evs = []stepEvent{{Event: Event{Kind: EvTick}, Actor: EngineActor}}
	}
	for _, ev := range evs {
		if ev.Expected != "" {
			if cur := state.Steps[ev.Key]; cur.Status != ev.Expected {
				return fmt.Errorf("%w: step %q is %s, not %s", ErrStatusMismatch, ev.Key, cur.Status, ev.Expected)
			}
		}
		next, effs, err := Next(state, ev.Key, ev.Event)
		if errors.Is(err, ErrNoTransition) && ev.Event.Kind != EvDecision {
			next, effs, err = Next(state, "", Event{Kind: EvTick})
			if errors.Is(err, ErrNoTransition) {
				continue
			}
		}
		if err != nil {
			return err
		}
		if state, err = e.execute(ctx, tx, q, snap, next, effs, ev.Actor, out); err != nil {
			return err
		}
	}
	return e.persist(ctx, q, snap, state, out)
}

// execute performs effects in order. A dispatch that cannot start (agent
// archived, access revoked, slot busy) feeds a failure back into Next, whose
// follow-up effects are appended (spec §4.2, §10).
func (e *Engine) execute(ctx context.Context, tx pgx.Tx, q *db.Queries, snap *RunSnapshot, state RunState, effs []Effect, actor Actor, out *outbox) (RunState, error) {
	for i := 0; i < len(effs); i++ {
		f := effs[i]
		by := actor
		if f.Engine {
			by = EngineActor
		}
		out.touched = true
		var follow *Event
		var err error
		switch f.Kind {
		case EffEnqueueStep:
			if err = e.enqueueStep(ctx, tx, snap, state, f.Step, out); err != nil && isDispatchFailure(err) {
				follow, err = &Event{Kind: EvStepFailed, Reason: "dispatch_failed", Detail: map[string]any{"error": err.Error()}}, nil
			}
		case EffEnqueueSupervisor:
			if err = e.enqueueSupervisor(ctx, tx, snap, f, out); err != nil && isDispatchFailure(err) {
				state.SupervisorBusy = false
				if f.Step == "" {
					follow = &Event{Kind: EvSummaryEnded}
				} else {
					delete(state.SupervisorFor, f.Step)
					follow = &Event{Kind: EvSupervisorNoDecision, Reason: "the supervisor could not be started: " + err.Error()}
				}
				err = nil
			}
		case EffSetChildStatus:
			err = e.setIssueStatus(ctx, q, snap.ByKey[f.Step].IssueID, f.IssueStatus, out)
		case EffSetParentStatus:
			err = e.setIssueStatus(ctx, q, snap.Run.IssueID, f.IssueStatus, out)
		case EffCancelStepWork:
			err = e.cancelTasks(ctx, q, out, func() ([]db.AgentTaskQueue, error) {
				return q.CancelExtWorkflowStepTasks(ctx, db.CancelExtWorkflowStepTasksParams{StepID: snap.ByKey[f.Step].ID, ExceptTaskID: actor.TaskID})
			})
		case EffCancelRunTasks:
			err = e.cancelTasks(ctx, q, out, func() ([]db.AgentTaskQueue, error) {
				return q.CancelExtWorkflowRunTasks(ctx, db.CancelExtWorkflowRunTasksParams{RunID: snap.Run.ID, ExceptTaskID: actor.TaskID})
			})
		case EffEscalate:
			err = e.escalate(ctx, q, snap, f, out)
		case EffMilestone:
			err = e.milestone(ctx, q, snap, f.Milestone, f.Step, f.Reason, out)
		case EffRecordEvent:
			err = e.recordEvent(ctx, q, snap, f, by)
		}
		if err != nil {
			return state, err
		}
		if follow != nil {
			next, more, nerr := Next(state, f.Step, *follow)
			if nerr != nil && !errors.Is(nerr, ErrNoTransition) {
				return state, nerr
			}
			if nerr == nil {
				state = next
				for j := range more {
					more[j].Engine = true
				}
				effs = append(effs, more...)
			}
		}
	}
	return state, nil
}

func isDispatchFailure(err error) bool {
	return errors.Is(err, ErrAgentNotInvokable) || errors.Is(err, service.ErrExtAgentUnavailable) ||
		errors.Is(err, service.ErrExtTaskSlotBusy) || errors.Is(err, service.ErrAttributionFailClosed)
}

func (e *Engine) checkInvoke(ctx context.Context, run db.ExtWorkflowRun, agentID pgtype.UUID) error {
	if e.access == nil {
		return nil
	}
	ok, err := e.access.CanInvokeAgent(ctx, run.WorkspaceID, run.TriggeredByType, run.TriggeredByID, agentID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAgentNotInvokable
	}
	return nil
}

func memberActor(run db.ExtWorkflowRun) pgtype.UUID {
	if run.TriggeredByType == "member" {
		return run.TriggeredByID
	}
	return pgtype.UUID{}
}

func (e *Engine) enqueueStep(ctx context.Context, tx pgx.Tx, snap *RunSnapshot, state RunState, key string, out *outbox) error {
	node, _ := snap.Def.NodeByKey(key)
	agentID, err := util.ParseUUID(node.AgentID)
	if err != nil {
		return fmt.Errorf("%w: node %q has no valid agent", ErrAgentNotInvokable, key)
	}
	if err := e.checkInvoke(ctx, snap.Run, agentID); err != nil {
		return err
	}
	note := fmt.Sprintf("Workflow step %q (%s), attempt %d of %d. Your instructions carry the workflow briefing for this step.",
		node.Title, node.Key, state.Steps[key].Attempts, node.MaxAttempts)
	task, err := e.tasks.EnqueueExtWorkflowTask(ctx, tx, service.ExtWorkflowTaskParams{
		IssueID: snap.ByKey[key].IssueID, AgentID: agentID, RunID: snap.Run.ID, StepID: snap.ByKey[key].ID,
		Role: RoleStep, Kind: KindStep, HandoffNote: note, ActorUserID: memberActor(snap.Run),
	})
	if err != nil {
		return err
	}
	out.tasks = append(out.tasks, task)
	return nil
}

func (e *Engine) enqueueSupervisor(ctx context.Context, tx pgx.Tx, snap *RunSnapshot, f Effect, out *outbox) error {
	supervisorID, err := util.ParseUUID(snap.Def.SupervisorAgentID)
	if err != nil {
		return fmt.Errorf("%w: the run has no valid supervisor", ErrAgentNotInvokable)
	}
	if err := e.checkInvoke(ctx, snap.Run, supervisorID); err != nil {
		return err
	}
	note := "Workflow supervisor: every step has settled. Write the run summary as one comment on this issue."
	var stepID pgtype.UUID
	if f.Step != "" {
		node, _ := snap.Def.NodeByKey(f.Step)
		stepID = snap.ByKey[f.Step].ID
		note = fmt.Sprintf("Workflow supervisor: %s for step %q (%s). Your instructions carry the briefing and the decision format.",
			f.SupervisorKind, node.Title, node.Key)
	}
	task, err := e.tasks.EnqueueExtWorkflowTask(ctx, tx, service.ExtWorkflowTaskParams{
		IssueID: snap.Run.IssueID, AgentID: supervisorID, RunID: snap.Run.ID, StepID: stepID,
		Role: RoleSupervisor, Kind: f.SupervisorKind, HandoffNote: note, ActorUserID: memberActor(snap.Run),
	})
	if err != nil {
		return err
	}
	out.tasks = append(out.tasks, task)
	return nil
}

// setIssueStatus writes a status through UpdateIssueStatus: no HTTP handler,
// so no WillEnqueueRun and no agent enqueue. issue:updated goes out after the
// commit.
func (e *Engine) setIssueStatus(ctx context.Context, q *db.Queries, issueID pgtype.UUID, status string, out *outbox) error {
	prev, err := q.GetIssue(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load issue: %w", err)
	}
	if issuestatus.Effective(ctx, q, prev.WorkspaceID, prev.Status) == status {
		return nil
	}
	updated, err := q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issueID, Status: status, WorkspaceID: prev.WorkspaceID})
	if err != nil {
		return fmt.Errorf("set issue status: %w", err)
	}
	out.issueUpdates = append(out.issueUpdates, issueUpdate{issue: updated, prev: prev.Status})
	return nil
}

func (e *Engine) cancelTasks(ctx context.Context, q *db.Queries, out *outbox, cancel func() ([]db.AgentTaskQueue, error)) error {
	cancelled, err := cancel()
	if err != nil {
		return fmt.Errorf("cancel tasks: %w", err)
	}
	if err := service.SettleTerminalTaskState(ctx, q, cancelled...); err != nil {
		return fmt.Errorf("settle cancelled tasks: %w", err)
	}
	out.cancelled = append(out.cancelled, cancelled...)
	return nil
}

func (e *Engine) recordEvent(ctx context.Context, q *db.Queries, snap *RunSnapshot, f Effect, by Actor) error {
	payload := f.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode event payload: %w", err)
	}
	var stepID pgtype.UUID
	if f.Step != "" {
		stepID = snap.ByKey[f.Step].ID
	}
	_, err = q.CreateExtWorkflowRunEvent(ctx, db.CreateExtWorkflowRunEventParams{
		ID: dbid.NewV7(), RunID: snap.Run.ID, WorkspaceID: snap.Run.WorkspaceID, StepID: stepID,
		Kind: f.RunEvent, ActorType: by.Type, ActorID: by.ID, OnBehalfOf: by.OnBehalfOf, Payload: raw,
	})
	if err != nil {
		return fmt.Errorf("record run event: %w", err)
	}
	return nil
}

// escalate notifies a person (the triggering member, else the workflow's
// creator) and posts the escalation milestone on the parent.
func (e *Engine) escalate(ctx context.Context, q *db.Queries, snap *RunSnapshot, f Effect, out *outbox) error {
	parent, err := q.GetIssue(ctx, snap.Run.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load parent: %w", err)
	}
	recipient := memberActor(snap.Run)
	if !recipient.Valid {
		if wf, err := q.GetExtWorkflowInWorkspace(ctx, db.GetExtWorkflowInWorkspaceParams{ID: snap.Run.WorkflowID, WorkspaceID: snap.Run.WorkspaceID}); err == nil {
			recipient = wf.CreatorID
		}
	}
	if recipient.Valid {
		details, _ := json.Marshal(map[string]any{
			"run_id": util.UUIDToString(snap.Run.ID), "step_id": util.UUIDToString(snap.ByKey[f.Step].ID),
			"node_key": f.Step, "reason": f.Reason,
		})
		item, err := q.CreateInboxItem(ctx, db.CreateInboxItemParams{
			ID: dbid.NewV7(), WorkspaceID: parent.WorkspaceID, RecipientType: "member", RecipientID: recipient,
			Type: InboxTypeEscalation, Severity: "action_required", IssueID: parent.ID, Title: parent.Title,
			Body: pgtype.Text{String: f.Reason, Valid: f.Reason != ""}, ActorType: pgtype.Text{String: "system", Valid: true},
			Details: details,
		})
		if err != nil {
			return fmt.Errorf("create escalation inbox item: %w", err)
		}
		out.inbox = append(out.inbox, inboxNote{item: item, issueStatus: parent.Status})
	}
	return e.milestone(ctx, q, snap, MilestoneEscalated, f.Step, f.Reason, out)
}

// milestone posts one of the few system comments the engine writes on the
// parent (spec §5.6).
func (e *Engine) milestone(ctx context.Context, q *db.Queries, snap *RunSnapshot, kind, stepKey, reason string, out *outbox) error {
	parent, err := q.GetIssue(ctx, snap.Run.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load parent: %w", err)
	}
	name := "workflow"
	if wf, err := q.GetExtWorkflowInWorkspace(ctx, db.GetExtWorkflowInWorkspaceParams{ID: snap.Run.WorkflowID, WorkspaceID: snap.Run.WorkspaceID}); err == nil {
		name = wf.Name
	}
	created, err := q.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: parent.ID, WorkspaceID: parent.WorkspaceID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true},
		Content: milestoneText(kind, name, snap.Def, stepKey, reason), Type: "system",
	})
	if err != nil {
		return fmt.Errorf("create milestone comment: %w", err)
	}
	out.comments = append(out.comments, commentNote{issue: parent, row: created})
	return nil
}

func milestoneText(kind, workflowName string, def Definition, stepKey, reason string) string {
	switch kind {
	case MilestoneRunStarted:
		return fmt.Sprintf("Workflow **%s** started with %d steps.", workflowName, len(def.Nodes))
	case MilestoneEscalated:
		title := stepKey
		if n, ok := def.NodeByKey(stepKey); ok {
			title = n.Title
		}
		return fmt.Sprintf("Workflow step **%s** needs a decision from a person: %s", title, reason)
	case MilestoneRunFinished:
		return fmt.Sprintf("Workflow **%s** finished.", workflowName)
	case MilestoneRunFailed:
		return fmt.Sprintf("Workflow **%s** was aborted: %s", workflowName, reason)
	case MilestoneRunCancelled:
		return fmt.Sprintf("Workflow **%s** was cancelled: %s", workflowName, reason)
	}
	return fmt.Sprintf("Workflow **%s**: %s", workflowName, kind)
}

// persist writes the changed steps and run row.
func (e *Engine) persist(ctx context.Context, q *db.Queries, snap *RunSnapshot, state RunState, out *outbox) error {
	changed := false
	for _, row := range snap.Steps {
		before, after := snap.State.Steps[row.NodeKey], state.Steps[row.NodeKey]
		if before == after {
			continue
		}
		changed = true
		if _, err := q.UpdateExtWorkflowRunStep(ctx, db.UpdateExtWorkflowRunStepParams{
			ID: row.ID, Status: string(after.Status), Attempts: int32(after.Attempts),
			PendingReason: text(string(after.PendingReason)), LastFeedback: text(after.LastFeedback),
			EscalationReason: text(after.EscalationReason), SupervisorWakes: int32(after.SupervisorWakes),
		}); err != nil {
			return fmt.Errorf("update step %q: %w", row.NodeKey, err)
		}
	}
	switch {
	case state.Status != snap.State.Status || state.RewindsUsed != snap.State.RewindsUsed:
		run, err := q.UpdateExtWorkflowRunState(ctx, db.UpdateExtWorkflowRunStateParams{
			ID: snap.Run.ID, Status: string(state.Status), RewindsUsed: int32(state.RewindsUsed),
		})
		if err != nil {
			return fmt.Errorf("update run: %w", err)
		}
		out.run = run
		changed = true
	case changed || out.touched:
		if err := q.TouchExtWorkflowRun(ctx, snap.Run.ID); err != nil {
			return fmt.Errorf("touch run: %w", err)
		}
	}
	out.runChanged = changed || out.touched
	return nil
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

type issueUpdate struct {
	issue db.Issue
	prev  string
}

type commentNote struct {
	issue db.Issue
	row   db.CreateCommentRow
}

type inboxNote struct {
	item        db.InboxItem
	issueStatus string
}

type createdIssue struct {
	issue     db.Issue
	actorType string
	actorID   string
}

// outbox collects what a committed advance announces.
type outbox struct {
	run          db.ExtWorkflowRun
	created      []createdIssue
	issueUpdates []issueUpdate
	comments     []commentNote
	inbox        []inboxNote
	tasks        []db.AgentTaskQueue
	cancelled    []db.AgentTaskQueue
	touched      bool
	runChanged   bool
}

// flush publishes after the commit. Cancelled tasks go last: their
// task:cancelled listeners re-enter the engine synchronously.
func (e *Engine) flush(ctx context.Context, out *outbox) {
	for _, c := range out.created {
		e.issues.ExtPublishIssueCreated(ctx, c.issue, c.actorType, c.actorID)
	}
	for _, u := range out.issueUpdates {
		e.tasks.ExtBroadcastIssueUpdated(ctx, u.issue, u.prev)
	}
	for _, c := range out.comments {
		e.tasks.ExtPublishSystemComment(c.issue, c.row)
	}
	for _, n := range out.inbox {
		e.tasks.ExtPublishInbox(n.item, n.issueStatus)
	}
	for _, t := range out.tasks {
		e.tasks.PublishExtWorkflowTaskQueued(ctx, t)
	}
	if out.runChanged && e.pub != nil {
		e.pub.Publish(protocol.EventExtWorkflowRunUpdated, util.UUIDToString(out.run.WorkspaceID), "system", "", map[string]any{
			"run_id":      util.UUIDToString(out.run.ID),
			"issue_id":    util.UUIDToString(out.run.IssueID),
			"workflow_id": util.UUIDToString(out.run.WorkflowID),
		})
	}
	if len(out.cancelled) > 0 {
		e.tasks.BroadcastCancelledTasks(ctx, util.UUIDToString(out.run.WorkspaceID), out.cancelled)
	}
}
