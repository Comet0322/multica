# Ext Workflow — Design

Status: draft for review · Base: `v0.6.1` · Branch: `feat/ext-workflow`

## 1. Goal

Make programmatic workflows a first-class feature of this fork:

- A **Workflows** page sits under Squads in the sidebar. It works like Squads: list, create, detail.
- A workflow is a reusable template: a DAG of **nodes**, each run by an existing agent, plus one **supervisor agent**.
- An issue is assigned to a workflow the same way it is assigned to a squad (`assignee_type = 'workflow'`). Assignment starts a **run**.
- A deterministic engine schedules the nodes. The supervisor handles exceptions: it reviews flagged nodes, decides what to do after failures, arbitrates rewind requests, and writes the final report. It hands off to a human only when it cannot decide.

This replaces the metadata/YAML prototype on `feat/workflow-engine`. That prototype added no tables, and paid for it:

- no UI, and no reusable definitions;
- no output passed between steps;
- human-only `/accept`/`/reject`;
- races with platform retries;
- runs that left tasks running after they stopped.

Its pure parts (DAG validation, rule-table transitions, e2e test style) are ported.

### Non-goals (this iteration)

- Mobile support. Mobile must not break; it shows a generic actor name.
- `@workflow` mentions; autopilot or quick-action targets of type workflow; wakeup conditions on workflows.
- Conditional branching, or loops in the definition. Loops happen only at run time, through rewind (§5.5).
- New `multica` CLI commands. Agents use the existing CLI only (§6.3).
- Per-step progress comments from the supervisor (§5.6).

## 2. Fork-isolation rules

The fork must keep merging upstream releases cleanly.

1. **Namespace.** Every new DB object, SQL file, API route, WS event, inbox type and frontend module uses the `ext_` / `ext-` / `/api/ext/` prefix.
2. **Migrations** are named `ext_NNNN_<name>.up.sql` / `.down.sql`.
   - The runner (`server/internal/migrations/migrations.go:54`) sorts filenames as strings and tracks each by full basename. `ext_*` therefore sorts after every numeric upstream migration and never collides with upstream numbers.
   - The first plan task verifies that the migration tests (`concurrentIndexCleanups`, `TestEveryConcurrentUpBuildHasCleanup`, etc.) accept non-numeric names. If they do not, it adapts the tests minimally.
3. **Shared constraint guard.** `issue_assignee_type_check` is shared with upstream. A later upstream migration that redefines it will drop `'workflow'`. Two defences:
   - a Go test that runs all migrations and asserts the constraint admits `'workflow'`;
   - an engine startup check that logs an error and refuses to start the engine (kill-switch behaviour) when the constraint lacks `'workflow'`.
4. **Hooks, not rewrites.** New code lives in new files and packages:
   - `server/internal/extworkflow/`
   - `server/internal/handler/ext_workflow*.go`
   - `packages/core/ext-workflows/`
   - `packages/views/ext-workflows/`

   Existing files get small `case "workflow"` branches that call into those packages. §9 lists every touch point.

## 3. Data model

There are no foreign keys and no cascades. Every index is `CREATE [UNIQUE] INDEX CONCURRENTLY IF NOT EXISTS`, in its own migration file, registered in `concurrentIndexCleanups`.

### 3.1 Template

`ext_workflow`

| column | type | notes |
|---|---|---|
| id | uuid pk | |
| workspace_id | uuid | |
| name | text | not unique |
| description | text default '' | |
| supervisor_agent_id | uuid | |
| max_rewinds | int default 3 | rewind budget per run |
| creator_id | uuid | member |
| avatar_url | text null | |
| archived_at / archived_by | timestamptz / uuid null | |
| created_at / updated_at | timestamptz | |

`ext_workflow_node`

| column | type | notes |
|---|---|---|
| id | uuid pk | |
| workflow_id | uuid | |
| workspace_id | uuid | |
| key | text | `[a-z0-9_-]{1,40}`, unique per workflow |
| title | text | |
| agent_id | uuid | agents only |
| prompt | text | ≤ 20 000 bytes |
| requires_review | bool default false | |
| max_attempts | int default 3 | 1–10 |
| depends_on | text[] | node keys |
| position | int | display order |

Nodes are replaced as a whole set in one transaction on update.

Validation, done server-side and mirrored client-side:

- at most 50 nodes;
- keys are valid and unique;
- every dependency exists, no node depends on itself, and there are no cycles;
- every agent exists, is not archived, and the editor may invoke it;
- the supervisor exists and the editor may invoke it.

Indexes:

- `(workspace_id) WHERE archived_at IS NULL` on `ext_workflow`;
- `(workflow_id)` on `ext_workflow_node`;
- unique `(workflow_id, key)` on `ext_workflow_node`;
- `(agent_id)` on `ext_workflow_node`, for "which workflows use this agent".

### 3.2 Runs

`ext_workflow_run`

| column | type | notes |
|---|---|---|
| id | uuid pk | |
| workspace_id, workflow_id | uuid | |
| issue_id | uuid | the original (parent) issue |
| triggered_by_type / triggered_by_id | text / uuid | actor who assigned |
| status | text | `running` · `waiting_human` · `done` · `failed` · `cancelled` |
| definition | jsonb | snapshot: supervisor, max_rewinds, nodes |
| rewinds_used | int default 0 | |
| started_at / finished_at | timestamptz | |
| created_at / updated_at | timestamptz | |

`waiting_human` is maintained by the engine: it is set while any step is `awaiting_human`.

Indexes:

- unique `(issue_id) WHERE status IN ('running','waiting_human')`, so an issue has at most one active run;
- `(workflow_id, created_at DESC)`;
- `(status) WHERE status IN ('running','waiting_human')`.

`ext_workflow_run_step`

| column | type | notes |
|---|---|---|
| id | uuid pk | |
| run_id, workspace_id | uuid | |
| node_key | text | |
| agent_id | uuid | from snapshot |
| issue_id | uuid | child issue |
| status | text | `pending` · `running` · `awaiting_supervisor` · `awaiting_human` · `done` · `skipped` · `failed` · `cancelled` |
| attempts | int default 0 | |
| pending_reason | text null | why it waits: `review` · `failure` · `rewind_request` |
| last_feedback | text null | passed to the next attempt |
| escalation_reason | text null | |
| started_at / finished_at | timestamptz null | |
| updated_at | timestamptz | |

Indexes:

- unique `(run_id, node_key)`;
- `(issue_id)`.

`ext_workflow_run_event` is the timeline, and is append-only.

| column | type | notes |
|---|---|---|
| id | uuid pk | |
| run_id, workspace_id | uuid | |
| step_id | uuid null | |
| kind | text | `run_started` · `step_started` · `step_finished` · `step_failed` · `decision` · `rewind` · `rewind_requested` · `escalated` · `run_finished` · `run_cancelled` · `protocol_error` |
| actor_type / actor_id | text / uuid null | `engine` · `agent` · `member` |
| on_behalf_of | uuid null | member, when the supervisor acts for a commenter |
| payload | jsonb | action, reason, feedback, target, … |
| created_at | timestamptz | |

Index: `(run_id, created_at)`.

### 3.3 Changes to existing tables

- `issue.issue_assignee_type_check` admits `'workflow'`. This is done with idempotent DROP/ADD, and the down migration restores the v0.6.1 list.
- `agent_task_queue` gets four new columns:
  - `ext_workflow_run_id uuid null`
  - `ext_workflow_step_id uuid null`
  - `ext_workflow_role text null` (`step` | `supervisor`)
  - `ext_workflow_kind text null`: `step` for step tasks; `review` · `failure` · `rewind_request` · `summary` · `conversation` for supervisor tasks

  `CreateRetryTask` (`pkg/db/queries/agent.sql`) copies all four.

  Index: `(ext_workflow_run_id) WHERE ext_workflow_run_id IS NOT NULL`.
- Inbox type `ext_workflow_escalation`. `type` is free text, so no migration is needed.

## 4. Starting a run

### 4.1 Assignment

`assignee_type='workflow'` is accepted by `validateAssigneePair`. These checks reject the request immediately (4xx):

- the workflow is not archived;
- the engine is enabled;
- the supervisor agent exists, is not archived, and has a runtime;
- the **assigning actor** may invoke every node agent and the supervisor.

Backlog rules match agents and squads: an issue in `backlog` does not start a run, and leaving backlog does.

`WillEnqueueRun` (`service/issue_trigger.go`) and `dispatchIssueRun` (`handler/issue_trigger.go`) get `case "workflow"`, which calls `extworkflow.Engine.StartRun`. Issue creation (`IssueService.maybeEnqueueOnAssign`) does the same.

### 4.2 `StartRun` (one transaction)

1. Insert `ext_workflow_run`, with a snapshot of the current definition.
2. For each node:
   - Insert `ext_workflow_run_step` in `pending`.
   - Create a child issue through `IssueService.Create`:
     - title `"<parent title> · <node title>"`
     - description: the node prompt
     - parent: the original issue
     - assignee: the node agent
     - status: `backlog`, so the platform does not auto-enqueue
     - creator: the triggering actor
     - project and priority copied from the parent
3. Set the parent issue to `in_progress`, and append a `run_started` event.
4. Call `Advance(run)` inside the same transaction.

If the triggering actor later loses access to an agent, the dispatch fails. The step then goes to `awaiting_supervisor` with `pending_reason=failure`.

## 5. Engine

### 5.1 Single entry point

`Engine.Advance(ctx, runID, event)`:

1. `SELECT … FOR UPDATE` the run row. Runs are serialized per run, so step-level CAS is not needed.
2. Load the steps, and apply the pure transition function `Next(step, event) → (step', actions)`.
3. Persist the new step and run state and the events, and enqueue tasks **in the same transaction**.
4. Commit, then publish WS events (`ext_workflow_run:updated`, `issue:updated`).

Actions are: enqueue a step task, enqueue a supervisor task, set a child or parent issue status, cancel tasks for an issue, create an escalation inbox item, and post a milestone system comment. An escalation inbox item is archived (and marked read) when its step leaves awaiting_human or the run ends.

### 5.2 Event sources

| source | wiring |
|---|---|
| Task terminal | Bus listener on `task:completed` / `task:failed` / `task:cancelled`. It reloads the task and acts only when `ext_workflow_run_id` is set. `task:failed` with `retry_pending=true` is ignored: the retry row inherits the ext columns. |
| Child issue status | Hook in `service/issue_wakeup_system.go` `processChildEvents`, after `ClaimChildEvents`. When the parent has `assignee_type='workflow'`, the claimed events go to `Engine.OnChildEvents` in that transaction. `resolveWakeTarget` returns `none` for `'workflow'`, so the `child_done` system rule wakes nobody. |
| Comment protocol | Bus listener on `comment:created` (§6.3). |
| Human decision | API endpoint (§7.1). |
| Parent issue change | Hook in issue update/delete paths: the parent is cancelled or deleted, or reassigned away from the workflow → `run_cancelled`. |
| Safety net | Scheduler job `ext_workflow_reconcile`: global scope, cadence 30s, `CatchUpLatestOnly`, `MaxAttempts 1`. It re-derives events for active runs (at most 200 per tick, oldest `updated_at` first) from task and issue state. |

### 5.3 What "the agent finished" means

| observation | event |
|---|---|
| child issue moved to `done` or `in_review` | `step_finished` |
| step task `failed` (no retry pending) | `step_failed{reason}` |
| step task `cancelled` (not by the engine) | `step_failed{cancelled}` |
| step task `completed` and child issue still not done/in_review | `step_failed{ended_without_finishing}` |
| `request-rewind` block from the step agent | `rewind_requested{to?, reason}` |
| human sets the child to `done` | `step_finished` |
| human cancels the child | `skip` by that human |

Only tasks whose `ext_workflow_step_id` matches the step count as attempts. Other tasks on the child issue, such as a human comment waking the agent, are ignored, but their effect on issue status is still observed.

### 5.4 Step transitions

| from | event | guard | to | actions |
|---|---|---|---|---|
| pending | deps satisfied (all `done`/`skipped`) | | running | child → `in_progress`; attempts+1; enqueue step task |
| running | step_finished | !requires_review | done | child stays done; advance downstream |
| running | step_finished | requires_review | awaiting_supervisor (`review`) | enqueue supervisor `review` |
| running | step_failed | | awaiting_supervisor (`failure`) | enqueue supervisor `failure` |
| running | rewind_requested | budget left | awaiting_supervisor (`rewind_request`) | enqueue supervisor `rewind_request` |
| running | rewind_requested | budget exhausted | awaiting_supervisor (`failure`) | protocol note: budget exhausted |
| awaiting_* | approve | | done | child → `done` |
| awaiting_* | redo{feedback} | attempts < max | running | last_feedback; attempts+1; enqueue step task |
| awaiting_* | retry | attempts < max | running | attempts+1; enqueue step task |
| awaiting_* | skip | | skipped | child → `cancelled` |
| awaiting_* | rewind{to, feedback} | to ∈ ancestors(step) ∪ {step}; budget left | (see §5.5) | |
| awaiting_supervisor | escalate{reason} | | awaiting_human | inbox; milestone comment |
| any non-terminal | abort{reason} | | (run → failed) | cancel all tasks |
| awaiting_supervisor | supervisor task failed / ended with no valid decision (2nd time) | | awaiting_human | auto-escalate |

Notes:

- `awaiting_*` means `awaiting_supervisor` or `awaiting_human`.
- Guards that fail reject the decision: the API returns a 4xx, and the comment protocol gets a `protocol_error` reply.
- After the first supervisor task ends with no valid decision, the supervisor task is re-enqueued once, with the error in its briefing.

### 5.5 Rewind

`rewind{to=T, feedback}` from a step S:

1. Requires `T` to be S itself or a transitive upstream of S, and `run.rewinds_used < max_rewinds`. Increment `rewinds_used`.
2. Reset set R = {T} ∪ every transitive downstream of T that is not `pending`. For every step in R:
   - cancel in-flight step tasks on its child issue;
   - set `attempts = 0` and status `pending`;
   - move the child issue back to `backlog`. Child issues are reused, not recreated.
3. Set `T.last_feedback = feedback`, then advance. T starts again when its own deps are satisfied (they are untouched), and downstream re-runs naturally on T's new output.

A step agent can **request** a rewind. Only the supervisor, or a permitted human, **performs** it.

### 5.6 Run completion and parent issue status

- All steps `done`/`skipped`:
  1. Enqueue a supervisor `summary` task on the parent.
  2. When that task ends, whether it completed or failed, set the parent to `in_review` and the run to `done`.
  3. Post a milestone system comment.
- `abort`: run → `failed`; parent → `blocked`; cancel all in-flight tasks; open children → `cancelled`.
- Parent cancelled, deleted, or reassigned: run → `cancelled`; cancel all in-flight step and supervisor tasks (`CancelTasksForIssue`); open children → `cancelled`.
- Milestone system comments go on the parent only for: run started, escalation, run finished, failed, or cancelled.

The supervisor is woken **only** for review, failure, rewind request, summary and conversation. Per-step progress shows in the run panel and timeline, not in comments.

### 5.7 Kill switch

`MULTICA_WORKFLOW_ENGINE` (default `true`). When it is off:

- no listeners, no reconcile job, no hooks;
- assigning to a workflow returns 409 `workflow_engine_disabled`;
- CRUD stays available.

## 6. Agent protocol

### 6.1 Task placement

| role | issue | kinds |
|---|---|---|
| step | child issue | `step` |
| supervisor | parent issue | `review`, `failure`, `rewind_request`, `summary`, `conversation` |

Enqueue goes through a new `TaskService.EnqueueExtWorkflowTask(tx, params)`, which stamps the four ext columns and a handoff note. Pending-task dedup applies per (issue, agent, role).

### 6.2 Claim-time briefing

In `handler/daemon.go` `buildClaimedTaskResponse`, after the squad-leader block, call `extworkflow.BuildBriefing(ctx, task)`, then append the result to `resp.Agent.Instructions`.

First the task is re-validated against live state: the run is active, and the step status and attempts match the task's purpose. A stale task gets a one-line briefing: "This workflow run has moved on; do nothing and end."

**Step briefing**

- The workflow name, the step title, "step k of n", and a compact outline of the DAG.
- The parent issue title and description, as shared context.
- For each direct upstream:
  - node title, child issue id/link, status (`done` or `skipped`);
  - the **last comment by that step's agent** on its child issue, truncated to 4 000 chars each and 16 KB in total, with links when truncated.
- `last_feedback`, when present.
- Rules:
  - Work only in this child issue.
  - When finished, post the result as a comment and set the issue to `done`.
  - Do not modify the parent issue or sibling issues.
  - Use `request-rewind` (§6.3) only when an upstream output is wrong or infeasible, not because the work is hard.

**Supervisor briefing**

Common part:

- a run overview table (step, agent, status, attempts/max);
- rewinds used/max;
- the timeline of past decisions;
- the decision block format and the actions allowed now;
- the rule: never do a step's work yourself.

Per kind:

- `review` / `failure`:
  - the focus step's prompt and child issue id;
  - the agent's last comment;
  - for failure: `failure_reason`, the error, and whether the task ended without finishing;
  - required: exactly one decision block on the **focus step's child issue**.
- `rewind_request`: the requesting step, the requested target, and the reason. Required: a decision block. Typical answers are `rewind` or `redo` with a rejection explanation.
- `summary`: every step's outcome with links. Required: one plain comment on the parent. No decision block.
- `conversation`: the triggering member comment. The supervisor may answer. It may also act **on behalf of the commenter** with a decision block on the parent that names `step:`, but only if that member is permitted (§7.2). The engine checks permission against the commenter, not the supervisor.

### 6.3 Comment decision protocol (no CLI changes)

Agents post with the existing `multica issue comment add <issue-id>`. The comment contains one fenced block:

````
```ext-workflow
action: rewind          # approve | redo | retry | skip | rewind | escalate | abort | request-rewind
step: backend           # required on parent-issue comments, except for abort
to: spec                # rewind / request-rewind
reason: Spec assumed paginated API; it is not.
feedback: |
  Switch to cursor pagination and update the data-flow section.
```
````

- Parsed as YAML with `KnownFields(true)`. One block per comment; a second block is a protocol error. A one-line unquoted `reason` or `feedback` containing `: ` is read as the whole rest of the line, because agents write prose unquoted; `action`, `step` and `to` stay strict.
- Required fields:
  - `redo` and `rewind` need `feedback`;
  - `escalate`, `abort` and `request-rewind` need `reason`;
  - `rewind` needs `to`.
- **Authorization.** Comments carry no source task, so the engine checks the author agent:

| author | issue the comment is on | allowed actions |
|---|---|---|
| supervisor agent, with an in-flight supervisor task for that run | child issue of the focus step | all except `request-rewind` |
| supervisor agent, with an in-flight `conversation` task | parent issue, with `step:` | all except `request-rewind`, gated by the commenter's permission |
| step agent, with an in-flight step task on that child issue | its child issue | `request-rewind` only |
| anyone else | — | ignored |

- An invalid block (parse error, illegal action, failed guard) gets a system reply on the same issue explaining the error, plus a `protocol_error` event. The agent's turn is not otherwise affected.
- `handler/comment.go` `isNoteComment` is extended to treat comments that contain an `ext-workflow` block as notes. They wake no agent; in particular, the supervisor's decision on a child issue does not trigger the step agent's assignee fallback.

### 6.4 Comments by humans

| where | behaviour |
|---|---|
| Parent issue, no mention | Wake the supervisor with kind `conversation`. Hooked into the assignee-fallback switch (`comment.go` `routeAssigneeFallback`) for `'workflow'`. |
| Parent issue, @agent | Unchanged platform behaviour. |
| Parent issue, agent author (including the supervisor) | No trigger. |
| Child issue | Unchanged platform behaviour (wakes the step agent). Not counted as an attempt (§5.3). |

## 7. API

### 7.1 Endpoints

All endpoints are workspace-scoped through `X-Workspace-ID` and gated on membership.

| method & path | purpose |
|---|---|
| `GET /api/ext/workflows` | list. Includes node count, active run count, last run time. |
| `POST /api/ext/workflows` | create with `{name, description, supervisor_agent_id}`. Starts with no nodes. |
| `GET /api/ext/workflows/{id}` | workflow and its nodes |
| `PUT /api/ext/workflows/{id}` | update fields and replace the node set. On validation failure, returns 422 `{errors:[{node_key?, field, message}]}`. |
| `DELETE /api/ext/workflows/{id}` | archive. Active runs continue; no new assignments. |
| `GET /api/ext/workflows/{id}/runs` | run history, paginated |
| `GET /api/ext/workflow-runs?issue_id=` | runs for an issue, newest first. Also used from a child issue, through its step. |
| `GET /api/ext/workflow-runs/{id}` | run, steps and events |
| `POST /api/ext/workflow-runs/{id}/cancel` | cancel the run |
| `POST /api/ext/workflow-runs/{run}/steps/{step}/decision` | `{action, to?, reason?, feedback?, expected_status}`. Returns 409 on a status mismatch. |

UUID parameters are handled by `parseUUIDOrBadRequest`, or by loaders when a param can be human-readable.

WS events:

- `ext_workflow:created` / `ext_workflow:updated` / `ext_workflow:deleted` with `{workflow_id}`;
- `ext_workflow_run:updated` with `{run_id, issue_id, workflow_id}`.

### 7.2 Permissions

- **Manage a workflow** (update or archive): its creator or a workspace admin, mirroring `canManageSquad`.
- **Create a workflow**: any member.
- **Decide on, or cancel, a run**: the triggering member, the workflow creator, or a workspace admin.
- **Assign an issue to a workflow**: anyone who may assign the issue, provided that person may invoke every agent in the workflow (§4.1).

## 8. Frontend

### 8.1 Core (`packages/core/ext-workflows/`)

- Types, API client methods, zod schemas parsed with `parseWithFallback` (with defaults, and fallbacks for unknown enums such as step status → `unknown`), and malformed-response tests.
- Query keys include `wsId`.
- Hooks and mutations. Decision and cancel invalidate run queries on settle; they are not optimistic.
- `use-realtime-sync.ts` gets handlers for `ext_workflow:` (invalidates the workflow list and detail) and for `ext_workflow_run:` (invalidates the run and the issue).
- `IssueAssigneeType` adds `"workflow"`. `getActorName`, `ActorAvatar` and the actor existence lookup resolve workflows.
- `paths.workflows()` and `paths.workflowDetail(id)`, with route icon `Workflow`.
- `workflows` is added to `reserved_slugs.json`; regenerate `reserved-slugs.ts`.
- Modal key `create-ext-workflow`.

### 8.2 Views (`packages/views/ext-workflows/`)

- **Sidebar.** `aiTeamNav` gets `workflows` after `squads`. Add the matching `NavKey`/`NavLabelKey`/`WORKSPACE_PAGES` entries, the icon mapping, the shortcut and the search alias, following the squads pattern.
- **List page.** Modelled on `squads-page.tsx`. Columns: name, supervisor, node count, active runs, last run. Header has a "New workflow" button; each row has an archive action.
- **Create dialog.** Name, description, supervisor picker. On success it navigates to the detail page's **Nodes** tab.
- **Detail page.** Inspector on the right: name, description, supervisor, max rewinds, archive. Two tabs:
  - **Nodes**: a row editor.
    - Each row has: title (with the key derived from it, editable), agent picker, "depends on" multi-select, review toggle, max attempts, reorder and delete. Expanding a row shows the prompt textarea.
    - The dependency options exclude any choice that would create a cycle.
    - Server 422 errors are mapped onto rows.
    - Above the rows is a read-only DAG preview: nodes in columns by depth, edges drawn in plain SVG, no new dependency.
    - An unsaved-changes guard applies.
    - When runs are active, a note says they use the old definition.
  - **Runs**: history table (issue, status, started, finished). Clicking a row opens the issue.

### 8.3 Issue surfaces

- **Assignee picker.** A Workflows group after Squads lists non-archived workflows. `run-confirm-gate` treats workflow like squad, and opens the run-confirm modal.
- **Parent issue sidebar.** `WorkflowRunSection` sits near `ExecutionLogSection` and is shown when the issue has any run. It contains:
  - the run status;
  - per step: status icon, agent, attempts n/max, and a link to the child issue;
  - for a step in `awaiting_human`: the escalation reason and buttons for **Approve · Redo (feedback) · Retry · Skip · Rewind to… · Abort**, each with a dialog when input is needed;
  - a collapsible timeline;
  - "Cancel run".
- **Child issue sidebar.** One line: "Workflow step k of n · <parent>", with a link.
- **Inbox.** Label and rendering for `ext_workflow_escalation` (severity `action_required`); clicking it opens the parent issue.
- **Activity.** Nothing new beyond the milestone system comments.

### 8.4 i18n, desktop, mobile

- **i18n.** New `ext-workflows.json` in en, zh-Hans, ko, ja and fr, registered in `locales/index.ts`. Keys are added to `layout.json` (nav/tab), `modals.json` (create dialog, picker group) and `inbox.json` (type label). Copy follows the UI copy rules in AGENTS.md and the conventions pages.
- **Desktop.** `routes.tsx` gets `workflows` and `workflows/:id`, plus tab presentation for the workflow actor.
- **Mobile.** No change in scope. Its loose schemas parse `'workflow'`; the actor shows a generic name.

## 9. Touch points in existing code (v0.6.1)

Every place that branches on `"squad"` was reviewed. The table below lists the ones the workflow feature changes.

| file | change |
|---|---|
| `server/internal/handler/issue.go` `validateAssigneePair` | accept `workflow` (§4.1) |
| `server/internal/handler/issue.go` create-path parent check (~:3252) | treat workflow like squad |
| `server/internal/handler/issue.go` `isIssueActorType`, assignee sort CASE, involves-user filters | include `workflow` |
| `server/internal/handler/issue_table_query.go`, `issue_table_group.go` | group label and order for workflow |
| `server/internal/service/issue_trigger.go` `WillEnqueueRun` | `case "workflow"` |
| `server/internal/handler/issue_trigger.go` `dispatchIssueRun` | `case "workflow"` → `StartRun` |
| `server/internal/service/issue.go` `maybeEnqueueOnAssign` | workflow branch → `StartRun` |
| `server/internal/service/issue_wakeup_system.go` | `resolveWakeTarget` returns none for workflow; `processChildEvents` hook |
| `server/internal/handler/comment.go` | `isNoteComment` ext block; `routeAssigneeFallback` workflow → supervisor `conversation` |
| `server/internal/handler/daemon.go` `buildClaimedTaskResponse` | briefing hook |
| `server/internal/service/task.go` `RerunIssue` | workflow assignee → 409 (rerun the run, not the issue) |
| `server/pkg/db/queries/agent.sql` `CreateRetryTask` | copy ext columns |
| `server/pkg/db/queries/issue.sql` involves-user filters | include workflow |
| `server/cmd/server/main.go` | wire engine, listeners, reconcile job, kill switch, constraint startup check |
| `server/cmd/server/router.go` | `/api/ext/...` routes |
| `server/internal/handler/reserved_slugs.json` | `workflows` |
| `packages/core/types/issue.ts` | `IssueAssigneeType` |
| `packages/core/workspace/hooks.ts` | actor name, avatar, existence |
| `packages/core/realtime/use-realtime-sync.ts`, `types/events.ts` | ext events |
| `packages/core/paths/*`, `route-icons.ts`, `modals/store.ts` | nav, paths, modal |
| `packages/views/layout/app-sidebar.tsx`, `route-icon-components.tsx` | nav item |
| `packages/views/common/actor-avatar.tsx` | workflow avatar and link |
| `packages/views/issues/components/pickers/assignee-picker.tsx`, `actions/run-confirm-gate.ts`, `modals/create-issue.tsx`, `modals/quick-create-issue.tsx` | workflow group and agent-like handling |
| `packages/views/issues/components/issue-detail.tsx` | mount the run section |
| `packages/views/inbox/components/inbox-detail-label.tsx` | escalation label |
| `packages/views/modals/registry.tsx` | create dialog |
| `apps/web/app/[workspaceSlug]/(dashboard)/workflows/**` | route re-exports |
| `apps/desktop/src/renderer/src/routes.tsx` | routes |

Deliberately untouched: autopilot, quick action, mentions, wakeup conditions, CLI, mobile.

## 10. Error handling summary

| failure | handling |
|---|---|
| Lost bus event / server crash | Child events are durable (existing queue). Task outcomes are re-derived by the reconcile job. |
| Platform task retry | `retry_pending` is ignored; the retry inherits the ext columns. |
| Supervisor fails or stays silent | One re-wake, then auto-escalate to a human. |
| Infinite redo or rewind loops | `max_attempts` per step and `max_rewinds` per run. Once exhausted, only skip/escalate/abort are allowed. |
| Agent access revoked mid-run | The dispatch fails, which counts as a step failure and goes to the supervisor. |
| Concurrent decisions | Run row lock plus `expected_status`. The loser gets 409 or a protocol error. |
| Workflow archived mid-run | The run continues on its snapshot. |
| Node agent archived | The dispatch fails, and the step is handled as a failure. The editor warns about archived agents. |
| Upstream migration drops `'workflow'` from the constraint | Guard test plus startup check (§2.3). |

## 11. Testing

Each behaviour has one canonical layer.

**Pure Go (table tests)**

- `Next` transitions, including guards and budgets;
- DAG validation;
- ancestor/descendant sets for rewind;
- `ext-workflow` block parser;
- briefing builder (truncation, stale task).

**DB-backed Go (`testutil`, `dbfx`)**

- assignment validation;
- `StartRun`;
- advance on child events;
- supervisor enqueue with ext columns;
- decisions via API and via comment, including authorization negatives;
- rewind reset and task cancellation;
- request-rewind;
- parent cancel and reassign;
- `retry_pending` ignore, and the `CreateRetryTask` copy;
- reconcile recovery;
- the constraint guard test;
- `ext_` migration names passing the migration tests.

**Engine e2e (Go, simulated task completion)**

- happy path;
- review redo then approve;
- request-rewind → rewind → downstream rerun;
- escalate → human decision.

**Frontend**

- core schema malformed-response tests;
- node editor: cycle exclusion, row-mapped errors;
- run section: buttons only on `awaiting_human`, calls the API;
- sidebar entry; assignee-picker group;
- locale parity.

**Playwright**

- create a workflow, edit its nodes, assign an issue;
- check the run panel and child issues. No real agents.

Real-agent smoke tests are not run unless explicitly authorized.

## 12. Delivery order

Each step is an atomic conventional commit and passes its own checks.

1. Migrations and sqlc; `ext_` naming check; constraint guard test.
2. CRUD API, schemas, core client and queries.
3. Engine pure logic: transitions, DAG, rewind sets, block parser.
4. Engine persistence: `StartRun`, `Advance`, listeners, child-event hook, reconcile job, kill switch, wiring.
5. Claim-time briefing.
6. Comment protocol and decision API; `isNoteComment` and assignee-fallback hooks.
7. Frontend: sidebar, list, create dialog, detail page with node editor and runs tab.
8. Issue surfaces: assignee picker, run section, child-issue line, inbox label.
9. Desktop routes, i18n completion, docs page.
