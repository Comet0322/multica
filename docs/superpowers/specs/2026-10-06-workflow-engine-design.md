# Workflow Engine (prototype) — Design

Status: draft for review. Branch: `feat/workflow-engine`.

## 1. Goal

Multica has no fixed, repeatable workflow. This prototype lets a person describe a
workflow as YAML inside an issue; a backend component then expands it into step issues,
dispatches them to agents in dependency order, gates steps on human review, and closes
the definition issue when every step is done.

Success criteria:

- Writing a valid YAML workflow in an issue labelled `flow:<name>` and moving it to `todo`
  produces one backlog issue per step, and the steps run in dependency order.
- A step marked `approval: true` waits in `blocked` until the definition issue's creator
  comments `/accept` (done) or `/reject` (agent redoes the work).
- A failed step can be retried with `/retry`.
- The definition issue becomes `done` only when all step issues are `done`.
- No new tables and no migrations. New code lives in `server/internal/workflow/`;
  edits to existing code are limited to the wiring points listed in section 9.

Non-goals (prototype): UI, `bash`/`loop` nodes, conditional branching, per-step timeouts,
cross-workspace flows, editing a running workflow.

## 2. Definition issue

- A definition issue is an issue carrying a label named `flow:<name>` where `<name>` is
  non-empty (case-insensitive prefix `flow:`; the name itself carries no meaning).
- The workflow YAML is the **first fenced ```` ```yaml ```` block** in the issue description.
- The issue is eligible when: it has such a label, its status is `todo`, and its metadata
  has no `workflow` key yet. Authors draft in `backlog` and move to `todo` to start.

## 3. YAML format (modelled on Archon's `nodes:` DAG)

```yaml
nodes:
  - id: plan
    agent: Planner
    prompt: Explore the codebase and write an implementation plan

  - id: implement
    depends_on: [plan]
    agent: Coder
    prompt: Implement the plan
    max_retries: 2

  - id: review
    depends_on: [implement]
    agent: Reviewer
    approval: true
```

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | Unique within the workflow; `[a-z0-9_-]+`. |
| `agent` | yes | Agent name, resolved within the workspace at expansion time. |
| `prompt` | yes | Becomes the step issue's description. |
| `depends_on` | no | Node ids that must be `done` before this node is dispatched. |
| `approval` | no (false) | After the agent finishes, wait in `blocked` for `/accept`. |
| `max_retries` | no (1) | Redo attempts after a `/reject` or an agent failure. |

Validation (all errors are collected and posted as one comment on the definition issue;
nothing is expanded): duplicate ids, unknown `depends_on`, dependency cycles, unknown or
archived agents, missing required fields, malformed YAML. The definition issue stays in
`todo` with `workflow.state = "invalid"` recorded so it is not re-validated every tick
until its description or labels change.

## 4. State lives in `issue.metadata` (no new tables)

Definition issue, key `workflow`:

```json
{"state": "expanding | running | invalid | blocked | done", "error_hash": "…"}
```

Step issue, key `workflow`:

```json
{"run": "<definition issue id>", "node": "implement", "deps": ["plan"],
 "agent": "Coder", "approval": false, "max_retries": 2,
 "attempts": 0, "phase": "pending"}
```

- A step issue also has `parent_issue_id` = the definition issue, an assignee = the agent,
  and the node `prompt` as description. **The step issues are the snapshot**: later edits to
  the definition description do not affect a running workflow.
- Queries use `metadata @> '{"workflow": {...}}'` (GIN-indexed) via a new sqlc file
  `server/pkg/db/queries/workflow.sql`; writes use the existing atomic
  `SetIssueMetadataKey`.
- Metadata is user-writable through the API. Each tick reconciles: if a step's `phase`
  contradicts its issue status per the mapping in section 5, the engine rewrites the
  phase it can justify and leaves a comment. Size is well under the 8KB metadata cap.

## 5. State machine as data

All transition rules live in one file, `workflow/transitions.go`, as two tables. No other
file branches on phase or status.

Phases: `pending`, `running`, `blocked`, `done`, `failed`.

Phase → issue status mapping:

| Phase | Issue status |
|---|---|
| pending | backlog |
| running | in_progress |
| blocked | blocked |
| done | done |
| failed | blocked |

Transition rules `(phase, event, guard) → (phase, actions)`:

| From | Event | Guard | To | Actions |
|---|---|---|---|---|
| pending | deps_met | | running | dispatch |
| running | agent_finished | `!approval` | done | |
| running | agent_finished | `approval` | blocked | comment asking for review |
| running | agent_failed | attempts < max_retries | running | attempts++, dispatch |
| running | agent_failed | otherwise | failed | comment with failure |
| blocked | accept | | done | |
| blocked | reject | attempts < max_retries | running | attempts++, dispatch with reject comment |
| blocked | reject | otherwise | failed | |
| failed | retry | | running | attempts = 0, dispatch |

Events are produced by the tick (`deps_met`, `agent_finished`, `agent_failed`) or the
command listener (`accept`, `reject`, `retry`). The transition function is pure
(`Next(step, event) (Transition, ok)`), so it is unit-testable without a database.

## 6. Components (`server/internal/workflow/`)

| File | Responsibility |
|---|---|
| `definition.go` | Find the YAML block; parse and validate into a `Definition`. |
| `transitions.go` | The two tables and the pure `Next` function. |
| `engine.go` | `Engine.Tick(ctx)`: expand eligible definitions, derive events, apply transitions, complete runs. |
| `expand.go` | Create step issues in `backlog` via `IssueService.Create` (creator = definition creator, parent, assignee, metadata). |
| `dispatch.go` | Move a step to its running status and enqueue the agent task. |
| `commands.go` | Bus listener on `comment:created`; parse `/accept`, `/reject`, `/retry`; authorize; emit events. |
| `store.go` | Thin wrappers over the new sqlc queries and `SetIssueMetadataKey`. |

Reused as-is: `IssueService.Create` (labels, parent, duplicate guard, `issue:created`
event), `TaskService.EnqueueTaskForIssue` / `EnqueueTaskForMention`, the event bus, the DB
scheduler (`internal/scheduler`, multi-instance safe via `sys_cron_executions`).

## 7. Tick algorithm

`WorkflowTickJob` is a global scheduler job (cadence ~15s, modelled on
`scheduler/jobs_issue_wakeup.go`) calling `Engine.Tick`:

1. **Expand.** For each eligible definition issue: claim it by conditionally setting
   `workflow.state = "expanding"` (only one instance wins). Parse and validate; on error
   comment and mark `invalid`. Otherwise create one backlog step issue per node, skipping
   nodes that already exist (matched on `run` + `node`) so a crash mid-expansion resumes
   cleanly. Then set `state = "running"` and move the definition issue to `in_progress`.
   An `expanding` state older than a timeout is reclaimed.
2. **Observe.** For each `running` workflow, load its step issues and derive events:
   - `running` step whose issue is `done` or `in_review` → `agent_finished`.
   - `running` step whose latest agent task failed → `agent_failed`.
   - `pending` step whose deps are all `done` → `deps_met`.
3. **Apply.** Run each event through `Next`; update metadata phase, issue status, and
   perform actions. Each update is conditional on the observed revision/phase so
   concurrent instances do not double-apply.
4. **Complete.** All steps `done` → definition issue `done`, `state = "done"`. Any step
   `failed` → definition issue `blocked` with a summary comment, `state = "blocked"`;
   `/retry` returns it to `in_progress`/`running`.

## 8. Commands

`/accept`, `/reject`, `/retry` are recognised when they are the first token of a comment.

- `/accept`, `/reject` on a step issue in phase `blocked`.
- `/retry` on a `failed` step issue (that step) or on the definition issue (all failed
  steps).
- **Authorization:** the commenter must be the definition issue's creator. Others get a
  short refusal comment.
- `/reject` passes the comment as the trigger of the re-dispatched agent task so the agent
  sees the reviewer's feedback.
- Applying the transition is done by the listener directly (it does not wait for the tick)
  so the response is immediate.

## 9. Touch points in existing code

1. `server/cmd/server/main.go`: construct the engine from `backgroundServices(h)` (reuse
   the shared `TaskService`), register the comment listener next to
   `registerAutopilotListeners`, and register the job with `schedulerMgr`.
2. `server/internal/scheduler/jobs_workflow.go`: new file, the job wrapper.
3. `server/pkg/db/queries/workflow.sql`: new file, generated via `make sqlc`.
4. **Possible one-line change in `handler/comment.go`** (decision needed, see section 12):
   command comments would otherwise also wake the step's assigned agent through the
   assignee fallback, like `/note` is excluded today via `isNoteComment`.

No migrations. Status changes: there is no service-level "update status"; side effects
live in `Handler.UpdateIssue`. The engine performs the narrow sequence it needs itself
(status update, `issue:updated` event with the same payload shape, stop wakeups on closed
issues, parent notification is not needed because the engine owns completion).

## 10. Error handling

- Invalid YAML / graph: one comment, no side effects, no repeated spam.
- Agent missing or archived at dispatch time: step `failed` with a comment.
- Enqueue refused (`ErrDuplicatePendingTask`): treated as already dispatched.
- Deleted step or definition issue: the run is abandoned silently.
- Every transition is idempotent against the observed phase, so a re-run tick is safe.

## 11. Testing

- Unit: YAML parse/validate matrix (cycles, duplicates, unknown deps); `Next` over the
  whole transition table including guards.
- DB-backed Go tests with `server/internal/testutil` fixtures: expansion, crash-resume of
  `expanding`, dependency ordering, approval gate, reject → redo → exhausted → failed,
  `/retry`, completion, creator-only authorization, label/status eligibility. Fake or
  missing agent executables only; no real agent CLIs.
- Malformed-metadata reconciliation test.

## 12. Open questions for review

1. **Command comments waking the agent.** Resolved: accept the one-line `comment.go`
   touch so `/accept`, `/reject`, `/retry` do not trigger agents (same mechanism as `/note`).
2. **`agent_failed` detection** relies on the latest agent task status for the issue; the
   exact query is confirmed during planning.
3. **Cadence** of 15s is a guess; adjust if too chatty.

## 13. Implementation notes

1. Step metadata also stores `agent_id` (the resolved agent UUID) next to `agent` (the name), so dispatch never re-resolves names.
2. Definition metadata also stores `claimed_at` (to reclaim a stale `expanding` claim) and `total` (number of nodes, to detect a deleted step issue).
3. Step titles are `"<definition title> · <node id>"`. On resume after a crash between issue creation and its metadata write, an orphan child with that exact title and no `workflow` metadata is adopted instead of duplicated.
4. Because `IssueCreateParams` has no metadata field, step metadata is written by `SetIssueMetadataKey` immediately after `Create`.
5. The engine does not stop issue wakeups when it closes an issue (that logic lives only in `Handler.UpdateIssue`); a user-set wakeup on a step issue is out of scope for the prototype.
6. Status keys are compared as built-in literals (`done`, `in_review`, `todo`), like the autopilot listener does. Custom statuses are not supported in the prototype.
7. Steps are located by `metadata.workflow.run` (`ListWorkflowSteps`), not by `parent_issue_id`; the parent link is still set on creation and verified (a warning comment is posted if a step is not a direct child).
8. `handler.WorkflowEvents` (new file `internal/handler/workflow_bridge.go`) is the adapter that lets the engine emit events in the exact shapes existing listeners and the UI expect, without `workflow` importing `handler`.

### Decisions made during implementation

- Steps are located by `metadata.workflow.run`; `parent_issue_id` is still set and verified, and a warning comment is posted when a step is not a direct child.
- Expansion is fenced by a `claimed_at` token (renewed before every create and before the final write); `expanding` definitions are candidates so a crashed expansion resumes; an inconsistent expansion (duplicate or missing steps) blocks the definition with one explanatory comment.
- Step transitions are claimed by a conditional UPDATE keyed on the caller-observed phase, attempts and `dispatched_at`; `dispatched_at` is stamped from the database clock.
- Two extra events beyond the original table: `dispatch_lost` (no task row after a 90s grace) and `agent_cancelled` (the run was cancelled; step fails, `/retry` restarts it). A task that completed without moving the issue to done or in_review counts as a failed attempt.
- A task in any non-terminal status counts as in flight; only terminal task statuses are bounded by the dispatch time.
- Commands are accepted only from members and only from the definition creator; the listener ignores system and agent comments, runs each command in a goroutine with a recover and a 30s timeout; `isNoteComment` also treats `/accept`, `/reject` and `/retry` as non-triggering comments on every issue.
- The claim query needs PostgreSQL 16 or newer (`pg_input_is_valid`).
- Agent invoke check: the engine runs the platform's invoke gate (`canInvokeAgent`) for the definition creator through `AgentInvokeChecker`, implemented by `handler.WorkflowEvents`. A nil checker denies every agent. At expansion an agent the creator cannot invoke gets the same `unknown agent "X"` error as a missing one, so private agents are not disclosed. The check is repeated right before each dispatch; a denial fails the step visibly.
- Stopped state: closing the definition issue (cancelled, or done by hand) while the run is `running` moves the run to `stopped` with one comment; nothing is observed or dispatched afterwards. A run that has actually completed is closed as `done` instead.
- Size caps: at most 50 nodes, 20000 bytes per prompt, 100000 bytes for the YAML block.
- Approval steps need a member-created definition, because only members can `/accept`; an agent-created definition with an approval step is marked invalid.
- Invalid definitions are excluded from the candidate list in SQL while their stored `error_hash` equals the hash of the current description, so abandoned definitions cannot occupy candidate slots.
- `MULTICA_WORKFLOW_ENGINE` (default on; `false` or `0` disables) controls whether the engine, its comment listener and its scheduler job are started.

### Known limitations

- The platform has its own retry for failed tasks; a tick can race it and dispatch a second run of the same step.
- `/reject` sent while the previous task is still running enqueues a second concurrent task (duplicate protection only covers queued and dispatched tasks).
- Status reconciliation resets a step issue's status to the phase's status, so a human cancelling a step issue by hand is undone.
- `ListRunningWorkflowDefinitions` is global and capped at 200 per tick.
- Tasks started on a step issue for another reason (for example a comment mention) can be mistaken for a step attempt.
- Custom issue statuses are not supported; built-in keys are compared literally.
- Review requests and failure summaries are system comments, so they do not create an inbox notification; the creator only gets the generic status-change notification.
- Expansion does not check that the agent's runtime is online; an offline runtime leaves the step queued.
- Stopping a definition does not stop the tasks of its step issues.
