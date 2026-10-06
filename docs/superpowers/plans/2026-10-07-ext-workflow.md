# Ext Workflow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make workflows first-class in this fork. A workflow is a reusable DAG of agent nodes plus a supervisor agent. It gets a Workflows page under Squads, and issues can be assigned to it. Assigning an issue starts a run, driven by a deterministic engine; the supervisor handles reviews, failures, rewinds and the final report.

**Architecture:**
- **Data.** New `ext_`-prefixed tables hold templates, runs, steps and a timeline. `agent_task_queue` gets ext columns, and `issue_assignee_type_check` admits `'workflow'`.
- **Engine.** A pure state machine (`Next`) sits behind one serialized entry point, `Engine.Advance`, which locks the run row.
- **Events into the engine:** task-terminal bus events, the durable child-event queue, comment decision blocks, the decision API, and a reconcile job.
- **Agents** get their context through claim-time briefings. They answer through fenced `ext-workflow` blocks in comments, using the existing CLI; there are no CLI changes.
- **Frontend:** a new core package, new views, and small hooks into issue surfaces.

**Tech Stack:** Go (Chi, sqlc, pgx/v5, PostgreSQL 17), TypeScript, React, TanStack Query, zod, Base UI/shadcn, vitest, Playwright.

**Spec:** `docs/superpowers/specs/2026-10-07-ext-workflow-design.md`. Executors read both the spec and this plan.

## Global Constraints

- Base is `v0.6.1`; the branch is `feat/ext-workflow`. Push it to `origin` (`https://github.com/Comet0322/multica`) only at the end.
- Follow `AGENTS.md` in full. Before touching `apps/mobile/`, read `apps/mobile/AGENTS.md`; A6 widens two mobile files.
- **Fork isolation:**
  - New DB objects, files, routes, events and inbox types carry the `ext_` / `ext-` / `/api/ext/` prefix.
  - Edits to upstream files are minimal hooks marked `// ext-workflow:`.
- **Migrations:**
  - Files are named `server/migrations/ext_NNNN_*.{up,down}.sql`. `ext_0001`–`ext_0014` belong to Part A, and `ext_0015` to Part B.
  - No foreign keys.
  - One `CREATE [UNIQUE] INDEX CONCURRENTLY IF NOT EXISTS` per file, registered in `concurrentIndexCleanups`.
- **Dependencies:** `service` never imports `extworkflow`; the direction is `handler → extworkflow → service, db`.
- No changes to the `multica` CLI (`server/cmd/multica`).
- **Kill switch:** `MULTICA_WORKFLOW_ENGINE` (default `true`). When it is off, assignment returns 409, CRUD and reads still work, and decisions and cancel return 409.
- **Limits:**
  - at most 50 nodes, and 20 000 bytes per prompt;
  - `max_attempts` 1–10, default 3;
  - `max_rewinds` 0–10, default 3;
  - each upstream comment in a briefing is capped at 4 000 chars, and the total at 16 KB;
  - the reconcile job processes at most 200 runs per tick and runs every 30 s.
- **Statuses:**
  - A finished run moves the parent to `in_review`.
  - An abort moves the parent to `blocked`.
  - A cancel leaves the parent's status as it is.
- **i18n:** every new string is in en, zh-Hans, ko, ja and fr, and the locale parity test passes.
- **Tests:** DB-backed Go tests skip silently without a database, so always run them with `-v` and confirm they ran. Use `make env-exec ARGS="-- bash -c 'cd server && go test <pkg> -count=1 -v'"` from the repo root, after `make up`.
- **Commits:** atomic conventional commits, each ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Execution order

`A1 → A2 → A3 → A4 → A5 → A6 → B1 … B10 → C1 … C7 → D1 … D8`

All four parts are sequential. Their dependencies are:

- B1 needs A3;
- B3 needs A1 and A2;
- B8 needs A4's test helpers;
- C needs all of B, plus A4's helpers and `registerExtRoutes`;
- D needs A5 and A6, plus C's API.

Each part below begins with its author's "Notes for integrator". They are authoritative for that part's names and signatures, and override `contract.md` where they differ.

## Integration overrides (resolve overlaps between parts)

1. **D1 vs A6.** A6 already adds `nav.workflows` to all five `layout.json` files.
   - D1's add-key helper must skip keys that already exist, or drop `nav.workflows` from D1's list.
   - D1's test assertion `toHaveProperty("nav.workflows")` still holds.
2. **D2 vs A6.** A6 already adds:
   - the `Workflow` icon in `route-icon-components.tsx`;
   - `PAGE_KEYWORDS.workflows` and the search-command test mock builder;
   - the diagnostics routes.

   In D2, first run `grep -n "Workflow\|workflows" packages/views/layout/route-icon-components.tsx packages/views/search/search-command.tsx`. Apply only what is missing: the sidebar `NavKey`/`aiTeamNav` entry, shortcuts, tab presentation, and any tests not yet present.
3. **Contract names superseded by part notes:**
   - The hook field is `TaskService.ExtWorkflow`, not `IssueService.ExtWorkflow`.
   - `ExtWorkflowTaskParams` has the extra fields `ActorUserID` and `TriggerCommentID`.
   - `EnqueueExtWorkflowTask` does not publish; call `PublishExtWorkflowTaskQueued` after commit.
   - `DecideInput` has the extra field `TaskID`.
   - Run endpoints accept an issue UUID or identifier.
4. **Workspace deletion manifest.** Any new table must be added to the manifest and to `DeleteExtWorkflowWorkspaceData` (A1/A4). No later part adds a table.
5. **Final verification task (after D8)** — Task Z1 below.

## Review Focus

These are the input classes the spec implies but no part's tests pinned. Each line names the owning task, which must add the test shown (adapt helper names to those defined in that task).

1. **Re-assigning a running parent to a different workflow or to an agent.** The old run must end `cancelled` and its step tasks must be cancelled. A new workflow assignee starts exactly one new run. Owner: **B8**.
   ```go
   func TestExtWorkflowReassignCancelsTheRunAndStartsTheNewOne(t *testing.T) {
       requireExtWorkflowDB(t)
       withExtWorkflowEngine(t)
       wfA := hookWorkflow(t, "")
       wfB := hookWorkflow(t, "")
       var issue IssueResponse
       createWorkflowIssue(t, wfA, "todo").Want(http.StatusCreated).JSON(&issue)
       req := withURLParam(newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"assignee_type": "workflow", "assignee_id": wfB}), "id", issue.ID)
       testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
       if n := dbfx.Count(t, `SELECT count(*) FROM ext_workflow_run WHERE issue_id=$1 AND workflow_id=$2 AND status='cancelled'`, issue.ID, wfA); n != 1 {
           t.Fatalf("old run not cancelled (%d)", n)
       }
       if n := dbfx.Count(t, `SELECT count(*) FROM ext_workflow_run WHERE issue_id=$1 AND workflow_id=$2 AND status IN ('running','waiting_human')`, issue.ID, wfB); n != 1 {
           t.Fatalf("want exactly one active run of the new workflow, got %d", n)
       }
   }
   ```
2. **Duplicate start triggers.** Examples are a batch update re-sending the same workflow assignee, or the status leaving backlog twice. These must never create a second active run or error out. Owner: **B5**. Call `StartRun` twice for the same parent; expect one active run, one set of child issues, and a nil error on the second call.
   ```go
   func TestStartRunIsIdempotentWhileARunIsActive(t *testing.T) {
       e := newEnv(t)
       a := e.agent(t, "A")
       wf := e.workflow(t, e.agent(t, "Sup"), 3, wfNode{key: "a", title: "A", agent: a})
       parent := e.parentIssue(t, wf, "todo")
       e.start(t, parent)
       if err := e.engine.StartRun(context.Background(), parent, "member", e.user); err != nil {
           t.Fatalf("second StartRun: %v", err)
       }
       if n := e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run WHERE issue_id=$1`, parent); n != 1 {
           t.Fatalf("runs = %d, want 1", n)
       }
       if n := e.fx.Count(t, `SELECT count(*) FROM issue WHERE parent_issue_id=$1`, parent); n != 1 {
           t.Fatalf("children = %d, want 1", n)
       }
   }
   ```
3. **Editing a workflow while a run is active.** The run keeps executing its snapshot: removed nodes still run, and added nodes do not appear. Owner: **B10**. Start a run with nodes a→b, replace the template's nodes with only `c` through `DeleteExtWorkflowNodes`/`CreateExtWorkflowNode`, then finish `a`. Assert that `b` dispatches and that no step `c` exists.
4. **A human posts a fenced `ext-workflow` block** on a child or parent issue. No decision is applied, no `protocol_error` is recorded, and no agent wakes. Humans decide through the UI. Owner: **C5**.
   ```go
   func TestExtWorkflowBlockFromAMemberIsInert(t *testing.T) {
       // Start a run whose first step is awaiting_supervisor, using the C7 helpers. Then a member posts
       // "```ext-workflow\naction: approve\n```" on that child issue.
       // Assert: the step status is unchanged, no protocol_error event exists, and no new agent task
       // was queued on the child.
   }
   ```
   Implement it with the C7 helpers (`startExtRun`, `postComment`, `extStep`, `wantExtStep`); every assertion is listed in the comment above.
5. **Running a workflow again on a finished parent.** After a run is `done` (parent `in_review`), moving the parent to `backlog` and then `todo` starts a fresh run, because the unique index only covers active runs. Owner: **B8**. Use `createWorkflowIssue`, drive the run to done with B10's helpers or direct SQL on the step rows plus `Advance`, then PUT status `backlog` and then `todo`. Assert two runs for the issue, the newer one `running`.

---

# Part A

# Part A: Migrations, workflow CRUD backend, core frontend package

## Notes for integrator

**Contract deviations (with reason)**

1. **Migration numbering.** The contract lists indexes as `ext_0004`..`ext_0012` (9 files). Spec section 3 lists 11 indexes (1 workflow, 3 node, 3 run, 2 step, 1 run-event, 1 task). Part A therefore creates `ext_0004`..`ext_0014`. Parts B/C add no migrations; if they need one, start at `ext_0015`.
2. **Extra Part A name `extworkflow.ValidateSettings(supervisorAgentID string, maxRewinds int) []ValidationError`** (in `definition.go`, used by `ValidateStructure` and by the handler for settings-only updates). Part B may use it; it does not change any contract name.
3. **Extra queries** beyond the contract list: `LockExtWorkflowForUpdate(id, workspace_id)` (serializes node replacement) and `DeleteExtWorkflowWorkspaceData(workspace_id)` (workspace teardown). `ArchiveExtWorkflow` only matches non-archived rows.
4. **Workspace deletion manifest.** `TestWorkspaceDeletionManifestCoversPublicSchema` (`server/internal/handler/workspace_delete_manifest_test.go`) fails for every new table. Part A registers all five `ext_workflow*` tables there as `workspaceDelete` and adds a step `delete ext workflows` in `handler/workspace.go`, backed by `DeleteExtWorkflowWorkspaceData`. Part B's run/step/event tables are the same five tables, so Part B needs nothing further. Any NEW table Part B/C add must be added to that manifest and to `DeleteExtWorkflowWorkspaceData`.
5. **`ext_0003` down** also does `UPDATE issue SET assignee_type=NULL, assignee_id=NULL WHERE assignee_type='workflow'` (otherwise the restored v0.6.1 CHECK cannot be added).
6. **Mutation hooks take `wsId` first** (coordinator instruction): `useCreateExtWorkflow(wsId)`, `useUpdateExtWorkflow(wsId)`, `useArchiveExtWorkflow(wsId)`, `useDecideExtWorkflowStep(wsId)`, `useCancelExtWorkflowRun(wsId)`. `ExtWorkflowDecisionAction` has 6 human actions: approve, redo, retry, skip, rewind, abort (no `escalate`/`request-rewind`; those are agent-only).
7. **`ExtWorkflow.nodes` is always an array** after parsing (list entries carry `[]`); the Go JSON omits `nodes` when empty and the zod schema defaults it.
8. **Admins may wire any workspace agent** into a workflow and the supervisor slot, mirroring `memberCanWireAgent` (squads). Non-admin members need `canInvokeAgent` as themselves. Spec says "editor may invoke"; this is the squad-consistent reading.
9. **Frontend ripple fixes pulled into Part A so each task stays green.** Adding `"workflows"` to `WORKSPACE_PAGES`/`RouteIconName`/`NavLabelKey` makes `packages/views` fail typecheck, so task A6 also adds the `Workflow` icon to `route-icon-components.tsx`, `PAGE_KEYWORDS.workflows` in `search-command.tsx` (+ the `workflows` builder in its test mock), and `nav.workflows` in all 5 `layout.json`. Widening `IssueAssigneeType` breaks the **mobile** typecheck (ActorAvatar / useActorLookup types), so A6 also widens those two mobile files (generic name "Workflow", generic glyph). `bucketDiagnosticPath` route table (`diagnostics/diagnostic-context.ts`) also needs the `workflows` routes. `InboxItemType` is NOT touched.
10. The command palette will list a "Workflows" page (generated from `WORKSPACE_PAGES`) as soon as A6 lands, before Part D adds the route. Harmless in a feature branch; Part D owns the route and sidebar entry (`app-sidebar.tsx` `NavKey`/`aiTeamNav` are NOT touched by Part A).

**Facts later parts must know**

- Migration tests are name-agnostic: `server/internal/migrations/migrations_lint_test.go` only inspects numeric prefixes >= 129 and skips non-numeric names; `cmd/migrate` tests (`TestEveryConcurrentUpBuildHasCleanup`, `TestConcurrentIndexCleanupsMatchTheirMigrations`) key off file names in `concurrentIndexCleanups`. **No test adaptation was needed**; each concurrent index file must contain exactly one `CREATE [UNIQUE] INDEX CONCURRENTLY` and be registered. `ext_*` sorts after every numeric file (`'e' > '9'`).
- Existing-DB hazard (spec section 2.3): `ext_0003` is skipped for DBs that already applied it, so a later upstream migration touching `issue_assignee_type_check` would drop `'workflow'`. `TestAssigneeConstraintOnlyRedefinedByKnownMigrations` fails on any such migration.
- `make sqlc` (= `cd server && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate`) works offline here. After `ext_0002`, generated `db.AgentTaskQueue` gains `ExtWorkflowRunID, ExtWorkflowStepID pgtype.UUID` and `ExtWorkflowRole, ExtWorkflowKind pgtype.Text`; `agent.sql.go`, `autopilot.sql.go`, `chat.sql.go`, `runtime.sql.go`, `supplement.sql.go`, `wakeup.sql.go`, `models.go` change mechanically. Generated types: `db.ExtWorkflow`, `db.ExtWorkflowNode`, `db.ExtWorkflowRun`, `db.ExtWorkflowRunStep`, `db.ExtWorkflowRunEvent`.
- Route registration: `server/cmd/server/ext_routes.go` has `registerExtRoutes(r chi.Router, h *handler.Handler)`; router.go has ONE hook line. **Part C adds its run routes inside `registerExtRoutes`** (not router.go).
- Handler test helpers (package `handler`): `requireExtWorkflowDB(t)`, `extWFReq(userID, method, path, body, id)` (empty userID = workspace owner), `createExtWorkflowAs(t, userID, name, supervisorID) ExtWorkflowResponse`, `updateExtWorkflow(t, userID, id, body)`, `extWFNode(key, agentID, deps...)`, `subscribeExtWorkflowEvents()` + `extWorkflowEventTypes(t, workflowID)`. Existing helpers: `createHandlerTestAgent(t, name, nil)` (public agent invocable by all members), `privateAgentTestFixture(t) (agentID, ownerID, memberID)`, `createPlainMember(t, email)`, `newRequest`, `newRequestAs`, `dbfx` (a `*testutil.Fixture`: `Insert`, `Exec`, `QueryRow(...).Scan`, `Count`). NOTE: the real signature is `testutil.Call(t, handlerFunc, req).Want(status).JSON(&out)` (AGENTS.md omits `t`).
- Handler tests skip silently when no DB is reachable (`testHandler == nil`). Run: `cd server && go test ./internal/handler -run TestExtWorkflow -count=1 -v`. Confirm they ran (not skipped) by `-v`.
- Handler creates workflows with `max_rewinds=3`. List response: `{"workflows":[...]}` with `node_count`, `active_run_count`, `last_run_at`. 422 body: `{"error":"validation_failed","errors":[{node_key?,field,message}]}`; agent problems use field `agent_id` (with `node_key`) or `supervisor_agent_id`.
- `h.ExtWorkflow` (engine) is not referenced by Part A handlers: CRUD works with the engine disabled.
- Frontend commands (from repo root): `pnpm --filter @multica/core test`, `pnpm --filter @multica/core typecheck`, `pnpm --filter @multica/views typecheck`, `pnpm --filter @multica/mobile typecheck`, `pnpm generate:reserved-slugs`. A single vitest file: `cd packages/core && pnpm exec vitest run ext-workflows`.
- core exports added: `@multica/core/ext-workflows` (+ `/queries`, `/mutations`). `ExtWorkflowValidationFailed` is exported from the index. `extWorkflowListOptions` returns `ExtWorkflow[]`; `extWorkflowRunsOptions(wsId,id)` returns `{runs,total}` (no paging args).
- Verification status: every code block below was applied to a scratch copy of the repo and run: Go migrations (`up`, `down`, `up` on Postgres 17), `go test` for `cmd/migrate`, `internal/migrations`, `internal/handler` (ext tests + manifest), `internal/extworkflow`; core vitest (all), views vitest (only the pre-existing, unrelated `editor/extensions/select-all-delete.test.ts` fails on a clean checkout too), `tsc --noEmit` for core, views, web and mobile.

---

### Task A1: Migrations ext_0001..ext_0014 and concurrent-index registration

**Files:**
- Create: `server/migrations/ext_0001_workflow_tables.{up,down}.sql` .. `ext_0014_task_ext_workflow_run_index.{up,down}.sql` (28 files)
- Modify: `server/cmd/migrate/main.go` (`concurrentIndexCleanups` map; append after the `"537_issue_duplicate_of_index"` entry, currently line ~171, before the closing `}`)
- Create: `server/internal/migrations/ext_workflow_constraint_test.go`
- Regenerate: `server/pkg/db/generated/*` (`make sqlc`)

**Interfaces:** Produces tables `ext_workflow`, `ext_workflow_node`, `ext_workflow_run`, `ext_workflow_run_step`, `ext_workflow_run_event`; `agent_task_queue.ext_workflow_{run_id,step_id,role,kind}`; `issue_assignee_type_check` admits `'workflow'`; generated `db.ExtWorkflow*` models and the 4 new `db.AgentTaskQueue` fields. Consumes nothing.

- [ ] **Step 1: Write the failing tests**


**Create `server/internal/migrations/ext_workflow_constraint_test.go`:**

```go
package migrations

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ext-workflow: issue_assignee_type_check is shared with upstream. ext_0003
// widens it to admit 'workflow', but a LATER upstream migration that redefines
// the constraint would silently drop 'workflow' on databases that already
// applied ext_0003 (the runner applies the new numeric migration after it).
// These tests are the guard described in the ext-workflow design spec, section 2.

const assigneeConstraintName = "issue_assignee_type_check"

// TestAssigneeConstraintOnlyRedefinedByKnownMigrations fails when a migration
// other than the known ones touches the constraint. The fix is a new ext_NNNN
// migration that re-asserts the full list including 'workflow'; then add the
// new file to knownAssigneeConstraintMigrations.
func TestAssigneeConstraintOnlyRedefinedByKnownMigrations(t *testing.T) {
	known := map[string]bool{
		"084_squad.up.sql":                          true, // adds 'squad'
		"084_squad.down.sql":                        true,
		"ext_0003_issue_assignee_workflow.up.sql":   true, // adds 'workflow'
		"ext_0003_issue_assignee_workflow.down.sql": true,
	}
	files, err := filepath.Glob(filepath.Join(realMigrationsDir(t), "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var unexpected []string
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		if strings.Contains(string(body), assigneeConstraintName) && !known[name] {
			unexpected = append(unexpected, name)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Fatalf("migrations %v touch %s. An upstream change can drop 'workflow' from it; add an ext_NNNN migration that re-asserts ('member','agent','squad','workflow') and register the file in this test", unexpected, assigneeConstraintName)
	}
}

// TestAssigneeConstraintAdmitsWorkflowAfterAllMigrations checks the real
// database (migrated by `make test` / `go run ./cmd/migrate up` before the
// suite runs).
func TestAssigneeConstraintAdmitsWorkflowAfterAllMigrations(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	defer pool.Close()

	var def string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = $1 AND conrelid = 'issue'::regclass
	`, assigneeConstraintName).Scan(&def); err != nil {
		t.Fatalf("read %s: %v", assigneeConstraintName, err)
	}
	for _, want := range []string{"member", "agent", "squad", "workflow"} {
		if !strings.Contains(def, "'"+want+"'") {
			t.Errorf("%s = %s; want it to admit %q", assigneeConstraintName, def, want)
		}
	}
}

// TestIssueAssigneeWorkflowMigration runs ext_0003 up and down against a
// minimal issue table in an isolated schema.
func TestIssueAssigneeWorkflowMigration(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	const schema = "ext_issue_assignee_workflow_migration_test"
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	cleanup := func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") }
	cleanup()
	t.Cleanup(func() { cleanup(); pool.Close() })
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE issue (
			id UUID NOT NULL DEFAULT gen_random_uuid(),
			assignee_type TEXT,
			assignee_id UUID
		);
		ALTER TABLE issue ADD CONSTRAINT issue_assignee_type_check
			CHECK (assignee_type IN ('member', 'agent', 'squad'));
	`); err != nil {
		t.Fatalf("create v0.6.1 issue table: %v", err)
	}

	insertWorkflow := `INSERT INTO issue (assignee_type, assignee_id) VALUES ('workflow', gen_random_uuid())`
	assertInsertCheckViolation(t, ctx, pool, insertWorkflow)

	applyMigrationFile(t, ctx, pool, "ext_0003_issue_assignee_workflow.up.sql")
	if _, err := pool.Exec(ctx, insertWorkflow); err != nil {
		t.Fatalf("workflow assignee after ext_0003 up: %v", err)
	}
	// Idempotent re-apply.
	applyMigrationFile(t, ctx, pool, "ext_0003_issue_assignee_workflow.up.sql")

	applyMigrationFile(t, ctx, pool, "ext_0003_issue_assignee_workflow.down.sql")
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE assignee_type = 'workflow'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("down left %d workflow-assigned rows", remaining)
	}
	assertInsertCheckViolation(t, ctx, pool, insertWorkflow)
}
```

The registration check already exists and needs no new test: once the migration files exist, `TestEveryConcurrentUpBuildHasCleanup` fails until Step 3b adds the map entries. Create the 28 migration files first (Step 3a) to see it fail.


- [ ] **Step 2: Run, expect FAIL**

```bash
# DB tests need the checkout's environment (make up creates .env.worktree / .env and the database).
cd server && set -a && . ../.env && set +a   # use ../.env.worktree in a worktree
go run ./cmd/migrate up
```

```bash
# before creating any migration file:
go test ./internal/migrations -run 'IssueAssigneeWorkflowMigration' -count=1   # FAIL: apply migration ext_0003...up.sql: no such file
# after Step 3a, before Step 3b:
go test ./cmd/migrate -run 'TestEveryConcurrentUpBuildHasCleanup' -count=1     # FAIL: "ext_0004_...": builds "idx_ext_workflow_workspace" concurrently on up but has no up cleanup (11 lines)
```


- [ ] **Step 3: Implement**

**3a. Migration files** (all under `server/migrations/`; each index file holds exactly one statement):


`ext_0001_workflow_tables.down.sql`

```sql
DROP TABLE IF EXISTS ext_workflow_run_event;
DROP TABLE IF EXISTS ext_workflow_run_step;
DROP TABLE IF EXISTS ext_workflow_run;
DROP TABLE IF EXISTS ext_workflow_node;
DROP TABLE IF EXISTS ext_workflow;
```

`ext_0001_workflow_tables.up.sql`

```sql
CREATE TABLE IF NOT EXISTS ext_workflow (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    supervisor_agent_id UUID NOT NULL,
    max_rewinds INT NOT NULL DEFAULT 3,
    creator_id UUID NOT NULL,
    avatar_url TEXT,
    archived_at TIMESTAMPTZ,
    archived_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ext_workflow_node (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    key TEXT NOT NULL,
    title TEXT NOT NULL,
    agent_id UUID NOT NULL,
    prompt TEXT NOT NULL DEFAULT '',
    requires_review BOOLEAN NOT NULL DEFAULT false,
    max_attempts INT NOT NULL DEFAULT 3,
    depends_on TEXT[] NOT NULL DEFAULT '{}',
    position INT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS ext_workflow_run (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    workflow_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    triggered_by_type TEXT NOT NULL,
    triggered_by_id UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'waiting_human', 'done', 'failed', 'cancelled')),
    definition JSONB NOT NULL,
    rewinds_used INT NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ext_workflow_run_step (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    node_key TEXT NOT NULL,
    agent_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'awaiting_supervisor', 'awaiting_human',
                          'done', 'skipped', 'failed', 'cancelled')),
    attempts INT NOT NULL DEFAULT 0,
    pending_reason TEXT,
    last_feedback TEXT,
    escalation_reason TEXT,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ext_workflow_run_event (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    step_id UUID,
    kind TEXT NOT NULL,
    actor_type TEXT NOT NULL,
    actor_id UUID,
    on_behalf_of UUID,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`ext_0002_task_ext_columns.down.sql`

```sql
ALTER TABLE agent_task_queue
    DROP COLUMN IF EXISTS ext_workflow_kind,
    DROP COLUMN IF EXISTS ext_workflow_role,
    DROP COLUMN IF EXISTS ext_workflow_step_id,
    DROP COLUMN IF EXISTS ext_workflow_run_id;
```

`ext_0002_task_ext_columns.up.sql`

```sql
ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS ext_workflow_run_id UUID NULL,
    ADD COLUMN IF NOT EXISTS ext_workflow_step_id UUID NULL,
    ADD COLUMN IF NOT EXISTS ext_workflow_role TEXT NULL,
    ADD COLUMN IF NOT EXISTS ext_workflow_kind TEXT NULL;
```

`ext_0003_issue_assignee_workflow.down.sql`

```sql
-- Rows assigned to a workflow cannot satisfy the v0.6.1 constraint; unassign them.
UPDATE issue SET assignee_type = NULL, assignee_id = NULL WHERE assignee_type = 'workflow';
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_assignee_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_assignee_type_check
    CHECK (assignee_type IN ('member', 'agent', 'squad'));
```

`ext_0003_issue_assignee_workflow.up.sql`

```sql
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_assignee_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_assignee_type_check
    CHECK (assignee_type IN ('member', 'agent', 'squad', 'workflow'));
```

`ext_0004_workflow_workspace_index.down.sql`

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_ext_workflow_workspace;
```

`ext_0004_workflow_workspace_index.up.sql`

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_workspace
    ON ext_workflow (workspace_id)
    WHERE archived_at IS NULL;
```

`ext_0005_workflow_node_workflow_index.down.sql`

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_ext_workflow_node_workflow;
```

`ext_0005_workflow_node_workflow_index.up.sql`

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_node_workflow
    ON ext_workflow_node (workflow_id);
```

`ext_0006_workflow_node_key_unique_index.down.sql`

```sql
DROP INDEX CONCURRENTLY IF EXISTS uidx_ext_workflow_node_key;
```

`ext_0006_workflow_node_key_unique_index.up.sql`

```sql
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uidx_ext_workflow_node_key
    ON ext_workflow_node (workflow_id, key);
```

`ext_0007_workflow_node_agent_index.down.sql`

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_ext_workflow_node_agent;
```

`ext_0007_workflow_node_agent_index.up.sql`

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_node_agent
    ON ext_workflow_node (agent_id);
```

`ext_0008_workflow_run_active_issue_index.down.sql`

```sql
DROP INDEX CONCURRENTLY IF EXISTS uidx_ext_workflow_run_active_issue;
```

`ext_0008_workflow_run_active_issue_index.up.sql`

```sql
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uidx_ext_workflow_run_active_issue
    ON ext_workflow_run (issue_id)
    WHERE status IN ('running', 'waiting_human');
```

`ext_0009_workflow_run_workflow_created_index.down.sql`

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_ext_workflow_run_workflow_created;
```

`ext_0009_workflow_run_workflow_created_index.up.sql`

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_run_workflow_created
    ON ext_workflow_run (workflow_id, created_at DESC);
```

`ext_0010_workflow_run_active_status_index.down.sql`

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_ext_workflow_run_active_status;
```

`ext_0010_workflow_run_active_status_index.up.sql`

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_run_active_status
    ON ext_workflow_run (status)
    WHERE status IN ('running', 'waiting_human');
```

`ext_0011_workflow_run_step_node_unique_index.down.sql`

```sql
DROP INDEX CONCURRENTLY IF EXISTS uidx_ext_workflow_run_step_node;
```

`ext_0011_workflow_run_step_node_unique_index.up.sql`

```sql
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uidx_ext_workflow_run_step_node
    ON ext_workflow_run_step (run_id, node_key);
```

`ext_0012_workflow_run_step_issue_index.down.sql`

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_ext_workflow_run_step_issue;
```

`ext_0012_workflow_run_step_issue_index.up.sql`

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_run_step_issue
    ON ext_workflow_run_step (issue_id);
```

`ext_0013_workflow_run_event_run_created_index.down.sql`

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_ext_workflow_run_event_run_created;
```

`ext_0013_workflow_run_event_run_created_index.up.sql`

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_run_event_run_created
    ON ext_workflow_run_event (run_id, created_at);
```

`ext_0014_task_ext_workflow_run_index.down.sql`

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_agent_task_queue_ext_workflow_run;
```

`ext_0014_task_ext_workflow_run_index.up.sql`

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_ext_workflow_run
    ON agent_task_queue (ext_workflow_run_id)
    WHERE ext_workflow_run_id IS NOT NULL;
```

**3b. Register the indexes** in `server/cmd/migrate/main.go`:

```diff
--- a/server/cmd/migrate/main.go
+++ b/server/cmd/migrate/main.go
@@ -348,6 +348,20 @@
 	"482_agent_task_queue_telemetry_started_index":              "idx_agent_task_queue_telemetry_started",
 	"484_issue_triage_state_index":                              "idx_issue_triage_state",
 	"537_issue_duplicate_of_index":                              "idx_issue_duplicate_of",
+
+	// ext-workflow: indexes for the fork's workflow tables (docs/superpowers/specs/2026-10-07-ext-workflow-design.md).
+	// Kept as a contiguous block at the end so upstream merges touch no other line.
+	"ext_0004_workflow_workspace_index":             "idx_ext_workflow_workspace",
+	"ext_0005_workflow_node_workflow_index":         "idx_ext_workflow_node_workflow",
+	"ext_0006_workflow_node_key_unique_index":       "uidx_ext_workflow_node_key",
+	"ext_0007_workflow_node_agent_index":            "idx_ext_workflow_node_agent",
+	"ext_0008_workflow_run_active_issue_index":      "uidx_ext_workflow_run_active_issue",
+	"ext_0009_workflow_run_workflow_created_index":  "idx_ext_workflow_run_workflow_created",
+	"ext_0010_workflow_run_active_status_index":     "idx_ext_workflow_run_active_status",
+	"ext_0011_workflow_run_step_node_unique_index":  "uidx_ext_workflow_run_step_node",
+	"ext_0012_workflow_run_step_issue_index":        "idx_ext_workflow_run_step_issue",
+	"ext_0013_workflow_run_event_run_created_index": "idx_ext_workflow_run_event_run_created",
+	"ext_0014_task_ext_workflow_run_index":          "idx_agent_task_queue_ext_workflow_run",
 }
 
 // concurrentDownIndexCleanups covers every migration whose down direction
```

**3c. Regenerate sqlc** (models gain the new tables and `agent_task_queue` columns):

```bash
make sqlc && cd server && go build ./...
```


- [ ] **Step 4: Run, expect PASS**

```bash
cd server && go run ./cmd/migrate up
go test ./cmd/migrate ./internal/migrations -count=1
go test ./internal/handler -run 'TestWorkspaceDeletionManifest' -count=1   # EXPECTED to fail until Task A4 (5 unclassified ext_ tables); do not commit A1 with this suite red in CI: see note below
```

Note: the manifest test is part of the `internal/handler` suite and goes red as soon as `ext_0001` is applied to the test database. To keep every commit green, **land A1 and the A4 manifest edit together**, or apply the manifest hunk from Task A4 Step 3d in this commit. Do the latter: apply A4 Step 3d (manifest rows only, a test-file edit) now.

Also verify rollback once: `for f in $(ls server/migrations/ext_*.down.sql | sort -r); do psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -f $f; done` then re-apply the `.up.sql` files in order (psql runs outside a transaction, so CONCURRENTLY works).


- [ ] **Step 5: Commit**

```bash
git add server/migrations/ext_* server/cmd/migrate/main.go server/internal/migrations/ext_workflow_constraint_test.go server/pkg/db/generated server/internal/handler/workspace_delete_manifest_test.go
git commit -m "feat(ext-workflow): add workflow tables, task columns and assignee constraint migrations

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


### Task A2: Workflow and node CRUD queries

**Files:**
- Create: `server/pkg/db/queries/ext_workflow.sql`
- Regenerate: `server/pkg/db/generated/ext_workflow.sql.go`

**Interfaces:** Produces `Queries` methods (generated): `CreateExtWorkflow(ctx, CreateExtWorkflowParams{WorkspaceID, Name, Description, SupervisorAgentID, MaxRewinds int32, CreatorID, AvatarUrl pgtype.Text}) (ExtWorkflow, error)`; `GetExtWorkflowInWorkspace(ctx, {ID, WorkspaceID})`; `LockExtWorkflowForUpdate(ctx, {ID, WorkspaceID})`; `ListExtWorkflows(ctx, workspaceID) ([]ListExtWorkflowsRow, error)` (row = all `ext_workflow` columns + `NodeCount int32`, `ActiveRunCount int32`, `LastRunAt pgtype.Timestamptz`); `UpdateExtWorkflow(ctx, {ID, Name pgtype.Text, Description pgtype.Text, SupervisorAgentID pgtype.UUID, MaxRewinds pgtype.Int4, AvatarUrl pgtype.Text})`; `ArchiveExtWorkflow(ctx, {ID, ArchivedBy})`; `ListExtWorkflowNodes(ctx, workflowID)` (ordered by position, key); `DeleteExtWorkflowNodes(ctx, workflowID) error`; `CreateExtWorkflowNode(ctx, {WorkflowID, WorkspaceID, Key, Title, AgentID, Prompt, RequiresReview bool, MaxAttempts int32, DependsOn []string, Position int32})`; `CountActiveExtWorkflowRuns(ctx, workflowID) (int64, error)`; `DeleteExtWorkflowWorkspaceData(ctx, workspaceID) error`. Consumes tables from A1.

- [ ] **Step 1: Write the failing test.** The queries are exercised by the Task A4 handler tests; here the failing signal is compilation. Create a throwaway compile check:

```go
// server/pkg/db/generated/ext_workflow_compile_test.go  (delete after Step 4)
package db

import "testing"

func TestExtWorkflowQueriesExist(t *testing.T) {
	var q *Queries
	_ = q.ListExtWorkflows
	_ = q.CountActiveExtWorkflowRuns
	_ = q.DeleteExtWorkflowWorkspaceData
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
cd server && go test ./pkg/db/generated -run TestExtWorkflowQueriesExist   # FAIL: q.ListExtWorkflows undefined
```

- [ ] **Step 3: Implement**

**Create `server/pkg/db/queries/ext_workflow.sql`:**

```sql
-- name: CreateExtWorkflow :one
INSERT INTO ext_workflow (
    workspace_id, name, description, supervisor_agent_id, max_rewinds, creator_id, avatar_url
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetExtWorkflowInWorkspace :one
SELECT * FROM ext_workflow WHERE id = $1 AND workspace_id = $2;

-- name: LockExtWorkflowForUpdate :one
SELECT * FROM ext_workflow WHERE id = $1 AND workspace_id = $2 FOR UPDATE;

-- name: ListExtWorkflows :many
SELECT
    w.*,
    (SELECT count(*) FROM ext_workflow_node n WHERE n.workflow_id = w.id)::int AS node_count,
    (SELECT count(*) FROM ext_workflow_run r
        WHERE r.workflow_id = w.id AND r.status IN ('running', 'waiting_human'))::int AS active_run_count,
    (SELECT max(r.created_at) FROM ext_workflow_run r WHERE r.workflow_id = w.id)::timestamptz AS last_run_at
FROM ext_workflow w
WHERE w.workspace_id = $1 AND w.archived_at IS NULL
ORDER BY w.created_at ASC;

-- name: UpdateExtWorkflow :one
UPDATE ext_workflow SET
    name = COALESCE(sqlc.narg('name'), name),
    description = COALESCE(sqlc.narg('description'), description),
    supervisor_agent_id = COALESCE(sqlc.narg('supervisor_agent_id'), supervisor_agent_id),
    max_rewinds = COALESCE(sqlc.narg('max_rewinds'), max_rewinds),
    avatar_url = COALESCE(sqlc.narg('avatar_url'), avatar_url),
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ArchiveExtWorkflow :one
UPDATE ext_workflow SET archived_at = now(), archived_by = $2, updated_at = now()
WHERE id = $1 AND archived_at IS NULL
RETURNING *;

-- name: ListExtWorkflowNodes :many
SELECT * FROM ext_workflow_node WHERE workflow_id = $1 ORDER BY position ASC, key ASC;

-- name: DeleteExtWorkflowNodes :exec
DELETE FROM ext_workflow_node WHERE workflow_id = $1;

-- name: CreateExtWorkflowNode :one
INSERT INTO ext_workflow_node (
    workflow_id, workspace_id, key, title, agent_id, prompt,
    requires_review, max_attempts, depends_on, position
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: CountActiveExtWorkflowRuns :one
SELECT count(*)::bigint FROM ext_workflow_run
WHERE workflow_id = $1 AND status IN ('running', 'waiting_human');

-- name: DeleteExtWorkflowWorkspaceData :exec
-- Workspace teardown. No foreign keys or cascades exist, so every ext workflow
-- table is swept by workspace_id. Called from the workspace-delete step list.
WITH deleted_events AS (
    DELETE FROM ext_workflow_run_event WHERE ext_workflow_run_event.workspace_id = $1
),
deleted_steps AS (
    DELETE FROM ext_workflow_run_step WHERE ext_workflow_run_step.workspace_id = $1
),
deleted_runs AS (
    DELETE FROM ext_workflow_run WHERE ext_workflow_run.workspace_id = $1
),
deleted_nodes AS (
    DELETE FROM ext_workflow_node WHERE ext_workflow_node.workspace_id = $1
)
DELETE FROM ext_workflow WHERE ext_workflow.workspace_id = $1;
```

Then `make sqlc`.


- [ ] **Step 4: Run, expect PASS**

```bash
cd server && go test ./pkg/db/generated -run TestExtWorkflowQueriesExist -count=1 && rm pkg/db/generated/ext_workflow_compile_test.go && go build ./...
```

- [ ] **Step 5: Commit**

```bash
git add server/pkg/db/queries/ext_workflow.sql server/pkg/db/generated
git commit -m "feat(ext-workflow): add workflow CRUD and teardown queries

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


### Task A3: Pure definition validation (`extworkflow/definition.go`)

**Files:**
- Create: `server/internal/extworkflow/definition.go`, `server/internal/extworkflow/definition_test.go`

**Interfaces:** Produces exactly the contract names: `Node`, `Definition`, `ValidationError`, `MaxNodes=50`, `MaxPromptBytes=20000`, `ValidateStructure`, `(Definition).NodeByKey`, `Ancestors`, `Descendants`, `DepthOf`; plus `ValidateSettings` and `(ValidationError).Error()`. Consumes nothing. Part B must NOT recreate this file.

- [ ] **Step 1: Write the failing test**

**Create `server/internal/extworkflow/definition_test.go`:**

```go
package extworkflow

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func node(key string, deps ...string) Node {
	return Node{
		Key:         key,
		Title:       "Title " + key,
		AgentID:     "11111111-1111-1111-1111-111111111111",
		Prompt:      "do " + key,
		MaxAttempts: 3,
		DependsOn:   deps,
	}
}

func def(nodes ...Node) Definition {
	return Definition{
		SupervisorAgentID: "22222222-2222-2222-2222-222222222222",
		MaxRewinds:        3,
		Nodes:             nodes,
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diamond: a -> b, a -> c, b -> d, c -> d  (arrow = "is depended on by")
func diamond() Definition {
	return def(node("a"), node("b", "a"), node("c", "a"), node("d", "b", "c"))
}

func TestValidateStructure(t *testing.T) {
	longPrompt := strings.Repeat("x", MaxPromptBytes+1)
	tooMany := make([]Node, MaxNodes+1)
	for i := range tooMany {
		tooMany[i] = node("n" + strings.Repeat("0", 3) + string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}

	tests := []struct {
		name string
		d    Definition
		want []ValidationError // exact, ordered
	}{
		{name: "valid diamond", d: diamond(), want: nil},
		{name: "valid single node", d: def(node("only")), want: nil},
		{
			name: "no nodes",
			d:    def(),
			want: []ValidationError{{Field: "nodes", Message: "at least one node is required"}},
		},
		{
			name: "missing supervisor",
			d:    Definition{MaxRewinds: 3, Nodes: []Node{node("a")}},
			want: []ValidationError{{Field: "supervisor_agent_id", Message: "supervisor agent is required"}},
		},
		{
			name: "max rewinds out of range",
			d:    Definition{SupervisorAgentID: "s", MaxRewinds: 11, Nodes: []Node{node("a")}},
			want: []ValidationError{{Field: "max_rewinds", Message: "must be between 0 and 10"}},
		},
		{
			name: "negative max rewinds",
			d:    Definition{SupervisorAgentID: "s", MaxRewinds: -1, Nodes: []Node{node("a")}},
			want: []ValidationError{{Field: "max_rewinds", Message: "must be between 0 and 10"}},
		},
		{
			name: "zero max rewinds is allowed",
			d:    Definition{SupervisorAgentID: "s", MaxRewinds: 0, Nodes: []Node{node("a")}},
			want: nil,
		},
		{
			name: "too many nodes",
			d:    def(tooMany...),
			want: []ValidationError{{Field: "nodes", Message: "at most 50 nodes are allowed"}},
		},
		{
			name: "invalid key uppercase",
			d:    def(node("Build")),
			want: []ValidationError{{NodeKey: "Build", Field: "key", Message: "must match [a-z0-9_-] and be 1-40 characters"}},
		},
		{
			name: "invalid key empty",
			d:    def(node("")),
			want: []ValidationError{{NodeKey: "", Field: "key", Message: "must match [a-z0-9_-] and be 1-40 characters"}},
		},
		{
			name: "invalid key too long",
			d:    def(node(strings.Repeat("a", 41))),
			want: []ValidationError{{NodeKey: strings.Repeat("a", 41), Field: "key", Message: "must match [a-z0-9_-] and be 1-40 characters"}},
		},
		{
			name: "valid key at 40 chars with dash and underscore",
			d:    def(node(strings.Repeat("a", 38) + "-_")),
			want: nil,
		},
		{
			name: "duplicate key reported once on the second occurrence",
			d:    def(node("a"), node("a")),
			want: []ValidationError{{NodeKey: "a", Field: "key", Message: "duplicate key"}},
		},
		{
			name: "missing title",
			d:    def(Node{Key: "a", Title: "  ", AgentID: "x", MaxAttempts: 1}),
			want: []ValidationError{{NodeKey: "a", Field: "title", Message: "title is required"}},
		},
		{
			name: "title too long",
			d:    def(Node{Key: "a", Title: strings.Repeat("é", 201), AgentID: "x", MaxAttempts: 1}),
			want: []ValidationError{{NodeKey: "a", Field: "title", Message: "must be at most 200 characters"}},
		},
		{
			name: "missing agent",
			d:    def(Node{Key: "a", Title: "t", MaxAttempts: 1}),
			want: []ValidationError{{NodeKey: "a", Field: "agent_id", Message: "agent is required"}},
		},
		{
			name: "prompt too long",
			d:    def(Node{Key: "a", Title: "t", AgentID: "x", MaxAttempts: 1, Prompt: longPrompt}),
			want: []ValidationError{{NodeKey: "a", Field: "prompt", Message: "must be at most 20000 bytes"}},
		},
		{
			name: "prompt at the limit is allowed",
			d:    def(Node{Key: "a", Title: "t", AgentID: "x", MaxAttempts: 1, Prompt: strings.Repeat("x", MaxPromptBytes)}),
			want: nil,
		},
		{
			name: "max attempts zero",
			d:    def(Node{Key: "a", Title: "t", AgentID: "x", MaxAttempts: 0}),
			want: []ValidationError{{NodeKey: "a", Field: "max_attempts", Message: "must be between 1 and 10"}},
		},
		{
			name: "max attempts eleven",
			d:    def(Node{Key: "a", Title: "t", AgentID: "x", MaxAttempts: 11}),
			want: []ValidationError{{NodeKey: "a", Field: "max_attempts", Message: "must be between 1 and 10"}},
		},
		{
			name: "self dependency",
			d:    def(node("a", "a")),
			want: []ValidationError{{NodeKey: "a", Field: "depends_on", Message: "a node cannot depend on itself"}},
		},
		{
			name: "unknown dependency",
			d:    def(node("a", "ghost")),
			want: []ValidationError{{NodeKey: "a", Field: "depends_on", Message: `unknown dependency "ghost"`}},
		},
		{
			name: "duplicate dependency",
			d:    def(node("a"), node("b", "a", "a")),
			want: []ValidationError{{NodeKey: "b", Field: "depends_on", Message: `duplicate dependency "a"`}},
		},
		{
			name: "two node cycle",
			d:    def(node("a", "b"), node("b", "a")),
			want: []ValidationError{
				{NodeKey: "a", Field: "depends_on", Message: "node is part of a dependency cycle"},
				{NodeKey: "b", Field: "depends_on", Message: "node is part of a dependency cycle"},
			},
		},
		{
			name: "cycle with a healthy tail only flags cycle members",
			d:    def(node("root"), node("a", "root", "c"), node("b", "a"), node("c", "b"), node("tail", "c")),
			want: []ValidationError{
				{NodeKey: "a", Field: "depends_on", Message: "node is part of a dependency cycle"},
				{NodeKey: "b", Field: "depends_on", Message: "node is part of a dependency cycle"},
				{NodeKey: "c", Field: "depends_on", Message: "node is part of a dependency cycle"},
			},
		},
		{
			name: "errors are ordered definition first then by node",
			d: Definition{SupervisorAgentID: "", MaxRewinds: 99, Nodes: []Node{
				{Key: "a", Title: "", AgentID: "", MaxAttempts: 0},
				{Key: "b", Title: "t", AgentID: "x", MaxAttempts: 1, DependsOn: []string{"zzz"}},
			}},
			want: []ValidationError{
				{Field: "supervisor_agent_id", Message: "supervisor agent is required"},
				{Field: "max_rewinds", Message: "must be between 0 and 10"},
				{NodeKey: "a", Field: "title", Message: "title is required"},
				{NodeKey: "a", Field: "agent_id", Message: "agent is required"},
				{NodeKey: "a", Field: "max_attempts", Message: "must be between 1 and 10"},
				{NodeKey: "b", Field: "depends_on", Message: `unknown dependency "zzz"`},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateStructure(tc.d)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ValidateStructure mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

func TestValidationErrorString(t *testing.T) {
	if got := (ValidationError{Field: "nodes", Message: "bad"}).Error(); got != "nodes: bad" {
		t.Fatalf("definition-level error string = %q", got)
	}
	if got := (ValidationError{NodeKey: "a", Field: "key", Message: "bad"}).Error(); got != `node "a": key: bad` {
		t.Fatalf("node-level error string = %q", got)
	}
}

func TestNodeByKey(t *testing.T) {
	d := diamond()
	n, ok := d.NodeByKey("c")
	if !ok || n.Key != "c" {
		t.Fatalf("NodeByKey(c) = %+v, %v", n, ok)
	}
	if _, ok := d.NodeByKey("nope"); ok {
		t.Fatal("NodeByKey(nope) should not be found")
	}
}

func TestAncestorsAndDescendants(t *testing.T) {
	d := diamond()
	tests := []struct {
		key             string
		wantAncestors   []string
		wantDescendants []string
	}{
		{"a", nil, []string{"b", "c", "d"}},
		{"b", []string{"a"}, []string{"d"}},
		{"c", []string{"a"}, []string{"d"}},
		{"d", []string{"a", "b", "c"}, nil},
		{"missing", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			if got := sortedKeys(Ancestors(d, tc.key)); !reflect.DeepEqual(got, orEmpty(tc.wantAncestors)) {
				t.Errorf("Ancestors(%s) = %v, want %v", tc.key, got, tc.wantAncestors)
			}
			if got := sortedKeys(Descendants(d, tc.key)); !reflect.DeepEqual(got, orEmpty(tc.wantDescendants)) {
				t.Errorf("Descendants(%s) = %v, want %v", tc.key, got, tc.wantDescendants)
			}
		})
	}
}

func TestAncestorsDescendantsExcludeSelfOnCycle(t *testing.T) {
	d := def(node("a", "b"), node("b", "a"))
	if Ancestors(d, "a")["a"] {
		t.Error("Ancestors must exclude the key itself even on a cycle")
	}
	if Descendants(d, "a")["a"] {
		t.Error("Descendants must exclude the key itself even on a cycle")
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func TestDepthOf(t *testing.T) {
	tests := []struct {
		name string
		d    Definition
		want map[string]int
	}{
		{"diamond", diamond(), map[string]int{"a": 0, "b": 1, "c": 1, "d": 2}},
		{
			"longest path wins",
			def(node("a"), node("b", "a"), node("c", "b"), node("d", "a", "c")),
			map[string]int{"a": 0, "b": 1, "c": 2, "d": 3},
		},
		{"independent roots", def(node("x"), node("y")), map[string]int{"x": 0, "y": 0}},
		{"unknown dependency ignored", def(node("a", "ghost")), map[string]int{"a": 0}},
		{"empty", def(), map[string]int{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DepthOf(tc.d); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DepthOf = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDepthOfTerminatesOnCycle(t *testing.T) {
	d := def(node("a", "b"), node("b", "a"))
	got := DepthOf(d)
	if len(got) != 2 {
		t.Fatalf("DepthOf on a cycle should still cover every node, got %v", got)
	}
}

func TestValidateSettings(t *testing.T) {
	tests := []struct {
		name       string
		supervisor string
		rewinds    int
		want       []ValidationError
	}{
		{"ok", "s", 3, nil},
		{"zero rewinds ok", "s", 0, nil},
		{"ten rewinds ok", "s", 10, nil},
		{"blank supervisor", "  ", 3, []ValidationError{{Field: "supervisor_agent_id", Message: "supervisor agent is required"}}},
		{"rewinds too high", "s", 11, []ValidationError{{Field: "max_rewinds", Message: "must be between 0 and 10"}}},
		{"rewinds negative", "s", -1, []ValidationError{{Field: "max_rewinds", Message: "must be between 0 and 10"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateSettings(tc.supervisor, tc.rewinds)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
cd server && go test ./internal/extworkflow/... -count=1   # FAIL: undefined: ValidateStructure, Definition, Node ...
```

- [ ] **Step 3: Implement**

**Create `server/internal/extworkflow/definition.go`:**

```go
// Package extworkflow implements the first-class workflow feature of this fork:
// reusable DAG templates, a deterministic run engine, and a supervisor agent.
//
// This file is the pure definition layer: types and structural validation with
// no database or network access, so it can be shared by the CRUD handler (which
// rejects invalid templates) and the engine (which snapshots a validated
// template into each run).
package extworkflow

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// MaxNodes bounds the size of one workflow definition.
	MaxNodes = 50
	// MaxPromptBytes bounds one node prompt (UTF-8 bytes, not runes).
	MaxPromptBytes = 20000

	maxTitleRunes  = 200
	minMaxAttempts = 1
	maxMaxAttempts = 10
	maxMaxRewinds  = 10
)

var nodeKeyPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

// Node is one step template: a prompt run by one agent.
type Node struct {
	Key            string   `json:"key"`
	Title          string   `json:"title"`
	AgentID        string   `json:"agent_id"` // uuid string
	Prompt         string   `json:"prompt"`
	RequiresReview bool     `json:"requires_review"`
	MaxAttempts    int      `json:"max_attempts"`
	DependsOn      []string `json:"depends_on"`
}

// Definition is a workflow template. It is also the JSON snapshot stored in
// ext_workflow_run.definition.
type Definition struct {
	SupervisorAgentID string `json:"supervisor_agent_id"`
	MaxRewinds        int    `json:"max_rewinds"`
	Nodes             []Node `json:"nodes"` // slice order = position
}

// ValidationError is one problem found in a definition. NodeKey is empty for
// definition-level problems.
type ValidationError struct {
	NodeKey string `json:"node_key,omitempty"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e ValidationError) Error() string {
	if e.NodeKey != "" {
		return fmt.Sprintf("node %q: %s: %s", e.NodeKey, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidateStructure checks everything that can be decided without the
// database: limits, key format and uniqueness, dependency references, self
// dependencies, cycles, attempt and rewind ranges. Agent existence and
// invokability are checked by the caller. The result is empty for a valid
// definition and ordered deterministically (definition-level errors first, then
// by node position, then cycles).
func ValidateStructure(d Definition) []ValidationError {
	var errs []ValidationError

	errs = append(errs, ValidateSettings(d.SupervisorAgentID, d.MaxRewinds)...)
	switch {
	case len(d.Nodes) == 0:
		errs = append(errs, ValidationError{Field: "nodes", Message: "at least one node is required"})
	case len(d.Nodes) > MaxNodes:
		errs = append(errs, ValidationError{Field: "nodes", Message: fmt.Sprintf("at most %d nodes are allowed", MaxNodes)})
	}

	// First occurrence of each key wins; later ones are reported as duplicates
	// and excluded from the graph so one typo does not cascade.
	known := make(map[string]bool, len(d.Nodes))
	for _, n := range d.Nodes {
		if nodeKeyPattern.MatchString(n.Key) {
			known[n.Key] = true
		}
	}
	seen := make(map[string]bool, len(d.Nodes))
	for _, n := range d.Nodes {
		switch {
		case !nodeKeyPattern.MatchString(n.Key):
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "key", Message: "must match [a-z0-9_-] and be 1-40 characters"})
		case seen[n.Key]:
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "key", Message: "duplicate key"})
		}
		seen[n.Key] = true

		if strings.TrimSpace(n.Title) == "" {
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "title", Message: "title is required"})
		} else if utf8.RuneCountInString(n.Title) > maxTitleRunes {
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "title", Message: fmt.Sprintf("must be at most %d characters", maxTitleRunes)})
		}
		if strings.TrimSpace(n.AgentID) == "" {
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "agent_id", Message: "agent is required"})
		}
		if len(n.Prompt) > MaxPromptBytes {
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "prompt", Message: fmt.Sprintf("must be at most %d bytes", MaxPromptBytes)})
		}
		if n.MaxAttempts < minMaxAttempts || n.MaxAttempts > maxMaxAttempts {
			errs = append(errs, ValidationError{NodeKey: n.Key, Field: "max_attempts", Message: fmt.Sprintf("must be between %d and %d", minMaxAttempts, maxMaxAttempts)})
		}

		depSeen := make(map[string]bool, len(n.DependsOn))
		for _, dep := range n.DependsOn {
			switch {
			case dep == n.Key:
				errs = append(errs, ValidationError{NodeKey: n.Key, Field: "depends_on", Message: "a node cannot depend on itself"})
			case !known[dep]:
				errs = append(errs, ValidationError{NodeKey: n.Key, Field: "depends_on", Message: fmt.Sprintf("unknown dependency %q", dep)})
			case depSeen[dep]:
				errs = append(errs, ValidationError{NodeKey: n.Key, Field: "depends_on", Message: fmt.Sprintf("duplicate dependency %q", dep)})
			}
			depSeen[dep] = true
		}
	}

	for _, key := range cyclicNodeKeys(d, known) {
		errs = append(errs, ValidationError{NodeKey: key, Field: "depends_on", Message: "node is part of a dependency cycle"})
	}
	return errs
}

// ValidateSettings checks the definition-level fields that do not depend on
// the node set. The CRUD handler calls it on its own when an update changes
// the supervisor or the rewind budget without replacing the nodes.
func ValidateSettings(supervisorAgentID string, maxRewinds int) []ValidationError {
	var errs []ValidationError
	if strings.TrimSpace(supervisorAgentID) == "" {
		errs = append(errs, ValidationError{Field: "supervisor_agent_id", Message: "supervisor agent is required"})
	}
	if maxRewinds < 0 || maxRewinds > maxMaxRewinds {
		errs = append(errs, ValidationError{Field: "max_rewinds", Message: fmt.Sprintf("must be between 0 and %d", maxMaxRewinds)})
	}
	return errs
}

// NodeByKey returns the node with the given key.
func (d Definition) NodeByKey(key string) (Node, bool) {
	for _, n := range d.Nodes {
		if n.Key == key {
			return n, true
		}
	}
	return Node{}, false
}

// Ancestors returns every node transitively upstream of key (its dependencies,
// their dependencies, ...), excluding key itself.
func Ancestors(d Definition, key string) map[string]bool {
	deps := make(map[string][]string, len(d.Nodes))
	for _, n := range d.Nodes {
		deps[n.Key] = n.DependsOn
	}
	out := reach(key, func(k string) []string { return deps[k] })
	delete(out, key)
	return out
}

// Descendants returns every node transitively downstream of key (nodes that
// depend on it, directly or not), excluding key itself.
func Descendants(d Definition, key string) map[string]bool {
	dependents := make(map[string][]string, len(d.Nodes))
	for _, n := range d.Nodes {
		for _, dep := range n.DependsOn {
			dependents[dep] = append(dependents[dep], n.Key)
		}
	}
	out := reach(key, func(k string) []string { return dependents[k] })
	delete(out, key)
	return out
}

// DepthOf returns the longest-path depth of every node: roots are 0 and a node
// is one deeper than its deepest dependency. Unknown dependencies are ignored.
// On a cyclic (invalid) definition the edge that closes a cycle is ignored, so
// the call always terminates.
func DepthOf(d Definition) map[string]int {
	byKey := make(map[string]Node, len(d.Nodes))
	for _, n := range d.Nodes {
		byKey[n.Key] = n
	}
	depth := make(map[string]int, len(d.Nodes))
	visiting := make(map[string]bool, len(d.Nodes))
	var visit func(key string) int
	visit = func(key string) int {
		if v, ok := depth[key]; ok {
			return v
		}
		visiting[key] = true
		best := 0
		for _, dep := range byKey[key].DependsOn {
			if _, ok := byKey[dep]; !ok || visiting[dep] {
				continue
			}
			if v := visit(dep) + 1; v > best {
				best = v
			}
		}
		visiting[key] = false
		depth[key] = best
		return best
	}
	for _, n := range d.Nodes {
		visit(n.Key)
	}
	return depth
}

// reach returns every node reachable from start by following next one or more
// times. start is included only when it lies on a cycle.
func reach(start string, next func(string) []string) map[string]bool {
	out := map[string]bool{}
	queue := append([]string(nil), next(start)...)
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		if out[k] {
			continue
		}
		out[k] = true
		queue = append(queue, next(k)...)
	}
	return out
}

// cyclicNodeKeys returns, in node order, the keys that lie on a dependency
// cycle. Nodes that merely depend on a cycle are not reported.
func cyclicNodeKeys(d Definition, known map[string]bool) []string {
	deps := make(map[string][]string, len(d.Nodes))
	for _, n := range d.Nodes {
		for _, dep := range n.DependsOn {
			if known[dep] && dep != n.Key {
				deps[n.Key] = append(deps[n.Key], dep)
			}
		}
	}
	var keys []string
	done := map[string]bool{}
	for _, n := range d.Nodes {
		if !known[n.Key] || done[n.Key] {
			continue
		}
		done[n.Key] = true
		if reach(n.Key, func(k string) []string { return deps[k] })[n.Key] {
			keys = append(keys, n.Key)
		}
	}
	return keys
}
```

- [ ] **Step 4: Run, expect PASS**

```bash
cd server && gofmt -l internal/extworkflow && go vet ./internal/extworkflow && go test ./internal/extworkflow/... -count=1   # 43 tests pass
```

- [ ] **Step 5: Commit**

```bash
git add server/internal/extworkflow/definition.go server/internal/extworkflow/definition_test.go
git commit -m "feat(ext-workflow): add pure workflow definition validation

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


### Task A4: Workflow CRUD handler, routes, events, workspace teardown

**Files:**
- Create: `server/internal/handler/ext_workflow.go`, `server/internal/handler/ext_workflow_test.go`, `server/cmd/server/ext_routes.go`
- Modify: `server/cmd/server/router.go` (after the `r.Post("/api/issues/{id}/squad-evaluated", h.RecordSquadLeaderEvaluation)` line, ~2156, inside the same group)
- Modify: `server/pkg/protocol/events.go` (after `EventSquadDeleted`, line ~143)
- Modify: `server/internal/handler/workspace.go` (`deleteSteps`, after the `"delete squads and skills"` step, ~line 1310)
- Modify: `server/internal/handler/workspace_delete_manifest_test.go` (`workspaceDeletionManifest`, end of the map)

**Interfaces:** Consumes A1-A3 (`db.ExtWorkflow*`, queries, `extworkflow.ValidateStructure/ValidateSettings`), existing `memberCanWireAgent`, `requireWorkspaceMember`, `parseUUIDOrBadRequest`, `h.publish`, `h.TxStarter`. Produces handlers `ListExtWorkflows`, `CreateExtWorkflow`, `GetExtWorkflow`, `UpdateExtWorkflow`, `DeleteExtWorkflow`; types `ExtWorkflowResponse`, `ExtWorkflowNodeResponse`; `canManageExtWorkflow(member, wf) bool`; `protocol.EventExtWorkflowCreated/Updated/Deleted`; `registerExtRoutes`.

Endpoints: `GET/POST /api/ext/workflows`, `GET/PUT/DELETE /api/ext/workflows/{id}`. Permissions: any member creates; creator or owner/admin updates/archives; anyone in the workspace reads. Archived workflow: GET 200, PUT 409, second DELETE 400.

- [ ] **Step 1: Write the failing tests**

**Create `server/internal/handler/ext_workflow_test.go`:**

```go
package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// extWFReq builds a request as userID (empty = workspace owner) carrying the
// chi params the ext workflow handlers read (workspaceId plus optional id).
func extWFReq(userID, method, path string, body any, id string) *http.Request {
	var req *http.Request
	if userID == "" {
		req = newRequest(method, path, body)
	} else {
		req = newRequestAs(userID, method, path, body)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("workspaceId", testWorkspaceID)
	if id != "" {
		rctx.URLParams.Add("id", id)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

type extWFValidationBody struct {
	Error  string `json:"error"`
	Errors []struct {
		NodeKey string `json:"node_key"`
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"errors"`
}

func extWFNode(key, agentID string, deps ...string) map[string]any {
	if deps == nil {
		deps = []string{}
	}
	return map[string]any{
		"key": key, "title": "Title " + key, "agent_id": agentID, "prompt": "do " + key,
		"requires_review": false, "max_attempts": 3, "depends_on": deps,
	}
}

// createExtWorkflowAs creates a workflow through the handler and registers
// cleanup for it and its nodes (there are no cascading deletes).
func createExtWorkflowAs(t *testing.T, userID, name, supervisorID string) ExtWorkflowResponse {
	t.Helper()
	var resp ExtWorkflowResponse
	testutil.Call(t, testHandler.CreateExtWorkflow, extWFReq(userID, "POST", "/api/ext/workflows", map[string]any{
		"name": name, "description": "d", "supervisor_agent_id": supervisorID,
	}, "")).Want(http.StatusCreated).JSON(&resp)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM ext_workflow_node WHERE workflow_id = $1`, resp.ID)
		testPool.Exec(context.Background(), `DELETE FROM ext_workflow WHERE id = $1`, resp.ID)
	})
	return resp
}

func updateExtWorkflow(t *testing.T, userID, id string, body any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.UpdateExtWorkflow, extWFReq(userID, "PUT", "/api/ext/workflows/"+id, body, id))
}

var (
	extWFEventsOnce sync.Once
	extWFEventsMu   sync.Mutex
	extWFEvents     []events.Event
)

// extWorkflowEventTypes returns, in publish order, the ext_workflow:* event
// types recorded so far for one workflow id. subscribeExtWorkflowEvents must
// have been called first.
func extWorkflowEventTypes(t *testing.T, workflowID string) []string {
	t.Helper()
	extWFEventsMu.Lock()
	defer extWFEventsMu.Unlock()
	var out []string
	for _, e := range extWFEvents {
		if p, ok := e.Payload.(map[string]any); ok && p["workflow_id"] == workflowID {
			out = append(out, e.Type)
		}
	}
	return out
}

// subscribeExtWorkflowEvents registers the recorder once per process; the bus
// has no unsubscribe.
func subscribeExtWorkflowEvents() {
	extWFEventsOnce.Do(func() {
		for _, typ := range []string{protocol.EventExtWorkflowCreated, protocol.EventExtWorkflowUpdated, protocol.EventExtWorkflowDeleted} {
			testHandler.Bus.Subscribe(typ, func(e events.Event) {
				extWFEventsMu.Lock()
				extWFEvents = append(extWFEvents, e)
				extWFEventsMu.Unlock()
			})
		}
	})
}

func requireExtWorkflowDB(t *testing.T) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
}

func TestExtWorkflow_Lifecycle(t *testing.T) {
	requireExtWorkflowDB(t)
	subscribeExtWorkflowEvents()
	supervisor := createHandlerTestAgent(t, "ext-wf-supervisor", nil)
	worker := createHandlerTestAgent(t, "ext-wf-worker", nil)

	wf := createExtWorkflowAs(t, "", "Release train", supervisor)
	if wf.MaxRewinds != 3 || wf.NodeCount != 0 || wf.CreatorID != testUserID || wf.ArchivedAt != nil {
		t.Fatalf("unexpected created workflow: %+v", wf)
	}

	var upd ExtWorkflowResponse
	updateExtWorkflow(t, "", wf.ID, map[string]any{
		"name":        "Release train v2",
		"max_rewinds": 5,
		"nodes": []any{
			extWFNode("build", worker),
			extWFNode("test", worker, "build"),
		},
	}).Want(http.StatusOK).JSON(&upd)
	if upd.Name != "Release train v2" || upd.MaxRewinds != 5 || upd.NodeCount != 2 || len(upd.Nodes) != 2 {
		t.Fatalf("unexpected updated workflow: %+v", upd)
	}
	if upd.Nodes[0].Key != "build" || upd.Nodes[0].Position != 0 || upd.Nodes[1].Key != "test" || upd.Nodes[1].Position != 1 {
		t.Fatalf("nodes not stored in request order: %+v", upd.Nodes)
	}
	if got := upd.Nodes[1].DependsOn; len(got) != 1 || got[0] != "build" {
		t.Fatalf("depends_on not stored: %+v", upd.Nodes[1])
	}

	// A rename without nodes keeps the stored node set.
	updateExtWorkflow(t, "", wf.ID, map[string]any{"description": "only text"}).Want(http.StatusOK).JSON(&upd)
	if upd.Description != "only text" || len(upd.Nodes) != 2 {
		t.Fatalf("settings-only update changed nodes: %+v", upd)
	}

	// Replacing the node set replaces it as a whole.
	updateExtWorkflow(t, "", wf.ID, map[string]any{
		"nodes": []any{extWFNode("only", worker)},
	}).Want(http.StatusOK).JSON(&upd)
	if len(upd.Nodes) != 1 || upd.Nodes[0].Key != "only" {
		t.Fatalf("node set not replaced: %+v", upd.Nodes)
	}

	var got ExtWorkflowResponse
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq("", "GET", "/api/ext/workflows/"+wf.ID, nil, wf.ID)).
		Want(http.StatusOK).JSON(&got)
	if len(got.Nodes) != 1 || got.NodeCount != 1 || got.ActiveRunCount != 0 {
		t.Fatalf("unexpected detail: %+v", got)
	}

	var list struct {
		Workflows []ExtWorkflowResponse `json:"workflows"`
	}
	testutil.Call(t, testHandler.ListExtWorkflows, extWFReq("", "GET", "/api/ext/workflows", nil, "")).
		Want(http.StatusOK).JSON(&list)
	var listed *ExtWorkflowResponse
	for i := range list.Workflows {
		if list.Workflows[i].ID == wf.ID {
			listed = &list.Workflows[i]
		}
	}
	if listed == nil || listed.NodeCount != 1 || listed.LastRunAt != nil || len(listed.Nodes) != 0 {
		t.Fatalf("unexpected list entry: %+v", listed)
	}

	testutil.Call(t, testHandler.DeleteExtWorkflow, extWFReq("", "DELETE", "/api/ext/workflows/"+wf.ID, nil, wf.ID)).
		Want(http.StatusNoContent)
	testutil.Call(t, testHandler.DeleteExtWorkflow, extWFReq("", "DELETE", "/api/ext/workflows/"+wf.ID, nil, wf.ID)).
		Want(http.StatusBadRequest)
	testutil.Call(t, testHandler.ListExtWorkflows, extWFReq("", "GET", "/api/ext/workflows", nil, "")).
		Want(http.StatusOK).JSON(&list)
	for _, w := range list.Workflows {
		if w.ID == wf.ID {
			t.Fatal("archived workflow still listed")
		}
	}
	// Archived workflows stay readable (run history links to them) but are read-only.
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq("", "GET", "/api/ext/workflows/"+wf.ID, nil, wf.ID)).
		Want(http.StatusOK).JSON(&got)
	if got.ArchivedAt == nil {
		t.Fatal("archived_at not set on archived workflow")
	}
	updateExtWorkflow(t, "", wf.ID, map[string]any{"name": "nope"}).Want(http.StatusConflict)

	want := []string{protocol.EventExtWorkflowCreated, protocol.EventExtWorkflowUpdated, protocol.EventExtWorkflowUpdated,
		protocol.EventExtWorkflowUpdated, protocol.EventExtWorkflowDeleted}
	gotEvents := extWorkflowEventTypes(t, wf.ID)
	if len(gotEvents) != len(want) {
		t.Fatalf("events = %v, want %v", gotEvents, want)
	}
	for i := range want {
		if gotEvents[i] != want[i] {
			t.Fatalf("events = %v, want %v", gotEvents, want)
		}
	}
}

func TestExtWorkflow_CreateValidation(t *testing.T) {
	requireExtWorkflowDB(t)
	supervisor := createHandlerTestAgent(t, "ext-wf-create-sup", nil)

	post := func(body any) *testutil.Response {
		return testutil.Call(t, testHandler.CreateExtWorkflow, extWFReq("", "POST", "/api/ext/workflows", body, ""))
	}
	post("{not json").Want(http.StatusBadRequest)
	post(map[string]any{"name": "  ", "supervisor_agent_id": supervisor}).Want(http.StatusBadRequest)

	var body extWFValidationBody
	post(map[string]any{"name": "x"}).Want(http.StatusUnprocessableEntity).JSON(&body)
	if body.Error != "validation_failed" || len(body.Errors) != 1 || body.Errors[0].Field != "supervisor_agent_id" {
		t.Fatalf("missing supervisor: %+v", body)
	}
	post(map[string]any{"name": "x", "supervisor_agent_id": "00000000-0000-0000-0000-00000000dead"}).
		Want(http.StatusUnprocessableEntity).JSON(&body)
	if len(body.Errors) != 1 || body.Errors[0].Message != "agent not found in this workspace" {
		t.Fatalf("unknown supervisor: %+v", body)
	}
	post(map[string]any{"name": "x", "supervisor_agent_id": "not-a-uuid"}).
		Want(http.StatusUnprocessableEntity).JSON(&body)
	if len(body.Errors) != 1 || body.Errors[0].Message != "invalid agent id" {
		t.Fatalf("malformed supervisor id: %+v", body)
	}
}

func TestExtWorkflow_UpdateStructureValidation(t *testing.T) {
	requireExtWorkflowDB(t)
	supervisor := createHandlerTestAgent(t, "ext-wf-struct-sup", nil)
	worker := createHandlerTestAgent(t, "ext-wf-struct-worker", nil)
	wf := createExtWorkflowAs(t, "", "Structure", supervisor)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"nodes": []any{extWFNode("keep", worker)}}).Want(http.StatusOK)

	var body extWFValidationBody
	updateExtWorkflow(t, "", wf.ID, map[string]any{
		"nodes": []any{
			extWFNode("a", worker, "b"),
			extWFNode("b", worker, "a"),
			extWFNode("c", worker, "ghost"),
			extWFNode("Bad Key", worker),
		},
	}).Want(http.StatusUnprocessableEntity).JSON(&body)
	if body.Error != "validation_failed" {
		t.Fatalf("error = %q", body.Error)
	}
	seen := map[string]bool{}
	for _, e := range body.Errors {
		seen[e.NodeKey+"/"+e.Field] = true
	}
	for _, key := range []string{"a/depends_on", "b/depends_on", "c/depends_on", "Bad Key/key"} {
		if !seen[key] {
			t.Errorf("missing validation error %q in %+v", key, body.Errors)
		}
	}

	updateExtWorkflow(t, "", wf.ID, map[string]any{"nodes": []any{}}).Want(http.StatusUnprocessableEntity)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"max_rewinds": 11}).Want(http.StatusUnprocessableEntity)
	updateExtWorkflow(t, "", wf.ID, "{not json").Want(http.StatusBadRequest)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"name": " "}).Want(http.StatusBadRequest)

	// Every rejected save left the stored definition untouched.
	var got ExtWorkflowResponse
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq("", "GET", "/", nil, wf.ID)).Want(http.StatusOK).JSON(&got)
	if len(got.Nodes) != 1 || got.Nodes[0].Key != "keep" || got.MaxRewinds != 3 || got.Name != "Structure" {
		t.Fatalf("rejected update mutated the workflow: %+v", got)
	}
}

func TestExtWorkflow_AgentChecks(t *testing.T) {
	requireExtWorkflowDB(t)
	privateAgent, _, memberID := privateAgentTestFixture(t)
	publicSupervisor := createHandlerTestAgent(t, "ext-wf-agent-sup", nil)
	publicWorker := createHandlerTestAgent(t, "ext-wf-agent-worker", nil)
	archivedAgent := createHandlerTestAgent(t, "ext-wf-archived", nil)
	if _, err := testPool.Exec(context.Background(), `UPDATE agent SET archived_at = now() WHERE id = $1`, archivedAgent); err != nil {
		t.Fatalf("archive agent: %v", err)
	}

	// A plain member cannot make a private agent they cannot invoke the supervisor.
	var body extWFValidationBody
	testutil.Call(t, testHandler.CreateExtWorkflow, extWFReq(memberID, "POST", "/api/ext/workflows", map[string]any{
		"name": "Private sup", "supervisor_agent_id": privateAgent,
	}, "")).Want(http.StatusUnprocessableEntity).JSON(&body)
	if len(body.Errors) != 1 || body.Errors[0].Field != "supervisor_agent_id" || body.Errors[0].Message != "you do not have access to this agent" {
		t.Fatalf("private supervisor: %+v", body)
	}

	wf := createExtWorkflowAs(t, memberID, "Agent checks", publicSupervisor)
	updateExtWorkflow(t, memberID, wf.ID, map[string]any{
		"nodes": []any{
			extWFNode("ok", publicWorker),
			extWFNode("private", privateAgent, "ok"),
			extWFNode("gone", archivedAgent, "ok"),
			extWFNode("missing", "00000000-0000-0000-0000-00000000dead", "ok"),
		},
	}).Want(http.StatusUnprocessableEntity).JSON(&body)
	got := map[string]string{}
	for _, e := range body.Errors {
		if e.Field != "agent_id" {
			t.Errorf("unexpected field %q in %+v", e.Field, e)
		}
		got[e.NodeKey] = e.Message
	}
	want := map[string]string{
		"private": "you do not have access to this agent",
		"gone":    "agent is archived",
		"missing": "agent not found in this workspace",
	}
	if len(got) != len(want) {
		t.Fatalf("agent errors = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("agent errors = %v, want %v", got, want)
		}
	}

	// The workspace owner may wire the same private agent (admin wiring is unrestricted).
	updateExtWorkflow(t, "", wf.ID, map[string]any{
		"supervisor_agent_id": privateAgent,
		"nodes":               []any{extWFNode("private", privateAgent)},
	}).Want(http.StatusOK)

	// A rename by the creator is not blocked by the agents wired earlier.
	updateExtWorkflow(t, memberID, wf.ID, map[string]any{"name": "Renamed"}).Want(http.StatusOK)
}

func TestExtWorkflow_Permissions(t *testing.T) {
	requireExtWorkflowDB(t)
	creatorID := createPlainMember(t, "ext-wf-creator@multica.test")
	strangerID := createPlainMember(t, "ext-wf-stranger@multica.test")
	supervisor := createHandlerTestAgent(t, "ext-wf-perm-sup", nil)

	wf := createExtWorkflowAs(t, creatorID, "Perms", supervisor)
	if wf.CreatorID != creatorID {
		t.Fatalf("creator_id = %s, want %s", wf.CreatorID, creatorID)
	}

	updateExtWorkflow(t, strangerID, wf.ID, map[string]any{"name": "Hijack"}).Want(http.StatusForbidden)
	testutil.Call(t, testHandler.DeleteExtWorkflow, extWFReq(strangerID, "DELETE", "/", nil, wf.ID)).Want(http.StatusForbidden)

	// Visible workspace-wide.
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq(strangerID, "GET", "/", nil, wf.ID)).Want(http.StatusOK)

	updateExtWorkflow(t, creatorID, wf.ID, map[string]any{"name": "Mine"}).Want(http.StatusOK)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"name": "Admin edit"}).Want(http.StatusOK)
	testutil.Call(t, testHandler.DeleteExtWorkflow, extWFReq(creatorID, "DELETE", "/", nil, wf.ID)).Want(http.StatusNoContent)
}

func TestExtWorkflow_NotFoundAndBadID(t *testing.T) {
	requireExtWorkflowDB(t)
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq("", "GET", "/", nil, "00000000-0000-0000-0000-00000000dead")).
		Want(http.StatusNotFound)
	testutil.Call(t, testHandler.GetExtWorkflow, extWFReq("", "GET", "/", nil, "not-a-uuid")).
		Want(http.StatusBadRequest)
}

// TestExtWorkflow_WorkspaceTeardownSweepsEveryTable pins the workspace-delete
// hook: the ext tables carry no foreign keys, so DeleteExtWorkflowWorkspaceData
// is the only thing that removes their rows.
func TestExtWorkflow_WorkspaceTeardownSweepsEveryTable(t *testing.T) {
	requireExtWorkflowDB(t)
	ctx := context.Background()

	tables := []string{"ext_workflow", "ext_workflow_node", "ext_workflow_run", "ext_workflow_run_step", "ext_workflow_run_event"}
	seed := func(workspaceID string) {
		t.Helper()
		for _, stmt := range []string{
			`INSERT INTO ext_workflow (workspace_id, name, supervisor_agent_id, creator_id)
			 VALUES ($1, 'teardown', gen_random_uuid(), gen_random_uuid())`,
			`INSERT INTO ext_workflow_node (workflow_id, workspace_id, key, title, agent_id)
			 VALUES (gen_random_uuid(), $1, 'k', 't', gen_random_uuid())`,
			`INSERT INTO ext_workflow_run (workspace_id, workflow_id, issue_id, triggered_by_type, triggered_by_id, definition)
			 VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'member', gen_random_uuid(), '{}'::jsonb)`,
			`INSERT INTO ext_workflow_run_step (run_id, workspace_id, node_key, agent_id, issue_id)
			 VALUES (gen_random_uuid(), $1, 'k', gen_random_uuid(), gen_random_uuid())`,
			`INSERT INTO ext_workflow_run_event (run_id, workspace_id, kind, actor_type)
			 VALUES (gen_random_uuid(), $1, 'run_started', 'engine')`,
		} {
			if _, err := testPool.Exec(ctx, stmt, workspaceID); err != nil {
				t.Fatalf("seed %q: %v", stmt, err)
			}
		}
	}
	var doomed, survivor string
	dbfx.QueryRow(t, `SELECT gen_random_uuid()::text`).Scan(&doomed)
	dbfx.QueryRow(t, `SELECT gen_random_uuid()::text`).Scan(&survivor)
	t.Cleanup(func() {
		for _, table := range tables {
			testPool.Exec(ctx, `DELETE FROM `+table+` WHERE workspace_id = ANY($1::uuid[])`, []string{doomed, survivor})
		}
	})
	seed(doomed)
	seed(survivor)

	doomedUUID, err := util.ParseUUID(doomed)
	if err != nil {
		t.Fatal(err)
	}
	if err := testHandler.Queries.DeleteExtWorkflowWorkspaceData(ctx, doomedUUID); err != nil {
		t.Fatalf("DeleteExtWorkflowWorkspaceData: %v", err)
	}
	for _, table := range tables {
		if n := dbfx.Count(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, doomed); n != 0 {
			t.Errorf("%s: %d rows survived teardown", table, n)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, survivor); n != 1 {
			t.Errorf("%s: other workspace has %d rows, want 1", table, n)
		}
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
# DB tests need the checkout's environment (make up creates .env.worktree / .env and the database).
cd server && set -a && . ../.env && set +a   # use ../.env.worktree in a worktree
go run ./cmd/migrate up
```

```bash
go test ./internal/handler -run 'TestExtWorkflow' -count=1   # FAIL (build): testHandler.CreateExtWorkflow undefined, ExtWorkflowResponse undefined, protocol.EventExtWorkflowCreated undefined
```

- [ ] **Step 3: Implement**

**3a. Protocol events**

```diff
--- a/server/pkg/protocol/events.go
+++ b/server/pkg/protocol/events.go
@@ -142,6 +142,11 @@
 	EventSquadUpdated = "squad:updated"
 	EventSquadDeleted = "squad:deleted"
 
+	// ext-workflow events (fork-only). Payload: {workflow_id}.
+	EventExtWorkflowCreated = "ext_workflow:created"
+	EventExtWorkflowUpdated = "ext_workflow:updated"
+	EventExtWorkflowDeleted = "ext_workflow:deleted"
+
 	// Daemon events
 	EventDaemonHeartbeat               = "daemon:heartbeat"
 	EventDaemonHeartbeatAck            = "daemon:heartbeat_ack"
```

**3b. Handler**

**Create `server/internal/handler/ext_workflow.go`:**

```go
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ext-workflow: workflow template CRUD. Run endpoints live in ext_workflow_run.go.

const (
	extWorkflowDefaultMaxRewinds = 3
	extWorkflowMaxNameRunes      = 200
)

// ── Response types ──────────────────────────────────────────────────────────

type ExtWorkflowNodeResponse struct {
	ID             string   `json:"id"`
	Key            string   `json:"key"`
	Title          string   `json:"title"`
	AgentID        string   `json:"agent_id"`
	Prompt         string   `json:"prompt"`
	RequiresReview bool     `json:"requires_review"`
	MaxAttempts    int      `json:"max_attempts"`
	DependsOn      []string `json:"depends_on"`
	Position       int      `json:"position"`
}

type ExtWorkflowResponse struct {
	ID                string                    `json:"id"`
	WorkspaceID       string                    `json:"workspace_id"`
	Name              string                    `json:"name"`
	Description       string                    `json:"description"`
	SupervisorAgentID string                    `json:"supervisor_agent_id"`
	MaxRewinds        int                       `json:"max_rewinds"`
	CreatorID         string                    `json:"creator_id"`
	AvatarURL         *string                   `json:"avatar_url"`
	ArchivedAt        *string                   `json:"archived_at"`
	CreatedAt         string                    `json:"created_at"`
	UpdatedAt         string                    `json:"updated_at"`
	NodeCount         int                       `json:"node_count"`
	ActiveRunCount    int                       `json:"active_run_count"`
	LastRunAt         *string                   `json:"last_run_at"`
	Nodes             []ExtWorkflowNodeResponse `json:"nodes,omitempty"`
}

// ── Converters ──────────────────────────────────────────────────────────────

func (h *Handler) extWorkflowToResponse(wf db.ExtWorkflow) ExtWorkflowResponse {
	return ExtWorkflowResponse{
		ID:                uuidToString(wf.ID),
		WorkspaceID:       uuidToString(wf.WorkspaceID),
		Name:              wf.Name,
		Description:       wf.Description,
		SupervisorAgentID: uuidToString(wf.SupervisorAgentID),
		MaxRewinds:        int(wf.MaxRewinds),
		CreatorID:         uuidToString(wf.CreatorID),
		AvatarURL:         h.resolveAvatarURLPtr(textToPtr(wf.AvatarUrl)),
		ArchivedAt:        timestampToPtr(wf.ArchivedAt),
		CreatedAt:         timestampToString(wf.CreatedAt),
		UpdatedAt:         timestampToString(wf.UpdatedAt),
	}
}

func extWorkflowNodeToResponse(n db.ExtWorkflowNode) ExtWorkflowNodeResponse {
	deps := n.DependsOn
	if deps == nil {
		deps = []string{}
	}
	return ExtWorkflowNodeResponse{
		ID:             uuidToString(n.ID),
		Key:            n.Key,
		Title:          n.Title,
		AgentID:        uuidToString(n.AgentID),
		Prompt:         n.Prompt,
		RequiresReview: n.RequiresReview,
		MaxAttempts:    int(n.MaxAttempts),
		DependsOn:      deps,
		Position:       int(n.Position),
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// canManageExtWorkflow mirrors canManageSquad: workspace owner/admin manage
// every workflow; a regular member manages only the workflows they created.
func canManageExtWorkflow(member db.Member, wf db.ExtWorkflow) bool {
	if roleAllowed(member.Role, "owner", "admin") {
		return true
	}
	return uuidToString(wf.CreatorID) == uuidToString(member.UserID)
}

func (h *Handler) loadExtWorkflowInWorkspace(w http.ResponseWriter, r *http.Request) (db.ExtWorkflow, bool) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	wfUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workflow id")
	if !ok {
		return db.ExtWorkflow{}, false
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return db.ExtWorkflow{}, false
	}
	wf, err := h.Queries.GetExtWorkflowInWorkspace(r.Context(), db.GetExtWorkflowInWorkspaceParams{
		ID:          wfUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "workflow not found")
		return db.ExtWorkflow{}, false
	}
	return wf, true
}

func writeExtWorkflowValidationFailed(w http.ResponseWriter, errs []extworkflow.ValidationError) {
	if errs == nil {
		errs = []extworkflow.ValidationError{}
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":  "validation_failed",
		"errors": errs,
	})
}

// extAgentChecker resolves and vets the agents a workflow definition refers to.
// Results are cached per agent id so a definition that reuses one agent across
// many nodes costs one lookup.
type extAgentChecker struct {
	h           *Handler
	ctx         context.Context
	member      db.Member
	workspaceID string
	wsUUID      pgtype.UUID
	cache       map[string]string // agent id -> problem ("" = ok)
}

func (h *Handler) newExtAgentChecker(ctx context.Context, member db.Member, workspaceID string, wsUUID pgtype.UUID) *extAgentChecker {
	return &extAgentChecker{h: h, ctx: ctx, member: member, workspaceID: workspaceID, wsUUID: wsUUID, cache: map[string]string{}}
}

// problem returns "" when the member may wire agentID into a workflow, and a
// user-facing message otherwise. Workspace owner/admin may wire any workspace
// agent; other members only agents they can invoke (memberCanWireAgent).
func (c *extAgentChecker) problem(agentID string) string {
	if msg, ok := c.cache[agentID]; ok {
		return msg
	}
	msg := c.lookup(agentID)
	c.cache[agentID] = msg
	return msg
}

func (c *extAgentChecker) lookup(agentID string) string {
	agentUUID, err := util.ParseUUID(agentID)
	if err != nil {
		return "invalid agent id"
	}
	agent, err := c.h.Queries.GetAgentInWorkspace(c.ctx, db.GetAgentInWorkspaceParams{
		ID:          agentUUID,
		WorkspaceID: c.wsUUID,
	})
	if err != nil {
		return "agent not found in this workspace"
	}
	if agent.ArchivedAt.Valid {
		return "agent is archived"
	}
	if !c.h.memberCanWireAgent(c.ctx, c.member, agent, c.workspaceID) {
		return "you do not have access to this agent"
	}
	return ""
}

// check returns the agent problems of a definition: the supervisor (field
// supervisor_agent_id) and, when withNodes is set, every node agent (field
// agent_id on that node). Blank ids are left to extworkflow.ValidateStructure.
func (c *extAgentChecker) check(d extworkflow.Definition, withNodes bool) []extworkflow.ValidationError {
	var errs []extworkflow.ValidationError
	if d.SupervisorAgentID != "" {
		if msg := c.problem(d.SupervisorAgentID); msg != "" {
			errs = append(errs, extworkflow.ValidationError{Field: "supervisor_agent_id", Message: msg})
		}
	}
	if withNodes {
		for _, n := range d.Nodes {
			if n.AgentID == "" {
				continue
			}
			if msg := c.problem(n.AgentID); msg != "" {
				errs = append(errs, extworkflow.ValidationError{NodeKey: n.Key, Field: "agent_id", Message: msg})
			}
		}
	}
	return errs
}

func (h *Handler) loadExtWorkflowNodes(ctx context.Context, q *db.Queries, workflowID pgtype.UUID) ([]ExtWorkflowNodeResponse, error) {
	rows, err := q.ListExtWorkflowNodes(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	out := make([]ExtWorkflowNodeResponse, len(rows))
	for i, n := range rows {
		out[i] = extWorkflowNodeToResponse(n)
	}
	return out, nil
}

// extWorkflowDetailResponse is the single-workflow shape: the row, its nodes
// and its live counters.
func (h *Handler) extWorkflowDetailResponse(ctx context.Context, wf db.ExtWorkflow) (ExtWorkflowResponse, error) {
	resp := h.extWorkflowToResponse(wf)
	nodes, err := h.loadExtWorkflowNodes(ctx, h.Queries, wf.ID)
	if err != nil {
		return resp, err
	}
	resp.Nodes = nodes
	resp.NodeCount = len(nodes)
	active, err := h.Queries.CountActiveExtWorkflowRuns(ctx, wf.ID)
	if err != nil {
		return resp, err
	}
	resp.ActiveRunCount = int(active)
	return resp, nil
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (h *Handler) ListExtWorkflows(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	rows, err := h.Queries.ListExtWorkflows(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list workflows")
		return
	}
	resp := make([]ExtWorkflowResponse, len(rows))
	for i, row := range rows {
		resp[i] = h.extWorkflowToResponse(db.ExtWorkflow{
			ID:                row.ID,
			WorkspaceID:       row.WorkspaceID,
			Name:              row.Name,
			Description:       row.Description,
			SupervisorAgentID: row.SupervisorAgentID,
			MaxRewinds:        row.MaxRewinds,
			CreatorID:         row.CreatorID,
			AvatarUrl:         row.AvatarUrl,
			ArchivedAt:        row.ArchivedAt,
			ArchivedBy:        row.ArchivedBy,
			CreatedAt:         row.CreatedAt,
			UpdatedAt:         row.UpdatedAt,
		})
		resp[i].NodeCount = int(row.NodeCount)
		resp[i].ActiveRunCount = int(row.ActiveRunCount)
		resp[i].LastRunAt = timestampToPtr(row.LastRunAt)
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": resp})
}

func (h *Handler) CreateExtWorkflow(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	// Any workspace member can create a workflow and becomes its creator;
	// management stays creator-scoped (see canManageExtWorkflow).
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}

	var req struct {
		Name              string `json:"name"`
		Description       string `json:"description"`
		SupervisorAgentID string `json:"supervisor_agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len([]rune(name)) > extWorkflowMaxNameRunes {
		writeError(w, http.StatusBadRequest, "name is too long")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	d := extworkflow.Definition{SupervisorAgentID: req.SupervisorAgentID, MaxRewinds: extWorkflowDefaultMaxRewinds}
	errs := extworkflow.ValidateSettings(d.SupervisorAgentID, d.MaxRewinds)
	if len(errs) == 0 {
		errs = h.newExtAgentChecker(r.Context(), member, workspaceID, wsUUID).check(d, false)
	}
	if len(errs) > 0 {
		writeExtWorkflowValidationFailed(w, errs)
		return
	}
	supervisorUUID, ok := parseUUIDOrBadRequest(w, req.SupervisorAgentID, "supervisor_agent_id")
	if !ok {
		return
	}

	wf, err := h.Queries.CreateExtWorkflow(r.Context(), db.CreateExtWorkflowParams{
		WorkspaceID:       wsUUID,
		Name:              name,
		Description:       req.Description,
		SupervisorAgentID: supervisorUUID,
		MaxRewinds:        extWorkflowDefaultMaxRewinds,
		CreatorID:         member.UserID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create workflow")
		return
	}

	h.publish(protocol.EventExtWorkflowCreated, workspaceID, "member", uuidToString(member.UserID), map[string]any{
		"workflow_id": uuidToString(wf.ID),
	})
	writeJSON(w, http.StatusCreated, h.extWorkflowToResponse(wf))
}

func (h *Handler) GetExtWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := h.loadExtWorkflowInWorkspace(w, r)
	if !ok {
		return
	}
	resp, err := h.extWorkflowDetailResponse(r.Context(), wf)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workflow")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type extWorkflowNodeRequest struct {
	Key            string   `json:"key"`
	Title          string   `json:"title"`
	AgentID        string   `json:"agent_id"`
	Prompt         string   `json:"prompt"`
	RequiresReview bool     `json:"requires_review"`
	MaxAttempts    int      `json:"max_attempts"`
	DependsOn      []string `json:"depends_on"`
}

func (h *Handler) UpdateExtWorkflow(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}
	wf, ok := h.loadExtWorkflowInWorkspace(w, r)
	if !ok {
		return
	}
	if !canManageExtWorkflow(member, wf) {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	var req struct {
		Name              *string                   `json:"name"`
		Description       *string                   `json:"description"`
		SupervisorAgentID *string                   `json:"supervisor_agent_id"`
		MaxRewinds        *int                      `json:"max_rewinds"`
		Nodes             *[]extWorkflowNodeRequest `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	params := db.UpdateExtWorkflowParams{ID: wf.ID}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		if len([]rune(name)) > extWorkflowMaxNameRunes {
			writeError(w, http.StatusBadRequest, "name is too long")
			return
		}
		params.Name = pgtype.Text{String: name, Valid: true}
	}
	if req.Description != nil {
		params.Description = pgtype.Text{String: *req.Description, Valid: true}
	}

	// Effective definition after the update; unspecified fields keep their
	// stored value.
	d := extworkflow.Definition{
		SupervisorAgentID: uuidToString(wf.SupervisorAgentID),
		MaxRewinds:        int(wf.MaxRewinds),
	}
	if req.SupervisorAgentID != nil {
		d.SupervisorAgentID = *req.SupervisorAgentID
	}
	if req.MaxRewinds != nil {
		d.MaxRewinds = *req.MaxRewinds
	}
	if req.Nodes != nil {
		d.Nodes = make([]extworkflow.Node, len(*req.Nodes))
		for i, n := range *req.Nodes {
			deps := n.DependsOn
			if deps == nil {
				deps = []string{}
			}
			d.Nodes[i] = extworkflow.Node{
				Key: n.Key, Title: n.Title, AgentID: n.AgentID, Prompt: n.Prompt,
				RequiresReview: n.RequiresReview, MaxAttempts: n.MaxAttempts, DependsOn: deps,
			}
		}
	}

	var errs []extworkflow.ValidationError
	if req.Nodes != nil {
		errs = extworkflow.ValidateStructure(d)
	} else {
		errs = extworkflow.ValidateSettings(d.SupervisorAgentID, d.MaxRewinds)
	}
	// Agent checks only for what this request changes, so renaming a workflow
	// is never blocked by a node agent that was later made private.
	if req.SupervisorAgentID != nil || req.Nodes != nil {
		checkDef := extworkflow.Definition{Nodes: d.Nodes}
		if req.SupervisorAgentID != nil {
			checkDef.SupervisorAgentID = d.SupervisorAgentID
		}
		errs = append(errs, h.newExtAgentChecker(r.Context(), member, workspaceID, wsUUID).check(checkDef, req.Nodes != nil)...)
	}
	if len(errs) > 0 {
		writeExtWorkflowValidationFailed(w, errs)
		return
	}
	if req.SupervisorAgentID != nil {
		supervisorUUID, ok := parseUUIDOrBadRequest(w, *req.SupervisorAgentID, "supervisor_agent_id")
		if !ok {
			return
		}
		params.SupervisorAgentID = supervisorUUID
	}
	if req.MaxRewinds != nil {
		params.MaxRewinds = pgtype.Int4{Int32: int32(*req.MaxRewinds), Valid: true}
	}

	// Parse node agent ids before opening the transaction so a malformed id
	// cannot leave it half applied.
	type nodeWrite struct {
		node  extworkflow.Node
		agent pgtype.UUID
	}
	var writes []nodeWrite
	if req.Nodes != nil {
		writes = make([]nodeWrite, len(d.Nodes))
		for i, n := range d.Nodes {
			agentUUID, ok := parseUUIDOrBadRequest(w, n.AgentID, "agent_id")
			if !ok {
				return
			}
			writes[i] = nodeWrite{node: n, agent: agentUUID}
		}
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update workflow")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	// Serialize concurrent saves of the same workflow: the node set is replaced
	// as a whole, so two interleaved replacements could mix their rows.
	locked, err := qtx.LockExtWorkflowForUpdate(r.Context(), db.LockExtWorkflowForUpdateParams{
		ID:          wf.ID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update workflow")
		return
	}
	if locked.ArchivedAt.Valid {
		writeError(w, http.StatusConflict, "workflow is archived")
		return
	}
	if _, err := qtx.UpdateExtWorkflow(r.Context(), params); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update workflow")
		return
	}
	if req.Nodes != nil {
		if err := qtx.DeleteExtWorkflowNodes(r.Context(), wf.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update workflow")
			return
		}
		for i, nw := range writes {
			if _, err := qtx.CreateExtWorkflowNode(r.Context(), db.CreateExtWorkflowNodeParams{
				WorkflowID:     wf.ID,
				WorkspaceID:    wsUUID,
				Key:            nw.node.Key,
				Title:          strings.TrimSpace(nw.node.Title),
				AgentID:        nw.agent,
				Prompt:         nw.node.Prompt,
				RequiresReview: nw.node.RequiresReview,
				MaxAttempts:    int32(nw.node.MaxAttempts),
				DependsOn:      nw.node.DependsOn,
				Position:       int32(i),
			}); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to update workflow")
				return
			}
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update workflow")
		return
	}

	updated, err := h.Queries.GetExtWorkflowInWorkspace(r.Context(), db.GetExtWorkflowInWorkspaceParams{ID: wf.ID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workflow")
		return
	}
	resp, err := h.extWorkflowDetailResponse(r.Context(), updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workflow")
		return
	}
	h.publish(protocol.EventExtWorkflowUpdated, workspaceID, "member", requestUserID(r), map[string]any{
		"workflow_id": uuidToString(wf.ID),
	})
	writeJSON(w, http.StatusOK, resp)
}

// DeleteExtWorkflow archives the workflow. Active runs keep running against
// their snapshot; the workflow just stops accepting new assignments.
func (h *Handler) DeleteExtWorkflow(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}
	wf, ok := h.loadExtWorkflowInWorkspace(w, r)
	if !ok {
		return
	}
	if !canManageExtWorkflow(member, wf) {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return
	}
	if wf.ArchivedAt.Valid {
		writeError(w, http.StatusBadRequest, "workflow is already archived")
		return
	}
	if _, err := h.Queries.ArchiveExtWorkflow(r.Context(), db.ArchiveExtWorkflowParams{
		ID:         wf.ID,
		ArchivedBy: member.UserID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to archive workflow")
		return
	}
	h.publish(protocol.EventExtWorkflowDeleted, workspaceID, "member", uuidToString(member.UserID), map[string]any{
		"workflow_id": uuidToString(wf.ID),
	})
	w.WriteHeader(http.StatusNoContent)
}
```

**3c. Routes**

**Create `server/cmd/server/ext_routes.go`:**

```go
package main

import (
	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/handler"
)

// registerExtRoutes mounts the fork-only /api/ext/* routes. They live in this
// file so router.go needs a single hook line, and an upstream merge of
// router.go never conflicts with them. Call it inside the workspace-scoped,
// authenticated route group.
func registerExtRoutes(r chi.Router, h *handler.Handler) {
	r.Route("/api/ext/workflows", func(r chi.Router) {
		r.Get("/", h.ListExtWorkflows)
		r.Post("/", h.CreateExtWorkflow)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.GetExtWorkflow)
			r.Put("/", h.UpdateExtWorkflow)
			r.Delete("/", h.DeleteExtWorkflow)
		})
	})
}
```

```diff
--- a/server/cmd/server/router.go
+++ b/server/cmd/server/router.go
@@ -2155,6 +2155,9 @@
 			// Squad leader evaluation (writes to activity_log)
 			r.Post("/api/issues/{id}/squad-evaluated", h.RecordSquadLeaderEvaluation)
 
+			// ext-workflow: fork-only routes (workflow CRUD, runs); see ext_routes.go.
+			registerExtRoutes(r, h)
+
 			// Autopilots
 			r.Route("/api/autopilots", func(r chi.Router) {
 				r.Get("/", h.ListAutopilots)
```

**3d. Workspace teardown + manifest** (required: `TestWorkspaceDeletionManifestCoversPublicSchema` fails for any new table; the `DeleteExtWorkflowWorkspaceData` query was added in A2)

```diff
--- a/server/internal/handler/workspace.go
+++ b/server/internal/handler/workspace.go
@@ -1309,6 +1309,11 @@
 			run:  func() error { return qtx.DeleteWorkspaceSquadsAndSkills(ctx, requester.WorkspaceID) },
 		},
 		{
+			// ext-workflow: no foreign keys, so the fork's workflow tables are swept explicitly.
+			name: "delete ext workflows",
+			run:  func() error { return qtx.DeleteExtWorkflowWorkspaceData(ctx, requester.WorkspaceID) },
+		},
+		{
 			name: "delete plugin data",
 			run:  func() error { return qtx.DeleteWorkspacePluginData(ctx, requester.WorkspaceID) },
 		},
```

```diff
--- a/server/internal/handler/workspace_delete_manifest_test.go
+++ b/server/internal/handler/workspace_delete_manifest_test.go
@@ -143,6 +143,13 @@
 	"workspace":                          workspaceDelete,
 	"workspace_invitation":               workspaceDelete,
 	"workspace_share_link":               workspaceDelete,
+
+	// ext-workflow: fork-only tables, swept by DeleteExtWorkflowWorkspaceData.
+	"ext_workflow":           workspaceDelete,
+	"ext_workflow_node":      workspaceDelete,
+	"ext_workflow_run":       workspaceDelete,
+	"ext_workflow_run_event": workspaceDelete,
+	"ext_workflow_run_step":  workspaceDelete,
 }
 
 func TestWorkspaceDeletionManifestCoversPublicSchema(t *testing.T) {
```

- [ ] **Step 4: Run, expect PASS**

```bash
cd server && go build ./... && go vet ./internal/handler ./cmd/server
go test ./internal/handler -run 'TestExtWorkflow|TestWorkspaceDeletionManifest' -count=1 -v | grep -E '^(---|ok|FAIL)'   # 8 PASS
go test ./cmd/server ./internal/service -count=1
```

- [ ] **Step 5: Commit**

```bash
git add server/internal/handler/ext_workflow.go server/internal/handler/ext_workflow_test.go server/cmd/server/ext_routes.go server/cmd/server/router.go server/pkg/protocol/events.go server/internal/handler/workspace.go server/internal/handler/workspace_delete_manifest_test.go
git commit -m "feat(ext-workflow): add workflow CRUD API, events and workspace teardown

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


### Task A5: Core package `packages/core/ext-workflows` and ApiClient methods

**Files:**
- Create: `packages/core/ext-workflows/{types.ts,errors.ts,schemas.ts,queries.ts,mutations.ts,index.ts}`, `packages/core/ext-workflows/{schemas.test.ts,client.test.ts,mutations.test.tsx}`
- Modify: `packages/core/api/client.ts` (import block after line 4 `import { WorkspaceWakeupPageSchema, ... } from "./schemas";`; methods block at the very end of the `ApiClient` class, before its closing `}`)
- Modify: `packages/core/package.json` (`exports`, after `"./squads/stores"`)

**Interfaces:** Produces everything in contract "packages/core/ext-workflows". `ApiClient`: `listExtWorkflows(): Promise<ExtWorkflow[]>`, `getExtWorkflow(id)`, `createExtWorkflow(data: CreateExtWorkflowRequest)`, `updateExtWorkflow(id, data: UpdateExtWorkflowRequest)` (throws `ExtWorkflowValidationFailed` on 422 `validation_failed`), `archiveExtWorkflow(id): Promise<void>`, `listExtWorkflowRuns(id, params?: {limit?, offset?}): Promise<ExtWorkflowRunList>`, `getIssueExtWorkflowRuns(issueId): Promise<ExtWorkflowIssueRuns>`, `getExtWorkflowRun(runId): Promise<ExtWorkflowRun>`, `cancelExtWorkflowRun(runId): Promise<void>`, `decideExtWorkflowStep(runId, stepId, body: DecideExtWorkflowStepRequest): Promise<ExtWorkflowRun>`. Hooks: `useCreateExtWorkflow(wsId)`, `useUpdateExtWorkflow(wsId)`, `useArchiveExtWorkflow(wsId)`, `useDecideExtWorkflowStep(wsId)`, `useCancelExtWorkflowRun(wsId)`. Consumes `parseWithFallback`, `ApiError` (has `.status`, `.body`).

- [ ] **Step 1: Write the failing tests**

**Create `packages/core/ext-workflows/schemas.test.ts`:**

```ts
import { describe, expect, it } from "vitest";
import {
  ExtWorkflowIssueRunsSchema,
  ExtWorkflowListSchema,
  ExtWorkflowRunListSchema,
  ExtWorkflowRunSchema,
  ExtWorkflowSchema,
  ExtWorkflowValidationBodySchema,
} from "./schemas";

const baseWorkflow = {
  id: "wf-1",
  workspace_id: "ws-1",
  name: "Release",
  supervisor_agent_id: "agent-1",
  creator_id: "user-1",
  created_at: "2026-10-07T00:00:00Z",
  updated_at: "2026-10-07T00:00:00Z",
};

const baseRun = {
  id: "run-1",
  workflow_id: "wf-1",
  issue_id: "issue-1",
};

describe("ExtWorkflowSchema drift tolerance", () => {
  it("defaults every optional field when an older backend omits it", () => {
    const parsed = ExtWorkflowSchema.parse(baseWorkflow);
    expect(parsed.description).toBe("");
    expect(parsed.max_rewinds).toBe(3);
    expect(parsed.avatar_url).toBeNull();
    expect(parsed.archived_at).toBeNull();
    expect(parsed.last_run_at).toBeNull();
    expect(parsed.node_count).toBe(0);
    expect(parsed.active_run_count).toBe(0);
    expect(parsed.nodes).toEqual([]);
  });

  it("treats explicit nulls like missing values", () => {
    const parsed = ExtWorkflowSchema.parse({
      ...baseWorkflow,
      avatar_url: null,
      archived_at: null,
      last_run_at: null,
      nodes: null,
    });
    expect(parsed.nodes).toEqual([]);
    expect(parsed.avatar_url).toBeNull();
  });

  it("keeps unknown extra fields", () => {
    const parsed = ExtWorkflowSchema.parse({ ...baseWorkflow, future_field: 1 }) as Record<string, unknown>;
    expect(parsed.future_field).toBe(1);
  });

  it("normalises node depends_on null to an empty list and defaults node fields", () => {
    const parsed = ExtWorkflowSchema.parse({
      ...baseWorkflow,
      nodes: [{ id: "n1", key: "build", agent_id: "agent-2", depends_on: null }],
    });
    expect(parsed.nodes[0]).toMatchObject({
      key: "build",
      title: "",
      prompt: "",
      requires_review: false,
      max_attempts: 3,
      depends_on: [],
      position: 0,
    });
  });

  it("rejects a response without the identity fields so the caller falls back", () => {
    expect(ExtWorkflowSchema.safeParse({ name: "no id" }).success).toBe(false);
    expect(ExtWorkflowSchema.safeParse("not an object").success).toBe(false);
  });
});

describe("ExtWorkflowListSchema", () => {
  it("defaults a missing workflows array", () => {
    expect(ExtWorkflowListSchema.parse({}).workflows).toEqual([]);
  });

  it("rejects a bare array (the old squad-style shape) so the caller falls back", () => {
    expect(ExtWorkflowListSchema.safeParse([baseWorkflow]).success).toBe(false);
  });
});

describe("ExtWorkflowRunSchema enum fallbacks", () => {
  it("maps an unknown run status and unknown step status to 'unknown'", () => {
    const parsed = ExtWorkflowRunSchema.parse({
      ...baseRun,
      status: "paused_by_future_feature",
      steps: [{ id: "s1", node_key: "a", status: "teleporting" }],
    });
    expect(parsed.status).toBe("unknown");
    expect(parsed.steps[0]?.status).toBe("unknown");
  });

  it("maps a missing or non-string status to 'unknown'", () => {
    expect(ExtWorkflowRunSchema.parse({ ...baseRun }).status).toBe("unknown");
    expect(ExtWorkflowRunSchema.parse({ ...baseRun, status: 7 }).status).toBe("unknown");
  });

  it("passes known statuses through", () => {
    const parsed = ExtWorkflowRunSchema.parse({
      ...baseRun,
      status: "waiting_human",
      steps: [{ id: "s1", node_key: "a", status: "awaiting_human" }],
    });
    expect(parsed.status).toBe("waiting_human");
    expect(parsed.steps[0]?.status).toBe("awaiting_human");
  });

  it("defaults steps, events, and nullable step fields", () => {
    const parsed = ExtWorkflowRunSchema.parse({ ...baseRun, status: "running", steps: null, events: undefined });
    expect(parsed.steps).toEqual([]);
    expect(parsed.events).toEqual([]);
    expect(parsed.finished_at).toBeNull();

    const withStep = ExtWorkflowRunSchema.parse({
      ...baseRun,
      status: "running",
      steps: [{ id: "s1", node_key: "a", status: "pending" }],
      events: [{ id: "e1", kind: "run_started", payload: null }],
    });
    expect(withStep.steps[0]).toMatchObject({
      attempts: 0,
      max_attempts: 1,
      depends_on: [],
      pending_reason: null,
      last_feedback: null,
      escalation_reason: null,
      started_at: null,
      finished_at: null,
    });
    expect(withStep.events[0]).toMatchObject({ step_id: null, actor_id: null, on_behalf_of: null, payload: {} });
  });
});

describe("run list and issue runs schemas", () => {
  it("defaults list fields", () => {
    expect(ExtWorkflowRunListSchema.parse({})).toEqual({ runs: [], total: 0 });
  });

  it("defaults step_of to null and keeps a child issue's step_of", () => {
    expect(ExtWorkflowIssueRunsSchema.parse({ runs: [] }).step_of).toBeNull();
    const parsed = ExtWorkflowIssueRunsSchema.parse({
      runs: [{ ...baseRun, status: "running" }],
      step_of: { run_id: "run-1", step_id: "s1", parent_issue_id: "p1", node_key: "a", index: 1, total: 3 },
    });
    expect(parsed.step_of).toMatchObject({ index: 1, total: 3, parent_issue_id: "p1" });
  });
});

describe("ExtWorkflowValidationBodySchema", () => {
  it("parses the 422 body and defaults missing parts", () => {
    const parsed = ExtWorkflowValidationBodySchema.parse({
      error: "validation_failed",
      errors: [{ node_key: "a", field: "depends_on", message: "cycle" }, { field: "nodes" }],
    });
    expect(parsed.errors[0]).toMatchObject({ node_key: "a", field: "depends_on", message: "cycle" });
    expect(parsed.errors[1]).toMatchObject({ field: "nodes", message: "" });
  });

  it("does not match other error bodies", () => {
    expect(ExtWorkflowValidationBodySchema.safeParse({ error: "forbidden" }).success).toBe(false);
  });
});
```

**Create `packages/core/ext-workflows/client.test.ts`:**

```ts
// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient, ApiError } from "../api/client";
import { ExtWorkflowValidationFailed } from "./errors";

afterEach(() => vi.unstubAllGlobals());

function respond(body: unknown, status = 200) {
  const fetch = vi.fn().mockImplementation(
    async () => new Response(status === 204 ? null : JSON.stringify(body), { status }),
  );
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

const workflow = {
  id: "wf-1",
  workspace_id: "ws-1",
  name: "Release",
  supervisor_agent_id: "agent-1",
  creator_id: "user-1",
  created_at: "2026-10-07T00:00:00Z",
  updated_at: "2026-10-07T00:00:00Z",
};

const api = () => new ApiClient("https://api.example.test");

function lastCall(fetch: ReturnType<typeof vi.fn>) {
  const [url, init] = fetch.mock.calls.at(-1)!;
  return { url: new URL(url as string), init: init as RequestInit };
}

describe("ext workflow API client", () => {
  it("unwraps the workflow list and applies defaults", async () => {
    const fetch = respond({ workflows: [workflow] });
    const list = await api().listExtWorkflows();
    expect(lastCall(fetch).url.pathname).toBe("/api/ext/workflows");
    expect(list).toHaveLength(1);
    expect(list[0]).toMatchObject({ id: "wf-1", node_count: 0, nodes: [], max_rewinds: 3 });
  });

  it.each([[[]], [{ workflows: "wrong" }], ["nope"], [null]])(
    "falls back to an empty list on a malformed list response: %j",
    async (body) => {
      respond(body);
      await expect(api().listExtWorkflows()).resolves.toEqual([]);
    },
  );

  it("falls back to an empty workflow when the detail response lacks an id", async () => {
    respond({ name: "no id" });
    await expect(api().getExtWorkflow("wf-1")).resolves.toMatchObject({ id: "", nodes: [] });
  });

  it("creates with POST and parses the response", async () => {
    const fetch = respond(workflow, 201);
    const created = await api().createExtWorkflow({ name: "Release", supervisor_agent_id: "agent-1" });
    const { url, init } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflows");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ name: "Release", supervisor_agent_id: "agent-1" });
    expect(created.id).toBe("wf-1");
  });

  it("sends the node set with PUT", async () => {
    const fetch = respond({ ...workflow, nodes: [] });
    await api().updateExtWorkflow("wf-1", {
      max_rewinds: 2,
      nodes: [
        { key: "a", title: "A", agent_id: "agent-2", prompt: "", requires_review: false, max_attempts: 3, depends_on: [] },
      ],
    });
    const { url, init } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflows/wf-1");
    expect(init.method).toBe("PUT");
    expect(JSON.parse(init.body as string).nodes[0].key).toBe("a");
  });

  it("throws ExtWorkflowValidationFailed with typed errors on a 422", async () => {
    respond(
      {
        error: "validation_failed",
        errors: [{ node_key: "a", field: "depends_on", message: "node is part of a dependency cycle" }],
      },
      422,
    );
    const err = await api().updateExtWorkflow("wf-1", { nodes: [] }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ExtWorkflowValidationFailed);
    expect((err as ExtWorkflowValidationFailed).errors).toEqual([
      { node_key: "a", field: "depends_on", message: "node is part of a dependency cycle" },
    ]);
  });

  it("keeps a 422 with an unrecognised body as a plain ApiError", async () => {
    respond({ error: "something_else" }, 422);
    await expect(api().updateExtWorkflow("wf-1", { name: "x" })).rejects.toBeInstanceOf(ApiError);
  });

  it("does not convert other failures", async () => {
    respond({ error: "insufficient permissions" }, 403);
    const err = await api().updateExtWorkflow("wf-1", { name: "x" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(403);
  });

  it("archives with DELETE", async () => {
    const fetch = respond(null, 204);
    await expect(api().archiveExtWorkflow("wf-1")).resolves.toBeUndefined();
    const { url, init } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflows/wf-1");
    expect(init.method).toBe("DELETE");
  });

  it("passes pagination to the run history and parses it", async () => {
    const fetch = respond({ runs: [{ id: "r1", workflow_id: "wf-1", issue_id: "i1", status: "running" }], total: 1 });
    const page = await api().listExtWorkflowRuns("wf-1", { limit: 20, offset: 40 });
    const { url } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflows/wf-1/runs");
    expect(Object.fromEntries(url.searchParams)).toEqual({ limit: "20", offset: "40" });
    expect(page.total).toBe(1);
    expect(page.runs[0]?.status).toBe("running");
  });

  it("queries issue runs by issue id and defaults step_of to null", async () => {
    const fetch = respond({ runs: [] });
    const result = await api().getIssueExtWorkflowRuns("issue 1");
    expect(lastCall(fetch).url.search).toBe("?issue_id=issue%201");
    expect(result).toEqual({ runs: [], step_of: null });
  });

  it("maps an unknown run status to 'unknown' on the run detail", async () => {
    respond({ id: "r1", workflow_id: "wf-1", issue_id: "i1", status: "paused_by_future", steps: [{ id: "s1", node_key: "a", status: "warp" }] });
    const run = await api().getExtWorkflowRun("r1");
    expect(run.status).toBe("unknown");
    expect(run.steps[0]?.status).toBe("unknown");
  });

  it("posts a decision with expected_status and parses the returned run", async () => {
    const fetch = respond({ id: "r1", workflow_id: "wf-1", issue_id: "i1", status: "running" });
    const run = await api().decideExtWorkflowStep("r1", "s1", { action: "approve", expected_status: "awaiting_human" });
    const { url, init } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflow-runs/r1/steps/s1/decision");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ action: "approve", expected_status: "awaiting_human" });
    expect(run.status).toBe("running");
  });

  it("surfaces a 409 status mismatch as an ApiError", async () => {
    respond({ error: "status mismatch" }, 409);
    const err = await api()
      .decideExtWorkflowStep("r1", "s1", { action: "approve", expected_status: "awaiting_human" })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(409);
  });

  it("cancels a run with POST", async () => {
    const fetch = respond(null, 204);
    await expect(api().cancelExtWorkflowRun("r1")).resolves.toBeUndefined();
    const { url, init } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflow-runs/r1/cancel");
    expect(init.method).toBe("POST");
  });
});
```

**Create `packages/core/ext-workflows/mutations.test.tsx`:**

```tsx
/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, act } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../api/client";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { ExtWorkflowValidationFailed } from "./errors";
import { useCreateExtWorkflow, useDecideExtWorkflowStep, useUpdateExtWorkflow } from "./mutations";
import { extWorkflowKeys } from "./queries";

const wrapper = (qc: QueryClient) =>
  function W({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };

afterEach(() => vi.restoreAllMocks());

describe("ext workflow mutations", () => {
  it("create invalidates the list for the given workspace", async () => {
    const qc = new QueryClient();
    qc.setQueryData(extWorkflowKeys.list("ws-1"), []);
    setApiInstance({ createExtWorkflow: vi.fn().mockResolvedValue({ id: "wf-1" }) } as unknown as ApiClient);
    const { result } = renderHook(() => useCreateExtWorkflow("ws-1"), { wrapper: wrapper(qc) });
    await act(() => result.current.mutateAsync({ name: "x", supervisor_agent_id: "a" }));
    expect(qc.getQueryState(extWorkflowKeys.list("ws-1"))?.isInvalidated).toBe(true);
  });

  it("update seeds the detail cache and rejects with ExtWorkflowValidationFailed", async () => {
    const qc = new QueryClient();
    const updateExtWorkflow = vi
      .fn()
      .mockResolvedValueOnce({ id: "wf-1", name: "n", nodes: [] })
      .mockRejectedValueOnce(new ExtWorkflowValidationFailed([{ field: "nodes", message: "bad" }]));
    setApiInstance({ updateExtWorkflow } as unknown as ApiClient);
    const { result } = renderHook(() => useUpdateExtWorkflow("ws-1"), { wrapper: wrapper(qc) });
    await act(() => result.current.mutateAsync({ id: "wf-1", name: "n" }));
    expect(qc.getQueryData(extWorkflowKeys.detail("ws-1", "wf-1"))).toMatchObject({ id: "wf-1" });
    await expect(act(() => result.current.mutateAsync({ id: "wf-1", nodes: [] }))).rejects.toMatchObject({
      errors: [{ field: "nodes", message: "bad" }],
    });
  });

  it("decide exposes the HTTP status on failure and invalidates run caches", async () => {
    const qc = new QueryClient();
    qc.setQueryData(extWorkflowKeys.run("ws-1", "r1"), {});
    setApiInstance({
      decideExtWorkflowStep: vi.fn().mockRejectedValue(new ApiError("mismatch", 409, "Conflict")),
    } as unknown as ApiClient);
    const { result } = renderHook(() => useDecideExtWorkflowStep("ws-1"), { wrapper: wrapper(qc) });
    await expect(
      act(() => result.current.mutateAsync({ runId: "r1", stepId: "s1", action: "approve", expected_status: "awaiting_human" })),
    ).rejects.toMatchObject({ status: 409 });
    expect(qc.getQueryState(extWorkflowKeys.run("ws-1", "r1"))?.isInvalidated).toBe(true);
  });
});
```

- [ ] **Step 2: Run, expect FAIL**

```bash
cd packages/core && pnpm exec vitest run ext-workflows   # FAIL: Failed to resolve import "./schemas" / "./errors" / "./mutations"
```

- [ ] **Step 3: Implement**


**Create `packages/core/ext-workflows/types.ts`:**

```ts
// ext-workflow: wire types for the first-class workflow feature. Mirrors the
// JSON the Go handlers in server/internal/handler/ext_workflow*.go produce.

export type ExtWorkflowRunStatus =
  | "running"
  | "waiting_human"
  | "done"
  | "failed"
  | "cancelled"
  /** Any status the server adds later; render neutrally. */
  | "unknown";

export type ExtWorkflowStepStatus =
  | "pending"
  | "running"
  | "awaiting_supervisor"
  | "awaiting_human"
  | "done"
  | "skipped"
  | "failed"
  | "cancelled"
  /** Any status the server adds later; render neutrally. */
  | "unknown";

/** Actions a human may take on a step that is awaiting_human. */
export type ExtWorkflowDecisionAction =
  | "approve"
  | "redo"
  | "retry"
  | "skip"
  | "rewind"
  | "abort";

export interface ExtWorkflowNode {
  id: string;
  key: string;
  title: string;
  agent_id: string;
  prompt: string;
  requires_review: boolean;
  max_attempts: number;
  depends_on: string[];
  position: number;
}

/** One node as sent in `PUT /api/ext/workflows/{id}`. Slice order is the position. */
export interface ExtWorkflowNodeInput {
  key: string;
  title: string;
  agent_id: string;
  prompt: string;
  requires_review: boolean;
  max_attempts: number;
  depends_on: string[];
}

export interface ExtWorkflow {
  id: string;
  workspace_id: string;
  name: string;
  description: string;
  supervisor_agent_id: string;
  max_rewinds: number;
  creator_id: string;
  avatar_url: string | null;
  archived_at: string | null;
  created_at: string;
  updated_at: string;
  node_count: number;
  active_run_count: number;
  last_run_at: string | null;
  /** Populated by the detail endpoint only; list entries carry `[]`. */
  nodes: ExtWorkflowNode[];
}

export interface CreateExtWorkflowRequest {
  name: string;
  description?: string;
  supervisor_agent_id: string;
}

export interface UpdateExtWorkflowRequest {
  name?: string;
  description?: string;
  supervisor_agent_id?: string;
  max_rewinds?: number;
  /** Replaces the whole node set when present. */
  nodes?: ExtWorkflowNodeInput[];
}

export interface ExtWorkflowValidationError {
  node_key?: string;
  field: string;
  message: string;
}

export interface ExtWorkflowRunSummary {
  id: string;
  workspace_id: string;
  workflow_id: string;
  workflow_name: string;
  issue_id: string;
  issue_identifier: string;
  issue_title: string;
  triggered_by_type: string;
  triggered_by_id: string;
  status: ExtWorkflowRunStatus;
  rewinds_used: number;
  max_rewinds: number;
  started_at: string;
  finished_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface ExtWorkflowStep {
  id: string;
  node_key: string;
  title: string;
  agent_id: string;
  issue_id: string;
  status: ExtWorkflowStepStatus;
  attempts: number;
  max_attempts: number;
  requires_review: boolean;
  depends_on: string[];
  pending_reason: string | null;
  last_feedback: string | null;
  escalation_reason: string | null;
  started_at: string | null;
  finished_at: string | null;
}

export interface ExtWorkflowRunEvent {
  id: string;
  step_id: string | null;
  kind: string;
  actor_type: string;
  actor_id: string | null;
  on_behalf_of: string | null;
  payload: Record<string, unknown>;
  created_at: string;
}

export interface ExtWorkflowRun extends ExtWorkflowRunSummary {
  steps: ExtWorkflowStep[];
  events: ExtWorkflowRunEvent[];
}

export interface ExtWorkflowRunList {
  runs: ExtWorkflowRunSummary[];
  total: number;
}

/** Set when the queried issue is itself a child issue (a step) of a run. */
export interface ExtWorkflowStepOf {
  run_id: string;
  step_id: string;
  node_key: string;
  index: number;
  total: number;
  parent_issue_id: string;
}

export interface ExtWorkflowIssueRuns {
  runs: ExtWorkflowRunSummary[];
  step_of: ExtWorkflowStepOf | null;
}

export interface DecideExtWorkflowStepRequest {
  action: ExtWorkflowDecisionAction;
  to?: string;
  reason?: string;
  feedback?: string;
  /** The step status the caller saw; the server answers 409 on mismatch. */
  expected_status: ExtWorkflowStepStatus;
}
```


**Create `packages/core/ext-workflows/errors.ts`:**

```ts
import type { ExtWorkflowValidationError } from "./types";

/**
 * Thrown by `ApiClient.updateExtWorkflow` when the server answers 422
 * `{error: "validation_failed", errors: [...]}`. The editor maps `errors` onto
 * rows and fields instead of toasting one sentence.
 */
export class ExtWorkflowValidationFailed extends Error {
  readonly errors: ExtWorkflowValidationError[];

  constructor(errors: ExtWorkflowValidationError[]) {
    super(
      errors.length === 1 && errors[0]
        ? errors[0].message
        : `Workflow definition is invalid (${errors.length} problems)`,
    );
    this.name = "ExtWorkflowValidationFailed";
    this.errors = errors;
  }
}
```


**Create `packages/core/ext-workflows/schemas.ts`:**

```ts
import { z } from "zod";
import type {
  ExtWorkflow,
  ExtWorkflowIssueRuns,
  ExtWorkflowRun,
  ExtWorkflowRunList,
  ExtWorkflowRunStatus,
  ExtWorkflowStepStatus,
  ExtWorkflowValidationError,
} from "./types";

// ext-workflow: lenient response schemas. Every optional field has a default
// and every enum falls back to "unknown", so an older client talking to a newer
// backend keeps rendering. Parsed with `parseWithFallback` in ApiClient.

const RUN_STATUSES: readonly string[] = ["running", "waiting_human", "done", "failed", "cancelled"];
const STEP_STATUSES: readonly string[] = [
  "pending",
  "running",
  "awaiting_supervisor",
  "awaiting_human",
  "done",
  "skipped",
  "failed",
  "cancelled",
];

const nullableString = z
  .string()
  .nullable()
  .optional()
  .transform((v) => v ?? null);

export const ExtWorkflowRunStatusSchema = z
  .string()
  .transform((v): ExtWorkflowRunStatus => (RUN_STATUSES.includes(v) ? (v as ExtWorkflowRunStatus) : "unknown"))
  .catch("unknown");

export const ExtWorkflowStepStatusSchema = z
  .string()
  .transform((v): ExtWorkflowStepStatus => (STEP_STATUSES.includes(v) ? (v as ExtWorkflowStepStatus) : "unknown"))
  .catch("unknown");

export const ExtWorkflowNodeSchema = z
  .object({
    id: z.string(),
    key: z.string(),
    title: z.string().default(""),
    agent_id: z.string(),
    prompt: z.string().default(""),
    requires_review: z.boolean().default(false),
    max_attempts: z.number().default(3),
    depends_on: z.array(z.string()).nullable().optional().transform((v) => v ?? []),
    position: z.number().default(0),
  })
  .loose();

export const ExtWorkflowSchema = z
  .object({
    id: z.string(),
    workspace_id: z.string(),
    name: z.string(),
    description: z.string().default(""),
    supervisor_agent_id: z.string(),
    max_rewinds: z.number().default(3),
    creator_id: z.string(),
    avatar_url: nullableString,
    archived_at: nullableString,
    created_at: z.string(),
    updated_at: z.string(),
    node_count: z.number().default(0),
    active_run_count: z.number().default(0),
    last_run_at: nullableString,
    nodes: z.array(ExtWorkflowNodeSchema).nullable().optional().transform((v) => v ?? []),
  })
  .loose();

export const ExtWorkflowListSchema = z.object({
  workflows: z.array(ExtWorkflowSchema).default([]),
});

export const ExtWorkflowValidationErrorSchema = z
  .object({
    node_key: z.string().optional(),
    field: z.string().default(""),
    message: z.string().default(""),
  })
  .loose();

export const ExtWorkflowValidationBodySchema = z.object({
  error: z.literal("validation_failed"),
  errors: z.array(ExtWorkflowValidationErrorSchema).default([]),
});

export const ExtWorkflowRunSummarySchema = z
  .object({
    id: z.string(),
    workspace_id: z.string().default(""),
    workflow_id: z.string(),
    workflow_name: z.string().default(""),
    issue_id: z.string(),
    issue_identifier: z.string().default(""),
    issue_title: z.string().default(""),
    triggered_by_type: z.string().default(""),
    triggered_by_id: z.string().default(""),
    status: ExtWorkflowRunStatusSchema,
    rewinds_used: z.number().default(0),
    max_rewinds: z.number().default(0),
    started_at: z.string().default(""),
    finished_at: nullableString,
    created_at: z.string().default(""),
    updated_at: z.string().default(""),
  })
  .loose();

export const ExtWorkflowStepSchema = z
  .object({
    id: z.string(),
    node_key: z.string(),
    title: z.string().default(""),
    agent_id: z.string().default(""),
    issue_id: z.string().default(""),
    status: ExtWorkflowStepStatusSchema,
    attempts: z.number().default(0),
    max_attempts: z.number().default(1),
    requires_review: z.boolean().default(false),
    depends_on: z.array(z.string()).nullable().optional().transform((v) => v ?? []),
    pending_reason: nullableString,
    last_feedback: nullableString,
    escalation_reason: nullableString,
    started_at: nullableString,
    finished_at: nullableString,
  })
  .loose();

export const ExtWorkflowRunEventSchema = z
  .object({
    id: z.string(),
    step_id: nullableString,
    kind: z.string().default(""),
    actor_type: z.string().default(""),
    actor_id: nullableString,
    on_behalf_of: nullableString,
    payload: z.record(z.string(), z.unknown()).nullable().optional().transform((v) => v ?? {}),
    created_at: z.string().default(""),
  })
  .loose();

export const ExtWorkflowRunSchema = ExtWorkflowRunSummarySchema.extend({
  steps: z.array(ExtWorkflowStepSchema).nullable().optional().transform((v) => v ?? []),
  events: z.array(ExtWorkflowRunEventSchema).nullable().optional().transform((v) => v ?? []),
});

export const ExtWorkflowRunListSchema = z.object({
  runs: z.array(ExtWorkflowRunSummarySchema).default([]),
  total: z.number().default(0),
});

export const ExtWorkflowStepOfSchema = z
  .object({
    run_id: z.string(),
    step_id: z.string(),
    node_key: z.string().default(""),
    index: z.number().default(0),
    total: z.number().default(0),
    parent_issue_id: z.string(),
  })
  .loose();

export const ExtWorkflowIssueRunsSchema = z.object({
  runs: z.array(ExtWorkflowRunSummarySchema).default([]),
  step_of: ExtWorkflowStepOfSchema.nullable().optional().transform((v) => v ?? null),
});

// Fallbacks handed to parseWithFallback when a response cannot be parsed at
// all: empty, renderable, never throwing.
export const EMPTY_EXT_WORKFLOW_LIST: { workflows: ExtWorkflow[] } = { workflows: [] };

export const EMPTY_EXT_WORKFLOW: ExtWorkflow = {
  id: "",
  workspace_id: "",
  name: "",
  description: "",
  supervisor_agent_id: "",
  max_rewinds: 3,
  creator_id: "",
  avatar_url: null,
  archived_at: null,
  created_at: "",
  updated_at: "",
  node_count: 0,
  active_run_count: 0,
  last_run_at: null,
  nodes: [],
};

export const EMPTY_EXT_WORKFLOW_RUN_LIST: ExtWorkflowRunList = { runs: [], total: 0 };

export const EMPTY_EXT_WORKFLOW_ISSUE_RUNS: ExtWorkflowIssueRuns = { runs: [], step_of: null };

export const EMPTY_EXT_WORKFLOW_RUN: ExtWorkflowRun = {
  id: "",
  workspace_id: "",
  workflow_id: "",
  workflow_name: "",
  issue_id: "",
  issue_identifier: "",
  issue_title: "",
  triggered_by_type: "",
  triggered_by_id: "",
  status: "unknown",
  rewinds_used: 0,
  max_rewinds: 0,
  started_at: "",
  finished_at: null,
  created_at: "",
  updated_at: "",
  steps: [],
  events: [],
};

export type { ExtWorkflowValidationError };
```


**Create `packages/core/ext-workflows/queries.ts`:**

```ts
import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

// ext-workflow: every key is workspace-scoped. Run keys live under a separate
// root so a template edit does not refetch run history and vice versa.
export const extWorkflowKeys = {
  all: (wsId: string) => ["ext-workflows", wsId] as const,
  list: (wsId: string) => [...extWorkflowKeys.all(wsId), "list"] as const,
  detail: (wsId: string, id: string) => [...extWorkflowKeys.all(wsId), "detail", id] as const,
  runs: (wsId: string, id: string) => [...extWorkflowKeys.all(wsId), "runs", id] as const,
  runsAll: (wsId: string) => ["ext-workflow-runs", wsId] as const,
  issueRuns: (wsId: string, issueId: string) => ["ext-workflow-runs", wsId, "issue", issueId] as const,
  run: (wsId: string, runId: string) => ["ext-workflow-runs", wsId, "run", runId] as const,
};

export function extWorkflowListOptions(wsId: string) {
  return queryOptions({
    queryKey: extWorkflowKeys.list(wsId),
    queryFn: () => api.listExtWorkflows(),
    enabled: !!wsId,
  });
}

export function extWorkflowDetailOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: extWorkflowKeys.detail(wsId, id),
    queryFn: () => api.getExtWorkflow(id),
    enabled: !!wsId && !!id,
  });
}

export function extWorkflowRunsOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: extWorkflowKeys.runs(wsId, id),
    queryFn: () => api.listExtWorkflowRuns(id),
    enabled: !!wsId && !!id,
  });
}

export function extWorkflowIssueRunsOptions(wsId: string, issueId: string) {
  return queryOptions({
    queryKey: extWorkflowKeys.issueRuns(wsId, issueId),
    queryFn: () => api.getIssueExtWorkflowRuns(issueId),
    enabled: !!wsId && !!issueId,
  });
}

export function extWorkflowRunOptions(wsId: string, runId: string) {
  return queryOptions({
    queryKey: extWorkflowKeys.run(wsId, runId),
    queryFn: () => api.getExtWorkflowRun(runId),
    enabled: !!wsId && !!runId,
  });
}
```


**Create `packages/core/ext-workflows/mutations.ts`:**

```ts
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { extWorkflowKeys } from "./queries";
import type {
  CreateExtWorkflowRequest,
  DecideExtWorkflowStepRequest,
  ExtWorkflow,
  UpdateExtWorkflowRequest,
} from "./types";

export function useCreateExtWorkflow(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: CreateExtWorkflowRequest) => api.createExtWorkflow(data),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: extWorkflowKeys.list(wsId) });
    },
  });
}

// Not optimistic: a save can be rejected with a 422 that the editor maps onto
// rows, so the cache only moves once the server has accepted the definition.
export function useUpdateExtWorkflow(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...data }: { id: string } & UpdateExtWorkflowRequest) =>
      api.updateExtWorkflow(id, data),
    onSuccess: (workflow) => {
      qc.setQueryData<ExtWorkflow>(extWorkflowKeys.detail(wsId, workflow.id), workflow);
    },
    onSettled: (_data, _err, vars) => {
      qc.invalidateQueries({ queryKey: extWorkflowKeys.detail(wsId, vars.id) });
      qc.invalidateQueries({ queryKey: extWorkflowKeys.list(wsId) });
    },
  });
}

export function useArchiveExtWorkflow(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.archiveExtWorkflow(id),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: extWorkflowKeys.all(wsId) });
    },
  });
}

// Decisions and cancels are not optimistic: the engine is the source of truth
// for the resulting step and run state, and a stale expected_status answers 409.
export function useDecideExtWorkflowStep(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      runId,
      stepId,
      ...body
    }: { runId: string; stepId: string } & DecideExtWorkflowStepRequest) =>
      api.decideExtWorkflowStep(runId, stepId, body),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: extWorkflowKeys.runsAll(wsId) });
    },
  });
}

export function useCancelExtWorkflowRun(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (runId: string) => api.cancelExtWorkflowRun(runId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: extWorkflowKeys.runsAll(wsId) });
      qc.invalidateQueries({ queryKey: extWorkflowKeys.all(wsId) });
    },
  });
}
```


**Create `packages/core/ext-workflows/index.ts`:**

```ts
export * from "./types";
export { ExtWorkflowValidationFailed } from "./errors";
export {
  extWorkflowKeys,
  extWorkflowListOptions,
  extWorkflowDetailOptions,
  extWorkflowRunsOptions,
  extWorkflowIssueRunsOptions,
  extWorkflowRunOptions,
} from "./queries";
export {
  useCreateExtWorkflow,
  useUpdateExtWorkflow,
  useArchiveExtWorkflow,
  useDecideExtWorkflowStep,
  useCancelExtWorkflowRun,
} from "./mutations";
```


**ApiClient and package exports**

```diff
--- a/packages/core/api/client.ts
+++ b/packages/core/api/client.ts
@@ -2,6 +2,30 @@
 import type { IssueWakeup, IssueWakeupInput, IssueWakeupSummaryRow, PausedWakeup, SystemWakeup, WakeupRun, WorkspaceSystemWakeup } from "../types/issue-wakeup";
 import type { WorkspaceWakeupPage, WorkspaceWakeupFilters } from "../types/issue-wakeup";
 import { WorkspaceWakeupPageSchema, IssueWakeupSchema, IssueWakeupSummaryRowSchema, PausedWakeupSchema, SystemWakeupSchema, WakeupRunSchema, WorkspaceSystemWakeupSchema } from "./schemas";
+// ext-workflow: workflow client imports (block marked at the end of the class).
+import type {
+  CreateExtWorkflowRequest,
+  DecideExtWorkflowStepRequest,
+  ExtWorkflow,
+  ExtWorkflowIssueRuns,
+  ExtWorkflowRun,
+  ExtWorkflowRunList,
+  UpdateExtWorkflowRequest,
+} from "../ext-workflows/types";
+import { ExtWorkflowValidationFailed } from "../ext-workflows/errors";
+import {
+  EMPTY_EXT_WORKFLOW,
+  EMPTY_EXT_WORKFLOW_ISSUE_RUNS,
+  EMPTY_EXT_WORKFLOW_LIST,
+  EMPTY_EXT_WORKFLOW_RUN,
+  EMPTY_EXT_WORKFLOW_RUN_LIST,
+  ExtWorkflowIssueRunsSchema,
+  ExtWorkflowListSchema,
+  ExtWorkflowRunListSchema,
+  ExtWorkflowRunSchema,
+  ExtWorkflowSchema,
+  ExtWorkflowValidationBodySchema,
+} from "../ext-workflows/schemas";
 import type { InboxFilters } from "../inbox/filter-store";
 import type { ArchivedInboxPage, ArchivedInboxFacets } from "../types/inbox";
 import { configStore } from "../config";
@@ -5227,4 +5251,97 @@
       { endpoint: "POST /api/telegram/binding/redeem" },
     );
   }
+
+  // ext-workflow: workflow templates and runs (fork-only /api/ext/* endpoints).
+  async listExtWorkflows(): Promise<ExtWorkflow[]> {
+    const raw = await this.fetch<unknown>("/api/ext/workflows");
+    return parseWithFallback(raw, ExtWorkflowListSchema, EMPTY_EXT_WORKFLOW_LIST, {
+      endpoint: "GET /api/ext/workflows",
+    }).workflows as ExtWorkflow[];
+  }
+
+  async getExtWorkflow(id: string): Promise<ExtWorkflow> {
+    const raw = await this.fetch<unknown>(`/api/ext/workflows/${id}`);
+    return parseWithFallback(raw, ExtWorkflowSchema, EMPTY_EXT_WORKFLOW, {
+      endpoint: "GET /api/ext/workflows/:id",
+    }) as ExtWorkflow;
+  }
+
+  async createExtWorkflow(data: CreateExtWorkflowRequest): Promise<ExtWorkflow> {
+    const raw = await this.fetch<unknown>("/api/ext/workflows", {
+      method: "POST",
+      body: JSON.stringify(data),
+    });
+    return parseWithFallback(raw, ExtWorkflowSchema, EMPTY_EXT_WORKFLOW, {
+      endpoint: "POST /api/ext/workflows",
+    }) as ExtWorkflow;
+  }
+
+  /** Throws ExtWorkflowValidationFailed on a 422 so the editor can map `errors` onto rows. */
+  async updateExtWorkflow(id: string, data: UpdateExtWorkflowRequest): Promise<ExtWorkflow> {
+    let raw: unknown;
+    try {
+      raw = await this.fetch<unknown>(`/api/ext/workflows/${id}`, {
+        method: "PUT",
+        body: JSON.stringify(data),
+      });
+    } catch (err) {
+      if (err instanceof ApiError && err.status === 422) {
+        const parsed = ExtWorkflowValidationBodySchema.safeParse(err.body);
+        if (parsed.success) throw new ExtWorkflowValidationFailed(parsed.data.errors);
+      }
+      throw err;
+    }
+    return parseWithFallback(raw, ExtWorkflowSchema, EMPTY_EXT_WORKFLOW, {
+      endpoint: "PUT /api/ext/workflows/:id",
+    }) as ExtWorkflow;
+  }
+
+  async archiveExtWorkflow(id: string): Promise<void> {
+    await this.fetch(`/api/ext/workflows/${id}`, { method: "DELETE" });
+  }
+
+  async listExtWorkflowRuns(id: string, params?: { limit?: number; offset?: number }): Promise<ExtWorkflowRunList> {
+    const search = new URLSearchParams();
+    if (params?.limit !== undefined) search.set("limit", String(params.limit));
+    if (params?.offset !== undefined) search.set("offset", String(params.offset));
+    const qs = search.toString();
+    const raw = await this.fetch<unknown>(`/api/ext/workflows/${id}/runs${qs ? `?${qs}` : ""}`);
+    return parseWithFallback(raw, ExtWorkflowRunListSchema, EMPTY_EXT_WORKFLOW_RUN_LIST, {
+      endpoint: "GET /api/ext/workflows/:id/runs",
+    }) as ExtWorkflowRunList;
+  }
+
+  async getIssueExtWorkflowRuns(issueId: string): Promise<ExtWorkflowIssueRuns> {
+    const raw = await this.fetch<unknown>(`/api/ext/workflow-runs?issue_id=${encodeURIComponent(issueId)}`);
+    return parseWithFallback(raw, ExtWorkflowIssueRunsSchema, EMPTY_EXT_WORKFLOW_ISSUE_RUNS, {
+      endpoint: "GET /api/ext/workflow-runs",
+    }) as ExtWorkflowIssueRuns;
+  }
+
+  async getExtWorkflowRun(runId: string): Promise<ExtWorkflowRun> {
+    const raw = await this.fetch<unknown>(`/api/ext/workflow-runs/${runId}`);
+    return parseWithFallback(raw, ExtWorkflowRunSchema, EMPTY_EXT_WORKFLOW_RUN, {
+      endpoint: "GET /api/ext/workflow-runs/:id",
+    }) as ExtWorkflowRun;
+  }
+
+  async cancelExtWorkflowRun(runId: string): Promise<void> {
+    await this.fetch(`/api/ext/workflow-runs/${runId}/cancel`, { method: "POST" });
+  }
+
+  async decideExtWorkflowStep(
+    runId: string,
+    stepId: string,
+    body: DecideExtWorkflowStepRequest,
+  ): Promise<ExtWorkflowRun> {
+    const raw = await this.fetch<unknown>(`/api/ext/workflow-runs/${runId}/steps/${stepId}/decision`, {
+      method: "POST",
+      body: JSON.stringify(body),
+    });
+    return parseWithFallback(raw, ExtWorkflowRunSchema, EMPTY_EXT_WORKFLOW_RUN, {
+      endpoint: "POST /api/ext/workflow-runs/:id/steps/:stepId/decision",
+    }) as ExtWorkflowRun;
+  }
+  // ext-workflow: end of block.
 }
```

```diff
--- a/packages/core/package.json
+++ b/packages/core/package.json
@@ -83,6 +83,9 @@
     "./agents/stores": "./agents/stores/index.ts",
     "./squads": "./squads/index.ts",
     "./squads/stores": "./squads/stores/index.ts",
+    "./ext-workflows": "./ext-workflows/index.ts",
+    "./ext-workflows/queries": "./ext-workflows/queries.ts",
+    "./ext-workflows/mutations": "./ext-workflows/mutations.ts",
     "./permissions": "./permissions/index.ts",
     "./search/cancelled-rank": "./search/cancelled-rank.ts",
     "./search-index": "./search-index/index.ts",
```

- [ ] **Step 4: Run, expect PASS**

```bash
cd packages/core && pnpm exec vitest run ext-workflows && pnpm typecheck && pnpm exec eslint ext-workflows api/client.ts   # 36 tests pass
```

- [ ] **Step 5: Commit**

```bash
git add packages/core/ext-workflows packages/core/api/client.ts packages/core/package.json
git commit -m "feat(ext-workflow): add core workflow types, schemas, queries, mutations and API client

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


### Task A6: Core integration: realtime, paths, icons, modal key, actor resolution, reserved slug, ripple fixes

**Files (all Modify unless noted):**
- `packages/core/types/events.ts` (`WSEventType` union after `"squad:deleted"` ~line 72; `WSEventPayloadMap` after `"squad:deleted": unknown;` ~line 642; new payload interfaces before `WSEventPayloadMap`)
- `packages/core/realtime/use-realtime-sync.ts` (import; `invalidateWorkspaceScopedQueries`; `refreshMap.ext_workflow` before `label:`; `specificEvents`; new `ws.on("ext_workflow_run:updated")`; cleanup list); Create `packages/core/realtime/use-realtime-sync-ext-workflow.test.tsx`
- `packages/core/types/issue.ts` (`IssueAssigneeType`, line 35)
- `packages/core/workspace/hooks.ts` + `hooks.test.tsx` (actor name / avatar / existence)
- `packages/core/modals/store.ts` (modal key `"create-ext-workflow"`)
- `packages/core/paths/paths.ts` (`workflows`, `workflowDetail`), `route-icons.ts` (`Workflow` icon, `workflows` page/nav key), tests `paths.test.ts`, `consistency.test.ts`, `route-icons.test.ts`, `tab-subject.test.ts`, `reserved-slugs.test.ts`
- `server/internal/handler/reserved_slugs.json` + regenerate `packages/core/paths/reserved-slugs.ts` (`pnpm generate:reserved-slugs`)
- `packages/core/diagnostics/diagnostic-context.ts` + test (route table)
- Ripple fixes: `packages/views/layout/route-icon-components.tsx`, `packages/views/search/search-command.tsx` + `search-command.test.tsx` (mock), `packages/views/locales/{en,fr,ja,ko,zh-Hans}/layout.json`, `apps/mobile/data/use-actor-name.ts`, `apps/mobile/components/ui/actor-avatar.tsx`

**Interfaces:** Consumes A5 (`extWorkflowKeys`, `extWorkflowListOptions`). Produces: WS event types `ext_workflow:created|updated|deleted` (payload `{workflow_id}`) and `ext_workflow_run:updated` (payload `{run_id, issue_id, workflow_id}`); `paths.workspace(slug).workflows()` / `.workflowDetail(id)`; `WORKSPACE_PAGES.workflows` (segment `workflows`, icon `Workflow`, navKey `workflows`); `useActorName()` resolving type `"workflow"` (`getActorName` -> name or "Unknown Workflow", `getActorAvatarUrl`, `hasActor` -> `undefined` while the list loads); `buildActorNameResolver({..., workflows?})`; `IssueAssigneeType` includes `"workflow"`; modal key `"create-ext-workflow"`.

- [ ] **Step 1: Write the failing tests**

New realtime test:

**Create `packages/core/realtime/use-realtime-sync-ext-workflow.test.tsx`:**

```tsx
/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { WSClient } from "../api/ws-client";
import { extWorkflowKeys } from "../ext-workflows/queries";
import { issueKeys } from "../issues/queries";
import { useRealtimeSync, type RealtimeSyncStores } from "./use-realtime-sync";

vi.mock("../platform/workspace-storage", () => ({
  getCurrentWsId: () => "ws-1",
  getCurrentSlug: () => "test-ws",
  createWorkspaceAwareStorage: (adapter: unknown) => adapter,
  registerForWorkspaceRehydration: () => {},
}));

vi.mock("../paths", () => ({
  useHasOnboarded: () => true,
  resolvePostAuthDestination: () => "/",
}));

// Records ws.on handlers by event name and the single onAny handler.
function createRecordingWs() {
  const handlers: Record<string, (p: unknown) => void> = {};
  let any: ((msg: { type: string; payload: unknown }) => void) | null = null;
  const ws = {
    on: vi.fn((event: string, handler: (p: unknown) => void) => {
      handlers[event] = handler;
      return () => {};
    }),
    onAny: vi.fn((handler: (msg: { type: string; payload: unknown }) => void) => {
      any = handler;
      return () => {};
    }),
    onReconnect: vi.fn(() => () => {}),
  } as unknown as WSClient;
  return { ws, handlers, emit: (type: string, payload: unknown = {}) => any?.({ type, payload }) };
}

function createStores(): RealtimeSyncStores {
  return {
    authStore: Object.assign(() => ({}), {
      getState: () => ({ user: { id: "u1" } }),
      subscribe: () => () => {},
      setState: () => {},
      destroy: () => {},
    }),
  } as unknown as RealtimeSyncStores;
}

function createWrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

const invalidated = (qc: QueryClient, key: readonly unknown[]) => qc.getQueryState(key)?.isInvalidated;

describe("useRealtimeSync — ext workflow events", () => {
  let qc: QueryClient;

  beforeEach(() => {
    vi.useFakeTimers();
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  });

  afterEach(() => {
    vi.useRealTimers();
    qc.clear();
    vi.clearAllMocks();
  });

  it.each(["ext_workflow:created", "ext_workflow:updated", "ext_workflow:deleted"])(
    "%s invalidates the workflow list and detail caches only",
    (type) => {
      qc.setQueryData(extWorkflowKeys.list("ws-1"), []);
      qc.setQueryData(extWorkflowKeys.detail("ws-1", "wf-1"), {});
      qc.setQueryData(extWorkflowKeys.run("ws-1", "run-1"), {});
      qc.setQueryData(extWorkflowKeys.list("ws-other"), []);

      const { ws, emit } = createRecordingWs();
      renderHook(() => useRealtimeSync(ws, createStores()), { wrapper: createWrapper(qc) });

      emit(type, { workflow_id: "wf-1" });
      vi.advanceTimersByTime(150);

      expect(invalidated(qc, extWorkflowKeys.list("ws-1"))).toBe(true);
      expect(invalidated(qc, extWorkflowKeys.detail("ws-1", "wf-1"))).toBe(true);
      expect(invalidated(qc, extWorkflowKeys.run("ws-1", "run-1"))).toBe(false);
      expect(invalidated(qc, extWorkflowKeys.list("ws-other"))).toBe(false);
    },
  );

  it("ext_workflow_run:updated refreshes run caches and the parent issue detail and children", () => {
    qc.setQueryData(extWorkflowKeys.run("ws-1", "run-1"), {});
    qc.setQueryData(extWorkflowKeys.issueRuns("ws-1", "issue-1"), {});
    qc.setQueryData(extWorkflowKeys.runs("ws-1", "wf-1"), {});
    qc.setQueryData(issueKeys.detail("ws-1", "issue-1"), {});
    qc.setQueryData(issueKeys.children("ws-1", "issue-1"), []);
    qc.setQueryData(issueKeys.detail("ws-1", "issue-2"), {});

    const { ws, handlers } = createRecordingWs();
    renderHook(() => useRealtimeSync(ws, createStores()), { wrapper: createWrapper(qc) });

    handlers["ext_workflow_run:updated"]?.({ run_id: "run-1", issue_id: "issue-1", workflow_id: "wf-1" });

    expect(invalidated(qc, extWorkflowKeys.run("ws-1", "run-1"))).toBe(true);
    expect(invalidated(qc, extWorkflowKeys.issueRuns("ws-1", "issue-1"))).toBe(true);
    expect(invalidated(qc, extWorkflowKeys.runs("ws-1", "wf-1"))).toBe(true);
    expect(invalidated(qc, issueKeys.detail("ws-1", "issue-1"))).toBe(true);
    expect(invalidated(qc, issueKeys.children("ws-1", "issue-1"))).toBe(true);
    expect(invalidated(qc, issueKeys.detail("ws-1", "issue-2"))).toBe(false);
  });

  it("ext_workflow_run:updated tolerates a payload without issue_id", () => {
    qc.setQueryData(extWorkflowKeys.run("ws-1", "run-1"), {});
    const { ws, handlers } = createRecordingWs();
    renderHook(() => useRealtimeSync(ws, createStores()), { wrapper: createWrapper(qc) });

    expect(() => handlers["ext_workflow_run:updated"]?.({ run_id: "run-1" })).not.toThrow();
    expect(invalidated(qc, extWorkflowKeys.run("ws-1", "run-1"))).toBe(true);
  });

  it("does not double-handle ext_workflow_run:updated through the generic prefix path", () => {
    qc.setQueryData(extWorkflowKeys.list("ws-1"), []);
    const { ws, emit } = createRecordingWs();
    renderHook(() => useRealtimeSync(ws, createStores()), { wrapper: createWrapper(qc) });

    emit("ext_workflow_run:updated", { run_id: "run-1", issue_id: "issue-1", workflow_id: "wf-1" });
    vi.advanceTimersByTime(150);

    // The prefix path would have run the `ext_workflow_run` entry (absent by design);
    // the list cache is untouched by the generic dispatcher.
    expect(invalidated(qc, extWorkflowKeys.list("ws-1"))).toBe(false);
  });
});
```

Test edits (apply these hunks first; they fail until Step 3):

```diff
--- a/packages/core/workspace/hooks.test.tsx
+++ b/packages/core/workspace/hooks.test.tsx
@@ -8,8 +8,9 @@
 import { setApiInstance } from "../api";
 import type { ApiClient } from "../api/client";
 import type { Workspace } from "../types";
+import { extWorkflowKeys } from "../ext-workflows/queries";
 import { workspaceKeys } from "./queries";
-import { useActorName, useWorkspaceList } from "./hooks";
+import { buildActorNameResolver, useActorName, useWorkspaceList } from "./hooks";
 
 // useActorName reads the current workspace from the core WorkspaceId provider;
 // the directory-name resolution under test does not depend on the real id.
@@ -162,4 +163,55 @@
     expect(result.current.hasActor("member", "user-1")).toBe(true);
     expect(result.current.hasActor("member", "departed-user")).toBe(false);
   });
+
+  // ext-workflow: issues can be assigned to a workflow, so the actor helpers
+  // resolve its name, avatar and existence like they do for squads.
+  it("resolves workflow names, avatars and existence", () => {
+    const workflows = [{ id: "wf-1", name: "Release train", avatar_url: "/uploads/wf.png" }];
+    setApiInstance({
+      getBaseUrl: () => "https://api.example.test",
+      listMembers: () => Promise.resolve([]),
+      listAgents: () => Promise.resolve([]),
+      listSquads: () => Promise.resolve([]),
+      listExtWorkflows: () => Promise.resolve(workflows),
+    } as unknown as ApiClient);
+    qc.setQueryData(workspaceKeys.members("ws-1"), []);
+    qc.setQueryData(workspaceKeys.agents("ws-1"), []);
+    qc.setQueryData(workspaceKeys.squads("ws-1"), []);
+    qc.setQueryData(extWorkflowKeys.list("ws-1"), workflows);
+
+    const { result } = renderHook(() => useActorName(), {
+      wrapper: createWrapper(qc),
+    });
+
+    expect(result.current.getActorName("workflow", "wf-1")).toBe("Release train");
+    expect(result.current.getActorName("workflow", "gone")).toBe("Unknown Workflow");
+    expect(result.current.getActorAvatarUrl("workflow", "wf-1")).toBe("https://api.example.test/uploads/wf.png");
+    expect(result.current.getActorAvatarUrl("workflow", "gone")).toBeNull();
+    expect(result.current.hasActor("workflow", "wf-1")).toBe(true);
+    expect(result.current.hasActor("workflow", "gone")).toBe(false);
+  });
+
+  it("does not call a workflow missing before its directory has loaded", () => {
+    const pending = () => new Promise<never>(() => {});
+    setApiInstance({
+      listMembers: pending,
+      listAgents: pending,
+      listSquads: pending,
+      listExtWorkflows: pending,
+    } as unknown as ApiClient);
+
+    const { result } = renderHook(() => useActorName(), {
+      wrapper: createWrapper(qc),
+    });
+
+    expect(result.current.hasActor("workflow", "wf-1")).toBeUndefined();
+  });
+});
+
+describe("buildActorNameResolver", () => {
+  it("falls back to a generic name when no workflow directory is supplied", () => {
+    const resolve = buildActorNameResolver({ members: [], agents: [], squads: [] });
+    expect(resolve("workflow", "wf-1")).toBe("Unknown Workflow");
+  });
 });
```
```diff
--- a/packages/core/paths/paths.test.ts
+++ b/packages/core/paths/paths.test.ts
@@ -33,6 +33,9 @@
     expect(ws.skillDetail("skl_123")).toBe("/acme/skills/skl_123");
     expect(ws.squads()).toBe("/acme/squads");
     expect(ws.squadDetail("sq_1")).toBe("/acme/squads/sq_1");
+    expect(ws.workflows()).toBe("/acme/workflows");
+    expect(ws.workflowDetail("wf_1")).toBe("/acme/workflows/wf_1");
+    expect(ws.workflowDetail("a/b c")).toBe("/acme/workflows/a%2Fb%20c");
     expect(ws.settings()).toBe("/acme/settings");
     expect(ws.attachmentPreview("att_42")).toBe("/acme/attachments/att_42/preview");
   });
```
```diff
--- a/packages/core/paths/consistency.test.ts
+++ b/packages/core/paths/consistency.test.ts
@@ -24,6 +24,7 @@
       ["newAgentAi", "agents/new/ai"],
       ["chat", "chat"],
       ["squads", "squads"],
+      ["workflows", "workflows"],
       ["inbox", "inbox"],
       ["myIssues", "my-issues"],
       ["runtimes", "runtimes"],
```
```diff
--- a/packages/core/paths/route-icons.test.ts
+++ b/packages/core/paths/route-icons.test.ts
@@ -46,6 +46,7 @@
     expect(pageForSegment("projects")).toBe("projects");
     expect(pageForSegment("my-issues")).toBe("myIssues");
     expect(pageForSegment("settings")).toBe("settings");
+    expect(pageForSegment("workflows")).toBe("workflows");
   });
 
   it("returns null for an unknown segment", () => {
@@ -60,6 +61,8 @@
     expect(resolveRouteIconName("/acme/autopilots")).toBe("Zap");
     expect(resolveRouteIconName("/acme/chat")).toBe("MessageSquare");
     expect(resolveRouteIconName("/acme/squads")).toBe("Users");
+    expect(resolveRouteIconName("/acme/workflows")).toBe("Workflow");
+    expect(resolveRouteIconName("/acme/workflows/wf-1")).toBe("Workflow");
     expect(resolveRouteIconName("/acme/usage")).toBe("BarChart3");
     expect(resolveRouteIconName("/acme/my-issues")).toBe("CircleUser");
   });
```
```diff
--- a/packages/core/paths/tab-subject.test.ts
+++ b/packages/core/paths/tab-subject.test.ts
@@ -14,6 +14,8 @@
     ["/acme/autopilots", { kind: "page", page: "autopilots" }],
     ["/acme/agents", { kind: "page", page: "agents" }],
     ["/acme/squads", { kind: "page", page: "squads" }],
+    ["/acme/workflows", { kind: "page", page: "workflows" }],
+    ["/acme/workflows/wf1", { kind: "page", page: "workflows" }],
     ["/acme/usage", { kind: "page", page: "usage" }],
     ["/acme/runtimes", { kind: "page", page: "runtimes" }],
     ["/acme/skills", { kind: "page", page: "skills" }],
```
```diff
--- a/packages/core/paths/reserved-slugs.test.ts
+++ b/packages/core/paths/reserved-slugs.test.ts
@@ -6,6 +6,10 @@
     expect(isReservedSlug("login")).toBe(true);
   });
 
+  it("reserves the ext-workflow route segment so a workspace cannot shadow /workflows", () => {
+    expect(isReservedSlug("workflows")).toBe(true);
+  });
+
   it("returns false for an unreserved slug", () => {
     expect(isReservedSlug("my-cool-workspace")).toBe(false);
   });
```
```diff
--- a/packages/core/diagnostics/diagnostic-context.test.ts
+++ b/packages/core/diagnostics/diagnostic-context.test.ts
@@ -42,6 +42,8 @@
     expect(bucketDiagnosticPath("/acme/agents/agt_9")).toBe("/:slug/agents/:id");
     expect(bucketDiagnosticPath("/acme/members/m-3")).toBe("/:slug/members/:id");
     expect(bucketDiagnosticPath("/acme/squads/sq.4")).toBe("/:slug/squads/:id");
+    expect(bucketDiagnosticPath("/acme/workflows")).toBe("/:slug/workflows");
+    expect(bucketDiagnosticPath("/acme/workflows/wf-1")).toBe("/:slug/workflows/:id");
     expect(bucketDiagnosticPath("/acme/runtimes/machine-1")).toBe("/:slug/runtimes/:id");
     expect(bucketDiagnosticPath("/acme/skills/skl_123")).toBe("/:slug/skills/:id");
     expect(bucketDiagnosticPath("/acme/attachments/att-8/preview")).toBe(
```
```diff
--- a/packages/views/search/search-command.test.tsx
+++ b/packages/views/search/search-command.test.tsx
@@ -219,6 +219,7 @@
     autopilots: () => "/ws-test/autopilots",
     agents: () => "/ws-test/agents",
     squads: () => "/ws-test/squads",
+    workflows: () => "/ws-test/workflows",
     usage: () => "/ws-test/usage",
     runtimes: () => "/ws-test/runtimes",
     skills: () => "/ws-test/skills",
```

- [ ] **Step 2: Run, expect FAIL**

```bash
cd packages/core && pnpm exec vitest run realtime/use-realtime-sync-ext workspace/hooks paths diagnostics
# FAIL: ws.on handler for ext_workflow_run:updated missing; useActorName workflow -> "System"; paths.workflows is not a function; reserved slug false; diagnostics route coverage
```

- [ ] **Step 3: Implement** (apply each diff; all were produced from a verified working copy)

**3a. Events and realtime**

```diff
--- a/packages/core/types/events.ts
+++ b/packages/core/types/events.ts
@@ -70,6 +70,10 @@
   | "squad:created"
   | "squad:updated"
   | "squad:deleted"
+  | "ext_workflow:created"
+  | "ext_workflow:updated"
+  | "ext_workflow:deleted"
+  | "ext_workflow_run:updated"
   | "label:created"
   | "label:updated"
   | "label:deleted"
@@ -568,6 +572,16 @@
  * here. TS will compile-error every WSClient.on("new:event", …) site that
  * forgets the payload shape — that's the whole point.
  */
+export interface ExtWorkflowEventPayload {
+  workflow_id: string;
+}
+
+export interface ExtWorkflowRunUpdatedPayload {
+  run_id: string;
+  issue_id: string;
+  workflow_id: string;
+}
+
 export interface WSEventPayloadMap {
   "issue:created": IssueCreatedPayload;
   "issue:updated": IssueUpdatedPayload;
@@ -640,6 +654,11 @@
   "squad:created": unknown;
   "squad:updated": unknown;
   "squad:deleted": unknown;
+  // ext-workflow: payload {workflow_id}; run events {run_id, issue_id, workflow_id}.
+  "ext_workflow:created": ExtWorkflowEventPayload;
+  "ext_workflow:updated": ExtWorkflowEventPayload;
+  "ext_workflow:deleted": ExtWorkflowEventPayload;
+  "ext_workflow_run:updated": ExtWorkflowRunUpdatedPayload;
   "label:created": unknown;
   "label:updated": unknown;
   "label:deleted": unknown;
```
```diff
--- a/packages/core/realtime/use-realtime-sync.ts
+++ b/packages/core/realtime/use-realtime-sync.ts
@@ -14,6 +14,7 @@
 import { projectKeys } from "../projects/queries";
 import { pinKeys } from "../pins/queries";
 import { autopilotKeys } from "../autopilots/queries";
+import { extWorkflowKeys } from "../ext-workflows/queries";
 import { runtimeKeys } from "../runtimes/queries";
 import { labelKeys } from "../labels/queries";
 import { propertyKeys } from "../properties/queries";
@@ -82,6 +83,7 @@
   IssueUpdatedPayload,
   IssueCreatedPayload,
   IssueDeletedPayload,
+  ExtWorkflowRunUpdatedPayload,
   IssueAttachmentsChangedPayload,
   IssueLabelsChangedPayload,
   IssueMetadataChangedPayload,
@@ -657,6 +659,9 @@
     qc.invalidateQueries({ queryKey: projectKeys.all(wsId) });
     qc.invalidateQueries({ queryKey: runtimeKeys.all(wsId) });
     qc.invalidateQueries({ queryKey: autopilotKeys.all(wsId) });
+    // ext-workflow: templates and run caches (separate roots, see extWorkflowKeys).
+    qc.invalidateQueries({ queryKey: extWorkflowKeys.all(wsId) });
+    qc.invalidateQueries({ queryKey: extWorkflowKeys.runsAll(wsId) });
     qc.invalidateQueries({ queryKey: agentTaskSnapshotKeys.all(wsId) });
     qc.invalidateQueries({ queryKey: workspaceWorkingAgentsKeys.all(wsId) });
     qc.invalidateQueries({ queryKey: agentActivityKeys.all(wsId) });
@@ -843,6 +848,13 @@
           qc.invalidateQueries({ queryKey: issueKeys.all(wsId) });
         }
       },
+      // ext-workflow: ext_workflow:created/updated/deleted (template CRUD).
+      // ext_workflow_run:updated is handled by its own ws.on handler below
+      // because it needs the payload's issue_id.
+      ext_workflow: () => {
+        const wsId = getCurrentWsId();
+        if (wsId) qc.invalidateQueries({ queryKey: extWorkflowKeys.all(wsId) });
+      },
       label: () => {
         // Label catalogs are independently scoped to issues, agents, and
         // skills. The generic event prefix does not carry the scope into this
@@ -1037,6 +1049,7 @@
       "issue:updated", "issue:created", "issue:deleted", "issue_attachments:changed", "issue_labels:changed", "issue_metadata:changed", "issue_properties:changed", "property:created", "property:updated", "inbox:new",
       "comment:created", "comment:updated", "comment:deleted",
       "comment:resolved", "comment:unresolved",
+      "ext_workflow_run:updated",
       "activity:created",
       "reaction:added", "reaction:removed",
       "issue_reaction:added", "issue_reaction:removed",
@@ -1114,6 +1127,21 @@
       }
     });
 
+    // ext-workflow: a run changed. Refresh run caches and the parent issue's
+    // detail/children so the issue sidebar and child list repaint. The
+    // issue:updated events the engine also publishes cover status changes.
+    const unsubExtWorkflowRunUpdated = ws.on("ext_workflow_run:updated", (p) => {
+      const payload = p as ExtWorkflowRunUpdatedPayload;
+      const wsId = getCurrentWsId();
+      if (!wsId) return;
+      qc.invalidateQueries({ queryKey: extWorkflowKeys.runsAll(wsId) });
+      qc.invalidateQueries({ queryKey: extWorkflowKeys.all(wsId) });
+      if (payload?.issue_id) {
+        qc.invalidateQueries({ queryKey: issueKeys.detail(wsId, payload.issue_id) });
+        qc.invalidateQueries({ queryKey: issueKeys.children(wsId, payload.issue_id) });
+      }
+    });
+
     const unsubIssueLabelsChanged = ws.on("issue_labels:changed", (p) => {
       const { issue_id, labels, issue_revision } = p as IssueLabelsChangedPayload;
       if (!issue_id) return;
@@ -1813,6 +1841,7 @@
       unsubIssueUpdated();
       unsubIssueCreated();
       unsubIssueDeleted();
+      unsubExtWorkflowRunUpdated();
       unsubIssueAttachmentsChanged();
       unsubIssueLabelsChanged();
       unsubIssueMetadataChanged();
```

**3b. Types, modal key, actor resolution**

```diff
--- a/packages/core/types/issue.ts
+++ b/packages/core/types/issue.ts
@@ -32,7 +32,8 @@
 
 export type IssuePriority = "urgent" | "high" | "medium" | "low" | "none";
 
-export type IssueAssigneeType = "member" | "agent" | "squad";
+// ext-workflow: "workflow" assigns an issue to a workflow template (starts a run).
+export type IssueAssigneeType = "member" | "agent" | "squad" | "workflow";
 
 export interface IssueReaction {
   id: string;
```
```diff
--- a/packages/core/modals/store.ts
+++ b/packages/core/modals/store.ts
@@ -7,6 +7,7 @@
   | "quick-create-issue"
   | "create-project"
   | "create-squad"
+  | "create-ext-workflow"
   | "feedback"
   | "issue-set-parent"
   | "issue-mark-duplicate"
```
```diff
--- a/packages/core/workspace/hooks.ts
+++ b/packages/core/workspace/hooks.ts
@@ -3,6 +3,8 @@
 import { useCallback, useMemo } from "react";
 import { useQuery } from "@tanstack/react-query";
 import type { Agent, MemberWithUser, Squad, Workspace } from "../types";
+import type { ExtWorkflow } from "../ext-workflows/types";
+import { extWorkflowListOptions } from "../ext-workflows/queries";
 import { useWorkspaceId } from "../hooks";
 import {
   memberListOptions,
@@ -28,6 +30,7 @@
 const EMPTY_MEMBERS: MemberWithUser[] = [];
 const EMPTY_AGENTS: Agent[] = [];
 const EMPTY_SQUADS: Squad[] = [];
+const EMPTY_EXT_WORKFLOWS: ExtWorkflow[] = [];
 const EMPTY_WORKSPACES: Workspace[] = [];
 
 /**
@@ -66,6 +69,8 @@
   members: readonly { user_id: string; name: string }[];
   agents: readonly { id: string; name: string }[];
   squads: readonly { id: string; name: string }[];
+  /** ext-workflow: optional so callers that never name a workflow stay unchanged. */
+  workflows?: readonly { id: string; name: string }[];
   /**
    * Installed plugins. Optional because most callers have no reason to load
    * them, and an event-written row then falls back to a generic "Plugin" —
@@ -76,11 +81,13 @@
   const memberNames = new Map(directories.members.map((m) => [m.user_id, m.name]));
   const agentNames = new Map(directories.agents.map((a) => [a.id, a.name]));
   const squadNames = new Map(directories.squads.map((s) => [s.id, s.name]));
+  const workflowNames = new Map((directories.workflows ?? []).map((w) => [w.id, w.name]));
   const pluginNames = new Map((directories.plugins ?? []).map((p) => [p.id, p.name]));
   return (type: string, id: string) => {
     if (type === "member") return memberNames.get(id) ?? "Unknown";
     if (type === "agent") return agentNames.get(id) ?? "Unknown Agent";
     if (type === "squad") return squadNames.get(id) ?? "Unknown Squad";
+    if (type === "workflow") return workflowNames.get(id) ?? "Unknown Workflow";
     // An event-triggered hook writes as the installation itself: there is no
     // person behind it, and borrowing the last member who touched the issue
     // would be a lie the audit trail cannot undo. An id that no longer
@@ -96,9 +103,11 @@
   const { data: memberData } = useQuery(memberListOptions(wsId));
   const { data: agentData } = useQuery(agentListOptions(wsId));
   const { data: squadData } = useQuery(squadListOptions(wsId));
+  const { data: workflowData } = useQuery(extWorkflowListOptions(wsId));
   const members = memberData ?? EMPTY_MEMBERS;
   const agents = agentData ?? EMPTY_AGENTS;
   const squads = squadData ?? EMPTY_SQUADS;
+  const workflows = workflowData ?? EMPTY_EXT_WORKFLOWS;
   // Only for naming a plugin-authored row. Gated on the flag so a workspace
   // without plugins does not fetch a list it can never render an author from.
   const pluginsEnabled = useFeatureEnabled(PLUGINS_V1_FLAG, false);
@@ -123,8 +132,8 @@
   }, [squads]);
 
   const getActorName = useMemo(
-    () => buildActorNameResolver({ members, agents, squads, plugins: pluginData?.plugins }),
-    [agents, members, squads, pluginData],
+    () => buildActorNameResolver({ members, agents, squads, workflows, plugins: pluginData?.plugins }),
+    [agents, members, squads, workflows, pluginData],
   );
 
   const getActorInitials = useCallback(
@@ -144,8 +153,9 @@
     if (type === "member") return resolvePublicFileUrl(members.find((m) => m.user_id === id)?.avatar_url);
     if (type === "agent") return resolvePublicFileUrl(agents.find((a) => a.id === id)?.avatar_url);
     if (type === "squad") return resolvePublicFileUrl(squads.find((s) => s.id === id)?.avatar_url);
+    if (type === "workflow") return resolvePublicFileUrl(workflows.find((w) => w.id === id)?.avatar_url);
     return null;
-  }, [agents, members, squads]);
+  }, [agents, members, squads, workflows]);
 
   const hasActor = useCallback(
     (type: string, id: string): boolean | undefined => {
@@ -166,12 +176,17 @@
           ? undefined
           : squads.some((s) => s.id === id);
       }
+      if (type === "workflow") {
+        return workflowData === undefined
+          ? undefined
+          : workflows.some((w) => w.id === id);
+      }
       if (type === "plugin") {
         return pluginData?.plugins.some((p) => p.id === id);
       }
       return type === "system";
     },
-    [agentData, agents, memberData, members, pluginData, squadData, squads],
+    [agentData, agents, memberData, members, pluginData, squadData, squads, workflowData, workflows],
   );
 
   return useMemo(
```

**3c. Paths, route icons, reserved slug, diagnostics**

```diff
--- a/packages/core/paths/paths.ts
+++ b/packages/core/paths/paths.ts
@@ -53,6 +53,9 @@
     memberDetail: (id: string) => `${ws}/members/${encode(id)}`,
     squads: () => `${ws}/squads`,
     squadDetail: (id: string) => `${ws}/squads/${encode(id)}`,
+    // ext-workflow: fork-only workflows pages.
+    workflows: () => `${ws}/workflows`,
+    workflowDetail: (id: string) => `${ws}/workflows/${encode(id)}`,
     inbox: () => `${ws}/inbox`,
     chat: () => `${ws}/chat`,
     chatWithAgent: (agentId: string) =>
```
```diff
--- a/packages/core/paths/route-icons.ts
+++ b/packages/core/paths/route-icons.ts
@@ -28,6 +28,7 @@
   | "Zap"
   | "Bot"
   | "Users"
+  | "Workflow"
   | "BarChart3"
   | "Monitor"
   | "Server"
@@ -52,6 +53,7 @@
   | "autopilots"
   | "agents"
   | "squads"
+  | "workflows"
   | "usage"
   | "runtimes"
   | "skills"
@@ -67,6 +69,7 @@
   | "autopilots"
   | "agents"
   | "squads"
+  | "workflows"
   | "usage"
   | "runtimes"
   | "skills"
@@ -94,6 +97,8 @@
   autopilots: { segment: "autopilots", icon: "Zap", navKey: "autopilots" },
   agents: { segment: "agents", icon: "Bot", navKey: "agents" },
   squads: { segment: "squads", icon: "Users", navKey: "squads" },
+  // ext-workflow
+  workflows: { segment: "workflows", icon: "Workflow", navKey: "workflows" },
   usage: { segment: "usage", icon: "BarChart3", navKey: "usage" },
   runtimes: { segment: "runtimes", icon: "Monitor", navKey: "runtimes" },
   skills: { segment: "skills", icon: "BookOpenText", navKey: "skills" },
```
Then run `pnpm generate:reserved-slugs` from the repo root (rewrites `packages/core/paths/reserved-slugs.ts`; the resulting hunk adds `"workflows",` after `"squads",`).

```diff
--- a/server/internal/handler/reserved_slugs.json
+++ b/server/internal/handler/reserved_slugs.json
@@ -74,6 +74,7 @@
         "autopilots",
         "agents",
         "squads",
+        "workflows",
         "inbox",
         "my-issues",
         "usage",
```
```diff
--- a/packages/core/diagnostics/diagnostic-context.ts
+++ b/packages/core/diagnostics/diagnostic-context.ts
@@ -68,6 +68,9 @@
   ["members", ":id"],
   ["squads"],
   ["squads", ":id"],
+  // ext-workflow
+  ["workflows"],
+  ["workflows", ":id"],
   ["inbox"],
   ["chat"],
   ["my-issues"],
```

**3d. Ripple fixes that keep views and mobile green**

```diff
--- a/packages/views/layout/route-icon-components.tsx
+++ b/packages/views/layout/route-icon-components.tsx
@@ -7,6 +7,7 @@
   Zap,
   Bot,
   Users,
+  Workflow,
   BarChart3,
   Monitor,
   Server,
@@ -40,6 +41,7 @@
   Zap,
   Bot,
   Users,
+  Workflow,
   BarChart3,
   Monitor,
   Server,
```
```diff
--- a/packages/views/search/search-command.tsx
+++ b/packages/views/search/search-command.tsx
@@ -98,6 +98,7 @@
   autopilots: ["autopilot", "autopilots", "automation", "schedule", "cron", "webhook", "自动化", "定时"],
   agents: ["agents", "bots", "ai", "智能体"],
   squads: ["squads", "teams", "小队", "团队"],
+  workflows: ["workflows", "pipeline", "dag", "工作流", "流水线"],
   usage: ["usage", "analytics", "stats", "metrics", "统计", "分析", "用量"],
   runtimes: ["runtimes", "environments", "machines", "运行时"],
   skills: ["skills", "library", "技能"],
```
```diff
--- a/packages/views/locales/en/layout.json
+++ b/packages/views/locales/en/layout.json
@@ -8,6 +8,7 @@
     "autopilots": "Autopilot",
     "agents": "Agents",
     "squads": "Squads",
+    "workflows": "Workflows",
     "usage": "Analytics",
     "runtimes": "Runtimes",
     "skills": "Skills",
```
```diff
--- a/packages/views/locales/fr/layout.json
+++ b/packages/views/locales/fr/layout.json
@@ -8,6 +8,7 @@
     "autopilots": "Automatisation",
     "agents": "Agents",
     "squads": "Squads",
+    "workflows": "Workflows",
     "usage": "Statistiques",
     "runtimes": "Runtimes",
     "skills": "Skills",
```
```diff
--- a/packages/views/locales/ja/layout.json
+++ b/packages/views/locales/ja/layout.json
@@ -8,6 +8,7 @@
     "autopilots": "オートパイロット",
     "agents": "エージェント",
     "squads": "スクワッド",
+    "workflows": "ワークフロー",
     "usage": "分析",
     "runtimes": "ランタイム",
     "skills": "スキル",
```
```diff
--- a/packages/views/locales/ko/layout.json
+++ b/packages/views/locales/ko/layout.json
@@ -8,6 +8,7 @@
     "autopilots": "오토파일럿",
     "agents": "에이전트",
     "squads": "스쿼드",
+    "workflows": "워크플로",
     "usage": "분석",
     "runtimes": "런타임",
     "skills": "스킬",
```
```diff
--- a/packages/views/locales/zh-Hans/layout.json
+++ b/packages/views/locales/zh-Hans/layout.json
@@ -8,6 +8,7 @@
     "autopilots": "自动化",
     "agents": "智能体",
     "squads": "小队",
+    "workflows": "工作流",
     "usage": "统计",
     "runtimes": "运行时",
     "skills": "Skills",
```
```diff
--- a/apps/mobile/data/use-actor-name.ts
+++ b/apps/mobile/data/use-actor-name.ts
@@ -19,10 +19,12 @@
   const { data: squads = [] } = useQuery(squadListOptions(wsId));
 
   const getName = (
-    type: "member" | "agent" | "squad" | null | undefined,
+    type: "member" | "agent" | "squad" | "workflow" | null | undefined,
     id: string | null | undefined,
   ): string => {
     if (!type || !id) return "System";
+    // ext-workflow: mobile has no workflow directory; show a generic name.
+    if (type === "workflow") return "Workflow";
     if (type === "member") {
       const m = members.find((m) => m.user_id === id);
       return m?.name ?? "Unknown";
@@ -35,10 +37,11 @@
   };
 
   const getAvatarUrl = (
-    type: "member" | "agent" | "squad" | null | undefined,
+    type: "member" | "agent" | "squad" | "workflow" | null | undefined,
     id: string | null | undefined,
   ): string | null => {
     if (!type || !id) return null;
+    if (type === "workflow") return null;
     if (type === "member") {
       return members.find((m) => m.user_id === id)?.avatar_url ?? null;
     }
```
```diff
--- a/apps/mobile/components/ui/actor-avatar.tsx
+++ b/apps/mobile/components/ui/actor-avatar.tsx
@@ -34,7 +34,8 @@
 // a squad has an avatar_url we render it; otherwise fall back to a generic
 // group glyph so squad-assigned issues from web never render blank.
 interface Props {
-  type: "member" | "agent" | "system" | "squad" | null | undefined;
+  // ext-workflow: "workflow" renders a generic glyph; mobile has no workflow directory.
+  type: "member" | "agent" | "system" | "squad" | "workflow" | null | undefined;
   id: string | null | undefined;
   /** Timeline-provided identity for actors no longer in the live directory. */
   name?: string;
@@ -100,7 +101,7 @@
   // Squad gets a soft-square tile (matches web actor-avatar.tsx:42 which uses
   // rounded-md) so a group never reads as a single person at a glance.
   // Everyone else stays round.
-  const radius = type === "squad" ? Math.round(size * 0.22) : size / 2;
+  const radius = type === "squad" || type === "workflow" ? Math.round(size * 0.22) : size / 2;
 
   // URL lookup runs BEFORE the squad/system icon fallbacks so a squad with
   // an avatar_url renders its image instead of the generic group glyph.
@@ -173,6 +174,17 @@
       </View>
     );
   }
+
+  if (type === "workflow") {
+    return (
+      <View
+        style={{ width: size, height: size, borderRadius: radius }}
+        className="items-center justify-center bg-muted"
+      >
+        <Ionicons name="git-network" size={Math.round(size * 0.55)} color={iconColor} />
+      </View>
+    );
+  }
 
   const isAgent = type === "agent";
   return (
```

- [ ] **Step 4: Run, expect PASS**

```bash
pnpm generate:reserved-slugs && git diff --stat packages/core/paths/reserved-slugs.ts
pnpm --filter @multica/core test            # all green
pnpm --filter @multica/core typecheck
pnpm --filter @multica/views typecheck
pnpm --filter @multica/views test -- search layout     # green
pnpm --filter @multica/mobile typecheck
cd server && go test ./internal/handler -run 'TestCreateWorkspace' -count=1   # reserved slugs still consistent on the Go side
pnpm lint
```

(`packages/views` has one pre-existing unrelated failure on a clean tree: `editor/extensions/select-all-delete.test.ts`.)

- [ ] **Step 5: Commit**

```bash
git add packages/core packages/views apps/mobile server/internal/handler/reserved_slugs.json
git commit -m "feat(ext-workflow): wire workflow realtime, paths, actor resolution and assignee type

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

# Part B

# Part B: Workflow engine

## Notes for integrator

**Verification status.** Every code block below was applied in task order to a scratch copy of `server/` that also carried Part A's migrations, `ext_workflow.sql`, `definition.go` and handler files (extracted from `part-a.md`). It was regenerated with `make sqlc` (sqlc v1.31.1), migrated on Postgres 17 and run there:
- `internal/extworkflow` passes, including under `-race`.
- The whole `internal/service` package passes.
- `internal/scheduler` passes.
- `internal/handler` passes its ext tests. In the full-package run the only failures are environmental: Part A's workspace-deletion manifest edit was not applied in scratch, and the plugin example tests need `examples/` outside `server/`.
- The `cmd/server` ext tests pass.
- gofmt reports nothing for the new and edited files.

Not run: `make check`, Playwright, and the frontend.

The reconstruction from this file reproduces the verified tree byte for byte: every `server/...` code block was written, every diff was applied with `git apply`, and `make sqlc`, `go build ./...` and `go vet` all ran clean.

Scratch side effects:
- A throwaway database, `extwf_partb_verify` in the `multica-wf-test-pg` container, was created for these runs and then dropped.
- An early `rsync` of the repo's `server/` overwrote files in `scratchpad/srv/server`, which is Part A's scratch copy. `part-a.md` is unaffected.

**Contract deviations (with reason)**

1. **The hook field is `TaskService.ExtWorkflow ExtWorkflowHooks`, not `IssueService.ExtWorkflow`.**
   - Why: every `IssueWakeupService` is built on the spot from a TaskService (`&service.IssueWakeupService{Tasks: h.TaskService}` in `cmd/server/main.go:831/834` and `handler/issue_child_event.go:15`, among others). `processChildEvents` can therefore reach the hook only through TaskService.
   - `IssueService` uses `s.TaskService.ExtWorkflow`.
   - Wiring is `h.TaskService.ExtWorkflow = engine`. nil means disabled.
2. **`ExtWorkflowTaskParams` adds two fields.**
   - `ActorUserID pgtype.UUID`: the accountable member, which is the run's triggering member.
   - `TriggerCommentID pgtype.UUID`: set for `conversation` tasks. It puts the task in the member comment's thread slot of the pending-task unique index, so it does not collide with a pending review task on the parent.
3. **`EnqueueExtWorkflowTask` does not publish.** It runs inside the caller's uncommitted transaction. After commit, call `TaskService.PublishExtWorkflowTaskQueued(ctx, task)`, which sends `task:queued` and the runtime wakeup.
4. **Child issues are not created through `IssueService.Create`.** Create opens its own transaction and runs `maybeEnqueueOnAssign` after commit; spec §4.2 needs one transaction and no auto-enqueue.
   - The engine uses `IssueService.ExtCreateChildIssueTx(ctx, tx, IssueCreateParams)` instead: the same numbering, issue-count policy and positioning, with no duplicate guard.
   - After commit, it calls `ExtPublishIssueCreated`.
5. **New migration `ext_0015_run_step_supervisor_wakes`.**
   - It adds `ext_workflow_run_step.supervisor_wakes INT NOT NULL DEFAULT 0`, which implements "re-wake once, then escalate".
   - Part A reserved `ext_0015+` for later parts.
   - It adds no table, so the workspace-deletion manifest needs no change.
6. **Additions to the contract:**
   - Engine methods `OnParentDeleted(ctx, issueID)` (called before `CancelTasksForIssue` in the delete paths) and `Advance`, `LoadRun`.
   - Exported `LoadRunSnapshot`, `RunSnapshot` and `ValidateDecision`.
   - Constants `InboxTypeEscalation` and `service.ExtWorkflowEngineCancelReason`. The engine stamps the latter as `failure_reason='ext_workflow_engine'` on the tasks it cancels.
7. **`AgentAccess` carries no originator.** The bridge judges a `member` actor as that member. An `agent` actor is judged as an agent with no human originator, so only workspace-invocable agents qualify. Assignment-time (`validateAssigneePair`) and dispatch-time checks use the same rule, so they never disagree.

**Exact signatures Part C must use**

- **Pure layer:** `Next(rs RunState, key string, ev Event) (RunState, []Effect, error)`.
  - Event kinds: `EvTick, EvStepFinished, EvStepFailed, EvDecision, EvSupervisorNoDecision, EvChildCancelled, EvSummaryEnded, EvCancelRun`.
  - Every action from a comment or the API is `Event{Kind: EvDecision, Decision: d}`, including `request-rewind` (the step must be `running`) and `abort` (`key` may be empty).
- **Applying a decision.** `Engine.Decide` should be thin glue:
  1. Resolve the step id to its key with `snap.KeyOf(stepID)`.
  2. Check permission and return `ErrForbidden` on failure.
  3. Call:
     ```go
     engine.Advance(ctx, runID, extworkflow.AdvanceInput{
         StepKey: key,
         Event:   extworkflow.Event{Kind: extworkflow.EvDecision, Decision: d},
         Actor:   extworkflow.Actor{Type: "member" /* or "agent" */, ID: actorID, OnBehalfOf: commenterID, TaskID: decidingTaskID},
         ExpectedStatus: extworkflow.StepStatus(req.ExpectedStatus), // "" = no check
     })
     ```
  - Error mapping: `ErrStatusMismatch` → 409, `ErrIllegalDecision` → 422, `ErrRunNotFound` → 404, `ErrEngineDisabled` → 409.
  - `Actor.TaskID` is spared by step and run cancellations, so the deciding turn is not killed.
  - Validate the fields with `ValidateDecision(d)`. `ParseBlock` already calls it.
- **Cancel run:** `engine.Advance(ctx, runID, AdvanceInput{Event: Event{Kind: EvCancelRun, Reason: "…"}, Actor: Actor{Type: "member", ID: userID}})`.
- **Loading run state:**
  - Use `engine.LoadRun(ctx, runID) (*RunSnapshot, error)`, or `LoadRunSnapshot(ctx, q, run)` inside a transaction.
  - `RunSnapshot` has `Run`, `Def`, `Steps` (in node order), `ByKey`, `Active` (in-flight ext tasks, with `ExtWorkflowRole/Kind/StepID`) and `State`.
  - It provides `KeyOf(stepID)`, and `State.AllSettled()` is available.
- **Read queries for the run API:**
  - Summary lists: `ListExtWorkflowRunSummariesByIssue{IssueID, WorkspaceID}`, `ListExtWorkflowRunSummariesByWorkflow{WorkflowID, WorkspaceID, PageLimit, PageOffset}`, `CountExtWorkflowRunsByWorkflow`, `GetExtWorkflowRunSummary{ID, WorkspaceID}`. The rows add `WorkflowName string`, `IssueNumber pgtype.Int4` and `IssueTitle pgtype.Text`. `max_rewinds` and the node titles, attempts and review flags are in the `Definition` JSON.
  - Run details: `GetExtWorkflowRunInWorkspace`, `ListExtWorkflowRunSteps`, `ListExtWorkflowRunEvents`.
  - `step_of`: `GetExtWorkflowRunStepByIssue{IssueID, WorkspaceID}`.
  - Timeline writes such as `protocol_error`: `CreateExtWorkflowRunEvent`.
- **Conversation wake:**
  1. Call `TaskService.EnqueueExtWorkflowTask(ctx, tx, service.ExtWorkflowTaskParams{IssueID: parent, AgentID: supervisor, RunID: run, Role: "supervisor", Kind: "conversation", TriggerCommentID: commentID, ActorUserID: commenter, HandoffNote: "…"})`.
  2. Commit.
  3. Call `PublishExtWorkflowTaskQueued`.
  - Conversation tasks do not count as "supervisor busy". When one ends, `OnTaskTerminal` ticks the run.
- **Briefing staleness**, matching the engine:
  - A step task is current only if its step is `running` and it is that step's newest `role=step` task (`GetLatestExtWorkflowTaskForStep{StepID, Role}`).
  - A review, failure or rewind_request task is current only if the step is `awaiting_supervisor` and `pending_reason == kind`.
  - A summary task is current only if the run is active and `State.AllSettled()`.
- **Briefing inputs from the timeline:**
  - Failure: the last `step_failed` payload: `reason`, which is `ended_without_finishing`, `dispatch_failed`, `cancelled` or the task's `failure_reason`; plus `error` and `task_id`.
  - Re-wake error: the last `protocol_error` with `reason:"no_decision"`, `detail` and `wake`.
  - Rewind request: the last `rewind_requested` payload: `to`, `reason` and `budget_exhausted`.
- **Engine-written system comments.** The engine writes system comments (`author_type='system'`, `type='system'`) on the parent for run started, escalation, finished, failed and cancelled. `OnComment` must ignore non-agent authors.
- **Escalation inbox.** The item type is `extworkflow.InboxTypeEscalation` (`"ext_workflow_escalation"`, `action_required`). Its details are `{run_id, step_id, node_key, reason}`. The recipient is the triggering member, otherwise the workflow creator.

**Test harness helper names**

- **`internal/extworkflow` (`testenv_test.go`, `e2e_test.go`):**
  - Setup: `newEnv(t) *env`, with fields `engine`, `tasks` (with `ExtWorkflow` wired), `issues`, `q`, `fx`, `ws`, `user`, `access *fakeAccess` (`deny(agentID)`) and `pub *recPublisher` (`count()`).
  - Fixtures: `e.agent(t, name)`, `e.workflow(t, supervisor, maxRewinds, wfNode{key, title, agent, review, maxAttempts, deps}...)`, `e.parentIssue(t, workflowID, status)`, `e.start(t, parent)`.
  - Reads: `e.run(t, parent)`, `e.step(t, run, key)`, `e.issue(t, id)`, `e.latestTask(t, step, role)`, `e.countTasks(t, step, role)`, `e.runEvents(t, run)`, `e.summaryTask(t, run)`.
  - Assertions: `wantStepRow(t, step, status, attempts)`, `e.wantRunDone(t, parent)`.
  - Actions: `e.endTask(t, task, status)` (status write + `OnTaskTerminal`), `e.setStatus(t, issueID, status)`, `e.childMoves(t, parent, child, status)` (status write + the real `ProcessChildEvents` hook), `e.running(t, task)`, `e.decide(t, run, key, decision, actor)`.
  - Avoid Part A's test helper names in this package: `node`, `def`, `diamond`, `sortedKeys`, `orEmpty`.
- **`internal/handler`:** `withExtWorkflowEngine(t)`, `setExtWorkflowEngine(t, engine|nil)`, `hookWorkflow(t, nodeAgent) string`, `createWorkflowIssue(t, workflowID, status)`, `cleanupWorkflowIssue(t, issueID)`, `activeRunStatus(t, issueID)`.
- **`internal/service`:** `fakeExtHooks`, `newExtTaskFixture`, `extWorkflowServiceFixture`, `workflowFamily`.

**Commands**

- DB-backed tests skip silently without a database, so use `-v` to confirm PASS: `make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -count=1 -v'"`.
- After B3, apply the new migration: `make env-exec ARGS="-- bash -c 'cd server && go run ./cmd/migrate up'"`.
- `make sqlc` works offline.
- Only run gofmt on the files you touched: several upstream files are already not gofmt-clean.

**Ordering**

- B1 → B10 are sequential.
- B1 needs Part A's `definition.go` (A3).
- B3 needs A1 and A2.
- B8's tests use Part A's handler test helpers (A4).

**Known limitations (deliberate)**

- A person reopening a finished child leaves its step `done`.
- A person moving a pending child out of backlog gets the platform's plain agent run. If that run is still queued, the engine adopts it when it dispatches the step.
- Agent-triggered runs are judged without a human originator (deviation 7).

## Tasks

- B1 Pure run state machine (`errors.go`, `transitions.go`)
- B2 Comment decision block parser (`block.go`)
- B3 Run/step/event/engine queries, `ext_0015`, `CreateRetryTask` copy
- B4 Service seam: hooks, `EnqueueExtWorkflowTask`, child issues
- B5 Engine core: `Advance`, effects, `ValidateAssignment`, `StartRun`
- B6 Observation: `OnTaskTerminal`, `OnChildEvents`, `OnParentChanged`, `OnParentDeleted`, `Reconcile`
- B7 Service hooks: `WillEnqueueRun`, create, `processChildEvents`, `RerunIssue`
- B8 Handler hooks and bridge
- B9 Server wiring: kill switch, constraint check, listeners, reconcile job
- B10 Engine end-to-end tests


---

### Task B1: Pure run state machine (`errors.go`, `transitions.go`)

**Files:**
- Create: `server/internal/extworkflow/errors.go`
- Create: `server/internal/extworkflow/transitions.go`
- Test: `server/internal/extworkflow/transitions_test.go`

**Interfaces:**
- Consumes (Part A, `definition.go`): `Definition`, `Node`, `(Definition).NodeByKey`, `Ancestors`, `Descendants`.
- Produces (used by B5/B6/B10 and Part C):
  - `type StepStatus string` + `Terminal() / Awaiting() / Settled()`; `type PendingReason string`; `type RunStatus string` + `Active()`; `type DecisionAction string`; `type Decision struct{Action DecisionAction; Step, To, Reason, Feedback string}`
  - `type StepState struct{Key string; Status StepStatus; Attempts int; PendingReason PendingReason; LastFeedback, EscalationReason string; SupervisorWakes int}`
  - `type RunState struct{Def Definition; Status RunStatus; RewindsUsed int; Steps map[string]StepState; SupervisorBusy bool; SupervisorFor map[string]bool; SummaryRequested bool}`; `NewRunState(Definition) RunState`; `(RunState).Clone()`; `(RunState).AllSettled()`
  - `type EventKind string` (`EvTick, EvStepFinished, EvStepFailed, EvDecision, EvSupervisorNoDecision, EvChildCancelled, EvSummaryEnded, EvCancelRun`); `type Event struct{Kind EventKind; Reason string; Decision Decision; Detail map[string]any}`
  - `type EffectKind string`; `type Effect struct{Kind EffectKind; Step, IssueStatus, SupervisorKind, Reason, Milestone, RunEvent string; Payload map[string]any; Engine bool}`
  - `func Next(rs RunState, key string, ev Event) (RunState, []Effect, error)`
  - Sentinels `ErrEngineDisabled, ErrWorkflowNotFound, ErrWorkflowArchived, ErrNoNodes, ErrSupervisorUnavailable, ErrAgentNotInvokable, ErrStatusMismatch, ErrIllegalDecision, ErrForbidden, ErrRunNotFound, ErrNoTransition`; `type AssignError struct{Status int; Code, Message string; Err error}`
  - Constants `RoleStep/RoleSupervisor`, `KindStep/KindReview/KindFailure/KindRewindRequest/KindSummary/KindConversation`, `MaxSupervisorWakes = 2`, `RunEvent*` timeline kinds, `Milestone*`.

Semantics of `Next` (whole-run, pure): it applies one event to one step (`key`, empty for run-level events), then runs the scheduler: start every pending step whose dependencies are all done/skipped (attempts+1), wake the supervisor for at most one awaiting step when no supervisor task is in flight, request the summary when every step is settled, and set `waiting_human` while any step is `awaiting_human`. Observation events that do not apply return `ErrNoTransition` (the engine downgrades them to a tick); decisions that do not apply return `ErrStatusMismatch` (409) or `ErrIllegalDecision` (422). On error the input state is returned unchanged. Supervisor demand is derived from state: a step in `awaiting_supervisor` with `SupervisorWakes < 2` and no in-flight supervisor task gets woken; `EvSupervisorNoDecision` with `SupervisorWakes >= 2` escalates to `awaiting_human`.

- [ ] **Step 1: Write the failing test**

`server/internal/extworkflow/transitions_test.go`:

```go
package extworkflow

import (
	"errors"
	"reflect"
	"testing"
)

// testDef is the run every table case starts from:
//
//	spec ──► backend (review, max 2) ──► qa
//	  └────► docs
func testDef() Definition {
	return Definition{
		SupervisorAgentID: "00000000-0000-0000-0000-0000000000aa",
		MaxRewinds:        2,
		Nodes: []Node{
			{Key: "spec", Title: "Spec", AgentID: "00000000-0000-0000-0000-000000000001", MaxAttempts: 3},
			{Key: "backend", Title: "Backend", AgentID: "00000000-0000-0000-0000-000000000002", RequiresReview: true, MaxAttempts: 2, DependsOn: []string{"spec"}},
			{Key: "qa", Title: "QA", AgentID: "00000000-0000-0000-0000-000000000003", MaxAttempts: 3, DependsOn: []string{"backend"}},
			{Key: "docs", Title: "Docs", AgentID: "00000000-0000-0000-0000-000000000004", MaxAttempts: 3, DependsOn: []string{"spec"}},
		},
	}
}

func state(mods ...func(*RunState)) RunState {
	rs := NewRunState(testDef())
	for _, m := range mods {
		m(&rs)
	}
	return rs
}

func step(key string, status StepStatus, attempts int, opts ...func(*StepState)) func(*RunState) {
	return func(rs *RunState) {
		st := rs.Steps[key]
		st.Status = status
		st.Attempts = attempts
		for _, o := range opts {
			o(&st)
		}
		rs.Steps[key] = st
	}
}

func because(r PendingReason) func(*StepState) { return func(s *StepState) { s.PendingReason = r } }
func woken(n int) func(*StepState)             { return func(s *StepState) { s.SupervisorWakes = n } }
func supervising(key string) func(*RunState) {
	return func(rs *RunState) { rs.SupervisorFor[key] = true; rs.SupervisorBusy = true }
}
func busy(rs *RunState)                     { rs.SupervisorBusy = true }
func rewinds(n int) func(*RunState)         { return func(rs *RunState) { rs.RewindsUsed = n } }
func summaryRequested(rs *RunState)         { rs.SummaryRequested = true }
func runStatus(s RunStatus) func(*RunState) { return func(rs *RunState) { rs.Status = s } }
func decision(d Decision) Event             { return Event{Kind: EvDecision, Decision: d} }

func sig(e Effect) string {
	switch e.Kind {
	case EffEnqueueStep:
		return "enqueue_step:" + e.Step
	case EffEnqueueSupervisor:
		return "enqueue_supervisor:" + e.Step + "/" + e.SupervisorKind
	case EffSetChildStatus:
		return "child:" + e.Step + "=" + e.IssueStatus
	case EffSetParentStatus:
		return "parent=" + e.IssueStatus
	case EffCancelStepWork:
		return "cancel_step:" + e.Step
	case EffCancelRunTasks:
		return "cancel_run"
	case EffEscalate:
		return "escalate:" + e.Step
	case EffMilestone:
		return "milestone:" + e.Milestone
	case EffRecordEvent:
		return "event:" + e.RunEvent + "@" + e.Step
	}
	return "unknown:" + string(e.Kind)
}

func sigs(effs []Effect) []string {
	out := []string{}
	for _, e := range effs {
		out = append(out, sig(e))
	}
	return out
}

func wantStep(t *testing.T, rs RunState, key string, status StepStatus, attempts int) StepState {
	t.Helper()
	st := rs.Steps[key]
	if st.Status != status || st.Attempts != attempts {
		t.Fatalf("step %s = %s/%d, want %s/%d", key, st.Status, st.Attempts, status, attempts)
	}
	return st
}

func payloadOf(t *testing.T, effs []Effect, runEvent string) map[string]any {
	t.Helper()
	for _, e := range effs {
		if e.Kind == EffRecordEvent && e.RunEvent == runEvent {
			return e.Payload
		}
	}
	t.Fatalf("no %s event in %v", runEvent, sigs(effs))
	return nil
}

func TestNext(t *testing.T) {
	cases := []struct {
		name    string
		state   RunState
		key     string
		event   Event
		wantErr error
		want    []string
		check   func(t *testing.T, got RunState, effs []Effect)
	}{
		{
			name:  "tick starts the roots",
			state: state(),
			event: Event{Kind: EvTick},
			want:  []string{"child:spec=in_progress", "enqueue_step:spec", "event:step_started@spec"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "spec", StepRunning, 1)
				wantStep(t, got, "backend", StepPending, 0)
			},
		},
		{
			name:  "idle tick does nothing",
			state: state(step("spec", StepRunning, 1)),
			event: Event{Kind: EvTick},
			want:  []string{},
		},
		{
			name:  "finishing a step without review starts its dependents",
			state: state(step("spec", StepRunning, 1)),
			key:   "spec",
			event: Event{Kind: EvStepFinished},
			want: []string{
				"child:spec=done", "event:step_finished@spec",
				"child:backend=in_progress", "enqueue_step:backend", "event:step_started@backend",
				"child:docs=in_progress", "enqueue_step:docs", "event:step_started@docs",
			},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "spec", StepDone, 1)
				wantStep(t, got, "backend", StepRunning, 1)
				wantStep(t, got, "qa", StepPending, 0)
			},
		},
		{
			name:  "finishing a review step wakes the supervisor",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepRunning, 1)),
			key:   "backend",
			event: Event{Kind: EvStepFinished},
			want:  []string{"event:step_finished@backend", "enqueue_supervisor:backend/review"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				st := wantStep(t, got, "backend", StepAwaitingSupervisor, 1)
				if st.PendingReason != PendingReview || st.SupervisorWakes != 1 || !got.SupervisorBusy || !got.SupervisorFor["backend"] {
					t.Fatalf("backend = %+v busy=%v for=%v", st, got.SupervisorBusy, got.SupervisorFor)
				}
			},
		},
		{
			name:  "a busy supervisor defers the wake",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepRunning, 1), busy),
			key:   "backend",
			event: Event{Kind: EvStepFinished},
			want:  []string{"event:step_finished@backend"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				if st := wantStep(t, got, "backend", StepAwaitingSupervisor, 1); st.SupervisorWakes != 0 {
					t.Fatalf("wakes = %d, want 0 while the supervisor is busy", st.SupervisorWakes)
				}
			},
		},
		{
			name:    "finishing a pending step is not a transition",
			state:   state(),
			key:     "backend",
			event:   Event{Kind: EvStepFinished},
			wantErr: ErrNoTransition,
		},
		{
			name:  "a failed step goes to the supervisor",
			state: state(step("spec", StepRunning, 1)),
			key:   "spec",
			event: Event{Kind: EvStepFailed, Reason: "agent_error", Detail: map[string]any{"error": "boom"}},
			want:  []string{"event:step_failed@spec", "enqueue_supervisor:spec/failure"},
			check: func(t *testing.T, got RunState, effs []Effect) {
				if st := wantStep(t, got, "spec", StepAwaitingSupervisor, 1); st.PendingReason != PendingFailure {
					t.Fatalf("reason = %s", st.PendingReason)
				}
				p := payloadOf(t, effs, RunEventStepFailed)
				if p["reason"] != "agent_error" || p["error"] != "boom" {
					t.Fatalf("payload = %v", p)
				}
			},
		},
		{
			name:  "request-rewind with budget left asks the supervisor",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepRunning, 1)),
			key:   "backend",
			event: decision(Decision{Action: ActionRequestRewind, To: "spec", Reason: "spec assumed pagination"}),
			want:  []string{"event:rewind_requested@backend", "enqueue_supervisor:backend/rewind_request"},
		},
		{
			name:  "request-rewind with the budget spent becomes a failure",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepRunning, 1), rewinds(2)),
			key:   "backend",
			event: decision(Decision{Action: ActionRequestRewind, To: "spec", Reason: "again"}),
			want:  []string{"event:rewind_requested@backend", "enqueue_supervisor:backend/failure"},
			check: func(t *testing.T, got RunState, effs []Effect) {
				if p := payloadOf(t, effs, RunEventRewindRequested); p["budget_exhausted"] != true {
					t.Fatalf("payload = %v", p)
				}
			},
		},
		{
			name:    "request-rewind to a non-ancestor is illegal",
			state:   state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepRunning, 1)),
			key:     "backend",
			event:   decision(Decision{Action: ActionRequestRewind, To: "docs", Reason: "x"}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:    "request-rewind needs a running step",
			state:   state(step("spec", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview))),
			key:     "backend",
			event:   decision(Decision{Action: ActionRequestRewind, Reason: "x"}),
			wantErr: ErrStatusMismatch,
		},
		{
			name: "approve finishes the step and starts its dependents",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1),
				step("backend", StepAwaitingSupervisor, 1, because(PendingReview), woken(1)), supervising("backend")),
			key:   "backend",
			event: decision(Decision{Action: ActionApprove}),
			want: []string{
				"event:decision@backend", "cancel_step:backend", "child:backend=done",
				"child:qa=in_progress", "enqueue_step:qa", "event:step_started@qa",
			},
		},
		{
			name:    "approve needs a pending decision",
			state:   state(step("spec", StepRunning, 1)),
			key:     "spec",
			event:   decision(Decision{Action: ActionApprove}),
			wantErr: ErrStatusMismatch,
		},
		{
			name:    "redo needs feedback",
			state:   state(step("spec", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview))),
			key:     "backend",
			event:   decision(Decision{Action: ActionRedo}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:  "redo restarts the step with feedback",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview), woken(1))),
			key:   "backend",
			event: decision(Decision{Action: ActionRedo, Feedback: "add tests"}),
			want:  []string{"event:decision@backend", "cancel_step:backend", "child:backend=in_progress", "enqueue_step:backend"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				st := wantStep(t, got, "backend", StepRunning, 2)
				if st.LastFeedback != "add tests" || st.PendingReason != "" || st.SupervisorWakes != 0 {
					t.Fatalf("backend = %+v", st)
				}
			},
		},
		{
			name:    "redo is refused once attempts are spent",
			state:   state(step("spec", StepDone, 1), step("backend", StepAwaitingSupervisor, 2, because(PendingReview))),
			key:     "backend",
			event:   decision(Decision{Action: ActionRedo, Feedback: "again"}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:  "retry restarts a failed step",
			state: state(step("spec", StepAwaitingSupervisor, 1, because(PendingFailure), woken(1))),
			key:   "spec",
			event: decision(Decision{Action: ActionRetry}),
			want:  []string{"event:decision@spec", "cancel_step:spec", "child:spec=in_progress", "enqueue_step:spec"},
			check: func(t *testing.T, got RunState, _ []Effect) { wantStep(t, got, "spec", StepRunning, 2) },
		},
		{
			name:  "skip settles the step and unblocks dependents",
			state: state(step("spec", StepAwaitingHuman, 3, because(PendingFailure)), runStatus(RunWaitingHuman)),
			key:   "spec",
			event: decision(Decision{Action: ActionSkip}),
			want: []string{
				"event:decision@spec", "cancel_step:spec", "child:spec=cancelled",
				"child:backend=in_progress", "enqueue_step:backend", "event:step_started@backend",
				"child:docs=in_progress", "enqueue_step:docs", "event:step_started@docs",
			},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "spec", StepSkipped, 3)
				if got.Status != RunRunning {
					t.Fatalf("run = %s, want running", got.Status)
				}
			},
		},
		{
			name:    "rewind to a non-ancestor is illegal",
			state:   state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingRewindRequest))),
			key:     "backend",
			event:   decision(Decision{Action: ActionRewind, To: "docs", Feedback: "x"}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:    "rewind needs budget",
			state:   state(step("spec", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingRewindRequest)), rewinds(2)),
			key:     "backend",
			event:   decision(Decision{Action: ActionRewind, To: "spec", Feedback: "x"}),
			wantErr: ErrIllegalDecision,
		},
		{
			name: "rewind resets the target and its started descendants",
			state: state(step("spec", StepDone, 1), step("docs", StepRunning, 1),
				step("backend", StepAwaitingSupervisor, 1, because(PendingRewindRequest), woken(1)), supervising("backend")),
			key:   "backend",
			event: decision(Decision{Action: ActionRewind, To: "spec", Feedback: "use cursor pagination"}),
			want: []string{
				"event:decision@backend", "event:rewind@backend",
				"cancel_step:spec", "child:spec=backlog",
				"cancel_step:backend", "child:backend=backlog",
				"cancel_step:docs", "child:docs=backlog",
				"child:spec=in_progress", "enqueue_step:spec", "event:step_started@spec",
			},
			check: func(t *testing.T, got RunState, effs []Effect) {
				if got.RewindsUsed != 1 {
					t.Fatalf("rewinds = %d", got.RewindsUsed)
				}
				if st := wantStep(t, got, "spec", StepRunning, 1); st.LastFeedback != "use cursor pagination" {
					t.Fatalf("spec feedback = %q", st.LastFeedback)
				}
				wantStep(t, got, "backend", StepPending, 0)
				wantStep(t, got, "docs", StepPending, 0)
				wantStep(t, got, "qa", StepPending, 0)
				if p := payloadOf(t, effs, RunEventRewind); !reflect.DeepEqual(p["reset"], []string{"spec", "backend", "docs"}) {
					t.Fatalf("reset = %v", p["reset"])
				}
			},
		},
		{
			name:  "rewind to the step itself reruns only that step",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingHuman, 2, because(PendingReview)), runStatus(RunWaitingHuman)),
			key:   "backend",
			event: decision(Decision{Action: ActionRewind, To: "backend", Feedback: "start over"}),
			want: []string{
				"event:decision@backend", "event:rewind@backend",
				"cancel_step:backend", "child:backend=backlog",
				"child:backend=in_progress", "enqueue_step:backend", "event:step_started@backend",
			},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "backend", StepRunning, 1)
				wantStep(t, got, "docs", StepDone, 1)
				if got.Status != RunRunning {
					t.Fatalf("run = %s", got.Status)
				}
			},
		},
		{
			name:  "escalate hands the step to a person",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview), woken(1)), supervising("backend")),
			key:   "backend",
			event: decision(Decision{Action: ActionEscalate, Reason: "needs product call"}),
			want:  []string{"event:decision@backend", "event:escalated@backend", "escalate:backend"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				if st := wantStep(t, got, "backend", StepAwaitingHuman, 1); st.EscalationReason != "needs product call" {
					t.Fatalf("reason = %q", st.EscalationReason)
				}
				if got.Status != RunWaitingHuman {
					t.Fatalf("run = %s, want waiting_human", got.Status)
				}
			},
		},
		{
			name:    "escalate is only for the supervisor's turn",
			state:   state(step("spec", StepAwaitingHuman, 1, because(PendingFailure)), runStatus(RunWaitingHuman)),
			key:     "spec",
			event:   decision(Decision{Action: ActionEscalate, Reason: "x"}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:  "abort fails the run",
			state: state(step("spec", StepDone, 1), step("backend", StepRunning, 1), step("docs", StepAwaitingHuman, 1, because(PendingFailure)), runStatus(RunWaitingHuman)),
			key:   "backend",
			event: decision(Decision{Action: ActionAbort, Reason: "wrong approach"}),
			want: []string{
				"event:decision@backend", "cancel_run",
				"child:backend=cancelled", "child:qa=cancelled", "child:docs=cancelled",
				"parent=blocked", "event:run_finished@", "milestone:run_failed",
			},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "spec", StepDone, 1)
				wantStep(t, got, "backend", StepFailed, 1)
				wantStep(t, got, "docs", StepCancelled, 1)
				if got.Status != RunFailed {
					t.Fatalf("run = %s", got.Status)
				}
			},
		},
		{
			name:    "abort needs a reason",
			state:   state(step("spec", StepRunning, 1)),
			event:   decision(Decision{Action: ActionAbort}),
			wantErr: ErrIllegalDecision,
		},
		{
			name:  "a silent supervisor is woken once more",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview), woken(1))),
			key:   "backend",
			event: Event{Kind: EvSupervisorNoDecision, Reason: "ended without a decision"},
			want:  []string{"event:protocol_error@backend", "enqueue_supervisor:backend/review"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				if st := got.Steps["backend"]; st.SupervisorWakes != 2 {
					t.Fatalf("wakes = %d, want 2", st.SupervisorWakes)
				}
			},
		},
		{
			name:  "a second silence escalates",
			state: state(step("spec", StepDone, 1), step("docs", StepDone, 1), step("backend", StepAwaitingSupervisor, 1, because(PendingReview), woken(2))),
			key:   "backend",
			event: Event{Kind: EvSupervisorNoDecision, Reason: "ended without a decision"},
			want:  []string{"event:escalated@backend", "escalate:backend"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "backend", StepAwaitingHuman, 1)
				if got.Status != RunWaitingHuman {
					t.Fatalf("run = %s", got.Status)
				}
			},
		},
		{
			name:    "supervisor silence on a decided step is not a transition",
			state:   state(step("spec", StepDone, 1)),
			key:     "spec",
			event:   Event{Kind: EvSupervisorNoDecision},
			wantErr: ErrNoTransition,
		},
		{
			name:  "a person cancelling the child skips the step",
			state: state(step("spec", StepRunning, 1)),
			key:   "spec",
			event: Event{Kind: EvChildCancelled},
			want: []string{
				"cancel_step:spec", "event:decision@spec",
				"child:backend=in_progress", "enqueue_step:backend", "event:step_started@backend",
				"child:docs=in_progress", "enqueue_step:docs", "event:step_started@docs",
			},
			check: func(t *testing.T, got RunState, _ []Effect) { wantStep(t, got, "spec", StepSkipped, 1) },
		},
		{
			name:  "the last settled step requests the summary",
			state: state(step("spec", StepDone, 1), step("backend", StepDone, 1), step("docs", StepDone, 1), step("qa", StepRunning, 1)),
			key:   "qa",
			event: Event{Kind: EvStepFinished},
			want:  []string{"child:qa=done", "event:step_finished@qa", "enqueue_supervisor:/summary"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				if !got.SummaryRequested || !got.SupervisorBusy {
					t.Fatalf("summary=%v busy=%v", got.SummaryRequested, got.SupervisorBusy)
				}
			},
		},
		{
			name:  "the summary ending finishes the run",
			state: state(step("spec", StepDone, 1), step("backend", StepDone, 1), step("docs", StepSkipped, 1), step("qa", StepDone, 1), summaryRequested),
			event: Event{Kind: EvSummaryEnded},
			want:  []string{"parent=in_review", "event:run_finished@", "milestone:run_finished"},
			check: func(t *testing.T, got RunState, _ []Effect) {
				if got.Status != RunDone {
					t.Fatalf("run = %s", got.Status)
				}
			},
		},
		{
			name:  "cancelling the run cancels open steps",
			state: state(step("spec", StepRunning, 1)),
			event: Event{Kind: EvCancelRun, Reason: "the parent issue was cancelled"},
			want: []string{
				"cancel_run", "child:spec=cancelled", "child:backend=cancelled", "child:qa=cancelled", "child:docs=cancelled",
				"event:run_cancelled@", "milestone:run_cancelled",
			},
			check: func(t *testing.T, got RunState, _ []Effect) {
				wantStep(t, got, "spec", StepCancelled, 1)
				if got.Status != RunCancelled {
					t.Fatalf("run = %s", got.Status)
				}
			},
		},
		{
			name:    "decisions on a finished run are a status mismatch",
			state:   state(runStatus(RunDone)),
			key:     "spec",
			event:   decision(Decision{Action: ActionApprove}),
			wantErr: ErrStatusMismatch,
		},
		{
			name:    "observations on a finished run are ignored",
			state:   state(runStatus(RunCancelled)),
			key:     "spec",
			event:   Event{Kind: EvStepFinished},
			wantErr: ErrNoTransition,
		},
		{
			name:    "unknown actions are illegal",
			state:   state(step("spec", StepAwaitingHuman, 1)),
			key:     "spec",
			event:   decision(Decision{Action: "promote"}),
			wantErr: ErrIllegalDecision,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.state.Clone()
			got, effs, err := Next(tc.state, tc.key, tc.event)
			if !reflect.DeepEqual(tc.state, before) {
				t.Fatalf("Next mutated its input")
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if !reflect.DeepEqual(got, before) {
					t.Fatalf("a rejected event changed the state")
				}
				return
			}
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if gotSigs := sigs(effs); !reflect.DeepEqual(gotSigs, tc.want) {
				t.Fatalf("effects\n got %v\nwant %v", gotSigs, tc.want)
			}
			if tc.check != nil {
				tc.check(t, got, effs)
			}
		})
	}
}

func TestScheduleMarksEngineEffects(t *testing.T) {
	_, effs, err := Next(state(step("spec", StepAwaitingHuman, 1)), "spec", decision(Decision{Action: ActionApprove}))
	if err != nil {
		t.Fatal(err)
	}
	// The decision's own three effects carry the decider; the scheduler's
	// follow-up (backend and docs starting) is the engine's.
	if len(effs) != 9 {
		t.Fatalf("effects = %v", sigs(effs))
	}
	for i, e := range effs {
		if want := i >= 3; e.Engine != want {
			t.Fatalf("effect %d %s engine=%v, want %v", i, sig(e), e.Engine, want)
		}
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `cd server && go test ./internal/extworkflow/ -run 'TestNext|TestScheduleMarksEngineEffects' -count=1`
Expected: build failure — `undefined: NewRunState`, `undefined: Next`, `undefined: EvTick`, … (only Part A's `definition.go` exists in the package).

- [ ] **Step 3: Implement**

`server/internal/extworkflow/errors.go`:

```go
package extworkflow

import "errors"

// Sentinels shared by the engine, the handlers and the comment protocol.
// Handlers map ErrStatusMismatch to 409, ErrIllegalDecision to 422 and
// ErrForbidden to 403.
var (
	ErrEngineDisabled        = errors.New("workflow engine is disabled")
	ErrWorkflowNotFound      = errors.New("workflow not found")
	ErrWorkflowArchived      = errors.New("workflow is archived")
	ErrNoNodes               = errors.New("workflow has no nodes")
	ErrSupervisorUnavailable = errors.New("workflow supervisor agent is unavailable")
	ErrAgentNotInvokable     = errors.New("agent may not be invoked by this actor")
	ErrStatusMismatch        = errors.New("workflow step status does not allow this")
	ErrIllegalDecision       = errors.New("illegal workflow decision")
	ErrForbidden             = errors.New("not allowed to act on this workflow run")
	ErrRunNotFound           = errors.New("workflow run not found")
	// ErrNoTransition reports an observation that does not apply to the
	// current state (a stale task event, a repeated child event). The engine
	// treats it as a tick, never as a failure.
	ErrNoTransition = errors.New("event does not apply in the current state")
)

// AssignError is a refused workflow assignment, carrying the HTTP status the
// assignment endpoints answer with.
type AssignError struct {
	Status  int
	Code    string
	Message string
	Err     error
}

func (e *AssignError) Error() string { return e.Message }
func (e *AssignError) Unwrap() error { return e.Err }
```

`server/internal/extworkflow/transitions.go`:

```go
package extworkflow

import "fmt"

// This file is the pure state machine of a workflow run (spec §5.4–§5.6). It
// performs no I/O: the engine loads a RunState, calls Next, executes the
// returned effects and persists the returned state in one transaction.

type StepStatus string

const (
	StepPending            StepStatus = "pending"
	StepRunning            StepStatus = "running"
	StepAwaitingSupervisor StepStatus = "awaiting_supervisor"
	StepAwaitingHuman      StepStatus = "awaiting_human"
	StepDone               StepStatus = "done"
	StepSkipped            StepStatus = "skipped"
	StepFailed             StepStatus = "failed"
	StepCancelled          StepStatus = "cancelled"
)

// Terminal reports whether the step can no longer change within its run.
func (s StepStatus) Terminal() bool {
	switch s {
	case StepDone, StepSkipped, StepFailed, StepCancelled:
		return true
	}
	return false
}

// Awaiting reports whether the step waits for a decision ("awaiting_*").
func (s StepStatus) Awaiting() bool {
	return s == StepAwaitingSupervisor || s == StepAwaitingHuman
}

// Settled reports whether the step satisfies its dependents.
func (s StepStatus) Settled() bool { return s == StepDone || s == StepSkipped }

type PendingReason string

const (
	PendingReview        PendingReason = "review"
	PendingFailure       PendingReason = "failure"
	PendingRewindRequest PendingReason = "rewind_request"
)

type RunStatus string

const (
	RunRunning      RunStatus = "running"
	RunWaitingHuman RunStatus = "waiting_human"
	RunDone         RunStatus = "done"
	RunFailed       RunStatus = "failed"
	RunCancelled    RunStatus = "cancelled"
)

func (s RunStatus) Active() bool { return s == RunRunning || s == RunWaitingHuman }

type DecisionAction string

const (
	ActionApprove       DecisionAction = "approve"
	ActionRedo          DecisionAction = "redo"
	ActionRetry         DecisionAction = "retry"
	ActionSkip          DecisionAction = "skip"
	ActionRewind        DecisionAction = "rewind"
	ActionEscalate      DecisionAction = "escalate"
	ActionAbort         DecisionAction = "abort"
	ActionRequestRewind DecisionAction = "request-rewind"
)

type Decision struct {
	Action   DecisionAction
	Step     string // node key (parent-issue comments)
	To       string // node key
	Reason   string
	Feedback string
}

// Task roles and kinds stamped on agent_task_queue (spec §6.1).
const (
	RoleStep       = "step"
	RoleSupervisor = "supervisor"

	KindStep          = "step"
	KindReview        = "review"
	KindFailure       = "failure"
	KindRewindRequest = "rewind_request"
	KindSummary       = "summary"
	KindConversation  = "conversation"
)

// MaxSupervisorWakes bounds the supervisor tasks spent on one pending
// decision: the first wake, one re-wake after a silent turn, then a person.
const MaxSupervisorWakes = 2

// Timeline kinds written to ext_workflow_run_event.kind.
const (
	RunEventRunStarted      = "run_started"
	RunEventStepStarted     = "step_started"
	RunEventStepFinished    = "step_finished"
	RunEventStepFailed      = "step_failed"
	RunEventDecision        = "decision"
	RunEventRewind          = "rewind"
	RunEventRewindRequested = "rewind_requested"
	RunEventEscalated       = "escalated"
	RunEventRunFinished     = "run_finished"
	RunEventRunCancelled    = "run_cancelled"
	RunEventProtocolError   = "protocol_error"
)

// Milestones are the only system comments the engine posts on the parent.
const (
	MilestoneRunStarted   = "run_started"
	MilestoneEscalated    = "escalated"
	MilestoneRunFinished  = "run_finished"
	MilestoneRunFailed    = "run_failed"
	MilestoneRunCancelled = "run_cancelled"
)

// StepState is the engine-owned part of one ext_workflow_run_step row.
type StepState struct {
	Key              string
	Status           StepStatus
	Attempts         int
	PendingReason    PendingReason
	LastFeedback     string
	EscalationReason string
	SupervisorWakes  int
}

// RunState is everything Next decides on. SupervisorBusy, SupervisorFor and
// SummaryRequested are derived by the engine from in-flight tasks.
type RunState struct {
	Def              Definition
	Status           RunStatus
	RewindsUsed      int
	Steps            map[string]StepState
	SupervisorBusy   bool            // a review/failure/rewind_request/summary task is in flight
	SupervisorFor    map[string]bool // steps with an in-flight supervisor task
	SummaryRequested bool
}

// NewRunState is a fresh run: every step pending.
func NewRunState(def Definition) RunState {
	rs := RunState{Def: def, Status: RunRunning, Steps: map[string]StepState{}, SupervisorFor: map[string]bool{}}
	for _, n := range def.Nodes {
		rs.Steps[n.Key] = StepState{Key: n.Key, Status: StepPending}
	}
	return rs
}

func (rs RunState) Clone() RunState {
	out := rs
	out.Steps = make(map[string]StepState, len(rs.Steps))
	for k, v := range rs.Steps {
		out.Steps[k] = v
	}
	out.SupervisorFor = make(map[string]bool, len(rs.SupervisorFor))
	for k, v := range rs.SupervisorFor {
		if v {
			out.SupervisorFor[k] = true
		}
	}
	return out
}

// AllSettled reports whether every step is done or skipped.
func (rs RunState) AllSettled() bool {
	for _, n := range rs.Def.Nodes {
		if !rs.Steps[n.Key].Status.Settled() {
			return false
		}
	}
	return true
}

func (rs RunState) depsSettled(n Node) bool {
	for _, dep := range n.DependsOn {
		if !rs.Steps[dep].Status.Settled() {
			return false
		}
	}
	return true
}

type EventKind string

const (
	EvTick                 EventKind = "tick"                   // run-level: only schedule
	EvStepFinished         EventKind = "step_finished"          // child moved to done/in_review
	EvStepFailed           EventKind = "step_failed"            // Reason: agent outcome
	EvDecision             EventKind = "decision"               // Decision, from supervisor, step agent (request-rewind) or person
	EvSupervisorNoDecision EventKind = "supervisor_no_decision" // the focus step's supervisor task ended undecided
	EvChildCancelled       EventKind = "child_cancelled"        // a person cancelled (or deleted) the child
	EvSummaryEnded         EventKind = "summary_ended"          // run-level: the summary task ended
	EvCancelRun            EventKind = "cancel_run"             // run-level: Reason
)

type Event struct {
	Kind     EventKind
	Reason   string
	Decision Decision
	// Detail is merged into the recorded timeline payload (task id, error).
	Detail map[string]any
}

type EffectKind string

const (
	EffEnqueueStep       EffectKind = "enqueue_step"       // Step
	EffEnqueueSupervisor EffectKind = "enqueue_supervisor" // Step ("" = summary), SupervisorKind
	EffSetChildStatus    EffectKind = "set_child_status"   // Step, IssueStatus
	EffSetParentStatus   EffectKind = "set_parent_status"  // IssueStatus
	EffCancelStepWork    EffectKind = "cancel_step_work"   // Step: its step tasks and its queued supervisor tasks
	EffCancelRunTasks    EffectKind = "cancel_run_tasks"   // every in-flight task of the run
	EffEscalate          EffectKind = "escalate"           // Step, Reason: inbox item + milestone
	EffMilestone         EffectKind = "milestone"          // Milestone, Reason
	EffRecordEvent       EffectKind = "record_event"       // RunEvent, Step, Payload
)

type Effect struct {
	Kind           EffectKind
	Step           string
	IssueStatus    string
	SupervisorKind string
	Reason         string
	Milestone      string
	RunEvent       string
	Payload        map[string]any
	// Engine marks effects the scheduler produced; the engine records them
	// with the engine as actor rather than the event's actor.
	Engine bool
}

// Next applies ev to the step named key ("" for run-level events) and then
// schedules the run. It never mutates rs; on error it returns rs unchanged.
func Next(rs RunState, key string, ev Event) (RunState, []Effect, error) {
	if !rs.Status.Active() {
		if ev.Kind == EvDecision {
			return rs, nil, fmt.Errorf("%w: the run is %s", ErrStatusMismatch, rs.Status)
		}
		return rs, nil, ErrNoTransition
	}
	out := rs.Clone()
	var effs []Effect
	var err error
	switch ev.Kind {
	case EvTick:
	case EvCancelRun:
		return out, endRun(&out, RunCancelled, "", ev.Reason), nil
	case EvSummaryEnded:
		if !out.SummaryRequested || !out.AllSettled() {
			return rs, nil, ErrNoTransition
		}
		out.Status = RunDone
		out.SupervisorBusy = false
		return out, []Effect{
			{Kind: EffSetParentStatus, IssueStatus: "in_review"},
			record(RunEventRunFinished, "", map[string]any{"outcome": string(RunDone)}),
			{Kind: EffMilestone, Milestone: MilestoneRunFinished},
		}, nil
	case EvDecision:
		if ev.Decision.Action == ActionAbort {
			if effs, err = abortRun(&out, key, ev.Decision); err != nil {
				return rs, nil, err
			}
			return out, effs, nil
		}
		effs, err = stepDecision(&out, key, ev.Decision)
	case EvStepFinished, EvStepFailed, EvSupervisorNoDecision, EvChildCancelled:
		effs, err = stepObservation(&out, key, ev)
	default:
		return rs, nil, fmt.Errorf("%w: unknown event %q", ErrNoTransition, ev.Kind)
	}
	if err != nil {
		return rs, nil, err
	}
	return out, append(effs, schedule(&out)...), nil
}

func stepObservation(out *RunState, key string, ev Event) ([]Effect, error) {
	st, ok := out.Steps[key]
	if !ok {
		return nil, ErrNoTransition
	}
	node, _ := out.Def.NodeByKey(key)
	switch ev.Kind {
	case EvStepFinished:
		if st.Status != StepRunning {
			return nil, ErrNoTransition
		}
		payload := withDetail(map[string]any{"attempt": st.Attempts, "review": node.RequiresReview}, ev.Detail)
		if node.RequiresReview {
			out.Steps[key] = awaitSupervisor(st, PendingReview)
			return []Effect{record(RunEventStepFinished, key, payload)}, nil
		}
		st.Status = StepDone
		clearWait(&st)
		out.Steps[key] = st
		return []Effect{
			{Kind: EffSetChildStatus, Step: key, IssueStatus: "done"},
			record(RunEventStepFinished, key, payload),
		}, nil
	case EvStepFailed:
		if st.Status != StepRunning {
			return nil, ErrNoTransition
		}
		out.Steps[key] = awaitSupervisor(st, PendingFailure)
		payload := withDetail(map[string]any{"reason": ev.Reason, "attempt": st.Attempts}, ev.Detail)
		return []Effect{record(RunEventStepFailed, key, payload)}, nil
	case EvSupervisorNoDecision:
		if st.Status != StepAwaitingSupervisor {
			return nil, ErrNoTransition
		}
		if st.SupervisorWakes >= MaxSupervisorWakes {
			st.Status = StepAwaitingHuman
			st.EscalationReason = "The supervisor did not reach a decision: " + ev.Reason
			out.Steps[key] = st
			return []Effect{
				record(RunEventEscalated, key, withDetail(map[string]any{"reason": st.EscalationReason, "auto": true}, ev.Detail)),
				{Kind: EffEscalate, Step: key, Reason: st.EscalationReason},
			}, nil
		}
		return []Effect{record(RunEventProtocolError, key,
			withDetail(map[string]any{"reason": "no_decision", "detail": ev.Reason, "wake": st.SupervisorWakes}, ev.Detail))}, nil
	case EvChildCancelled:
		if st.Status.Terminal() {
			return nil, ErrNoTransition
		}
		st.Status = StepSkipped
		clearWait(&st)
		out.Steps[key] = st
		return []Effect{
			{Kind: EffCancelStepWork, Step: key},
			record(RunEventDecision, key, withDetail(map[string]any{"action": string(ActionSkip), "source": "child_cancelled"}, ev.Detail)),
		}, nil
	}
	return nil, ErrNoTransition
}

func stepDecision(out *RunState, key string, d Decision) ([]Effect, error) {
	if d.Action == ActionRequestRewind {
		return requestRewind(out, key, d)
	}
	st, ok := out.Steps[key]
	if !ok {
		return nil, fmt.Errorf("%w: unknown step %q", ErrIllegalDecision, key)
	}
	if !st.Status.Awaiting() {
		return nil, fmt.Errorf("%w: step %q is %s", ErrStatusMismatch, key, st.Status)
	}
	node, _ := out.Def.NodeByKey(key)
	rec := record(RunEventDecision, key, decisionPayload(d))
	switch d.Action {
	case ActionApprove:
		st.Status = StepDone
		clearWait(&st)
		out.Steps[key] = st
		return []Effect{rec, {Kind: EffCancelStepWork, Step: key}, {Kind: EffSetChildStatus, Step: key, IssueStatus: "done"}}, nil
	case ActionRedo, ActionRetry:
		if d.Action == ActionRedo && d.Feedback == "" {
			return nil, fmt.Errorf("%w: redo needs feedback", ErrIllegalDecision)
		}
		if st.Attempts >= node.MaxAttempts {
			return nil, fmt.Errorf("%w: step %q used %d of %d attempts", ErrIllegalDecision, key, st.Attempts, node.MaxAttempts)
		}
		if d.Action == ActionRedo {
			st.LastFeedback = d.Feedback
		}
		st.Status = StepRunning
		st.Attempts++
		clearWait(&st)
		out.Steps[key] = st
		return []Effect{
			rec,
			{Kind: EffCancelStepWork, Step: key},
			{Kind: EffSetChildStatus, Step: key, IssueStatus: "in_progress"},
			{Kind: EffEnqueueStep, Step: key},
		}, nil
	case ActionSkip:
		st.Status = StepSkipped
		clearWait(&st)
		out.Steps[key] = st
		return []Effect{rec, {Kind: EffCancelStepWork, Step: key}, {Kind: EffSetChildStatus, Step: key, IssueStatus: "cancelled"}}, nil
	case ActionRewind:
		if d.To == "" || d.Feedback == "" {
			return nil, fmt.Errorf("%w: rewind needs to and feedback", ErrIllegalDecision)
		}
		if d.To != key && !Ancestors(out.Def, key)[d.To] {
			return nil, fmt.Errorf("%w: %q is not %q or upstream of it", ErrIllegalDecision, d.To, key)
		}
		if out.RewindsUsed >= out.Def.MaxRewinds {
			return nil, fmt.Errorf("%w: the rewind budget (%d) is spent", ErrIllegalDecision, out.Def.MaxRewinds)
		}
		return append([]Effect{rec}, applyRewind(out, key, d.To, d.Feedback)...), nil
	case ActionEscalate:
		if d.Reason == "" {
			return nil, fmt.Errorf("%w: escalate needs a reason", ErrIllegalDecision)
		}
		if st.Status != StepAwaitingSupervisor {
			return nil, fmt.Errorf("%w: only the supervisor's turn can be escalated", ErrIllegalDecision)
		}
		st.Status = StepAwaitingHuman
		st.EscalationReason = d.Reason
		out.Steps[key] = st
		return []Effect{
			rec,
			record(RunEventEscalated, key, map[string]any{"reason": d.Reason}),
			{Kind: EffEscalate, Step: key, Reason: d.Reason},
		}, nil
	}
	return nil, fmt.Errorf("%w: unknown action %q", ErrIllegalDecision, d.Action)
}

func requestRewind(out *RunState, key string, d Decision) ([]Effect, error) {
	st, ok := out.Steps[key]
	if !ok {
		return nil, fmt.Errorf("%w: unknown step %q", ErrIllegalDecision, key)
	}
	if st.Status != StepRunning {
		return nil, fmt.Errorf("%w: step %q is %s", ErrStatusMismatch, key, st.Status)
	}
	if d.Reason == "" {
		return nil, fmt.Errorf("%w: request-rewind needs a reason", ErrIllegalDecision)
	}
	if d.To != "" && d.To != key && !Ancestors(out.Def, key)[d.To] {
		return nil, fmt.Errorf("%w: %q is not %q or upstream of it", ErrIllegalDecision, d.To, key)
	}
	payload := map[string]any{"to": d.To, "reason": d.Reason}
	if out.RewindsUsed >= out.Def.MaxRewinds {
		payload["budget_exhausted"] = true
		out.Steps[key] = awaitSupervisor(st, PendingFailure)
	} else {
		out.Steps[key] = awaitSupervisor(st, PendingRewindRequest)
	}
	return []Effect{record(RunEventRewindRequested, key, payload)}, nil
}

// applyRewind resets R = {to} ∪ (descendants of to that are not pending)
// (spec §5.5). Child issues are reused: they go back to backlog.
func applyRewind(out *RunState, from, to, feedback string) []Effect {
	out.RewindsUsed++
	desc := Descendants(out.Def, to)
	reset := []string{}
	var effs []Effect
	for _, n := range out.Def.Nodes {
		st := out.Steps[n.Key]
		if n.Key != to && !(desc[n.Key] && st.Status != StepPending) {
			continue
		}
		st.Status = StepPending
		st.Attempts = 0
		clearWait(&st)
		st.LastFeedback = ""
		if n.Key == to {
			st.LastFeedback = feedback
		}
		out.Steps[n.Key] = st
		reset = append(reset, n.Key)
		effs = append(effs,
			Effect{Kind: EffCancelStepWork, Step: n.Key},
			Effect{Kind: EffSetChildStatus, Step: n.Key, IssueStatus: "backlog"})
	}
	rec := record(RunEventRewind, from, map[string]any{"from": from, "to": to, "reset": reset, "rewinds_used": out.RewindsUsed})
	return append([]Effect{rec}, effs...)
}

func abortRun(out *RunState, key string, d Decision) ([]Effect, error) {
	if d.Reason == "" {
		return nil, fmt.Errorf("%w: abort needs a reason", ErrIllegalDecision)
	}
	if key != "" {
		st, ok := out.Steps[key]
		if !ok {
			return nil, fmt.Errorf("%w: unknown step %q", ErrIllegalDecision, key)
		}
		if st.Status.Terminal() {
			return nil, fmt.Errorf("%w: step %q is %s", ErrStatusMismatch, key, st.Status)
		}
	}
	effs := []Effect{record(RunEventDecision, key, decisionPayload(d))}
	return append(effs, endRun(out, RunFailed, key, d.Reason)...), nil
}

// endRun closes the run: open steps are cancelled (the aborted focus step
// fails), their children are cancelled and every in-flight task stops.
func endRun(out *RunState, status RunStatus, focus, reason string) []Effect {
	effs := []Effect{{Kind: EffCancelRunTasks}}
	for _, n := range out.Def.Nodes {
		st := out.Steps[n.Key]
		if st.Status.Terminal() {
			continue
		}
		st.Status = StepCancelled
		if status == RunFailed && n.Key == focus {
			st.Status = StepFailed
		}
		st.PendingReason = ""
		out.Steps[n.Key] = st
		effs = append(effs, Effect{Kind: EffSetChildStatus, Step: n.Key, IssueStatus: "cancelled"})
	}
	out.Status = status
	out.SupervisorBusy = false
	out.SupervisorFor = map[string]bool{}
	if status == RunFailed {
		return append(effs,
			Effect{Kind: EffSetParentStatus, IssueStatus: "blocked"},
			record(RunEventRunFinished, "", map[string]any{"outcome": string(RunFailed), "reason": reason}),
			Effect{Kind: EffMilestone, Milestone: MilestoneRunFailed, Reason: reason})
	}
	return append(effs,
		record(RunEventRunCancelled, "", map[string]any{"reason": reason}),
		Effect{Kind: EffMilestone, Milestone: MilestoneRunCancelled, Reason: reason})
}

// schedule starts ready steps, wakes the supervisor for at most one pending
// decision, requests the summary once everything settled, and derives
// waiting_human. Its effects are the engine's own.
func schedule(out *RunState) []Effect {
	if !out.Status.Active() {
		return nil
	}
	var effs []Effect
	for _, n := range out.Def.Nodes {
		st := out.Steps[n.Key]
		if st.Status != StepPending || !out.depsSettled(n) {
			continue
		}
		st.Status = StepRunning
		st.Attempts++
		clearWait(&st)
		out.Steps[n.Key] = st
		effs = append(effs,
			engine(Effect{Kind: EffSetChildStatus, Step: n.Key, IssueStatus: "in_progress"}),
			engine(Effect{Kind: EffEnqueueStep, Step: n.Key}),
			engine(record(RunEventStepStarted, n.Key, map[string]any{"attempt": st.Attempts})))
	}
	if !out.SupervisorBusy {
		for _, n := range out.Def.Nodes {
			st := out.Steps[n.Key]
			if st.Status != StepAwaitingSupervisor || out.SupervisorFor[n.Key] || st.SupervisorWakes >= MaxSupervisorWakes {
				continue
			}
			st.SupervisorWakes++
			out.Steps[n.Key] = st
			out.SupervisorFor[n.Key] = true
			out.SupervisorBusy = true
			effs = append(effs, engine(Effect{Kind: EffEnqueueSupervisor, Step: n.Key, SupervisorKind: string(st.PendingReason)}))
			break
		}
	}
	if !out.SupervisorBusy && !out.SummaryRequested && out.AllSettled() {
		out.SummaryRequested = true
		out.SupervisorBusy = true
		effs = append(effs, engine(Effect{Kind: EffEnqueueSupervisor, SupervisorKind: KindSummary}))
	}
	out.Status = RunRunning
	for _, st := range out.Steps {
		if st.Status == StepAwaitingHuman {
			out.Status = RunWaitingHuman
			break
		}
	}
	return effs
}

func awaitSupervisor(st StepState, reason PendingReason) StepState {
	st.Status = StepAwaitingSupervisor
	st.PendingReason = reason
	st.EscalationReason = ""
	st.SupervisorWakes = 0
	return st
}

func clearWait(st *StepState) {
	st.PendingReason = ""
	st.EscalationReason = ""
	st.SupervisorWakes = 0
}

func engine(e Effect) Effect {
	e.Engine = true
	return e
}

func record(kind, step string, payload map[string]any) Effect {
	if payload == nil {
		payload = map[string]any{}
	}
	return Effect{Kind: EffRecordEvent, RunEvent: kind, Step: step, Payload: payload}
}

func withDetail(p, detail map[string]any) map[string]any {
	for k, v := range detail {
		if _, taken := p[k]; !taken {
			p[k] = v
		}
	}
	return p
}

func decisionPayload(d Decision) map[string]any {
	p := map[string]any{"action": string(d.Action)}
	if d.To != "" {
		p["to"] = d.To
	}
	if d.Reason != "" {
		p["reason"] = d.Reason
	}
	if d.Feedback != "" {
		p["feedback"] = d.Feedback
	}
	return p
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `cd server && gofmt -w ./internal/extworkflow && go vet ./internal/extworkflow/ && go test ./internal/extworkflow/ -run 'TestNext|TestScheduleMarksEngineEffects' -count=1`
Expected: `ok  github.com/multica-ai/multica/server/internal/extworkflow`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/extworkflow/errors.go server/internal/extworkflow/transitions.go server/internal/extworkflow/transitions_test.go
git commit -m "feat(ext-workflow): pure run state machine

Next applies one observation or decision to a run and schedules it:
step start, supervisor wakes (one re-wake, then escalation), rewind
reset set, abort, cancel and summary completion.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


---

### Task B2: Comment decision block parser (`block.go`)

**Files:**
- Create: `server/internal/extworkflow/block.go`
- Test: `server/internal/extworkflow/block_test.go`

**Interfaces:**
- Consumes: `Decision`, `DecisionAction`, `Action*`, `ErrIllegalDecision` (B1).
- Produces (Part C uses these in `isNoteComment` and the comment protocol / decision API):
  - `const BlockLang = "ext-workflow"`
  - `func ContainsBlock(markdown string) bool` — true when the comment carries any `ext-workflow` fence, valid or not (so a malformed decision still counts as a note and wakes nobody).
  - `func ParseBlock(markdown string) (d Decision, found bool, err error)` — `found=false, err=nil` when there is no block.
  - `func ValidateDecision(d Decision) error` — required fields per action (shared with the decision API).
  - Sentinels `ErrBlockMultiple`, `ErrBlockUnclosed`, `ErrBlockEmpty`, `ErrBlockInvalid` (YAML / unknown field); field errors wrap `ErrIllegalDecision`.

- [ ] **Step 1: Write the failing test**

`server/internal/extworkflow/block_test.go`:

````go
package extworkflow

import (
	"errors"
	"testing"
)

func TestParseBlock(t *testing.T) {
	cases := []struct {
		name      string
		markdown  string
		wantFound bool
		wantErr   error
		want      Decision
	}{
		{
			name:     "no block",
			markdown: "Looks good to me.\n\n```go\nfmt.Println(1)\n```",
		},
		{
			name:      "rewind with every field",
			markdown:  "Rewinding.\n\n```ext-workflow\naction: rewind          # approve | redo | ...\nstep: backend\nto: spec\nreason: Spec assumed paginated API; it is not.\nfeedback: |\n  Switch to cursor pagination and update the data-flow section.\n```\n",
			wantFound: true,
			want: Decision{Action: ActionRewind, Step: "backend", To: "spec",
				Reason: "Spec assumed paginated API; it is not.", Feedback: "Switch to cursor pagination and update the data-flow section."},
		},
		{
			name:      "approve needs nothing else",
			markdown:  "```ext-workflow\naction: approve\n```",
			wantFound: true,
			want:      Decision{Action: ActionApprove},
		},
		{
			name:      "tilde fence and info words",
			markdown:  "~~~ext-workflow decision\naction: skip\n~~~",
			wantFound: true,
			want:      Decision{Action: ActionSkip},
		},
		{
			name:      "request-rewind without target",
			markdown:  "```ext-workflow\naction: request-rewind\nreason: upstream output is wrong\n```",
			wantFound: true,
			want:      Decision{Action: ActionRequestRewind, Reason: "upstream output is wrong"},
		},
		{
			name:      "unicode values survive",
			markdown:  "```ext-workflow\naction: redo\nfeedback: 改用游标分页 ✅\n```",
			wantFound: true,
			want:      Decision{Action: ActionRedo, Feedback: "改用游标分页 ✅"},
		},
		{
			name:      "unicode reason for escalate",
			markdown:  "```ext-workflow\naction: escalate\nreason: 规格假设了分页 API；实际上没有。\n```",
			wantFound: true,
			want:      Decision{Action: ActionEscalate, Reason: "规格假设了分页 API；实际上没有。"},
		},
		{
			name:      "unknown field is rejected",
			markdown:  "```ext-workflow\naction: approve\nnote: hi\n```",
			wantFound: true,
			wantErr:   ErrBlockInvalid,
		},
		{
			name:      "two blocks are rejected",
			markdown:  "```ext-workflow\naction: approve\n```\n\n```ext-workflow\naction: skip\n```",
			wantFound: true,
			wantErr:   ErrBlockMultiple,
		},
		{
			name:      "unclosed block is rejected",
			markdown:  "```ext-workflow\naction: approve\n",
			wantFound: true,
			wantErr:   ErrBlockUnclosed,
		},
		{
			name:      "empty block is rejected",
			markdown:  "```ext-workflow\n```",
			wantFound: true,
			wantErr:   ErrBlockEmpty,
		},
		{
			name:      "missing action",
			markdown:  "```ext-workflow\nreason: x\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:      "unknown action",
			markdown:  "```ext-workflow\naction: promote\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:      "redo needs feedback",
			markdown:  "```ext-workflow\naction: redo\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:      "rewind needs to",
			markdown:  "```ext-workflow\naction: rewind\nfeedback: x\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:      "abort needs a reason",
			markdown:  "```ext-workflow\naction: abort\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:      "request-rewind needs a reason",
			markdown:  "```ext-workflow\naction: request-rewind\nto: spec\n```",
			wantFound: true,
			wantErr:   ErrIllegalDecision,
		},
		{
			name:     "a fenced example inside a longer fence is not a block",
			markdown: "````markdown\n```ext-workflow\naction: approve\n```\n````",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := ParseBlock(tc.markdown)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBlock: %v", err)
			}
			if got != tc.want {
				t.Fatalf("decision = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestContainsBlock(t *testing.T) {
	for markdown, want := range map[string]bool{
		"plain text":                                   false,
		"```go\nx := 1\n```":                           false,
		"```ext-workflow\naction: approve\n```":        true,
		"```ext-workflow\nthis: is not yaml: at all\n": true, // unclosed but still a decision attempt
		"```EXT-WORKFLOW\naction: approve\n```":        true,
	} {
		if got := ContainsBlock(markdown); got != want {
			t.Fatalf("ContainsBlock(%q) = %v, want %v", markdown, got, want)
		}
	}
}
````

- [ ] **Step 2: Run, expect FAIL**

Run: `cd server && go test ./internal/extworkflow/ -run 'TestParseBlock|TestContainsBlock' -count=1`
Expected: build failure — `undefined: ParseBlock`, `undefined: ContainsBlock`, `undefined: ErrBlockInvalid`.

- [ ] **Step 3: Implement**

`server/internal/extworkflow/block.go`:

````go
package extworkflow

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// BlockLang is the fence info string of a decision block (spec §6.3).
const BlockLang = "ext-workflow"

var (
	ErrBlockMultiple = errors.New("ext-workflow: a comment may carry only one ext-workflow block")
	ErrBlockUnclosed = errors.New("ext-workflow: the ext-workflow block is not closed")
	ErrBlockEmpty    = errors.New("ext-workflow: the ext-workflow block is empty")
	ErrBlockInvalid  = errors.New("ext-workflow: the ext-workflow block is not valid")
)

type blockFields struct {
	Action   string `yaml:"action"`
	Step     string `yaml:"step"`
	To       string `yaml:"to"`
	Reason   string `yaml:"reason"`
	Feedback string `yaml:"feedback"`
}

// ContainsBlock reports whether the comment carries an ext-workflow fence,
// valid or not.
func ContainsBlock(markdown string) bool {
	bodies, unclosed := findBlocks(markdown)
	return len(bodies) > 0 || unclosed
}

// ParseBlock extracts the one decision block of a comment.
func ParseBlock(markdown string) (Decision, bool, error) {
	bodies, unclosed := findBlocks(markdown)
	count := len(bodies)
	if unclosed {
		count++
	}
	switch {
	case count == 0:
		return Decision{}, false, nil
	case count > 1:
		return Decision{}, true, ErrBlockMultiple
	case unclosed:
		return Decision{}, true, ErrBlockUnclosed
	}
	var raw blockFields
	dec := yaml.NewDecoder(strings.NewReader(bodies[0]))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return Decision{}, true, ErrBlockEmpty
		}
		return Decision{}, true, fmt.Errorf("%w: %v", ErrBlockInvalid, err)
	}
	d := Decision{
		Action:   DecisionAction(strings.TrimSpace(raw.Action)),
		Step:     strings.TrimSpace(raw.Step),
		To:       strings.TrimSpace(raw.To),
		Reason:   strings.TrimSpace(raw.Reason),
		Feedback: strings.TrimSpace(raw.Feedback),
	}
	if err := ValidateDecision(d); err != nil {
		return d, true, err
	}
	return d, true, nil
}

// ValidateDecision checks the fields each action requires (spec §6.3).
func ValidateDecision(d Decision) error {
	need := func(field, value string) error {
		if value == "" {
			return fmt.Errorf("%w: %s needs %s", ErrIllegalDecision, d.Action, field)
		}
		return nil
	}
	switch d.Action {
	case ActionApprove, ActionRetry, ActionSkip:
		return nil
	case ActionRedo:
		return need("feedback", d.Feedback)
	case ActionRewind:
		if err := need("to", d.To); err != nil {
			return err
		}
		return need("feedback", d.Feedback)
	case ActionEscalate, ActionAbort, ActionRequestRewind:
		return need("reason", d.Reason)
	case "":
		return fmt.Errorf("%w: action is required", ErrIllegalDecision)
	}
	return fmt.Errorf("%w: unknown action %q", ErrIllegalDecision, d.Action)
}

type fence struct {
	char byte
	size int
}

// findBlocks returns the bodies of top-level ext-workflow fences (CommonMark
// fences: ``` or ~~~, at most three spaces of indent, closed by the same
// character at least as long). Fences nested inside another fence are text.
func findBlocks(markdown string) (bodies []string, unclosed bool) {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	var open *fence
	ours := false
	var buf []string
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if open == nil {
			if indent > 3 {
				continue
			}
			f, info, ok := openingFence(trimmed)
			if !ok {
				continue
			}
			open = &f
			ours = infoLang(info) == BlockLang
			buf = nil
			continue
		}
		if indent <= 3 && closesFence(trimmed, *open) {
			if ours {
				bodies = append(bodies, strings.Join(buf, "\n"))
			}
			open = nil
			continue
		}
		if ours {
			buf = append(buf, line)
		}
	}
	return bodies, open != nil && ours
}

func openingFence(s string) (fence, string, bool) {
	if len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return fence{}, "", false
	}
	n := 0
	for n < len(s) && s[n] == s[0] {
		n++
	}
	if n < 3 {
		return fence{}, "", false
	}
	info := strings.TrimSpace(s[n:])
	if s[0] == '`' && strings.Contains(info, "`") {
		return fence{}, "", false
	}
	return fence{char: s[0], size: n}, info, true
}

func closesFence(s string, f fence) bool {
	n := 0
	for n < len(s) && s[n] == f.char {
		n++
	}
	return n >= f.size && strings.TrimSpace(s[n:]) == ""
}

func infoLang(info string) string {
	if i := strings.IndexAny(info, " \t{"); i >= 0 {
		info = info[:i]
	}
	return strings.ToLower(info)
}
````

- [ ] **Step 4: Run, expect PASS**

Run: `cd server && gofmt -w ./internal/extworkflow && go vet ./internal/extworkflow/ && go test ./internal/extworkflow/ -run 'TestParseBlock|TestContainsBlock|TestNext' -count=1`
Expected: `ok  github.com/multica-ai/multica/server/internal/extworkflow`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/extworkflow/block.go server/internal/extworkflow/block_test.go
git commit -m "feat(ext-workflow): parse ext-workflow decision blocks

One fenced YAML block per comment, strict fields, required fields per
action; malformed blocks are still recognised so they wake no agent.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


---

### Task B3: Run, step, event and engine-task queries; retries keep the workflow stamp

**Files:**
- Create: `server/migrations/ext_0015_run_step_supervisor_wakes.up.sql`, `server/migrations/ext_0015_run_step_supervisor_wakes.down.sql`
- Create: `server/pkg/db/queries/ext_workflow_run.sql`
- Modify: `server/pkg/db/queries/agent.sql` — `-- name: CreateRetryTask :one` (INSERT column list ending `channel_context_revision, handoff_note, id`, ~line 547, and the SELECT line `CASE WHEN p.context->>'wakeup_id' IS NOT NULL THEN p.handoff_note END,`, ~line 568)
- Regenerate: `server/pkg/db/generated/*` (`make sqlc`)
- Test: `server/internal/service/ext_workflow_retry_test.go`

**Interfaces:**
- Consumes: Part A tables (`ext_workflow_run`, `ext_workflow_run_step`, `ext_workflow_run_event`, `agent_task_queue.ext_workflow_*`), Part A's `ext_workflow` table for joins.
- Produces (generated `db.Queries` methods; params structs named `<Query>Params`):
  - Runs: `CreateExtWorkflowRun{ID, WorkspaceID, WorkflowID, IssueID, TriggeredByType string, TriggeredByID, Definition []byte}`, `GetExtWorkflowRun(id)`, `GetExtWorkflowRunInWorkspace{ID, WorkspaceID}`, `LockExtWorkflowRun(id)` (FOR UPDATE), `GetActiveExtWorkflowRunByIssue(issueID)`, `UpdateExtWorkflowRunState{Status string, RewindsUsed int32, ID}`, `TouchExtWorkflowRun(id)`, `ListExtWorkflowRunsForReconcile(pageLimit int32)`.
  - Run summaries for the run API (Part C): `ListExtWorkflowRunSummariesByIssue{IssueID, WorkspaceID}`, `ListExtWorkflowRunSummariesByWorkflow{WorkflowID, WorkspaceID, PageLimit, PageOffset int32}`, `CountExtWorkflowRunsByWorkflow{WorkflowID, WorkspaceID} (int64)`, `GetExtWorkflowRunSummary{ID, WorkspaceID}`; rows = every `ext_workflow_run` column + `WorkflowName string`, `IssueNumber pgtype.Int4`, `IssueTitle pgtype.Text`.
  - Steps: `CreateExtWorkflowRunStep{ID, RunID, WorkspaceID, NodeKey, AgentID, IssueID}`, `ListExtWorkflowRunSteps(runID)` (id order = node order), `GetExtWorkflowRunStep(id)`, `GetExtWorkflowRunStepByIssue{IssueID, WorkspaceID}` (child issue → its step, newest run), `UpdateExtWorkflowRunStep{ID, Status string, Attempts int32, PendingReason, LastFeedback, EscalationReason pgtype.Text, SupervisorWakes int32}`.
  - Events: `CreateExtWorkflowRunEvent{ID, RunID, WorkspaceID, StepID, Kind string, ActorType string, ActorID, OnBehalfOf, Payload []byte}`, `ListExtWorkflowRunEvents(runID)`.
  - Engine: `LockIssueForExtWorkflowStart(id)`, `CreateExtWorkflowTask{…, ExtWorkflowRunID, ExtWorkflowStepID pgtype.UUID, ExtWorkflowRole, ExtWorkflowKind pgtype.Text, ID}` (ErrNoRows = slot taken or workspace gone), `ListPendingSlotTasksForIssueAgent{IssueID, AgentID}`, `AdoptTaskForExtWorkflow{ID, RunID, StepID, Role, Kind pgtype.Text, HandoffNote string}`, `ListActiveExtWorkflowTasksForRun(runID)`, `GetLatestExtWorkflowTaskForStep{StepID, Role pgtype.Text}`, `GetLatestExtWorkflowSummaryTask(runID)`, `HasExtWorkflowSummaryTask(runID) (bool)`, `CancelExtWorkflowStepTasks{StepID, ExceptTaskID}`, `CancelExtWorkflowRunTasks{RunID, ExceptTaskID}` (both stamp `failure_reason='ext_workflow_engine'`).
  - Column `ext_workflow_run_step.supervisor_wakes INT NOT NULL DEFAULT 0` (`db.ExtWorkflowRunStep.SupervisorWakes int32`). Part A's notes reserve `ext_0015+` for later parts; this is a column on an existing Part A table, so the workspace-deletion manifest needs no change.
  - `CreateRetryTask` copies `ext_workflow_run_id, ext_workflow_step_id, ext_workflow_role, ext_workflow_kind`.

- [ ] **Step 1: Write the failing test**

`server/internal/service/ext_workflow_retry_test.go`:

```go
package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The engine's run, step, event and task queries exist (B5 and B6 use them).
var _ = []any{
	(*db.Queries).CreateExtWorkflowRun, (*db.Queries).LockExtWorkflowRun, (*db.Queries).UpdateExtWorkflowRunStep,
	(*db.Queries).CreateExtWorkflowRunEvent, (*db.Queries).CreateExtWorkflowTask, (*db.Queries).CancelExtWorkflowRunTasks,
	(*db.Queries).ListExtWorkflowRunsForReconcile, (*db.Queries).ListExtWorkflowRunSummariesByIssue,
}

// A platform retry of a workflow task is the same attempt: the engine finds
// it through the ext columns, so CreateRetryTask must copy all four.
func TestCreateRetryTaskCopiesExtWorkflowColumns(t *testing.T) {
	f, owner := newPrincipalFixture(t)
	ctx := context.Background()
	agent := f.privateAgentOwnedBy(t, owner, "ext-retry")
	issue := f.Issue(t, "Workflow step")
	var runtimeID string
	f.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, agent).Scan(&runtimeID)
	runID, stepID := dbid.NewV7(), dbid.NewV7()
	parent := f.Task(t, agent, testutil.Cols{
		"runtime_id":           runtimeID,
		"issue_id":             issue,
		"status":               "failed",
		"attempt":              1,
		"max_attempts":         3,
		"ext_workflow_run_id":  runID,
		"ext_workflow_step_id": stepID,
		"ext_workflow_role":    "step",
		"ext_workflow_kind":    "step",
	})
	f.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id = $1`, parent)

	retry, err := f.q.CreateRetryTask(ctx, db.CreateRetryTaskParams{ID: parseTestUUID(t, parent)})
	if err != nil {
		t.Fatalf("CreateRetryTask: %v", err)
	}
	if retry.ExtWorkflowRunID != runID || retry.ExtWorkflowStepID != stepID ||
		retry.ExtWorkflowRole.String != "step" || retry.ExtWorkflowKind.String != "step" {
		t.Fatalf("retry ext columns = %v %v %q %q", retry.ExtWorkflowRunID, retry.ExtWorkflowStepID, retry.ExtWorkflowRole.String, retry.ExtWorkflowKind.String)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `cd server && go vet ./internal/service/`
Expected: build failure — `(*db.Queries).CreateExtWorkflowRun undefined (type *db.Queries has no method CreateExtWorkflowRun)` (and the other listed queries).

- [ ] **Step 3: Implement**

`server/migrations/ext_0015_run_step_supervisor_wakes.up.sql`:

```sql
-- How many supervisor tasks the engine started for a step's current pending
-- decision. The engine re-wakes a silent supervisor once, then escalates.
ALTER TABLE ext_workflow_run_step
    ADD COLUMN IF NOT EXISTS supervisor_wakes INT NOT NULL DEFAULT 0;
```

`server/migrations/ext_0015_run_step_supervisor_wakes.down.sql`:

```sql
ALTER TABLE ext_workflow_run_step DROP COLUMN IF EXISTS supervisor_wakes;
```

`server/pkg/db/queries/ext_workflow_run.sql`:

```sql
-- name: CreateExtWorkflowRun :one
INSERT INTO ext_workflow_run (
    id, workspace_id, workflow_id, issue_id, triggered_by_type, triggered_by_id,
    status, definition, rewinds_used, started_at
) VALUES (
    @id, @workspace_id, @workflow_id, @issue_id, @triggered_by_type, @triggered_by_id,
    'running', @definition, 0, now()
)
RETURNING *;

-- name: GetExtWorkflowRun :one
SELECT * FROM ext_workflow_run WHERE id = @id;

-- name: GetExtWorkflowRunInWorkspace :one
SELECT * FROM ext_workflow_run WHERE id = @id AND workspace_id = @workspace_id;

-- name: LockExtWorkflowRun :one
-- Serializes the engine per run: every state change takes this row lock first.
SELECT * FROM ext_workflow_run WHERE id = @id FOR UPDATE;

-- name: GetActiveExtWorkflowRunByIssue :one
SELECT * FROM ext_workflow_run
WHERE issue_id = @issue_id AND status IN ('running', 'waiting_human')
ORDER BY created_at DESC
LIMIT 1;

-- name: UpdateExtWorkflowRunState :one
UPDATE ext_workflow_run SET
    status = @status,
    rewinds_used = @rewinds_used,
    finished_at = CASE
        WHEN @status::text IN ('done', 'failed', 'cancelled') THEN COALESCE(finished_at, now())
        ELSE NULL END,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: TouchExtWorkflowRun :exec
UPDATE ext_workflow_run SET updated_at = now() WHERE id = @id;

-- name: ListExtWorkflowRunsForReconcile :many
-- Active runs, least recently touched first; the reconcile job touches every
-- run it visits so a full sweep rotates through all of them.
SELECT * FROM ext_workflow_run
WHERE status IN ('running', 'waiting_human')
ORDER BY updated_at ASC, id ASC
LIMIT @page_limit;

-- name: ListExtWorkflowRunSummariesByIssue :many
-- Runs of one parent issue, newest first, with what a run list renders.
SELECT r.*, w.name AS workflow_name, i.number AS issue_number, i.title AS issue_title
FROM ext_workflow_run r
JOIN ext_workflow w ON w.id = r.workflow_id
LEFT JOIN issue i ON i.id = r.issue_id
WHERE r.issue_id = @issue_id AND r.workspace_id = @workspace_id
ORDER BY r.created_at DESC;

-- name: ListExtWorkflowRunSummariesByWorkflow :many
SELECT r.*, w.name AS workflow_name, i.number AS issue_number, i.title AS issue_title
FROM ext_workflow_run r
JOIN ext_workflow w ON w.id = r.workflow_id
LEFT JOIN issue i ON i.id = r.issue_id
WHERE r.workflow_id = @workflow_id AND r.workspace_id = @workspace_id
ORDER BY r.created_at DESC
LIMIT @page_limit OFFSET @page_offset;

-- name: CountExtWorkflowRunsByWorkflow :one
SELECT count(*)::bigint FROM ext_workflow_run
WHERE workflow_id = @workflow_id AND workspace_id = @workspace_id;

-- name: GetExtWorkflowRunSummary :one
SELECT r.*, w.name AS workflow_name, i.number AS issue_number, i.title AS issue_title
FROM ext_workflow_run r
JOIN ext_workflow w ON w.id = r.workflow_id
LEFT JOIN issue i ON i.id = r.issue_id
WHERE r.id = @id AND r.workspace_id = @workspace_id;

-- name: CreateExtWorkflowRunStep :one
INSERT INTO ext_workflow_run_step (id, run_id, workspace_id, node_key, agent_id, issue_id, status, attempts)
VALUES (@id, @run_id, @workspace_id, @node_key, @agent_id, @issue_id, 'pending', 0)
RETURNING *;

-- name: ListExtWorkflowRunSteps :many
-- Ids are UUIDv7 minted in definition order, so id order is node order.
SELECT * FROM ext_workflow_run_step WHERE run_id = @run_id ORDER BY id;

-- name: GetExtWorkflowRunStep :one
SELECT * FROM ext_workflow_run_step WHERE id = @id;

-- name: GetExtWorkflowRunStepByIssue :one
-- The step whose child issue this is. A child is reused across rewinds but
-- never across runs, so the newest run wins.
SELECT s.* FROM ext_workflow_run_step s
JOIN ext_workflow_run r ON r.id = s.run_id
WHERE s.issue_id = @issue_id AND s.workspace_id = @workspace_id
ORDER BY r.created_at DESC
LIMIT 1;

-- name: UpdateExtWorkflowRunStep :one
UPDATE ext_workflow_run_step SET
    status = @status,
    attempts = @attempts,
    pending_reason = sqlc.narg(pending_reason),
    last_feedback = sqlc.narg(last_feedback),
    escalation_reason = sqlc.narg(escalation_reason),
    supervisor_wakes = @supervisor_wakes,
    started_at = CASE
        WHEN @status::text = 'running' AND status <> 'running' THEN now()
        WHEN @status::text = 'pending' THEN NULL
        ELSE started_at END,
    finished_at = CASE
        WHEN @status::text IN ('done', 'skipped', 'failed', 'cancelled') THEN COALESCE(finished_at, now())
        ELSE NULL END,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: CreateExtWorkflowRunEvent :one
INSERT INTO ext_workflow_run_event (id, run_id, workspace_id, step_id, kind, actor_type, actor_id, on_behalf_of, payload)
VALUES (@id, @run_id, @workspace_id, sqlc.narg(step_id), @kind, @actor_type, sqlc.narg(actor_id), sqlc.narg(on_behalf_of), @payload)
RETURNING *;

-- name: ListExtWorkflowRunEvents :many
SELECT * FROM ext_workflow_run_event WHERE run_id = @run_id ORDER BY created_at, id;

-- name: LockIssueForExtWorkflowStart :one
-- StartRun holds the parent row while it creates the run and its children.
SELECT * FROM issue WHERE id = @id FOR UPDATE;

-- name: CreateExtWorkflowTask :one
-- Same fences as CreateAgentTask (workspace teardown) and CreateRetryTask (the
-- pending (issue, agent, thread) slot): ErrNoRows means "not created", and the
-- caller decides between dedup, adoption and a busy slot.
INSERT INTO agent_task_queue (
    agent_id, runtime_id, issue_id, status, priority, trigger_comment_id, trigger_summary,
    handoff_note, originator_user_id, accountable_user_id, runtime_mcp_overlay, runtime_connected_apps,
    originator_source, delegated_from_task_id, rule_version_id, trigger_evidence_kind, trigger_evidence_ref_id,
    ext_workflow_run_id, ext_workflow_step_id, ext_workflow_role, ext_workflow_kind, id
)
SELECT
    @agent_id, @runtime_id, @issue_id, 'queued', @priority, sqlc.narg(trigger_comment_id), sqlc.narg(trigger_summary),
    sqlc.narg(handoff_note), sqlc.narg(originator_user_id), sqlc.narg(accountable_user_id),
    sqlc.narg(runtime_mcp_overlay), sqlc.narg(runtime_connected_apps),
    sqlc.narg(originator_source), sqlc.narg(delegated_from_task_id), sqlc.narg(rule_version_id),
    sqlc.narg(trigger_evidence_kind), sqlc.narg(trigger_evidence_ref_id),
    @ext_workflow_run_id, sqlc.narg(ext_workflow_step_id), @ext_workflow_role, @ext_workflow_kind, @id
WHERE lock_task_owner_rows(@agent_id, @issue_id, @runtime_id)
ON CONFLICT (issue_id, agent_id, (COALESCE(comment_thread_id, '00000000-0000-0000-0000-000000000000'::uuid))) WHERE status IN ('queued', 'dispatched')
       OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
DO NOTHING
RETURNING *;

-- name: ListPendingSlotTasksForIssueAgent :many
-- Tasks holding a pending slot of the (issue, agent) unique index.
SELECT * FROM agent_task_queue
WHERE issue_id = @issue_id AND agent_id = @agent_id
  AND (status IN ('queued', 'dispatched')
       OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true'))
ORDER BY created_at, id;

-- name: AdoptTaskForExtWorkflow :one
-- A plain queued run of the same agent on the same issue becomes the
-- workflow's task instead of blocking it.
UPDATE agent_task_queue SET
    ext_workflow_run_id = @run_id,
    ext_workflow_step_id = sqlc.narg(step_id),
    ext_workflow_role = @role,
    ext_workflow_kind = @kind,
    handoff_note = CASE
        WHEN COALESCE(handoff_note, '') = '' THEN @handoff_note::text
        ELSE handoff_note || E'\n\n' || @handoff_note::text END
WHERE id = @id AND status = 'queued' AND ext_workflow_run_id IS NULL
RETURNING *;

-- name: ListActiveExtWorkflowTasksForRun :many
SELECT * FROM agent_task_queue
WHERE ext_workflow_run_id = @run_id
  AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
ORDER BY created_at, id;

-- name: GetLatestExtWorkflowTaskForStep :one
SELECT * FROM agent_task_queue
WHERE ext_workflow_step_id = @step_id AND ext_workflow_role = @role
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetLatestExtWorkflowSummaryTask :one
SELECT * FROM agent_task_queue
WHERE ext_workflow_run_id = @run_id AND ext_workflow_kind = 'summary'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: HasExtWorkflowSummaryTask :one
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue WHERE ext_workflow_run_id = @run_id AND ext_workflow_kind = 'summary'
) AS has_summary;

-- name: CancelExtWorkflowStepTasks :many
-- A step's own tasks, plus supervisor tasks for it that have not started.
-- failure_reason marks the cancellation as the engine's, so its terminal
-- event is not read as an agent outcome.
UPDATE agent_task_queue SET
    status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    error = 'cancelled by the workflow engine', failure_reason = 'ext_workflow_engine',
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL
WHERE ext_workflow_step_id = @step_id
  AND id IS DISTINCT FROM sqlc.narg(except_task_id)::uuid
  AND (
    (ext_workflow_role = 'step' AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred'))
    OR (ext_workflow_role = 'supervisor' AND status IN ('queued', 'deferred'))
  )
RETURNING *;

-- name: CancelExtWorkflowRunTasks :many
UPDATE agent_task_queue SET
    status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    error = 'cancelled by the workflow engine', failure_reason = 'ext_workflow_engine',
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL
WHERE ext_workflow_run_id = @run_id
  AND id IS DISTINCT FROM sqlc.narg(except_task_id)::uuid
  AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
RETURNING *;
```

Edit `CreateRetryTask` in `server/pkg/db/queries/agent.sql`:

`server/pkg/db/queries/agent.sql`:

```diff
diff --git a/server/pkg/db/queries/agent.sql b/server/pkg/db/queries/agent.sql
--- a/server/pkg/db/queries/agent.sql
+++ b/server/pkg/db/queries/agent.sql
@@ -544,7 +544,10 @@ INSERT INTO agent_task_queue (
     originator_source, delegated_from_task_id, rule_version_id,
     trigger_evidence_kind, trigger_evidence_ref_id, retry_of_task_id,
     chat_input_task_id, fire_at,
-    channel_context_revision, handoff_note, id
+    channel_context_revision, handoff_note,
+    -- ext-workflow: a retry keeps its workflow stamp, so the engine counts it as the same attempt.
+    ext_workflow_run_id, ext_workflow_step_id, ext_workflow_role, ext_workflow_kind,
+    id
 )
 SELECT
     p.agent_id, p.runtime_id, p.issue_id, p.chat_session_id, p.autopilot_run_id,
@@ -566,6 +569,7 @@ SELECT
     p.chat_input_task_id, sqlc.narg(fire_at),
     p.channel_context_revision,
     CASE WHEN p.context->>'wakeup_id' IS NOT NULL THEN p.handoff_note END,
+    p.ext_workflow_run_id, p.ext_workflow_step_id, p.ext_workflow_role, p.ext_workflow_kind,
     -- Named new_task_id, not id: $1 above is the PARENT task's id.
     COALESCE(sqlc.narg('new_task_id')::uuid, gen_random_uuid())
 FROM agent_task_queue p
```

Then regenerate and migrate:

```bash
make sqlc
make env-exec ARGS="-- bash -c 'cd server && go run ./cmd/migrate up'"
```

- [ ] **Step 4: Run, expect PASS**

Run:
```bash
cd server && go build ./... && go vet ./internal/service/
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/service/ -run TestCreateRetryTaskCopiesExtWorkflowColumns -count=1 -v'"
```
Expected: `--- PASS: TestCreateRetryTaskCopiesExtWorkflowColumns` (not SKIP) and `ok`. Before the `agent.sql` edit the same test fails with `retry ext columns = {[0 …] false} {[0 …] false} "" ""`.

- [ ] **Step 5: Commit**

```bash
git add server/migrations/ext_0015_run_step_supervisor_wakes.up.sql server/migrations/ext_0015_run_step_supervisor_wakes.down.sql server/pkg/db/queries/ext_workflow_run.sql server/pkg/db/queries/agent.sql server/pkg/db/generated server/internal/service/ext_workflow_retry_test.go
git commit -m "feat(ext-workflow): run, step, event and engine task queries

Adds the engine's queries, the supervisor_wakes step column, and makes
CreateRetryTask copy the four ext_workflow columns so a platform retry
stays the same workflow attempt.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


---

### Task B4: Service seam — hooks interface, workflow task enqueue, child-issue creation

**Files:**
- Create: `server/internal/service/ext_workflow_hooks.go`
- Create: `server/internal/service/ext_workflow_task.go`
- Create: `server/internal/service/ext_workflow_issue.go`
- Modify: `server/internal/service/task.go` — `type TaskService struct` (after the `QuickActions ChatQuickActionsLLM` field, ~line 79)
- Test: `server/internal/service/ext_workflow_task_test.go`

**Interfaces:**
- Consumes: B3 queries; existing package-private helpers `attributionForIssueTask`, `applyAttributionFallback`, `attributionCreateParams`, `buildRuntimeMCPOverlay`, `buildCommentTriggerSummary`, `priorityToInt`, `broadcastTaskEvent`, `NotifyTaskEnqueued`, `broadcastIssueUpdated`, `commentEventFields`, `(*IssueWakeupService).publishSystemInbox`, `publishIssueCreated`, `AllocateIssueNumber`, `ResolveIssueCountPolicy`, `issueposition.NextTopPosition`, `IssueToMapResolved`.
- Produces:
  - `type ExtChildEvent struct{ChildID pgtype.UUID; Kind string}`; `type ExtWorkflowHooks interface{ValidateAssignment(...) error; StartRun(...) error; OnChildEvents(ctx, tx pgx.Tx, parentID pgtype.UUID, events []ExtChildEvent) error}` (contract signatures).
  - **`TaskService.ExtWorkflow ExtWorkflowHooks`** (contract said `IssueService.ExtWorkflow`; see deviation 1). `IssueService` reaches it as `s.TaskService.ExtWorkflow`.
  - `type ExtAfterCommit`; `WithExtAfterCommit(ctx) (context.Context, *ExtAfterCommit)`; `ExtAfterCommitFrom(ctx) *ExtAfterCommit`; `(*ExtAfterCommit).Add(func())`, `.Run()`.
  - `(*TaskService).runExtWorkflowChildEvents(ctx, tx, parent db.Issue, claimed []db.IssueChildEvent) (func(), error)` (used by B7).
  - `type ExtWorkflowTaskParams struct{IssueID, AgentID, RunID, StepID pgtype.UUID; Role, Kind, HandoffNote string; ActorUserID, TriggerCommentID pgtype.UUID}` (contract + two additive fields).
  - `func (s *TaskService) EnqueueExtWorkflowTask(ctx, tx pgx.Tx, p ExtWorkflowTaskParams) (db.AgentTaskQueue, error)`; errors `ErrExtAgentUnavailable`, `ErrExtTaskSlotBusy`, `ErrAttributionFailClosed` (existing).
  - `func (s *TaskService) PublishExtWorkflowTaskQueued(ctx, task)`, `ExtBroadcastIssueUpdated(ctx, issue, prevStatus)`, `ExtPublishSystemComment(issue, created db.CreateCommentRow)`, `ExtPublishInbox(item, issueStatus)`; const `ExtWorkflowEngineCancelReason = "ext_workflow_engine"`.
  - `func (s *IssueService) ExtCreateChildIssueTx(ctx, tx pgx.Tx, p IssueCreateParams) (db.Issue, error)`, `ExtPublishIssueCreated(ctx, issue, actorType, actorID string)`.

Why not `IssueService.Create`: it opens its own transaction and, after commit, runs `maybeEnqueueOnAssign`; spec §4.2 needs the run, its steps and all children in one transaction with no platform enqueue. `ExtCreateChildIssueTx` reuses Create's numbering (`AllocateIssueNumber`, including the issue-count policy) and positioning, skips the duplicate guard (titles repeat across runs of one parent by design) and publishes `issue:created` only after the engine commits.

Dedup per (issue, agent, role): the `agent_task_queue` partial unique index allows one pending task per (issue, agent, comment thread). `EnqueueExtWorkflowTask` inserts with `ON CONFLICT DO NOTHING`; on conflict it returns the identical pending workflow task (same run, step, role, kind), adopts a plain queued unthreaded run of that agent (stamping it), or returns `ErrExtTaskSlotBusy` (the engine treats that as a failed dispatch).

- [ ] **Step 1: Write the failing test**

`server/internal/service/ext_workflow_task_test.go`:

```go
package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type extTaskFixture struct {
	principalFixture
	owner   pgtype.UUID
	agent   pgtype.UUID
	runtime string
	issue   pgtype.UUID
}

func newExtTaskFixture(t *testing.T) extTaskFixture {
	t.Helper()
	f, owner := newPrincipalFixture(t)
	agent := f.privateAgentOwnedBy(t, owner, "ext-task")
	var runtimeID string
	f.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, agent).Scan(&runtimeID)
	issue := f.Issue(t, "Workflow child")
	// Fixture issues bypass the workspace counter that issue creation draws from.
	f.Exec(t, `UPDATE workspace SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) WHERE id = $1`, f.WorkspaceID)
	f.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issue)
	return extTaskFixture{principalFixture: f, owner: parseTestUUID(t, owner), agent: parseTestUUID(t, agent), runtime: runtimeID, issue: parseTestUUID(t, issue)}
}

func (f extTaskFixture) params(runID, stepID pgtype.UUID) ExtWorkflowTaskParams {
	return ExtWorkflowTaskParams{
		IssueID: f.issue, AgentID: f.agent, RunID: runID, StepID: stepID,
		Role: "step", Kind: "step", HandoffNote: "Workflow step build, attempt 1 of 3.", ActorUserID: f.owner,
	}
}

func (f extTaskFixture) enqueue(t *testing.T, p ExtWorkflowTaskParams) (db.AgentTaskQueue, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	task, err := f.svc.TaskSvc.EnqueueExtWorkflowTask(ctx, tx, p)
	if err != nil {
		return task, err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return task, nil
}

func TestEnqueueExtWorkflowTaskStampsColumns(t *testing.T) {
	f := newExtTaskFixture(t)
	runID, stepID := dbid.NewV7(), dbid.NewV7()
	task, err := f.enqueue(t, f.params(runID, stepID))
	if err != nil {
		t.Fatalf("EnqueueExtWorkflowTask: %v", err)
	}
	if task.Status != "queued" || task.IssueID != f.issue || task.AgentID != f.agent {
		t.Fatalf("task = %s issue=%v agent=%v", task.Status, task.IssueID, task.AgentID)
	}
	if task.ExtWorkflowRunID != runID || task.ExtWorkflowStepID != stepID || task.ExtWorkflowRole.String != "step" || task.ExtWorkflowKind.String != "step" {
		t.Fatalf("ext columns = %v %v %q %q", task.ExtWorkflowRunID, task.ExtWorkflowStepID, task.ExtWorkflowRole.String, task.ExtWorkflowKind.String)
	}
	if task.HandoffNote.String != "Workflow step build, attempt 1 of 3." || task.OriginatorUserID != f.owner {
		t.Fatalf("handoff=%q originator=%v", task.HandoffNote.String, task.OriginatorUserID)
	}

	again, err := f.enqueue(t, f.params(runID, stepID))
	if err != nil || again.ID != task.ID {
		t.Fatalf("second enqueue = %v, %v; want the pending task %v", again.ID, err, task.ID)
	}
}

func TestEnqueueExtWorkflowTaskAdoptsQueuedPlainRun(t *testing.T) {
	f := newExtTaskFixture(t)
	plain := f.Task(t, util.UUIDToString(f.agent), testutil.Cols{"issue_id": util.UUIDToString(f.issue), "runtime_id": f.runtime})
	runID, stepID := dbid.NewV7(), dbid.NewV7()
	task, err := f.enqueue(t, f.params(runID, stepID))
	if err != nil {
		t.Fatalf("EnqueueExtWorkflowTask: %v", err)
	}
	if util.UUIDToString(task.ID) != plain || task.ExtWorkflowRunID != runID || task.ExtWorkflowStepID != stepID {
		t.Fatalf("task %v run=%v; want the adopted plain run %s", task.ID, task.ExtWorkflowRunID, plain)
	}
}

func TestEnqueueExtWorkflowTaskReportsBusySlot(t *testing.T) {
	f := newExtTaskFixture(t)
	f.Task(t, util.UUIDToString(f.agent), testutil.Cols{"issue_id": util.UUIDToString(f.issue), "runtime_id": f.runtime, "status": "dispatched"})
	if _, err := f.enqueue(t, f.params(dbid.NewV7(), dbid.NewV7())); !errors.Is(err, ErrExtTaskSlotBusy) {
		t.Fatalf("err = %v, want ErrExtTaskSlotBusy", err)
	}
}

func TestEnqueueExtWorkflowTaskRefusesArchivedAgent(t *testing.T) {
	f := newExtTaskFixture(t)
	f.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, f.agent)
	if _, err := f.enqueue(t, f.params(dbid.NewV7(), dbid.NewV7())); !errors.Is(err, ErrExtAgentUnavailable) {
		t.Fatalf("err = %v, want ErrExtAgentUnavailable", err)
	}
}

func TestExtCreateChildIssueTxCreatesBacklogChild(t *testing.T) {
	f := newExtTaskFixture(t)
	ctx := context.Background()
	bus := events.New()
	issues := NewIssueService(f.q, f.Pool, bus, analytics.NoopClient{}, f.svc.TaskSvc)
	parent, err := f.q.GetIssue(ctx, f.issue)
	if err != nil {
		t.Fatal(err)
	}
	var published []map[string]any
	bus.Subscribe(protocol.EventIssueCreated, func(e events.Event) {
		published = append(published, e.Payload.(map[string]any))
	})

	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	child, err := issues.ExtCreateChildIssueTx(ctx, tx, IssueCreateParams{
		WorkspaceID: parent.WorkspaceID, Title: parent.Title + " · Build", Status: "backlog", Priority: "none",
		AssigneeType: pgtype.Text{String: "agent", Valid: true}, AssigneeID: f.agent,
		CreatorType: "member", CreatorID: f.owner, ParentIssueID: parent.ID,
	})
	if err != nil {
		t.Fatalf("ExtCreateChildIssueTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, `DELETE FROM issue WHERE id = $1`, child.ID)
	if child.ParentIssueID != parent.ID || child.Status != "backlog" || child.Number == parent.Number {
		t.Fatalf("child parent=%v status=%s number=%d", child.ParentIssueID, child.Status, child.Number)
	}
	if n := f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, child.ID); n != 0 {
		t.Fatalf("child has %d tasks, want none", n)
	}
	issues.ExtPublishIssueCreated(ctx, child, "member", util.UUIDToString(f.owner))
	if len(published) != 1 || published[0]["issue"].(map[string]any)["id"] != util.UUIDToString(child.ID) {
		t.Fatalf("issue:created payloads = %v", published)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `cd server && go vet ./internal/service/`
Expected: build failure — `undefined: ExtWorkflowTaskParams`, `f.svc.TaskSvc.EnqueueExtWorkflowTask undefined`, `undefined: ErrExtTaskSlotBusy`, `issues.ExtCreateChildIssueTx undefined`.

- [ ] **Step 3: Implement**

`server/internal/service/ext_workflow_hooks.go`:

```go
package service

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow (fork): the seam between upstream services and the workflow
// engine in internal/extworkflow. service must not import extworkflow, so the
// engine is reached only through this interface.

// ExtChildEvent is one claimed issue_child_event row of a workflow parent.
type ExtChildEvent struct {
	ChildID pgtype.UUID
	Kind    string
}

// ExtWorkflowHooks is implemented by *extworkflow.Engine.
type ExtWorkflowHooks interface {
	ValidateAssignment(ctx context.Context, workspaceID, workflowID pgtype.UUID, actorType string, actorID pgtype.UUID) error
	StartRun(ctx context.Context, issueID pgtype.UUID, actorType string, actorID pgtype.UUID) error
	// OnChildEvents runs inside the caller's transaction. Notifications it
	// produces are registered on ExtAfterCommitFrom(ctx) when present.
	OnChildEvents(ctx context.Context, tx pgx.Tx, parentID pgtype.UUID, events []ExtChildEvent) error
}

// ExtAfterCommit collects work a hook produced inside a caller's transaction
// that may only run once that transaction committed (bus events, daemon
// wakeups).
type ExtAfterCommit struct {
	mu  sync.Mutex
	fns []func()
}

type extAfterCommitKey struct{}

// WithExtAfterCommit returns a context carrying a fresh collector.
func WithExtAfterCommit(ctx context.Context) (context.Context, *ExtAfterCommit) {
	a := &ExtAfterCommit{}
	return context.WithValue(ctx, extAfterCommitKey{}, a), a
}

// ExtAfterCommitFrom returns the collector on ctx, or nil.
func ExtAfterCommitFrom(ctx context.Context) *ExtAfterCommit {
	a, _ := ctx.Value(extAfterCommitKey{}).(*ExtAfterCommit)
	return a
}

func (a *ExtAfterCommit) Add(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fns = append(a.fns, fn)
}

// Run executes and clears the collected functions in order.
func (a *ExtAfterCommit) Run() {
	a.mu.Lock()
	fns := a.fns
	a.fns = nil
	a.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// runExtWorkflowChildEvents hands the claimed sub-issue changes of a workflow
// parent to the engine inside tx. The returned func publishes what the engine
// produced and must run only after tx committed.
func (s *TaskService) runExtWorkflowChildEvents(ctx context.Context, tx pgx.Tx, parent db.Issue, claimed []db.IssueChildEvent) (func(), error) {
	if s == nil || s.ExtWorkflow == nil || parent.AssigneeType.String != "workflow" {
		return func() {}, nil
	}
	hctx, after := WithExtAfterCommit(ctx)
	events := make([]ExtChildEvent, 0, len(claimed))
	for _, e := range claimed {
		events = append(events, ExtChildEvent{ChildID: e.ChildID, Kind: e.Kind})
	}
	if err := s.ExtWorkflow.OnChildEvents(hctx, tx, parent.ID, events); err != nil {
		return nil, err
	}
	return after.Run, nil
}
```

`server/internal/service/ext_workflow_task.go`:

```go
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ext-workflow (fork): the queue entry point for workflow step and supervisor
// tasks, plus the post-commit broadcasts the engine needs from this package.

// ExtWorkflowEngineCancelReason is the failure_reason the engine stamps on the
// tasks it cancels itself, so their terminal event is not read as an agent
// outcome. Keep in sync with the Cancel*ExtWorkflow* queries.
const ExtWorkflowEngineCancelReason = "ext_workflow_engine"

var (
	// ErrExtAgentUnavailable: the target agent is gone, archived or has no runtime.
	ErrExtAgentUnavailable = errors.New("ext-workflow: agent is archived or has no runtime")
	// ErrExtTaskSlotBusy: the agent already holds this issue's pending slot
	// with a task the engine cannot reuse (claimed, or another run's).
	ErrExtTaskSlotBusy = errors.New("ext-workflow: the agent already holds a pending task on this issue")
)

// ExtWorkflowTaskParams describes one workflow task.
type ExtWorkflowTaskParams struct {
	IssueID, AgentID, RunID pgtype.UUID
	StepID                  pgtype.UUID // invalid for summary/conversation
	Role                    string      // "step" | "supervisor"
	Kind                    string      // "step","review","failure","rewind_request","summary","conversation"
	HandoffNote             string
	// ActorUserID is the accountable member (the run's triggering member);
	// invalid falls back to the issue's attribution chain.
	ActorUserID pgtype.UUID
	// TriggerCommentID is set for conversation tasks: the member comment that
	// woke the supervisor. It also gives the task that comment's thread slot.
	TriggerCommentID pgtype.UUID
}

// EnqueueExtWorkflowTask inserts a workflow task inside the engine's
// transaction, stamped with the four ext columns. Pending-task dedup applies
// per (issue, agent, role): an identical pending workflow task is returned as
// is, and a plain queued run of the same agent on the issue is adopted.
// Callers publish with PublishExtWorkflowTaskQueued after their commit.
func (s *TaskService) EnqueueExtWorkflowTask(ctx context.Context, tx pgx.Tx, p ExtWorkflowTaskParams) (db.AgentTaskQueue, error) {
	if !p.IssueID.Valid || !p.AgentID.Valid || !p.RunID.Valid || p.Role == "" || p.Kind == "" {
		return db.AgentTaskQueue{}, errors.New("ext-workflow: incomplete task params")
	}
	q := s.Queries.WithTx(tx)
	issue, err := q.GetIssue(ctx, p.IssueID)
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("load issue: %w", err)
	}
	agent, err := q.GetAgent(ctx, p.AgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTaskQueue{}, ErrExtAgentUnavailable
	}
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("load agent: %w", err)
	}
	if agent.ArchivedAt.Valid || !agent.RuntimeID.Valid {
		return db.AgentTaskQueue{}, ErrExtAgentUnavailable
	}
	attr := s.attributionForIssueTask(ctx, issue, p.TriggerCommentID, attribution.SourceDelegation, p.ActorUserID)
	if attr, err = s.applyAttributionFallback(ctx, attr, agent); err != nil {
		return db.AgentTaskQueue{}, err
	}
	overlay := s.buildRuntimeMCPOverlay(ctx, attr.UserID, agent)
	source, delegatedFrom, evidenceKind, evidenceRef := attributionCreateParams(attr)
	summary := pgtype.Text{String: "Workflow " + strings.ReplaceAll(p.Kind, "_", " "), Valid: true}
	if p.TriggerCommentID.Valid {
		summary = s.buildCommentTriggerSummary(ctx, issue.WorkspaceID, p.TriggerCommentID)
	}
	role := pgtype.Text{String: p.Role, Valid: true}
	kind := pgtype.Text{String: p.Kind, Valid: true}
	task, err := q.CreateExtWorkflowTask(ctx, db.CreateExtWorkflowTaskParams{
		ID:                   dbid.NewV7(),
		AgentID:              agent.ID,
		RuntimeID:            agent.RuntimeID,
		IssueID:              issue.ID,
		Priority:             priorityToInt(issue.Priority),
		TriggerCommentID:     p.TriggerCommentID,
		TriggerSummary:       summary,
		HandoffNote:          pgtype.Text{String: p.HandoffNote, Valid: p.HandoffNote != ""},
		OriginatorUserID:     attr.UserID,
		AccountableUserID:    attr.AccountableUserID,
		RuntimeMcpOverlay:    overlay.Overlay,
		RuntimeConnectedApps: overlay.ConnectedApps,
		OriginatorSource:     source,
		DelegatedFromTaskID:  delegatedFrom,
		RuleVersionID:        attr.RuleVersionID,
		TriggerEvidenceKind:  evidenceKind,
		TriggerEvidenceRefID: evidenceRef,
		ExtWorkflowRunID:     p.RunID,
		ExtWorkflowStepID:    p.StepID,
		ExtWorkflowRole:      role,
		ExtWorkflowKind:      kind,
	})
	if err == nil {
		return task, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTaskQueue{}, fmt.Errorf("create ext workflow task: %w", err)
	}
	holders, err := q.ListPendingSlotTasksForIssueAgent(ctx, db.ListPendingSlotTasksForIssueAgentParams{IssueID: issue.ID, AgentID: agent.ID})
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("list pending slot tasks: %w", err)
	}
	for _, h := range holders {
		if h.ExtWorkflowRunID == p.RunID && h.ExtWorkflowStepID == p.StepID && h.ExtWorkflowRole == role && h.ExtWorkflowKind == kind {
			return h, nil
		}
	}
	if !p.TriggerCommentID.Valid {
		for _, h := range holders {
			if h.Status != "queued" || h.CommentThreadID.Valid || h.ExtWorkflowRunID.Valid {
				continue
			}
			adopted, err := q.AdoptTaskForExtWorkflow(ctx, db.AdoptTaskForExtWorkflowParams{
				ID: h.ID, RunID: p.RunID, StepID: p.StepID, Role: role, Kind: kind, HandoffNote: p.HandoffNote,
			})
			if err == nil {
				return adopted, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return db.AgentTaskQueue{}, fmt.Errorf("adopt pending task: %w", err)
			}
		}
	}
	return db.AgentTaskQueue{}, ErrExtTaskSlotBusy
}

// PublishExtWorkflowTaskQueued announces a committed workflow task: the
// task:queued event and the runtime wakeup, as every enqueue path does.
func (s *TaskService) PublishExtWorkflowTaskQueued(ctx context.Context, task db.AgentTaskQueue) {
	s.broadcastTaskEvent(ctx, protocol.EventTaskQueued, task)
	s.NotifyTaskEnqueued(ctx, task)
}

// ExtBroadcastIssueUpdated emits issue:updated for a status the engine wrote
// directly (no HTTP handler involved).
func (s *TaskService) ExtBroadcastIssueUpdated(ctx context.Context, issue db.Issue, prevStatus string) {
	if s.Bus == nil {
		return
	}
	s.broadcastIssueUpdated(ctx, issue, prevStatus)
}

// ExtPublishSystemComment emits comment:created for a system comment the
// engine wrote in its transaction.
func (s *TaskService) ExtPublishSystemComment(issue db.Issue, created db.CreateCommentRow) {
	if s.Bus == nil {
		return
	}
	comment := created.Comment()
	fields := commentEventFields(comment)
	fields["revision"] = comment.Revision
	s.Bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   "system",
		Payload: map[string]any{
			"comment":        fields,
			"issue_title":    issue.Title,
			"issue_status":   issue.Status,
			"issue_revision": created.IssueRevision,
		},
	})
}

// ExtPublishInbox emits inbox:new for an inbox item the engine wrote, in the
// same shape as the system wakeup notifications.
func (s *TaskService) ExtPublishInbox(item db.InboxItem, issueStatus string) {
	(&IssueWakeupService{Tasks: s}).publishSystemInbox(item, issueStatus)
}
```

`server/internal/service/ext_workflow_issue.go`:

```go
package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/issueposition"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// ext-workflow (fork): child issues of a workflow run.

// ExtCreateChildIssueTx inserts a workflow child issue inside the engine's
// transaction. It shares Create's numbering and positioning, but not its own
// transaction, duplicate guard or assignment enqueue: the engine alone decides
// when a child's agent runs, and the run and its children commit together.
// Callers publish with ExtPublishIssueCreated after their commit.
func (s *IssueService) ExtCreateChildIssueTx(ctx context.Context, tx pgx.Tx, p IssueCreateParams) (db.Issue, error) {
	q := s.Queries.WithTx(tx)
	number, err := AllocateIssueNumber(ctx, q, p.WorkspaceID, ResolveIssueCountPolicy(ctx, s.Entitlements, p.WorkspaceID))
	if err != nil {
		return db.Issue{}, fmt.Errorf("allocate issue number: %w", err)
	}
	position, err := issueposition.NextTopPosition(ctx, tx, p.WorkspaceID, p.Status)
	if err != nil {
		return db.Issue{}, fmt.Errorf("next top position: %w", err)
	}
	issue, err := q.CreateIssue(ctx, db.CreateIssueParams{
		ID:            dbid.NewV7(),
		WorkspaceID:   p.WorkspaceID,
		Title:         p.Title,
		Description:   p.Description,
		Status:        p.Status,
		Priority:      p.Priority,
		AssigneeType:  p.AssigneeType,
		AssigneeID:    p.AssigneeID,
		CreatorType:   p.CreatorType,
		CreatorID:     p.CreatorID,
		ParentIssueID: p.ParentIssueID,
		Position:      position,
		Number:        number,
		ProjectID:     p.ProjectID,
		Properties:    []byte(`{}`),
	})
	if err != nil {
		return db.Issue{}, fmt.Errorf("create child issue: %w", err)
	}
	return issue, nil
}

// ExtPublishIssueCreated emits issue:created for a committed child issue, with
// the same issue map background creators (autopilot) send.
func (s *IssueService) ExtPublishIssueCreated(ctx context.Context, issue db.Issue, actorType, actorID string) {
	prefix := ""
	if ws, err := s.Queries.GetWorkspace(ctx, issue.WorkspaceID); err == nil {
		prefix = ws.IssuePrefix
	}
	s.publishIssueCreated(issue, nil, nil, actorType, actorID, IssueCreateOpts{
		BroadcastPayload: func(i db.Issue, _ []db.Attachment, _ []db.IssueLabel) map[string]any {
			return map[string]any{"issue": IssueToMapResolved(ctx, s.Queries, i, prefix)}
		},
	})
}
```

Add the field to `TaskService` in `server/internal/service/task.go`:

`server/internal/service/task.go`:

```diff
diff --git a/server/internal/service/task.go b/server/internal/service/task.go
--- a/server/internal/service/task.go
+++ b/server/internal/service/task.go
@@ -77,6 +77,10 @@ type TaskService struct {
 	// state for a self-hosted deployment with no MULTICA_LLM_* configuration.
 	// Wired in router.go from the same *llm.Client that backs chat auto-titling.
 	QuickActions ChatQuickActionsLLM
+	// ext-workflow: workflow engine hooks; nil when MULTICA_WORKFLOW_ENGINE is
+	// off. Lives here because every IssueWakeupService is built from a
+	// TaskService, and the child-event hook runs there.
+	ExtWorkflow ExtWorkflowHooks
 	// quickActionsInFlight (chat session id -> struct{}{}) and
 	// quickActionsRunning admit suggestion passes: one per session, and a
 	// process-wide ceiling. Both zero values are usable, so a TaskService built
```

- [ ] **Step 4: Run, expect PASS**

Run:
```bash
cd server && go build ./... && go vet ./internal/service/
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/service/ -run \"TestEnqueueExtWorkflow|TestExtCreateChildIssueTx\" -count=1 -v'"
```
Expected: five `--- PASS` lines (`TestEnqueueExtWorkflowTaskStampsColumns`, `…AdoptsQueuedPlainRun`, `…ReportsBusySlot`, `…RefusesArchivedAgent`, `TestExtCreateChildIssueTxCreatesBacklogChild`), `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/service/ext_workflow_hooks.go server/internal/service/ext_workflow_task.go server/internal/service/ext_workflow_issue.go server/internal/service/ext_workflow_task_test.go server/internal/service/task.go
git commit -m "feat(ext-workflow): service seam for workflow tasks and child issues

EnqueueExtWorkflowTask stamps the ext columns inside the engine's
transaction with per-slot dedup; child issues are created in the same
transaction; hooks and an after-commit collector let upstream services
call the engine without importing it.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


---

### Task B5: Engine core — `Advance`, effect execution, `ValidateAssignment`, `StartRun`

**Files:**
- Create: `server/internal/extworkflow/engine.go`
- Create: `server/internal/extworkflow/assign.go`
- Modify: `server/pkg/protocol/events.go` (append a const block at the end of the file, after the closing `)` of the main block)
- Test: `server/internal/extworkflow/testenv_test.go` (harness), `server/internal/extworkflow/engine_test.go`

**Interfaces:**
- Consumes: B1 (`Next`, types), B3 queries, B4 (`EnqueueExtWorkflowTask`, `ExtCreateChildIssueTx`, publish helpers, `SettleTerminalTaskState`), Part A (`GetExtWorkflowInWorkspace`, `ListExtWorkflowNodes`, `Definition`).
- Produces:
  - `type AgentAccess interface{CanInvokeAgent(ctx, workspaceID pgtype.UUID, actorType string, actorID pgtype.UUID, agentID pgtype.UUID) (bool, error)}`, `type Publisher interface{Publish(eventType, workspaceID, actorType, actorID string, payload map[string]any)}`, `type Deps struct{Pool *pgxpool.Pool; Queries *db.Queries; Issues *service.IssueService; Tasks *service.TaskService; Access AgentAccess; Publisher Publisher; Enabled bool}` — exactly the contract.
  - `func NewEngine(Deps) *Engine`, `func (e *Engine) Enabled() bool` (nil-safe).
  - `type Actor struct{Type string; ID, OnBehalfOf, TaskID pgtype.UUID}`, `var EngineActor`.
  - `type AdvanceInput struct{StepKey string; Event Event; Actor Actor; ExpectedStatus StepStatus}`; `func (e *Engine) Advance(ctx, runID pgtype.UUID, in AdvanceInput) error`.
  - `type RunSnapshot struct{Run db.ExtWorkflowRun; Def Definition; Steps []db.ExtWorkflowRunStep; ByKey map[string]db.ExtWorkflowRunStep; Active []db.AgentTaskQueue; State RunState}`, `(*RunSnapshot).KeyOf(stepID) (string, bool)`, `func LoadRunSnapshot(ctx, q *db.Queries, run db.ExtWorkflowRun) (*RunSnapshot, error)`, `func (e *Engine) LoadRun(ctx, runID) (*RunSnapshot, error)`.
  - `func (e *Engine) ValidateAssignment(ctx, workspaceID, workflowID pgtype.UUID, actorType string, actorID pgtype.UUID) error` (`*AssignError`: 409 `workflow_engine_disabled`, 400 not found / archived / no nodes / supervisor unavailable, 403 not invokable).
  - `func (e *Engine) StartRun(ctx, issueID pgtype.UUID, actorType string, actorID pgtype.UUID) error`.
  - `const InboxTypeEscalation = "ext_workflow_escalation"`; `protocol.EventExtWorkflowRunUpdated = "ext_workflow_run:updated"` (payload `{run_id, issue_id, workflow_id}`).
  - Internal: `advance(ctx, ext pgx.Tx, runID, derive)` — `LockExtWorkflowRun` (`SELECT … FOR UPDATE`), load snapshot, derive events, `apply` (Next → `execute` → `persist`), commit, `flush` (issue:created, issue:updated, comment:created, inbox:new, task:queued, `ext_workflow_run:updated`, then task:cancelled last because its listener re-enters the engine). With `ext != nil` the caller's transaction is used and `flush` is registered on `service.ExtAfterCommitFrom(ctx)`.

Effect execution notes:
- Child/parent status goes through `UpdateIssueStatus` (no handler, so no `WillEnqueueRun` and no agent enqueue) and is skipped when the effective status already matches; `issue:updated` is broadcast after commit via `ExtBroadcastIssueUpdated`.
- A dispatch the engine cannot start (`ErrAgentNotInvokable` from the access check against the run's triggering actor, `ErrExtAgentUnavailable`, `ErrExtTaskSlotBusy`, `ErrAttributionFailClosed`) is fed back into `Next` as `step_failed{dispatch_failed}` (step) or as supervisor silence / summary end (supervisor), inside the same transaction (spec §4.2, §10).
- Cancels (`EffCancelStepWork`, `EffCancelRunTasks`) settle the rows with `SettleTerminalTaskState` in the transaction and spare `Actor.TaskID` (the deciding agent's own turn).
- Escalation: inbox item `ext_workflow_escalation` (`action_required`) to the triggering member, else the workflow creator, plus the escalation milestone comment on the parent. Milestones are system comments (`author_type='system'`, zero author id, `type='system'`) only for run started, escalation, finished, failed, cancelled.
- `StartRun` falls back to the parent's creator as actor when given a non member/agent actor (`triggered_by_id` is NOT NULL in Part A's schema).

- [ ] **Step 1: Write the failing test**

Harness (reused by B6 and B10):

`server/internal/extworkflow/testenv_test.go`:

```go
package extworkflow

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DB-backed engine tests. Each test gets its own workspace; without a
// database they skip, like every other DB-backed package.

var (
	poolMu    sync.Mutex
	pool      *pgxpool.Pool
	envSerial atomic.Int64
)

func TestMain(m *testing.M) {
	code := m.Run()
	poolMu.Lock()
	if pool != nil {
		pool.Close()
	}
	poolMu.Unlock()
	os.Exit(code)
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolMu.Lock()
	defer poolMu.Unlock()
	if pool != nil {
		return pool
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	p, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Skipf("database not available: %v", err)
	}
	if err := p.Ping(context.Background()); err != nil {
		p.Close()
		t.Skipf("database not reachable: %v", err)
	}
	pool = p
	return pool
}

// recPublisher records Publisher calls.
type recPublisher struct {
	mu     sync.Mutex
	events []string // "<type>:<run_id>"
}

func (r *recPublisher) Publish(eventType, _, _, _ string, payload map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf("%s:%v", eventType, payload["run_id"]))
}

func (r *recPublisher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// fakeAccess allows every agent except the denied ones.
type fakeAccess struct {
	mu     sync.Mutex
	denied map[pgtype.UUID]bool
}

func (f *fakeAccess) CanInvokeAgent(_ context.Context, _ pgtype.UUID, _ string, _ pgtype.UUID, agentID pgtype.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.denied[agentID], nil
}

func (f *fakeAccess) deny(agentID pgtype.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.denied == nil {
		f.denied = map[pgtype.UUID]bool{}
	}
	f.denied[agentID] = true
}

type env struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	fx      *testutil.Fixture
	ws      pgtype.UUID
	user    pgtype.UUID
	runtime string
	bus     *events.Bus
	tasks   *service.TaskService
	issues  *service.IssueService
	access  *fakeAccess
	pub     *recPublisher
	engine  *Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	p := testPool(t)
	n := envSerial.Add(1)
	fx := testutil.New(p, "", "")
	user := fx.User(t, "Workflow User", fmt.Sprintf("extwf-%d-%d@multica.test", os.Getpid(), n))
	ws := fx.Workspace(t, "Workflow WS", fmt.Sprintf("extwf-%d-%d", os.Getpid(), n), testutil.Cols{"issue_prefix": "WF"})
	fx.WorkspaceID, fx.UserID = ws, user
	fx.Member(t, ws, user, "owner")
	rt := fx.Runtime(t, "wf-runtime")
	// Rows the engine creates. Registered after the workspace, so they are
	// removed before it (cleanup runs in reverse order).
	for _, stmt := range []string{
		`DELETE FROM issue WHERE workspace_id = $1`,
		`DELETE FROM agent_task_queue WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1)`,
		`DELETE FROM comment WHERE workspace_id = $1`,
		`DELETE FROM inbox_item WHERE workspace_id = $1`,
		`DELETE FROM activity_log WHERE workspace_id = $1`,
		`DELETE FROM issue_child_event WHERE workspace_id = $1`,
		`DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE workspace_id = $1)`,
		`DELETE FROM issue_wakeup WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_run_event WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_run_step WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_run WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow_node WHERE workspace_id = $1`,
		`DELETE FROM ext_workflow WHERE workspace_id = $1`,
	} {
		fx.Cleanup(t, stmt, ws)
	}
	q := db.New(p)
	bus := events.New()
	tasks := service.NewTaskService(q, p, nil, bus)
	issues := service.NewIssueService(q, p, bus, analytics.NoopClient{}, tasks)
	e := &env{
		pool: p, q: q, fx: fx, ws: util.MustParseUUID(ws), user: util.MustParseUUID(user), runtime: rt,
		bus: bus, tasks: tasks, issues: issues, access: &fakeAccess{}, pub: &recPublisher{},
	}
	e.engine = NewEngine(Deps{Pool: p, Queries: q, Issues: issues, Tasks: tasks, Access: e.access, Publisher: e.pub, Enabled: true})
	return e
}

func (e *env) agent(t *testing.T, name string) pgtype.UUID {
	t.Helper()
	return util.MustParseUUID(e.fx.Agent(t, name, e.runtime))
}

type wfNode struct {
	key, title  string
	agent       pgtype.UUID
	review      bool
	maxAttempts int
	deps        []string
}

// workflow inserts a workflow and its nodes in definition order.
func (e *env) workflow(t *testing.T, supervisor pgtype.UUID, maxRewinds int, nodes ...wfNode) pgtype.UUID {
	t.Helper()
	id := e.fx.Insert(t, "ext_workflow", testutil.Cols{
		"workspace_id": e.fx.WorkspaceID, "name": "Ship", "supervisor_agent_id": supervisor,
		"max_rewinds": maxRewinds, "creator_id": e.fx.UserID,
	})
	for i, n := range nodes {
		attempts := n.maxAttempts
		if attempts == 0 {
			attempts = 3
		}
		deps := n.deps
		if deps == nil {
			deps = []string{}
		}
		e.fx.Insert(t, "ext_workflow_node", testutil.Cols{
			"workflow_id": id, "workspace_id": e.fx.WorkspaceID, "key": n.key, "title": n.title, "agent_id": n.agent,
			"prompt": "Do " + n.title, "requires_review": n.review, "max_attempts": attempts, "depends_on": deps, "position": i,
		})
	}
	return util.MustParseUUID(id)
}

// parentIssue inserts an issue assigned to the workflow.
func (e *env) parentIssue(t *testing.T, workflowID pgtype.UUID, status string) pgtype.UUID {
	t.Helper()
	id := e.fx.Issue(t, "Ship feature", testutil.Cols{"status": status, "assignee_type": "workflow", "assignee_id": workflowID})
	// Fixture issues bypass the counter child issues draw their numbers from.
	e.fx.Exec(t, `UPDATE workspace SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) WHERE id = $1`, e.fx.WorkspaceID)
	return util.MustParseUUID(id)
}

func (e *env) start(t *testing.T, parent pgtype.UUID) db.ExtWorkflowRun {
	t.Helper()
	if err := e.engine.StartRun(context.Background(), parent, "member", e.user); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return e.run(t, parent)
}

// run returns the newest run of a parent issue, re-read.
func (e *env) run(t *testing.T, parent pgtype.UUID) db.ExtWorkflowRun {
	t.Helper()
	var id pgtype.UUID
	e.fx.QueryRow(t, `SELECT id FROM ext_workflow_run WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1`, parent).Scan(&id)
	run, err := e.q.GetExtWorkflowRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (e *env) step(t *testing.T, run db.ExtWorkflowRun, key string) db.ExtWorkflowRunStep {
	t.Helper()
	steps, err := e.q.ListExtWorkflowRunSteps(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.NodeKey == key {
			return s
		}
	}
	t.Fatalf("no step %q", key)
	return db.ExtWorkflowRunStep{}
}

func (e *env) issue(t *testing.T, id pgtype.UUID) db.Issue {
	t.Helper()
	issue, err := e.q.GetIssue(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

// latestTask is the newest task of a step for a role ("step"/"supervisor").
func (e *env) latestTask(t *testing.T, step db.ExtWorkflowRunStep, role string) db.AgentTaskQueue {
	t.Helper()
	task, err := e.q.GetLatestExtWorkflowTaskForStep(context.Background(), db.GetLatestExtWorkflowTaskForStepParams{
		StepID: step.ID, Role: pgtype.Text{String: role, Valid: true},
	})
	if err != nil {
		t.Fatalf("latest %s task of %s: %v", role, step.NodeKey, err)
	}
	return task
}

func (e *env) countTasks(t *testing.T, step db.ExtWorkflowRunStep, role string) int {
	t.Helper()
	return e.fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE ext_workflow_step_id = $1 AND ext_workflow_role = $2`, step.ID, role)
}

func (e *env) runEvents(t *testing.T, run db.ExtWorkflowRun) []string {
	t.Helper()
	rows, err := e.q.ListExtWorkflowRunEvents(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(rows))
	for _, r := range rows {
		kinds = append(kinds, r.Kind)
	}
	return kinds
}

func wantStepRow(t *testing.T, s db.ExtWorkflowRunStep, status StepStatus, attempts int) {
	t.Helper()
	if StepStatus(s.Status) != status || int(s.Attempts) != attempts {
		t.Fatalf("step %s = %s/%d, want %s/%d", s.NodeKey, s.Status, s.Attempts, status, attempts)
	}
}
```

`server/internal/extworkflow/engine_test.go`:

```go
package extworkflow

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestValidateAssignment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, worker := e.agent(t, "Supervisor"), e.agent(t, "Worker")
	ok := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: worker})
	empty := e.workflow(t, supervisor, 3)
	archived := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: worker})
	e.fx.Exec(t, `UPDATE ext_workflow SET archived_at = now() WHERE id = $1`, archived)
	idle := util.MustParseUUID(e.fx.Agent(t, "No runtime", ""))
	noRuntime := e.workflow(t, idle, 3, wfNode{key: "build", title: "Build", agent: worker})

	assignErr := func(err error) *AssignError {
		t.Helper()
		var ae *AssignError
		if !errors.As(err, &ae) {
			t.Fatalf("err = %v, want *AssignError", err)
		}
		return ae
	}
	if err := e.engine.ValidateAssignment(ctx, e.ws, ok, "member", e.user); err != nil {
		t.Fatalf("valid assignment: %v", err)
	}
	for name, tc := range map[string]struct {
		workflow pgtype.UUID
		status   int
		sentinel error
	}{
		"unknown":    {dbid.NewV7(), http.StatusBadRequest, ErrWorkflowNotFound},
		"archived":   {archived, http.StatusBadRequest, ErrWorkflowArchived},
		"no nodes":   {empty, http.StatusBadRequest, ErrNoNodes},
		"supervisor": {noRuntime, http.StatusBadRequest, ErrSupervisorUnavailable},
	} {
		ae := assignErr(e.engine.ValidateAssignment(ctx, e.ws, tc.workflow, "member", e.user))
		if ae.Status != tc.status || !errors.Is(ae, tc.sentinel) {
			t.Fatalf("%s: status=%d err=%v", name, ae.Status, ae.Err)
		}
	}
	e.access.deny(worker)
	if ae := assignErr(e.engine.ValidateAssignment(ctx, e.ws, ok, "member", e.user)); ae.Status != http.StatusForbidden || !errors.Is(ae, ErrAgentNotInvokable) {
		t.Fatalf("denied: status=%d err=%v", ae.Status, ae.Err)
	}
	off := NewEngine(Deps{Pool: e.pool, Queries: e.q, Issues: e.issues, Tasks: e.tasks, Access: e.access, Publisher: e.pub})
	if ae := assignErr(off.ValidateAssignment(ctx, e.ws, ok, "member", e.user)); ae.Status != http.StatusConflict || ae.Code != "workflow_engine_disabled" {
		t.Fatalf("disabled: status=%d code=%s", ae.Status, ae.Code)
	}
}

func TestStartRunCreatesChildrenAndDispatchesRoots(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, review: true, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")

	run := e.start(t, parent)
	if RunStatus(run.Status) != RunRunning || run.TriggeredByType != "member" || run.TriggeredByID != e.user {
		t.Fatalf("run = %s by %s:%v", run.Status, run.TriggeredByType, run.TriggeredByID)
	}
	spec, build := e.step(t, run, "spec"), e.step(t, run, "build")
	wantStepRow(t, spec, StepRunning, 1)
	wantStepRow(t, build, StepPending, 0)
	specChild, buildChild := e.issue(t, spec.IssueID), e.issue(t, build.IssueID)
	if specChild.ParentIssueID != parent || specChild.Status != "in_progress" || specChild.AssigneeID != planner || specChild.Title != "Ship feature · Spec" {
		t.Fatalf("spec child = %q %s parent=%v assignee=%v", specChild.Title, specChild.Status, specChild.ParentIssueID, specChild.AssigneeID)
	}
	if buildChild.Status != "backlog" || e.countTasks(t, build, RoleStep) != 0 {
		t.Fatalf("build child = %s with %d tasks, want backlog and none", buildChild.Status, e.countTasks(t, build, RoleStep))
	}
	task := e.latestTask(t, spec, RoleStep)
	if task.Status != "queued" || task.AgentID != planner || task.IssueID != spec.IssueID ||
		task.ExtWorkflowRunID != run.ID || task.ExtWorkflowKind.String != KindStep || task.OriginatorUserID != e.user {
		t.Fatalf("spec task = %+v", task)
	}
	if got := e.issue(t, parent).Status; got != "in_progress" {
		t.Fatalf("parent status = %s, want in_progress", got)
	}
	if got, want := e.runEvents(t, run), []string{RunEventRunStarted, RunEventStepStarted}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`, parent); n != 1 {
		t.Fatalf("milestone comments = %d, want 1", n)
	}
	if !reflect.DeepEqual(e.pub.events, []string{protocol.EventExtWorkflowRunUpdated + ":" + util.UUIDToString(run.ID)}) {
		t.Fatalf("published = %v", e.pub.events)
	}

	if err := e.engine.StartRun(ctx, parent, "member", e.user); err != nil {
		t.Fatalf("second StartRun: %v", err)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run WHERE issue_id = $1`, parent); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

func TestStartRunIgnoresOtherAssignees(t *testing.T) {
	e := newEnv(t)
	agent := e.agent(t, "Solo")
	issue := util.MustParseUUID(e.fx.Issue(t, "Plain", testutil.Cols{"assignee_type": "agent", "assignee_id": agent}))
	if err := e.engine.StartRun(context.Background(), issue, "member", e.user); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run WHERE issue_id = $1`, issue); n != 0 {
		t.Fatalf("runs = %d, want 0", n)
	}
}

func TestStartRunDispatchFailureGoesToSupervisor(t *testing.T) {
	e := newEnv(t)
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	parent := e.parentIssue(t, wf, "todo")
	e.access.deny(planner) // the assigner lost access after assigning

	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	wantStepRow(t, spec, StepAwaitingSupervisor, 1)
	if spec.PendingReason.String != string(PendingFailure) || spec.SupervisorWakes != 1 || e.countTasks(t, spec, RoleStep) != 0 {
		t.Fatalf("spec reason=%q wakes=%d step tasks=%d", spec.PendingReason.String, spec.SupervisorWakes, e.countTasks(t, spec, RoleStep))
	}
	sup := e.latestTask(t, spec, RoleSupervisor)
	if sup.IssueID != parent || sup.AgentID != supervisor || sup.ExtWorkflowKind.String != KindFailure {
		t.Fatalf("supervisor task issue=%v agent=%v kind=%q", sup.IssueID, sup.AgentID, sup.ExtWorkflowKind.String)
	}
}

func TestAdvanceGuardsDecisions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	run := e.start(t, e.parentIssue(t, wf, "todo"))

	err := e.engine.Advance(ctx, run.ID, AdvanceInput{StepKey: "spec", Event: Event{Kind: EvDecision, Decision: Decision{Action: ActionApprove}}, Actor: Actor{Type: "member", ID: e.user}})
	if !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("approve while running: %v", err)
	}
	err = e.engine.Advance(ctx, run.ID, AdvanceInput{StepKey: "spec", Event: Event{Kind: EvTick}, ExpectedStatus: StepAwaitingHuman})
	if !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("expected-status guard: %v", err)
	}
	if err := e.engine.Advance(ctx, dbid.NewV7(), AdvanceInput{Event: Event{Kind: EvTick}}); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	wantStepRow(t, e.step(t, run, "spec"), StepRunning, 1)
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `cd server && go vet ./internal/extworkflow/`
Expected: build failure — `undefined: Engine`, `undefined: NewEngine`, `undefined: Deps`, `undefined: AdvanceInput`, `undefined: protocol.EventExtWorkflowRunUpdated`.

- [ ] **Step 3: Implement**

`server/pkg/protocol/events.go`:

```diff
diff --git a/server/pkg/protocol/events.go b/server/pkg/protocol/events.go
--- a/server/pkg/protocol/events.go
+++ b/server/pkg/protocol/events.go
@@ -222,3 +222,9 @@ const (
 	EventTelegramInstallationCreated = "telegram_installation:created"
 	EventTelegramInstallationRevoked = "telegram_installation:revoked"
 )
+
+// ext-workflow: run lifecycle event (fork namespace). Payload: {run_id,
+// issue_id, workflow_id}. Clients refetch the run and the parent issue.
+const (
+	EventExtWorkflowRunUpdated = "ext_workflow_run:updated"
+)
```

`server/internal/extworkflow/engine.go`:

```go
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
```

`server/internal/extworkflow/assign.go`:

```go
package extworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// ValidateAssignment answers whether the actor may assign an issue to the
// workflow (spec §4.1). Refusals are *AssignError carrying the HTTP status.
func (e *Engine) ValidateAssignment(ctx context.Context, workspaceID, workflowID pgtype.UUID, actorType string, actorID pgtype.UUID) error {
	if !e.Enabled() {
		return &AssignError{Status: http.StatusConflict, Code: "workflow_engine_disabled", Message: "workflow_engine_disabled", Err: ErrEngineDisabled}
	}
	wf, err := e.q.GetExtWorkflowInWorkspace(ctx, db.GetExtWorkflowInWorkspaceParams{ID: workflowID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return &AssignError{Status: http.StatusBadRequest, Code: "workflow_not_found", Message: "assignee_id does not refer to a workflow in this workspace", Err: ErrWorkflowNotFound}
	}
	if err != nil {
		return fmt.Errorf("load workflow: %w", err)
	}
	if wf.ArchivedAt.Valid {
		return &AssignError{Status: http.StatusBadRequest, Code: "workflow_archived", Message: "cannot assign to an archived workflow", Err: ErrWorkflowArchived}
	}
	nodes, err := e.q.ListExtWorkflowNodes(ctx, wf.ID)
	if err != nil {
		return fmt.Errorf("load workflow nodes: %w", err)
	}
	if len(nodes) == 0 {
		return &AssignError{Status: http.StatusBadRequest, Code: "workflow_has_no_nodes", Message: "the workflow has no nodes", Err: ErrNoNodes}
	}
	supervisor, err := e.q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: wf.SupervisorAgentID, WorkspaceID: workspaceID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load supervisor: %w", err)
	}
	if err != nil || supervisor.ArchivedAt.Valid || !supervisor.RuntimeID.Valid {
		return &AssignError{Status: http.StatusBadRequest, Code: "workflow_supervisor_unavailable", Message: "the workflow's supervisor agent is archived or has no runtime", Err: ErrSupervisorUnavailable}
	}
	agents := []pgtype.UUID{wf.SupervisorAgentID}
	seen := map[pgtype.UUID]bool{wf.SupervisorAgentID: true}
	for _, n := range nodes {
		if !seen[n.AgentID] {
			seen[n.AgentID] = true
			agents = append(agents, n.AgentID)
		}
	}
	if e.access != nil {
		for _, agentID := range agents {
			ok, err := e.access.CanInvokeAgent(ctx, workspaceID, actorType, actorID, agentID)
			if err != nil {
				return fmt.Errorf("check agent access: %w", err)
			}
			if !ok {
				return &AssignError{Status: http.StatusForbidden, Code: "workflow_agent_not_invokable", Message: "you do not have permission to assign work to every agent in this workflow", Err: ErrAgentNotInvokable}
			}
		}
	}
	return nil
}

// StartRun creates a run for a workflow-assigned issue in one transaction:
// the run with a definition snapshot, one backlog child issue and one pending
// step per node, the parent in progress, then the first scheduling pass
// (spec §4.2). It is a no-op when the issue is not assigned to a workflow or
// already has an active run.
func (e *Engine) StartRun(ctx context.Context, issueID pgtype.UUID, actorType string, actorID pgtype.UUID) error {
	if !e.Enabled() {
		return ErrEngineDisabled
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := e.q.WithTx(tx)
	parent, err := q.LockIssueForExtWorkflowStart(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock issue: %w", err)
	}
	if parent.AssigneeType.String != "workflow" || !parent.AssigneeID.Valid {
		return nil
	}
	if _, err := q.GetActiveExtWorkflowRunByIssue(ctx, parent.ID); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check active run: %w", err)
	}
	wf, err := q.GetExtWorkflowInWorkspace(ctx, db.GetExtWorkflowInWorkspaceParams{ID: parent.AssigneeID, WorkspaceID: parent.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkflowNotFound
	}
	if err != nil {
		return fmt.Errorf("load workflow: %w", err)
	}
	if wf.ArchivedAt.Valid {
		return ErrWorkflowArchived
	}
	nodes, err := q.ListExtWorkflowNodes(ctx, wf.ID)
	if err != nil {
		return fmt.Errorf("load workflow nodes: %w", err)
	}
	if len(nodes) == 0 {
		return ErrNoNodes
	}
	def := definitionFromRows(wf, nodes)
	raw, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("encode definition: %w", err)
	}
	// The run, the children and their creator name a real actor; a system
	// trigger is attributed to the issue's creator.
	if (actorType != "member" && actorType != "agent") || !actorID.Valid {
		actorType, actorID = parent.CreatorType, parent.CreatorID
	}
	run, err := q.CreateExtWorkflowRun(ctx, db.CreateExtWorkflowRunParams{
		ID: dbid.NewV7(), WorkspaceID: parent.WorkspaceID, WorkflowID: wf.ID, IssueID: parent.ID,
		TriggeredByType: actorType, TriggeredByID: actorID, Definition: raw,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil // a concurrent StartRun won
		}
		return fmt.Errorf("create run: %w", err)
	}
	out := &outbox{run: run}
	for _, n := range def.Nodes {
		agentID, err := util.ParseUUID(n.AgentID)
		if err != nil {
			return fmt.Errorf("node %q agent: %w", n.Key, err)
		}
		child, err := e.issues.ExtCreateChildIssueTx(ctx, tx, service.IssueCreateParams{
			WorkspaceID:   parent.WorkspaceID,
			Title:         parent.Title + " · " + n.Title,
			Description:   pgtype.Text{String: n.Prompt, Valid: n.Prompt != ""},
			Status:        "backlog",
			Priority:      parent.Priority,
			AssigneeType:  pgtype.Text{String: "agent", Valid: true},
			AssigneeID:    agentID,
			CreatorType:   actorType,
			CreatorID:     actorID,
			ParentIssueID: parent.ID,
			ProjectID:     parent.ProjectID,
		})
		if err != nil {
			return fmt.Errorf("create child issue for %q: %w", n.Key, err)
		}
		if _, err := q.CreateExtWorkflowRunStep(ctx, db.CreateExtWorkflowRunStepParams{
			ID: dbid.NewV7(), RunID: run.ID, WorkspaceID: run.WorkspaceID, NodeKey: n.Key, AgentID: agentID, IssueID: child.ID,
		}); err != nil {
			return fmt.Errorf("create step %q: %w", n.Key, err)
		}
		out.created = append(out.created, createdIssue{issue: child, actorType: actorType, actorID: util.UUIDToString(actorID)})
	}
	if err := e.setIssueStatus(ctx, q, parent.ID, "in_progress", out); err != nil {
		return err
	}
	snap, err := LoadRunSnapshot(ctx, q, run)
	if err != nil {
		return err
	}
	starter := Actor{Type: actorType, ID: actorID}
	if err := e.recordEvent(ctx, q, snap, record(RunEventRunStarted, "", map[string]any{"workflow_id": util.UUIDToString(wf.ID), "steps": len(def.Nodes)}), starter); err != nil {
		return err
	}
	if err := e.milestone(ctx, q, snap, MilestoneRunStarted, "", "", out); err != nil {
		return err
	}
	if err := e.apply(ctx, tx, q, snap, nil, out); err != nil {
		return err
	}
	out.runChanged = true
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	e.flush(ctx, out)
	return nil
}

// definitionFromRows is the run's snapshot of a workflow and its nodes.
func definitionFromRows(wf db.ExtWorkflow, nodes []db.ExtWorkflowNode) Definition {
	d := Definition{SupervisorAgentID: util.UUIDToString(wf.SupervisorAgentID), MaxRewinds: int(wf.MaxRewinds)}
	for _, n := range nodes {
		d.Nodes = append(d.Nodes, Node{
			Key: n.Key, Title: n.Title, AgentID: util.UUIDToString(n.AgentID), Prompt: n.Prompt,
			RequiresReview: n.RequiresReview, MaxAttempts: int(n.MaxAttempts), DependsOn: append([]string{}, n.DependsOn...),
		})
	}
	return d
}
```

- [ ] **Step 4: Run, expect PASS**

Run:
```bash
cd server && gofmt -l ./internal/extworkflow && go vet ./internal/extworkflow/
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -count=1 -v'"
```
Expected: `gofmt -l` prints nothing; `--- PASS` for `TestValidateAssignment`, `TestStartRunCreatesChildrenAndDispatchesRoots`, `TestStartRunIgnoresOtherAssignees`, `TestStartRunDispatchFailureGoesToSupervisor`, `TestAdvanceGuardsDecisions` (not SKIP), plus the B1/B2/Part A pure tests; `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/extworkflow/engine.go server/internal/extworkflow/assign.go server/internal/extworkflow/testenv_test.go server/internal/extworkflow/engine_test.go server/pkg/protocol/events.go
git commit -m "feat(ext-workflow): engine core with StartRun and Advance

Each change locks the run row, applies Next, executes its effects and
persists in one transaction, then publishes. StartRun snapshots the
definition and creates the steps and backlog child issues atomically.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


---

### Task B6: Observation entry points — task terminal, child events, parent change, reconcile

**Files:**
- Create: `server/internal/extworkflow/observe.go`
- Modify: `server/internal/extworkflow/testenv_test.go` (wire the engine into the task service; add `endTask`, `setStatus`)
- Test: `server/internal/extworkflow/observe_test.go`

**Interfaces:**
- Consumes: B5 `advance`, B3 queries, `service.ExtChildEvent`, `service.ExtWorkflowEngineCancelReason`, `issuestatus.Effective`.
- Produces (contract): `OnTaskTerminal(ctx, taskID pgtype.UUID) error`, `OnChildEvents(ctx, tx pgx.Tx, parentID pgtype.UUID, events []service.ExtChildEvent) error`, `OnParentChanged(ctx, issueID pgtype.UUID) error`, `Reconcile(ctx) error`; plus `OnParentDeleted(ctx, issueID pgtype.UUID) error` (B8 calls it before `CancelTasksForIssue` on delete) and `const ReconcileBatch = 200`. `*Engine` now satisfies `service.ExtWorkflowHooks` (compile-time assertion).

Derivation rules (all under the run lock, so stale/duplicate signals converge):
- A step's child: deleted or effectively `cancelled` → `child_cancelled` (skip, spec §5.3 "human cancels the child"); while `running`, child effectively `done`/`in_review` → `step_finished`.
- Running step, child not finished: the step's **newest** `role=step` task decides, once settled (`completed` → `step_failed{ended_without_finishing}`, `failed` without a retry child → `step_failed{<failure_reason>}`, `cancelled` not by the engine → `step_failed{cancelled}`). Tasks of other runs or without the stamp never count (spec §5.3).
- `awaiting_supervisor` with no in-flight supervisor task and `supervisor_wakes > 0`: the newest settled `role=supervisor` task for the step, created in the same transaction that last updated the step (`created_at >= step.updated_at`), → `supervisor_no_decision`.
- Summary requested, all settled, newest summary task settled (completed, failed or cancelled) → `summary_ended`.
- Parent deleted, effectively cancelled, or not assigned to this run's workflow → `cancel_run`.
- `OnTaskTerminal` ignores non-workflow tasks and `failed` tasks with a pending retry (`HasRetryTaskForParent`); `Reconcile` visits ≤ 200 active runs, oldest `updated_at` first, and touches each.

- [ ] **Step 1: Write the failing test**

Update the harness (`server/internal/extworkflow/testenv_test.go`):

`server/internal/extworkflow/testenv_test.go`:

```diff
diff --git a/server/internal/extworkflow/testenv_test.go b/server/internal/extworkflow/testenv_test.go
--- a/server/internal/extworkflow/testenv_test.go
+++ b/server/internal/extworkflow/testenv_test.go
@@ -152,9 +152,28 @@ func newEnv(t *testing.T) *env {
 		bus: bus, tasks: tasks, issues: issues, access: &fakeAccess{}, pub: &recPublisher{},
 	}
 	e.engine = NewEngine(Deps{Pool: p, Queries: q, Issues: issues, Tasks: tasks, Access: e.access, Publisher: e.pub, Enabled: true})
+	tasks.ExtWorkflow = e.engine // the child-event hook reaches the engine through the task service
 	return e
 }
 
+// endTask moves a task to a terminal status the way a daemon report would,
+// then delivers the bus event the listener forwards.
+func (e *env) endTask(t *testing.T, task db.AgentTaskQueue, status string) {
+	t.Helper()
+	e.fx.Exec(t, `UPDATE agent_task_queue SET status = $2, started_at = COALESCE(started_at, now()), completed_at = now() WHERE id = $1`, task.ID, status)
+	if err := e.engine.OnTaskTerminal(context.Background(), task.ID); err != nil {
+		t.Fatalf("OnTaskTerminal: %v", err)
+	}
+}
+
+// setStatus writes an issue status directly (an agent or person moving it).
+func (e *env) setStatus(t *testing.T, issueID pgtype.UUID, status string) {
+	t.Helper()
+	if _, err := e.q.UpdateIssueStatus(context.Background(), db.UpdateIssueStatusParams{ID: issueID, Status: status, WorkspaceID: e.ws}); err != nil {
+		t.Fatalf("set status %s: %v", status, err)
+	}
+}
+
 func (e *env) agent(t *testing.T, name string) pgtype.UUID {
 	t.Helper()
 	return util.MustParseUUID(e.fx.Agent(t, name, e.runtime))
```

`server/internal/extworkflow/observe_test.go`:

```go
package extworkflow

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

func TestOnTaskTerminalCompletedWithoutFinishingIsAFailure(t *testing.T) {
	e := newEnv(t)
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)

	e.endTask(t, e.latestTask(t, e.step(t, run, "spec"), RoleStep), "completed")
	spec := e.step(t, run, "spec")
	wantStepRow(t, spec, StepAwaitingSupervisor, 1)
	if spec.PendingReason.String != string(PendingFailure) {
		t.Fatalf("reason = %q", spec.PendingReason.String)
	}
	if sup := e.latestTask(t, spec, RoleSupervisor); sup.ExtWorkflowKind.String != KindFailure || sup.IssueID != parent {
		t.Fatalf("supervisor task kind=%q issue=%v", sup.ExtWorkflowKind.String, sup.IssueID)
	}
}

func TestOnChildEventsDefersNotificationsToCommit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	e.setStatus(t, spec.IssueID, "done")
	published := e.pub.count()

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	hctx, after := service.WithExtAfterCommit(ctx)
	if err := e.engine.OnChildEvents(hctx, tx, parent, []service.ExtChildEvent{{ChildID: spec.IssueID, Kind: "closed"}}); err != nil {
		t.Fatalf("OnChildEvents: %v", err)
	}
	if e.pub.count() != published {
		t.Fatalf("published before commit")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after.Run()
	if e.pub.count() != published+1 {
		t.Fatalf("published %d events after commit, want 1", e.pub.count()-published)
	}
	wantStepRow(t, e.step(t, run, "spec"), StepDone, 1)
	build := e.step(t, run, "build")
	wantStepRow(t, build, StepRunning, 1)
	if task := e.latestTask(t, build, RoleStep); task.Status != "queued" || task.AgentID != coder {
		t.Fatalf("build task = %s for %v", task.Status, task.AgentID)
	}
}

func TestSilentSupervisorIsWokenOnceThenEscalated(t *testing.T) {
	e := newEnv(t)
	supervisor, coder := e.agent(t, "Supervisor"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: coder, review: true})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	build := e.step(t, run, "build")

	e.setStatus(t, build.IssueID, "in_review")
	e.endTask(t, e.latestTask(t, build, RoleStep), "completed")
	build = e.step(t, run, "build")
	wantStepRow(t, build, StepAwaitingSupervisor, 1)
	first := e.latestTask(t, build, RoleSupervisor)
	if first.ExtWorkflowKind.String != KindReview {
		t.Fatalf("first supervisor kind = %q", first.ExtWorkflowKind.String)
	}

	e.endTask(t, first, "completed")
	second := e.latestTask(t, e.step(t, run, "build"), RoleSupervisor)
	if second.ID == first.ID || e.step(t, run, "build").SupervisorWakes != 2 {
		t.Fatalf("supervisor was not re-woken (wakes=%d)", e.step(t, run, "build").SupervisorWakes)
	}

	e.endTask(t, second, "completed")
	build = e.step(t, run, "build")
	wantStepRow(t, build, StepAwaitingHuman, 1)
	if RunStatus(e.run(t, parent).Status) != RunWaitingHuman || !build.EscalationReason.Valid {
		t.Fatalf("run=%s escalation=%q", e.run(t, parent).Status, build.EscalationReason.String)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND type = $2 AND issue_id = $3`, e.user, InboxTypeEscalation, parent); n != 1 {
		t.Fatalf("escalation inbox items = %d, want 1", n)
	}
	if n := e.countTasks(t, build, RoleSupervisor); n != 2 {
		t.Fatalf("supervisor tasks = %d, want 2", n)
	}
}

func TestOnParentDeletedCancelsTheRun(t *testing.T) {
	e := newEnv(t)
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	task := e.latestTask(t, e.step(t, run, "spec"), RoleStep)

	if err := e.engine.OnParentDeleted(context.Background(), parent); err != nil {
		t.Fatalf("OnParentDeleted: %v", err)
	}
	if got := RunStatus(e.run(t, parent).Status); got != RunCancelled {
		t.Fatalf("run = %s, want cancelled", got)
	}
	spec := e.step(t, run, "spec")
	wantStepRow(t, spec, StepCancelled, 1)
	if got := e.latestTask(t, spec, RoleStep); got.ID != task.ID || got.Status != "cancelled" || got.FailureReason.String != service.ExtWorkflowEngineCancelReason {
		t.Fatalf("task = %s (%q)", got.Status, got.FailureReason.String)
	}
	if got := e.issue(t, spec.IssueID).Status; got != "cancelled" {
		t.Fatalf("child = %s, want cancelled", got)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `cd server && go vet ./internal/extworkflow/`
Expected: build failure — `cannot use e.engine (variable of type *Engine) as service.ExtWorkflowHooks value in assignment: *Engine does not implement service.ExtWorkflowHooks (missing method OnChildEvents)`, `e.engine.OnTaskTerminal undefined`, `e.engine.OnParentDeleted undefined`.

- [ ] **Step 3: Implement**

`server/internal/extworkflow/observe.go`:

```go
package extworkflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The engine's observation entry points (spec §5.2). Each re-derives events
// from current task and issue state under the run lock, so a late, repeated
// or lost signal converges to the same result.

var _ service.ExtWorkflowHooks = (*Engine)(nil)

// ReconcileBatch bounds one reconcile tick (spec §5.2).
const ReconcileBatch = 200

// OnTaskTerminal handles task:completed / task:failed / task:cancelled for a
// workflow task. A failed task whose platform retry is pending is ignored:
// the retry carries the same ext columns.
func (e *Engine) OnTaskTerminal(ctx context.Context, taskID pgtype.UUID) error {
	if !e.Enabled() {
		return nil
	}
	task, err := e.q.GetAgentTask(ctx, taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load task: %w", err)
	}
	if !task.ExtWorkflowRunID.Valid || !isTerminalTask(task.Status) {
		return nil
	}
	if task.Status == "failed" {
		retried, err := e.q.HasRetryTaskForParent(ctx, task.ID)
		if err != nil {
			return fmt.Errorf("check retry: %w", err)
		}
		if retried {
			return nil
		}
	}
	err = e.advance(ctx, nil, task.ExtWorkflowRunID, func(ctx context.Context, q *db.Queries, snap *RunSnapshot) ([]stepEvent, error) {
		if !snap.State.Status.Active() {
			return nil, nil
		}
		if task.ExtWorkflowKind.String == KindSummary {
			ev, ok, err := deriveSummary(ctx, q, snap)
			if err != nil || !ok {
				return nil, err
			}
			return []stepEvent{ev}, nil
		}
		key, ok := snap.KeyOf(task.ExtWorkflowStepID)
		if !ok {
			return nil, nil
		}
		ev, ok, err := deriveStep(ctx, q, snap, key)
		if err != nil || !ok {
			return nil, err
		}
		return []stepEvent{ev}, nil
	})
	if errors.Is(err, ErrRunNotFound) {
		return nil
	}
	return err
}

// OnChildEvents runs inside processChildEvents' transaction for a workflow
// parent (spec §5.2). Notifications are deferred to the caller's commit.
func (e *Engine) OnChildEvents(ctx context.Context, tx pgx.Tx, parentID pgtype.UUID, events []service.ExtChildEvent) error {
	if !e.Enabled() {
		return nil
	}
	run, err := e.q.WithTx(tx).GetActiveExtWorkflowRunByIssue(ctx, parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load active run: %w", err)
	}
	children := make(map[pgtype.UUID]bool, len(events))
	for _, ev := range events {
		children[ev.ChildID] = true
	}
	return e.advance(ctx, tx, run.ID, func(ctx context.Context, q *db.Queries, snap *RunSnapshot) ([]stepEvent, error) {
		var out []stepEvent
		for _, row := range snap.Steps {
			if !children[row.IssueID] {
				continue
			}
			ev, ok, err := deriveStep(ctx, q, snap, row.NodeKey)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, ev)
			}
		}
		return out, nil
	})
}

// OnParentChanged stops the active run when its parent was cancelled,
// deleted, or reassigned away from the run's workflow.
func (e *Engine) OnParentChanged(ctx context.Context, issueID pgtype.UUID) error {
	if !e.Enabled() {
		return nil
	}
	run, err := e.q.GetActiveExtWorkflowRunByIssue(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load active run: %w", err)
	}
	return e.ignoreGone(e.advance(ctx, nil, run.ID, func(ctx context.Context, q *db.Queries, snap *RunSnapshot) ([]stepEvent, error) {
		ev, ok, err := deriveParent(ctx, q, snap)
		if err != nil || !ok {
			return nil, err
		}
		return []stepEvent{ev}, nil
	}))
}

// OnParentDeleted cancels the active run before the parent's tasks are
// cancelled and the row is removed, so the platform's own cancellation of
// supervisor tasks is not read as a silent supervisor.
func (e *Engine) OnParentDeleted(ctx context.Context, issueID pgtype.UUID) error {
	if !e.Enabled() {
		return nil
	}
	run, err := e.q.GetActiveExtWorkflowRunByIssue(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load active run: %w", err)
	}
	return e.ignoreGone(e.advance(ctx, nil, run.ID, func(context.Context, *db.Queries, *RunSnapshot) ([]stepEvent, error) {
		return []stepEvent{{Event: Event{Kind: EvCancelRun, Reason: "the parent issue was deleted"}, Actor: EngineActor}}, nil
	}))
}

// Reconcile is the safety net (scheduler job ext_workflow_reconcile): it
// re-derives events for up to ReconcileBatch active runs, least recently
// touched first, and touches each so the next tick moves on.
func (e *Engine) Reconcile(ctx context.Context) error {
	if !e.Enabled() {
		return nil
	}
	runs, err := e.q.ListExtWorkflowRunsForReconcile(ctx, ReconcileBatch)
	if err != nil {
		return fmt.Errorf("list active runs: %w", err)
	}
	var errs []error
	for _, run := range runs {
		if err := e.ignoreGone(e.advance(ctx, nil, run.ID, deriveReconcile)); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", run.ID.String(), err))
		}
		if err := e.q.TouchExtWorkflowRun(ctx, run.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) ignoreGone(err error) error {
	if errors.Is(err, ErrRunNotFound) {
		return nil
	}
	return err
}

func deriveReconcile(ctx context.Context, q *db.Queries, snap *RunSnapshot) ([]stepEvent, error) {
	if !snap.State.Status.Active() {
		return nil, nil
	}
	if ev, ok, err := deriveParent(ctx, q, snap); err != nil || ok {
		if ok {
			return []stepEvent{ev}, nil
		}
		return nil, err
	}
	var out []stepEvent
	for _, row := range snap.Steps {
		ev, ok, err := deriveStep(ctx, q, snap, row.NodeKey)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ev)
		}
	}
	ev, ok, err := deriveSummary(ctx, q, snap)
	if err != nil {
		return nil, err
	}
	if ok {
		out = append(out, ev)
	}
	return out, nil
}

// deriveParent: the parent was deleted, cancelled, or no longer assigned to
// this run's workflow.
func deriveParent(ctx context.Context, q *db.Queries, snap *RunSnapshot) (stepEvent, bool, error) {
	cancel := func(reason string) (stepEvent, bool, error) {
		return stepEvent{Event: Event{Kind: EvCancelRun, Reason: reason}, Actor: EngineActor}, true, nil
	}
	parent, err := q.GetIssue(ctx, snap.Run.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return cancel("the parent issue was deleted")
	}
	if err != nil {
		return stepEvent{}, false, fmt.Errorf("load parent: %w", err)
	}
	if issuestatus.Effective(ctx, q, parent.WorkspaceID, parent.Status) == issuestatus.Cancelled {
		return cancel("the parent issue was cancelled")
	}
	if parent.AssigneeType.String != "workflow" || parent.AssigneeID != snap.Run.WorkflowID {
		return cancel("the parent issue was reassigned")
	}
	return stepEvent{}, false, nil
}

// deriveStep reads what a step's child issue and newest tasks say happened
// (spec §5.3). Only tasks stamped with the step count; a child moved to done
// or in_review finishes the step whatever ran it.
func deriveStep(ctx context.Context, q *db.Queries, snap *RunSnapshot, key string) (stepEvent, bool, error) {
	none := func() (stepEvent, bool, error) { return stepEvent{}, false, nil }
	st, row := snap.State.Steps[key], snap.ByKey[key]
	if st.Status.Terminal() {
		return none()
	}
	observed := func(ev Event) (stepEvent, bool, error) {
		return stepEvent{Key: key, Event: ev, Actor: EngineActor}, true, nil
	}
	child, err := q.GetIssue(ctx, row.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return observed(Event{Kind: EvChildCancelled, Detail: map[string]any{"child": "deleted"}})
	}
	if err != nil {
		return stepEvent{}, false, fmt.Errorf("load child issue: %w", err)
	}
	childStatus := issuestatus.Effective(ctx, q, child.WorkspaceID, child.Status)
	if childStatus == issuestatus.Cancelled {
		return observed(Event{Kind: EvChildCancelled})
	}
	switch st.Status {
	case StepRunning:
		if childStatus == issuestatus.Done || childStatus == issuestatus.InReview {
			return observed(Event{Kind: EvStepFinished, Detail: map[string]any{"child_status": child.Status}})
		}
		task, found, err := latestTask(ctx, q, row.ID, RoleStep)
		if err != nil || !found {
			return stepEvent{}, false, err
		}
		if settled, err := taskSettled(ctx, q, task); err != nil || !settled {
			return stepEvent{}, false, err
		}
		return observed(stepTaskOutcome(task))
	case StepAwaitingSupervisor:
		if snap.State.SupervisorFor[key] || st.SupervisorWakes == 0 {
			return none()
		}
		task, found, err := latestTask(ctx, q, row.ID, RoleSupervisor)
		if err != nil || !found {
			return stepEvent{}, false, err
		}
		if settled, err := taskSettled(ctx, q, task); err != nil || !settled {
			return stepEvent{}, false, err
		}
		// Only a task started for this pending decision counts: the step row
		// and that task were written in one transaction.
		if task.CreatedAt.Time.Before(row.UpdatedAt.Time) {
			return none()
		}
		return observed(Event{Kind: EvSupervisorNoDecision, Reason: describeSupervisorEnd(task), Detail: map[string]any{"task_id": task.ID.String()}})
	}
	return none()
}

// deriveSummary: the summary task ended, whether it completed or not.
func deriveSummary(ctx context.Context, q *db.Queries, snap *RunSnapshot) (stepEvent, bool, error) {
	if !snap.State.SummaryRequested || !snap.State.AllSettled() {
		return stepEvent{}, false, nil
	}
	task, err := q.GetLatestExtWorkflowSummaryTask(ctx, snap.Run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return stepEvent{}, false, nil
	}
	if err != nil {
		return stepEvent{}, false, fmt.Errorf("load summary task: %w", err)
	}
	if settled, err := taskSettled(ctx, q, task); err != nil || !settled {
		return stepEvent{}, false, err
	}
	return stepEvent{Event: Event{Kind: EvSummaryEnded}, Actor: EngineActor}, true, nil
}

func latestTask(ctx context.Context, q *db.Queries, stepID pgtype.UUID, role string) (db.AgentTaskQueue, bool, error) {
	task, err := q.GetLatestExtWorkflowTaskForStep(ctx, db.GetLatestExtWorkflowTaskForStepParams{
		StepID: stepID, Role: pgtype.Text{String: role, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return task, false, nil
	}
	if err != nil {
		return task, false, fmt.Errorf("load latest task: %w", err)
	}
	return task, true, nil
}

// taskSettled: the task ended for good, and not because the engine itself
// cancelled it.
func taskSettled(ctx context.Context, q *db.Queries, task db.AgentTaskQueue) (bool, error) {
	switch task.Status {
	case "completed":
		return true, nil
	case "cancelled":
		return task.FailureReason.String != service.ExtWorkflowEngineCancelReason, nil
	case "failed":
		retried, err := q.HasRetryTaskForParent(ctx, task.ID)
		return !retried, err
	}
	return false, nil
}

func isTerminalTask(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

// stepTaskOutcome maps a settled step task whose child is not finished.
func stepTaskOutcome(task db.AgentTaskQueue) Event {
	detail := map[string]any{"task_id": task.ID.String()}
	switch task.Status {
	case "completed":
		return Event{Kind: EvStepFailed, Reason: "ended_without_finishing", Detail: detail}
	case "cancelled":
		return Event{Kind: EvStepFailed, Reason: "cancelled", Detail: detail}
	}
	reason := task.FailureReason.String
	if reason == "" {
		reason = "failed"
	}
	if task.Error.String != "" {
		detail["error"] = task.Error.String
	}
	return Event{Kind: EvStepFailed, Reason: reason, Detail: detail}
}

func describeSupervisorEnd(task db.AgentTaskQueue) string {
	switch task.Status {
	case "completed":
		return "the supervisor's turn ended without a valid decision"
	case "cancelled":
		return "the supervisor's turn was cancelled"
	}
	if task.Error.String != "" {
		return "the supervisor's turn failed: " + task.Error.String
	}
	return "the supervisor's turn failed"
}
```

- [ ] **Step 4: Run, expect PASS**

Run:
```bash
cd server && gofmt -l ./internal/extworkflow && go vet ./internal/extworkflow/
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -count=1 -v'"
```
Expected: `--- PASS` for `TestOnTaskTerminalCompletedWithoutFinishingIsAFailure`, `TestOnChildEventsDefersNotificationsToCommit`, `TestSilentSupervisorIsWokenOnceThenEscalated`, `TestOnParentDeletedCancelsTheRun` and every earlier test; `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/extworkflow/observe.go server/internal/extworkflow/observe_test.go server/internal/extworkflow/testenv_test.go
git commit -m "feat(ext-workflow): derive run events from task and issue state

Task terminal, child-event, parent-change and reconcile entry points
re-derive step outcomes under the run lock; retries pending and the
engine's own cancellations are ignored.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


---

### Task B7: Service-side hooks — run trigger, create, child events, rerun

**Files:**
- Create: `server/internal/service/ext_workflow_trigger.go`
- Modify: `server/internal/service/issue_trigger.go` — `WillEnqueueRun` switch, after the `case "squad":` block (~line 194)
- Modify: `server/internal/service/issue.go` — `maybeEnqueueOnAssign`, after the backlog/triage early return (~line 830)
- Modify: `server/internal/service/issue_wakeup_system.go` — `processChildEvents`, the `finish` closure and right after `GetIssue` (~lines 191–203)
- Modify: `server/internal/service/task.go` — `RerunIssue`, after loading the issue (~line 5655) and after loading the source task (~line 5680)
- Test: `server/internal/service/ext_workflow_hooks_test.go`

**Interfaces:**
- Consumes: `TaskService.ExtWorkflow` (B4), `runExtWorkflowChildEvents` (B4), `GetExtWorkflowInWorkspace` (Part A), `GetActiveExtWorkflowRunByIssue` (B3).
- Produces: `var ErrRerunWorkflowIssue` (handler maps to 409 in B8); package-private `extWorkflowRunTrigger`, `extWorkflowStartOnAssign`, `extWorkflowRerunRefused`.

Decisions per touch point:
- `WillEnqueueRun` `case "workflow"`: same backlog rules as agents/squads (already applied above the switch); reports the supervisor as `AgentID` with `AssigneeType "workflow"`; refuses when the engine is off, the workflow is missing/archived, a run is already active, or the supervisor is archived/runtime-less or not visible to the preview probe.
- `maybeEnqueueOnAssign`: a workflow assignee calls `StartRun` instead of the agent/squad paths (creation outside backlog).
- `processChildEvents`: after `ClaimChildEvents` and `GetIssue`, a workflow parent's claimed events go to `OnChildEvents` in that transaction; an engine error rolls back so the sweep retries; the engine's notifications run after `tx.Commit`. The rest of the function is unchanged.
- `resolveWakeTarget`: **no code change** — its `default` branch already returns `none` for `'workflow'`; a test pins it.
- `RerunIssue`: refuses a workflow-assigned issue (no source task) and any task carrying `ext_workflow_run_id`, before cancelling anything.

- [ ] **Step 1: Write the failing test**

`server/internal/service/ext_workflow_hooks_test.go`:

```go
package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type extChildCall struct {
	parent pgtype.UUID
	events []ExtChildEvent
}

// fakeExtHooks records engine calls.
type fakeExtHooks struct {
	mu         sync.Mutex
	childCalls []extChildCall
	starts     []pgtype.UUID
	afterRuns  int
	childErr   error
}

func (f *fakeExtHooks) ValidateAssignment(context.Context, pgtype.UUID, pgtype.UUID, string, pgtype.UUID) error {
	return nil
}

func (f *fakeExtHooks) StartRun(_ context.Context, issueID pgtype.UUID, _ string, _ pgtype.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, issueID)
	return nil
}

func (f *fakeExtHooks) OnChildEvents(ctx context.Context, _ pgx.Tx, parentID pgtype.UUID, evs []ExtChildEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.childErr != nil {
		return f.childErr
	}
	f.childCalls = append(f.childCalls, extChildCall{parent: parentID, events: evs})
	if after := ExtAfterCommitFrom(ctx); after != nil {
		after.Add(func() { f.mu.Lock(); f.afterRuns++; f.mu.Unlock() })
	}
	return nil
}

// workflowFamily is a parent assigned to a (fake) workflow and one child that
// just closed, with the wakeup rows processing creates cleaned up.
func workflowFamily(t *testing.T, f principalFixture, assigneeType string) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	parent := f.Issue(t, "Workflow parent", testutil.Cols{"assignee_type": assigneeType, "assignee_id": dbid.NewV7()})
	child := f.Issue(t, "Workflow child", testutil.Cols{"parent_issue_id": parent})
	f.Cleanup(t, `DELETE FROM issue_child_event WHERE parent_id = $1`, parent)
	f.Cleanup(t, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id = $1)`, parent)
	f.Cleanup(t, `DELETE FROM issue_wakeup WHERE issue_id = $1`, parent)
	f.Exec(t, `UPDATE issue SET status = 'done' WHERE id = $1`, child)
	return parseTestUUID(t, parent), parseTestUUID(t, child)
}

func TestProcessChildEventsHandsWorkflowParentToEngine(t *testing.T) {
	f, _ := newPrincipalFixture(t)
	parent, child := workflowFamily(t, f, "workflow")
	hooks := &fakeExtHooks{}
	f.svc.TaskSvc.ExtWorkflow = hooks

	if err := (&IssueWakeupService{Tasks: f.svc.TaskSvc}).ProcessChildEvents(context.Background(), parent); err != nil {
		t.Fatalf("ProcessChildEvents: %v", err)
	}
	if len(hooks.childCalls) != 1 || hooks.childCalls[0].parent != parent {
		t.Fatalf("engine calls = %+v", hooks.childCalls)
	}
	kinds := map[string]bool{}
	for _, ev := range hooks.childCalls[0].events {
		if ev.ChildID != child {
			t.Fatalf("event for %v, want child %v", ev.ChildID, child)
		}
		kinds[ev.Kind] = true
	}
	if !kinds["attached"] || !kinds["closed"] {
		t.Fatalf("kinds = %v, want attached and closed", kinds)
	}
	if hooks.afterRuns != 1 {
		t.Fatalf("after-commit ran %d times, want 1", hooks.afterRuns)
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_child_event WHERE parent_id = $1 AND processed_at IS NULL`, parent); n != 0 {
		t.Fatalf("%d events left unprocessed", n)
	}
}

func TestProcessChildEventsKeepsEventsWhenTheEngineFails(t *testing.T) {
	f, _ := newPrincipalFixture(t)
	parent, _ := workflowFamily(t, f, "workflow")
	hooks := &fakeExtHooks{childErr: errors.New("engine down")}
	f.svc.TaskSvc.ExtWorkflow = hooks

	if err := (&IssueWakeupService{Tasks: f.svc.TaskSvc}).ProcessChildEvents(context.Background(), parent); err == nil {
		t.Fatal("ProcessChildEvents succeeded, want the engine error")
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_child_event WHERE parent_id = $1 AND processed_at IS NULL`, parent); n == 0 {
		t.Fatal("events were consumed although the engine failed")
	}
}

func TestProcessChildEventsSkipsOtherParents(t *testing.T) {
	f, owner := newPrincipalFixture(t)
	agent := f.privateAgentOwnedBy(t, owner, "plain-parent")
	parent := parseTestUUID(t, f.Issue(t, "Agent parent", testutil.Cols{"assignee_type": "agent", "assignee_id": agent}))
	f.Cleanup(t, `DELETE FROM issue_child_event WHERE parent_id = $1`, parent)
	f.Cleanup(t, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id = $1)`, parent)
	f.Cleanup(t, `DELETE FROM issue_wakeup WHERE issue_id = $1`, parent)
	f.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, parent)
	f.Issue(t, "Child", testutil.Cols{"parent_issue_id": util.UUIDToString(parent)})
	hooks := &fakeExtHooks{}
	f.svc.TaskSvc.ExtWorkflow = hooks
	if err := (&IssueWakeupService{Tasks: f.svc.TaskSvc}).ProcessChildEvents(context.Background(), parent); err != nil {
		t.Fatalf("ProcessChildEvents: %v", err)
	}
	if len(hooks.childCalls) != 0 {
		t.Fatalf("engine called for an agent parent: %+v", hooks.childCalls)
	}
}

func extWorkflowServiceFixture(t *testing.T) (principalFixture, *IssueService, *fakeExtHooks, pgtype.UUID, pgtype.UUID) {
	t.Helper()
	f, owner := newPrincipalFixture(t)
	supervisor := f.privateAgentOwnedBy(t, owner, "supervisor")
	workflow := f.Insert(t, "ext_workflow", testutil.Cols{
		"workspace_id": f.WorkspaceID, "name": "Ship", "supervisor_agent_id": supervisor, "creator_id": owner,
	})
	hooks := &fakeExtHooks{}
	f.svc.TaskSvc.ExtWorkflow = hooks
	issues := NewIssueService(f.q, f.Pool, events.New(), analytics.NoopClient{}, f.svc.TaskSvc)
	return f, issues, hooks, parseTestUUID(t, workflow), parseTestUUID(t, supervisor)
}

func TestWillEnqueueRunStartsAWorkflowRun(t *testing.T) {
	f, issues, _, workflow, supervisor := extWorkflowServiceFixture(t)
	ctx := context.Background()
	id := f.Issue(t, "Parked", testutil.Cols{"status": "backlog", "assignee_type": "workflow", "assignee_id": workflow})
	issue, err := f.q.GetIssue(ctx, parseTestUUID(t, id))
	if err != nil {
		t.Fatal(err)
	}
	issue.Status = "todo"
	in := IssueTriggerInput{Issue: issue, PrevStatus: "backlog", StatusChanged: true}

	trigger, ok := issues.WillEnqueueRun(ctx, in, IssueTriggerProbe{})
	if !ok || trigger.AssigneeType != "workflow" || trigger.AgentID != supervisor || trigger.Source != RunSourceStatus {
		t.Fatalf("trigger = %+v, %v", trigger, ok)
	}

	f.Insert(t, "ext_workflow_run", testutil.Cols{
		"workspace_id": f.WorkspaceID, "workflow_id": workflow, "issue_id": id, "triggered_by_type": "member",
		"triggered_by_id": f.UserID, "definition": testutil.Raw(`'{}'::jsonb`),
	})
	if _, ok := issues.WillEnqueueRun(ctx, in, IssueTriggerProbe{}); ok {
		t.Fatal("a second run was promised while one is active")
	}

	f.svc.TaskSvc.ExtWorkflow = nil
	f.Exec(t, `DELETE FROM ext_workflow_run WHERE issue_id = $1`, id)
	if _, ok := issues.WillEnqueueRun(ctx, in, IssueTriggerProbe{}); ok {
		t.Fatal("a run was promised with the engine off")
	}
}

func TestCreateWorkflowIssueStartsRunOutsideBacklog(t *testing.T) {
	f, issues, hooks, workflow, _ := extWorkflowServiceFixture(t)
	ctx := context.Background()
	create := func(status string) db.Issue {
		res, err := issues.Create(ctx, IssueCreateParams{
			WorkspaceID: parseTestUUID(t, f.WorkspaceID), Title: "Ship " + status, Status: status, Priority: "none",
			AssigneeType: pgtype.Text{String: "workflow", Valid: true}, AssigneeID: workflow,
			CreatorType: "member", CreatorID: parseTestUUID(t, f.UserID),
		}, IssueCreateOpts{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return res.Issue
	}
	parked := create("backlog")
	started := create("todo")
	if len(hooks.starts) != 1 || hooks.starts[0] != started.ID || hooks.starts[0] == parked.ID {
		t.Fatalf("StartRun calls = %v, want only %v", hooks.starts, started.ID)
	}
}

func TestRerunIssueRefusesWorkflowWork(t *testing.T) {
	f, _, _, workflow, supervisor := extWorkflowServiceFixture(t)
	ctx := context.Background()
	issue := f.Issue(t, "Run by workflow", testutil.Cols{"assignee_type": "workflow", "assignee_id": workflow})
	if _, err := f.svc.TaskSvc.RerunIssue(ctx, parseTestUUID(t, issue), pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, nil); !errors.Is(err, ErrRerunWorkflowIssue) {
		t.Fatalf("rerun workflow issue: %v", err)
	}
	agent := util.UUIDToString(supervisor)
	var runtimeID string
	f.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, agent).Scan(&runtimeID)
	child := f.Issue(t, "Workflow step", testutil.Cols{"assignee_type": "agent", "assignee_id": agent})
	task := f.Task(t, agent, testutil.Cols{"issue_id": child, "runtime_id": runtimeID, "status": "completed", "ext_workflow_run_id": dbid.NewV7(), "ext_workflow_role": "step"})
	if _, err := f.svc.TaskSvc.RerunIssue(ctx, parseTestUUID(t, child), parseTestUUID(t, task), pgtype.UUID{}, pgtype.UUID{}, nil); !errors.Is(err, ErrRerunWorkflowIssue) {
		t.Fatalf("rerun workflow task: %v", err)
	}
}

// The child_done system rule must wake nobody for a workflow parent: the
// engine owns it. resolveWakeTarget's default branch already answers "none".
func TestResolveWakeTargetIgnoresWorkflowParents(t *testing.T) {
	f, _ := newPrincipalFixture(t)
	issue, err := f.q.GetIssue(context.Background(), parseTestUUID(t, f.Issue(t, "Workflow parent", testutil.Cols{"assignee_type": "workflow", "assignee_id": dbid.NewV7()})))
	if err != nil {
		t.Fatal(err)
	}
	target, err := resolveWakeTarget(context.Background(), f.q, issue)
	if err != nil || target.Type != "none" || target.Agent.ID.Valid {
		t.Fatalf("target = %+v, %v; want none", target, err)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `cd server && go vet ./internal/service/`
Expected: build failure — `undefined: ErrRerunWorkflowIssue`. After adding only `ext_workflow_trigger.go`, the DB tests fail: `engine calls = []` (ProcessChildEvents), `trigger = {…}, false` (WillEnqueueRun), `StartRun calls = []`, `rerun workflow issue: issue is not assigned to an agent or squad`.

- [ ] **Step 3: Implement**

`server/internal/service/ext_workflow_trigger.go`:

```go
package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow (fork): what upstream issue-trigger paths do for a workflow
// assignee. Each is called from a one-line hook in an upstream file.

// ErrRerunWorkflowIssue refuses a manual rerun of a workflow-run issue or of
// a workflow task: those are driven through the run's decisions.
var ErrRerunWorkflowIssue = errors.New("this issue is run by a workflow; decide on the workflow run instead of rerunning it")

// extWorkflowRunTrigger is WillEnqueueRun's answer for a workflow assignee:
// a run starts unless the engine is off, the workflow is gone or archived, a
// run is already active, or the supervisor cannot run. The supervisor is the
// agent reported to the preview.
func (s *IssueService) extWorkflowRunTrigger(ctx context.Context, issue db.Issue, source RunEnqueueSource, canAccess func(db.Agent) bool) (IssueRunTrigger, bool) {
	if s.TaskService == nil || s.TaskService.ExtWorkflow == nil {
		return IssueRunTrigger{}, false
	}
	wf, err := s.Queries.GetExtWorkflowInWorkspace(ctx, db.GetExtWorkflowInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
	if err != nil || wf.ArchivedAt.Valid {
		return IssueRunTrigger{}, false
	}
	if _, err := s.Queries.GetActiveExtWorkflowRunByIssue(ctx, issue.ID); !errors.Is(err, pgx.ErrNoRows) {
		return IssueRunTrigger{}, false // active run, or the check failed: never over-promise
	}
	supervisor, err := s.Queries.GetAgent(ctx, wf.SupervisorAgentID)
	if err != nil || supervisor.ArchivedAt.Valid || !supervisor.RuntimeID.Valid || !canAccess(supervisor) {
		return IssueRunTrigger{}, false
	}
	return IssueRunTrigger{IssueID: issue.ID, AgentID: supervisor.ID, AssigneeType: "workflow", Source: source}, true
}

// extWorkflowStartOnAssign starts the run of a workflow-assigned issue that
// was just created outside backlog.
func (s *IssueService) extWorkflowStartOnAssign(ctx context.Context, issue db.Issue, actorType, actorID string) {
	if s.TaskService == nil || s.TaskService.ExtWorkflow == nil {
		return
	}
	actor, _ := util.ParseUUID(actorID)
	if err := s.TaskService.ExtWorkflow.StartRun(ctx, issue.ID, actorType, actor); err != nil {
		slog.Warn("ext-workflow: start run on create failed", "issue_id", util.UUIDToString(issue.ID), "error", err)
	}
}

// extWorkflowRerunRefused reports whether RerunIssue must refuse: the issue is
// workflow-assigned (no source task) or the named task is a workflow task.
func extWorkflowRerunRefused(issue db.Issue, sourceTaskID pgtype.UUID, sourceTask *db.AgentTaskQueue) bool {
	if sourceTask != nil {
		return sourceTask.ExtWorkflowRunID.Valid
	}
	return !sourceTaskID.Valid && issue.AssigneeType.String == "workflow"
}
```

`server/internal/service/issue_trigger.go`:

```diff
diff --git a/server/internal/service/issue_trigger.go b/server/internal/service/issue_trigger.go
--- a/server/internal/service/issue_trigger.go
+++ b/server/internal/service/issue_trigger.go
@@ -199,6 +199,10 @@ func (s *IssueService) WillEnqueueRun(ctx context.Context, in IssueTriggerInput,
 			AssigneeType: "squad",
 			Source:       source,
 		}, true
+
+	case "workflow":
+		// ext-workflow: a workflow assignee starts an engine-owned run.
+		return s.extWorkflowRunTrigger(ctx, issue, source, canAccess)
 	}
 	return IssueRunTrigger{}, false
 }
```

`server/internal/service/issue.go`:

```diff
diff --git a/server/internal/service/issue.go b/server/internal/service/issue.go
--- a/server/internal/service/issue.go
+++ b/server/internal/service/issue.go
@@ -839,6 +839,11 @@ func (s *IssueService) maybeEnqueueOnAssign(ctx context.Context, issue db.Issue,
 	if issue.TriageState.Valid || issuestatus.Effective(ctx, s.Queries, issue.WorkspaceID, issue.Status) == "backlog" {
 		return pgtype.UUID{}
 	}
+	// ext-workflow: a workflow assignee starts an engine run, not an agent task.
+	if issue.AssigneeType.String == "workflow" {
+		s.extWorkflowStartOnAssign(ctx, issue, creatorType, actorID)
+		return pgtype.UUID{}
+	}
 	verdict, admitted := agentAssigneeVerdict(ctx, s.runtimeLookup(s.Queries), issue)
 	if !admitted && RuntimeBlockedNeedsNotice(verdict.Reason) {
 		// Assignment has no response the assigner reads for this outcome, so the
```

`server/internal/service/issue_wakeup_system.go`:

```diff
diff --git a/server/internal/service/issue_wakeup_system.go b/server/internal/service/issue_wakeup_system.go
--- a/server/internal/service/issue_wakeup_system.go
+++ b/server/internal/service/issue_wakeup_system.go
@@ -188,11 +188,18 @@ func (s *IssueWakeupService) processChildEvents(ctx context.Context, parentID pg
 			allSourced = false
 		}
 	}
+	// ext-workflow: what a workflow engine produced in this transaction is
+	// published only after the commit.
+	extAfterCommit := func() {}
 	finish := func() error {
 		if err := q.FinishChildEvents(ctx, ids); err != nil {
 			return err
 		}
-		return tx.Commit(ctx)
+		if err := tx.Commit(ctx); err != nil {
+			return err
+		}
+		extAfterCommit() // ext-workflow
+		return nil
 	}
 	parent, err := q.GetIssue(ctx, parentID)
 	if errors.Is(err, pgx.ErrNoRows) {
@@ -201,6 +208,11 @@ func (s *IssueWakeupService) processChildEvents(ctx context.Context, parentID pg
 	if err != nil {
 		return err
 	}
+	// ext-workflow: a workflow parent's engine consumes the claimed changes in
+	// this transaction; resolveWakeTarget already wakes nobody for it.
+	if extAfterCommit, err = s.Tasks.runExtWorkflowChildEvents(ctx, tx, parent, events); err != nil {
+		return err
+	}
 	active, err := wakeupIssueActive(ctx, q, parent)
 	if err != nil {
 		return err
```

`server/internal/service/task.go`:

```diff
diff --git a/server/internal/service/task.go b/server/internal/service/task.go
--- a/server/internal/service/task.go
+++ b/server/internal/service/task.go
@@ -5659,6 +5659,10 @@ func (s *TaskService) RerunIssue(ctx context.Context, issueID pgtype.UUID, sourc
 	if err != nil {
 		return nil, fmt.Errorf("load issue: %w", err)
 	}
+	// ext-workflow: a workflow issue is rerun through its run's decisions.
+	if extWorkflowRerunRefused(issue, sourceTaskID, nil) {
+		return nil, ErrRerunWorkflowIssue
+	}
 	// In Triage a rerun follows its source, and the decision is made here —
 	// before anything is cancelled. The queue door would refuse a derived rerun
 	// anyway, but this path cancels the prior run on its way there, so a late
@@ -5687,6 +5691,10 @@ func (s *TaskService) RerunIssue(ctx context.Context, issueID pgtype.UUID, sourc
 		if err != nil {
 			return nil, fmt.Errorf("load source task: %w", err)
 		}
+		// ext-workflow: a workflow task is rerun through its run's decisions.
+		if extWorkflowRerunRefused(issue, sourceTaskID, &sourceTask) {
+			return nil, ErrRerunWorkflowIssue
+		}
 		if !sourceTask.IssueID.Valid || util.UUIDToString(sourceTask.IssueID) != util.UUIDToString(issueID) {
 			return nil, fmt.Errorf("source task does not belong to this issue")
 		}
```

- [ ] **Step 4: Run, expect PASS**

Run:
```bash
cd server && go build ./... && go vet ./internal/service/
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/service/ -count=1'"
```
Expected: `ok` for the whole package (the new `TestProcessChildEvents*`, `TestWillEnqueueRunStartsAWorkflowRun`, `TestCreateWorkflowIssueStartsRunOutsideBacklog`, `TestRerunIssueRefusesWorkflowWork`, `TestResolveWakeTargetIgnoresWorkflowParents` included; run once with `-v -run 'Workflow|ProcessChildEvents'` to see PASS, not SKIP).

- [ ] **Step 5: Commit**

```bash
git add server/internal/service/ext_workflow_trigger.go server/internal/service/ext_workflow_hooks_test.go server/internal/service/issue_trigger.go server/internal/service/issue.go server/internal/service/issue_wakeup_system.go server/internal/service/task.go
git commit -m "feat(ext-workflow): hook workflow runs into issue triggers

WillEnqueueRun and issue creation start a workflow run, workflow parents'
sub-issue changes reach the engine inside the claiming transaction, and
RerunIssue refuses workflow issues and tasks.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


---

### Task B8: Handler-side hooks and the engine bridge

**Files:**
- Create: `server/internal/handler/ext_workflow_bridge.go`
- Modify: `server/internal/handler/handler.go` — import block; `type Handler struct`, before the final `cfg Config` field (~line 417)
- Modify: `server/internal/handler/issue.go` — involves-user predicates in `ListIssues` (~:1686) and `ListGroupedIssues` (~:2144); `isIssueActorType` (~:1953); create-path parent check (~:3252); `UpdateIssue` before the "Reconcile the task queue" comment (~:4062); `validateAssigneePair` switch (~:4163); `DeleteIssue` (~:4314); `BatchUpdateIssues` before the "Reassignment does not cancel" comment (~:4800); `BatchDeleteIssues` loop (~:4888)
- Modify: `server/internal/handler/issue_trigger.go` — `dispatchIssueRun` switch (~:93)
- Modify: `server/internal/handler/issue_table_query.go` — `appendIssueTableInvolvedPredicate` (~:404)
- Modify: `server/internal/handler/issue_table_group.go` — assignee `groupSortExpr` (~:263)
- Modify: `server/internal/handler/task_lifecycle.go` — `RerunIssue`, after the `ErrIssueInTriage` branch (~:219)
- Modify: `server/pkg/db/queries/issue.sql` — involves-user filters of `ListIssues` (~:31), `ListOpenIssues` (~:588), `CountIssues` (~:634); then `make sqlc`
- Test: `server/internal/handler/ext_workflow_hooks_test.go`

**Interfaces:**
- Consumes: B5/B6 engine (`ValidateAssignment`, `StartRun`, `OnParentChanged`, `OnParentDeleted`, `Enabled`), B7 `service.ErrRerunWorkflowIssue`, existing `canInvokeAgent`, `resolveActor`, `publish`; Part A test helpers `requireExtWorkflowDB`, `createExtWorkflowAs`, `updateExtWorkflow`, `extWFNode`, `createHandlerTestAgent`, `privateAgentTestFixture`, `newRequestAs`.
- Produces: `Handler.ExtWorkflow *extworkflow.Engine` (contract; nil = disabled); `type ExtWorkflowBridge` + `NewExtWorkflowBridge(h *Handler) *ExtWorkflowBridge` implementing `extworkflow.AgentAccess` and `extworkflow.Publisher`; handler methods `validateExtWorkflowAssignee`, `startExtWorkflowRun`, `notifyExtWorkflowParentChanged`, `extWorkflowParentDeleting`.

Decisions per touch point:
- `validateAssigneePair` `case "workflow"`: delegates to `ValidateAssignment`; engine off or unwired → 409 `workflow_engine_disabled`. The assigning actor is passed as resolved by `resolveActor`; the bridge judges a member as themselves and an agent actor without an originator (dispatch-time checks have no request), so assignment and later dispatch agree.
- Create-path parent check: workflow is gated like agent/squad (parent validated before the assignee gate).
- `dispatchIssueRun` `case "workflow"`: `StartRun`, logged on failure (the assignment itself succeeded).
- `UpdateIssue` / `BatchUpdateIssues`: when the previous assignee was a workflow and status or assignee changed, `OnParentChanged` runs **before** `WillEnqueueRun`, so a workflow→workflow reassignment cancels the old run before the new one starts (one active run per issue).
- `DeleteIssue` / `BatchDeleteIssues`: `OnParentDeleted` runs **before** `CancelTasksForIssue`, so the platform's cancellation of supervisor tasks is not read as a silent supervisor.
- `isIssueActorType`: accepts `workflow` (assignee filters `workflow:<id>` and grouping).
- Involves-user filters (3 sqlc queries + 3 dynamic builders): an issue assigned to a workflow the user **created** involves them, the analogue of "assigned to an agent the user owns".
- Assignee sort CASE (`issue.go` ~:2373) and group order CASE (`issue_table_group.go` ~:509): **no change** — `ELSE 3` already places workflow after squad.
- Group label: workflow groups sort by workflow name.
- `RerunIssue`: `ErrRerunWorkflowIssue` → 409.

- [ ] **Step 1: Write the failing test**

`server/internal/handler/ext_workflow_hooks_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// withExtWorkflowEngine wires a live engine into the shared test handler for
// one test, the way cmd/server does, and restores the previous wiring.
func withExtWorkflowEngine(t *testing.T) *extworkflow.Engine {
	t.Helper()
	bridge := NewExtWorkflowBridge(testHandler)
	engine := extworkflow.NewEngine(extworkflow.Deps{
		Pool: testPool, Queries: testHandler.Queries, Issues: testHandler.IssueService, Tasks: testHandler.TaskService,
		Access: bridge, Publisher: bridge, Enabled: true,
	})
	setExtWorkflowEngine(t, engine)
	return engine
}

func setExtWorkflowEngine(t *testing.T, engine *extworkflow.Engine) {
	t.Helper()
	prevHandler, prevTasks := testHandler.ExtWorkflow, testHandler.TaskService.ExtWorkflow
	testHandler.ExtWorkflow = engine
	testHandler.TaskService.ExtWorkflow = nil
	if engine != nil {
		testHandler.TaskService.ExtWorkflow = engine
	}
	t.Cleanup(func() {
		testHandler.ExtWorkflow = prevHandler
		testHandler.TaskService.ExtWorkflow = prevTasks
	})
}

// hookWorkflow creates a one-node workflow whose node runs nodeAgent.
func hookWorkflow(t *testing.T, nodeAgent string) string {
	t.Helper()
	supervisor := createHandlerTestAgent(t, t.Name()+"-supervisor", nil)
	if nodeAgent == "" {
		nodeAgent = createHandlerTestAgent(t, t.Name()+"-worker", nil)
	}
	wf := createExtWorkflowAs(t, "", t.Name(), supervisor)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"nodes": []any{extWFNode("build", nodeAgent)}}).Want(http.StatusOK)
	return wf.ID
}

// cleanupWorkflowIssue removes an issue the handler created and everything a
// run hung off it. Registered statements run in reverse order.
func cleanupWorkflowIssue(t *testing.T, issueID string) {
	t.Helper()
	dbfx.Cleanup(t, `DELETE FROM issue WHERE id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE parent_issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM ext_workflow_run WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1 OR issue_id IN (SELECT id FROM issue WHERE parent_issue_id = $1)`, issueID)
	dbfx.Cleanup(t, `DELETE FROM ext_workflow_run_step WHERE run_id IN (SELECT id FROM ext_workflow_run WHERE issue_id = $1)`, issueID)
	dbfx.Cleanup(t, `DELETE FROM ext_workflow_run_event WHERE run_id IN (SELECT id FROM ext_workflow_run WHERE issue_id = $1)`, issueID)
	dbfx.Cleanup(t, `DELETE FROM issue_child_event WHERE parent_id = $1`, issueID)
}

func createWorkflowIssue(t *testing.T, workflowID, status string) *testutil.Response {
	t.Helper()
	resp := testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": t.Name(), "status": status, "assignee_type": "workflow", "assignee_id": workflowID,
	}))
	if resp.Code == http.StatusCreated {
		var issue IssueResponse
		resp.JSON(&issue)
		cleanupWorkflowIssue(t, issue.ID)
	}
	return resp
}

func activeRunStatus(t *testing.T, issueID string) string {
	t.Helper()
	status := ""
	_ = testPool.QueryRow(context.Background(), `SELECT status FROM ext_workflow_run WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1`, issueID).Scan(&status)
	return status
}

func TestExtWorkflowAssignRequiresTheEngine(t *testing.T) {
	requireExtWorkflowDB(t)
	setExtWorkflowEngine(t, nil)
	wf := hookWorkflow(t, "")
	resp := createWorkflowIssue(t, wf, "todo").Want(http.StatusConflict)
	if !strings.Contains(resp.Text(), "workflow_engine_disabled") {
		t.Fatalf("body = %s", resp.Text())
	}
}

func TestExtWorkflowAssignStartsARun(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	wf := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wf, "todo").Want(http.StatusCreated).JSON(&issue)

	if got := activeRunStatus(t, issue.ID); got != "running" {
		t.Fatalf("run status = %q, want running", got)
	}
	var kind, role string
	dbfx.QueryRow(t, `
		SELECT t.ext_workflow_kind, t.ext_workflow_role FROM agent_task_queue t
		JOIN issue c ON c.id = t.issue_id
		WHERE c.parent_issue_id = $1 AND t.status = 'queued'`, issue.ID).Scan(&kind, &role)
	if kind != "step" || role != "step" {
		t.Fatalf("child task kind=%q role=%q", kind, role)
	}
}

func TestExtWorkflowBacklogStartsOnPromotion(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	wf := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wf, "backlog").Want(http.StatusCreated).JSON(&issue)
	if got := activeRunStatus(t, issue.ID); got != "" {
		t.Fatalf("backlog started a run (%s)", got)
	}
	req := withURLParam(newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"status": "todo"}), "id", issue.ID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	if got := activeRunStatus(t, issue.ID); got != "running" {
		t.Fatalf("run status after promotion = %q, want running", got)
	}
}

func TestExtWorkflowParentCancelStopsTheRun(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	wf := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wf, "todo").Want(http.StatusCreated).JSON(&issue)

	req := withURLParam(newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"status": "cancelled"}), "id", issue.ID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	if got := activeRunStatus(t, issue.ID); got != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", got)
	}
	if n := dbfx.Count(t, `
		SELECT count(*) FROM agent_task_queue t JOIN issue c ON c.id = t.issue_id
		WHERE c.parent_issue_id = $1 AND t.status <> 'cancelled'`, issue.ID); n != 0 {
		t.Fatalf("%d step tasks still active", n)
	}
}

func TestExtWorkflowAssignNeedsInvokeOnEveryAgent(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	privateAgent, _, member := privateAgentTestFixture(t)
	wf := hookWorkflow(t, privateAgent)
	resp := testutil.Call(t, testHandler.CreateIssue, newRequestAs(member, "POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": t.Name(), "status": "todo", "assignee_type": "workflow", "assignee_id": wf,
	}))
	resp.Want(http.StatusForbidden)
}

func TestExtWorkflowRerunIsAConflict(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	wf := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wf, "todo").Want(http.StatusCreated).JSON(&issue)
	req := withURLParam(newRequest("POST", "/api/issues/"+issue.ID+"/rerun", nil), "id", issue.ID)
	testutil.Call(t, testHandler.RerunIssue, req).Want(http.StatusConflict)
}

func TestIsIssueActorTypeAcceptsWorkflow(t *testing.T) {
	if !isIssueActorType("workflow") || isIssueActorType("robot") {
		t.Fatal("isIssueActorType must accept workflow and only the known types")
	}
}

func TestInvolvesUserIncludesWorkflowsTheyCreated(t *testing.T) {
	requireExtWorkflowDB(t)
	setExtWorkflowEngine(t, nil)
	wf := hookWorkflow(t, "")
	issueID := dbfx.Issue(t, t.Name(), testutil.Cols{"assignee_type": "workflow", "assignee_id": wf})
	for _, query := range []string{"", "&open_only=true"} {
		req := newRequest("GET", "/api/issues?workspace_id="+testWorkspaceID+"&involves_user_id="+testUserID+query, nil)
		var body struct {
			Issues []IssueResponse `json:"issues"`
		}
		testutil.Call(t, testHandler.ListIssues, req).Want(http.StatusOK).JSON(&body)
		found := false
		for _, is := range body.Issues {
			found = found || is.ID == issueID
		}
		if !found {
			t.Fatalf("involves_user_id%s did not list the workflow issue %s", query, issueID)
		}
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `cd server && go vet ./internal/handler/`
Expected: build failure — `undefined: NewExtWorkflowBridge`, `testHandler.ExtWorkflow undefined (type *Handler has no field or method ExtWorkflow)`.

- [ ] **Step 3: Implement**

`server/internal/handler/ext_workflow_bridge.go`:

```go
package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow (fork): handler-side glue for the workflow engine. The engine
// cannot import handler, so the invoke gate and the event publisher reach it
// through ExtWorkflowBridge.

// ExtWorkflowBridge implements extworkflow.AgentAccess and extworkflow.Publisher.
type ExtWorkflowBridge struct{ h *Handler }

func NewExtWorkflowBridge(h *Handler) *ExtWorkflowBridge { return &ExtWorkflowBridge{h: h} }

// CanInvokeAgent applies canInvokeAgent to a stored actor. A member is judged
// as themselves; an agent actor has no request-scoped originator here, so it
// is judged like an unattributed agent (workspace-invocable agents only).
func (b *ExtWorkflowBridge) CanInvokeAgent(ctx context.Context, workspaceID pgtype.UUID, actorType string, actorID pgtype.UUID, agentID pgtype.UUID) (bool, error) {
	agent, err := b.h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if agent.ArchivedAt.Valid {
		return false, nil
	}
	actor, originator := uuidToString(actorID), ""
	if actorType == "member" {
		originator = actor
	}
	return b.h.canInvokeAgent(ctx, agent, actorType, actor, originator, uuidToString(workspaceID)), nil
}

func (b *ExtWorkflowBridge) Publish(eventType, workspaceID, actorType, actorID string, payload map[string]any) {
	b.h.publish(eventType, workspaceID, actorType, actorID, payload)
}

// validateExtWorkflowAssignee is validateAssigneePair for a workflow.
func (h *Handler) validateExtWorkflowAssignee(ctx context.Context, r *http.Request, workspaceID string, wsUUID, workflowID pgtype.UUID) (int, string) {
	if !h.ExtWorkflow.Enabled() {
		return http.StatusConflict, "workflow_engine_disabled"
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	actor, _ := util.ParseUUID(actorID)
	err := h.ExtWorkflow.ValidateAssignment(ctx, wsUUID, workflowID, actorType, actor)
	var refused *extworkflow.AssignError
	if errors.As(err, &refused) {
		return refused.Status, refused.Message
	}
	if err != nil {
		slog.Warn("ext-workflow: validate assignment failed", "workflow_id", uuidToString(workflowID), "error", err)
		return http.StatusInternalServerError, "failed to validate workflow assignment"
	}
	return 0, ""
}

// startExtWorkflowRun is dispatchIssueRun for a workflow assignee. The
// assignment already succeeded; a failed start is logged.
func (h *Handler) startExtWorkflowRun(ctx context.Context, issue db.Issue, actorType, actorID string) {
	if !h.ExtWorkflow.Enabled() {
		return
	}
	actor, _ := util.ParseUUID(actorID)
	if err := h.ExtWorkflow.StartRun(ctx, issue.ID, actorType, actor); err != nil {
		slog.Warn("ext-workflow: start run failed", "issue_id", uuidToString(issue.ID), "error", err)
	}
}

// notifyExtWorkflowParentChanged lets the engine stop a run whose parent was
// cancelled or reassigned away from its workflow.
func (h *Handler) notifyExtWorkflowParentChanged(ctx context.Context, prev db.Issue, statusChanged, assigneeChanged bool) {
	if !h.ExtWorkflow.Enabled() || prev.AssigneeType.String != "workflow" || (!statusChanged && !assigneeChanged) {
		return
	}
	if err := h.ExtWorkflow.OnParentChanged(ctx, prev.ID); err != nil {
		slog.Warn("ext-workflow: parent change failed", "issue_id", uuidToString(prev.ID), "error", err)
	}
}

// extWorkflowParentDeleting cancels the run of an issue about to be deleted.
func (h *Handler) extWorkflowParentDeleting(ctx context.Context, issue db.Issue) {
	if !h.ExtWorkflow.Enabled() || issue.AssigneeType.String != "workflow" {
		return
	}
	if err := h.ExtWorkflow.OnParentDeleted(ctx, issue.ID); err != nil {
		slog.Warn("ext-workflow: parent delete failed", "issue_id", uuidToString(issue.ID), "error", err)
	}
}
```

`server/internal/handler/handler.go`:

```diff
diff --git a/server/internal/handler/handler.go b/server/internal/handler/handler.go
--- a/server/internal/handler/handler.go
+++ b/server/internal/handler/handler.go
@@ -25,6 +25,7 @@ import (
 	"github.com/multica-ai/multica/server/internal/dbreader"
 	"github.com/multica-ai/multica/server/internal/entitlement"
 	"github.com/multica-ai/multica/server/internal/events"
+	"github.com/multica-ai/multica/server/internal/extworkflow"
 	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
 	composio "github.com/multica-ai/multica/server/internal/integrations/composio"
 	"github.com/multica-ai/multica/server/internal/integrations/dingtalk"
@@ -414,7 +415,10 @@ type Handler struct {
 	// so the feature degrades cleanly on deployments without a private key.
 	// Wired in cmd/server/router.go after New.
 	PRRefresh *ghsnapshot.Manager
-	cfg       Config
+	// ext-workflow: the workflow engine; nil when MULTICA_WORKFLOW_ENGINE is off
+	// or the schema does not admit workflow assignees. Wired in cmd/server.
+	ExtWorkflow *extworkflow.Engine
+	cfg         Config
 }
 
 func New(queries *db.Queries, txStarter txStarter, hub *realtime.Hub, bus *events.Bus, emailService *service.EmailService, store storage.Storage, cfSigner *auth.CloudFrontSigner, analyticsClient analytics.Client, cfg Config, daemonHubs ...*daemonws.Hub) *Handler {
```

`server/internal/handler/issue.go`:

```diff
diff --git a/server/internal/handler/issue.go b/server/internal/handler/issue.go
--- a/server/internal/handler/issue.go
+++ b/server/internal/handler/issue.go
@@ -1707,6 +1707,12 @@ func (h *Handler) ListIssues(w http.ResponseWriter, r *http.Request) {
           AND a.workspace_id = $1
           AND a.owner_id     = %[1]s::uuid
     ))
+    -- ext-workflow: a workflow the user created involves them, like an agent they own.
+    OR (i.assignee_type = 'workflow' AND i.assignee_id IN (
+       SELECT w.id FROM ext_workflow w
+        WHERE w.workspace_id = $1
+          AND w.creator_id   = %[1]s::uuid
+    ))
 )`, ref))
 	}
 
@@ -1951,7 +1957,8 @@ func splitCommaParam(raw string) []string {
 }
 
 func isIssueActorType(s string) bool {
-	return s == "member" || s == "agent" || s == "squad"
+	// ext-workflow: workflow is an assignee type for filters and grouping.
+	return s == "member" || s == "agent" || s == "squad" || s == "workflow"
 }
 
 func parseUUIDParamList(w http.ResponseWriter, raw, fieldName string) ([]pgtype.UUID, bool) {
@@ -2165,6 +2172,12 @@ func (h *Handler) ListGroupedIssues(w http.ResponseWriter, r *http.Request) {
           AND a.workspace_id = $1
           AND a.owner_id     = %[1]s::uuid
     ))
+    -- ext-workflow: a workflow the user created involves them, like an agent they own.
+    OR (i.assignee_type = 'workflow' AND i.assignee_id IN (
+       SELECT w.id FROM ext_workflow w
+        WHERE w.workspace_id = $1
+          AND w.creator_id   = %[1]s::uuid
+    ))
 )`, ref))
 	}
 
@@ -3249,7 +3262,8 @@ func (h *Handler) CreateIssue(w http.ResponseWriter, r *http.Request) {
 		// found" rather than a 403 that leaks nothing about which input was wrong.
 		// The row itself is no longer needed: the assignee gate keys on the actor's
 		// originator, not on a scope bound to the parent (MUL-6951).
-		if assigneeType.Valid && (assigneeType.String == "agent" || assigneeType.String == "squad") {
+		// ext-workflow: a workflow assignee is gated like a squad.
+		if assigneeType.Valid && (assigneeType.String == "agent" || assigneeType.String == "squad" || assigneeType.String == "workflow") {
 			parent, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{
 				ID:          parentIssueID,
 				WorkspaceID: wsUUID,
@@ -4059,6 +4073,10 @@ func (h *Handler) UpdateIssue(w http.ResponseWriter, r *http.Request) {
 		})
 	}
 
+	// ext-workflow: a parent cancelled or reassigned away from its workflow
+	// stops its run, before any new assignee's run starts.
+	h.notifyExtWorkflowParentChanged(r.Context(), prevIssue, statusChanged, assigneeChanged)
+
 	// Reconcile the task queue. Whether this write starts an agent run — and
 	// for whom (agent assignee or squad leader) — is decided by the single
 	// WillEnqueueRun predicate, shared verbatim with the preview endpoint so
@@ -4183,6 +4201,9 @@ func (h *Handler) validateAssigneePair(ctx context.Context, r *http.Request, wor
 			return http.StatusForbidden, "you do not have permission to assign work to this squad"
 		}
 		return 0, ""
+	case "workflow":
+		// ext-workflow: assignment starts a run; the engine owns the checks.
+		return h.validateExtWorkflowAssignee(ctx, r, workspaceID, wsUUID, assigneeID)
 	default:
 		return http.StatusBadRequest, "assignee_type must be 'member', 'agent', or 'squad'"
 	}
@@ -4311,6 +4332,8 @@ func (h *Handler) DeleteIssue(w http.ResponseWriter, r *http.Request) {
 		return
 	}
 
+	// ext-workflow: stop the issue's workflow run before its tasks are cancelled.
+	h.extWorkflowParentDeleting(r.Context(), issue)
 	h.TaskService.CancelTasksForIssue(r.Context(), issue.ID)
 	// Fail any linked autopilot runs before delete (ON DELETE SET NULL clears issue_id).
 	_ = h.AutopilotService.FailAutopilotRunsByIssue(r.Context(), issue.ID)
@@ -4800,6 +4823,9 @@ func (h *Handler) BatchUpdateIssues(w http.ResponseWriter, r *http.Request) {
 			"prev_duplicate_of_issue_id": liveDuplicateMark(prevIssue.Status, prevIssue.DuplicateOfIssueID),
 		})
 
+		// ext-workflow: mirrors UpdateIssue.
+		h.notifyExtWorkflowParentChanged(r.Context(), prevIssue, statusChanged, assigneeChanged)
+
 		// Reassignment does not cancel existing tasks (#4963 / MUL-4113) —
 		// mirrors UpdateIssue. See that handler for the rationale.
 		//
@@ -4886,6 +4912,7 @@ func (h *Handler) BatchDeleteIssues(w http.ResponseWriter, r *http.Request) {
 		seenIssueIDs[issueUUID] = struct{}{}
 		issues = append(issues, issue)
 		excludedIDs = append(excludedIDs, issue.ID)
+		h.extWorkflowParentDeleting(r.Context(), issue) // ext-workflow
 		h.TaskService.CancelTasksForIssue(r.Context(), issue.ID)
 		_ = h.AutopilotService.FailAutopilotRunsByIssue(r.Context(), issue.ID)
 	}
```

`server/internal/handler/issue_trigger.go`:

```diff
diff --git a/server/internal/handler/issue_trigger.go b/server/internal/handler/issue_trigger.go
--- a/server/internal/handler/issue_trigger.go
+++ b/server/internal/handler/issue_trigger.go
@@ -90,6 +90,9 @@ func (h *Handler) dispatchIssueRun(ctx context.Context, issue db.Issue, trigger
 		_, _ = h.TaskService.EnqueueTaskForIssueWithHandoff(ctx, issue, handoffNote, memberActorUserID(actorType, actorID))
 	case "squad":
 		h.enqueueSquadLeaderTask(ctx, issue, pgtype.UUID{}, actorType, actorID, handoffNote)
+	case "workflow":
+		// ext-workflow: the engine starts the run (children and first steps).
+		h.startExtWorkflowRun(ctx, issue, actorType, actorID)
 	}
 }
 
```

`server/internal/handler/issue_table_query.go`:

```diff
diff --git a/server/internal/handler/issue_table_query.go b/server/internal/handler/issue_table_query.go
--- a/server/internal/handler/issue_table_query.go
+++ b/server/internal/handler/issue_table_query.go
@@ -425,6 +425,12 @@ func appendIssueTableInvolvedPredicate(where []string, addArg func(any) string,
           AND a.workspace_id = $1
           AND a.owner_id     = %[1]s::uuid
     ))
+    -- ext-workflow: a workflow the user created involves them, like an agent they own.
+    OR (i.assignee_type = 'workflow' AND i.assignee_id IN (
+       SELECT w.id FROM ext_workflow w
+        WHERE w.workspace_id = $1
+          AND w.creator_id   = %[1]s::uuid
+    ))
 )`, ref))
 }
 
```

`server/internal/handler/issue_table_group.go`:

```diff
diff --git a/server/internal/handler/issue_table_group.go b/server/internal/handler/issue_table_group.go
--- a/server/internal/handler/issue_table_group.go
+++ b/server/internal/handler/issue_table_group.go
@@ -261,6 +261,7 @@ func (h *Handler) resolveIssueTableGroup(w http.ResponseWriter, r *http.Request,
   WHEN 'member' THEN (SELECT u.name FROM "user" u WHERE u.id = split_part(group_value, ':', 2)::uuid)
   WHEN 'agent' THEN (SELECT a.name FROM agent a WHERE a.workspace_id = $1 AND a.id = split_part(group_value, ':', 2)::uuid)
   WHEN 'squad' THEN (SELECT s.name FROM squad s WHERE s.workspace_id = $1 AND s.id = split_part(group_value, ':', 2)::uuid)
+  WHEN 'workflow' THEN (SELECT w.name FROM ext_workflow w WHERE w.workspace_id = $1 AND w.id = split_part(group_value, ':', 2)::uuid)
 END, ''))`,
 		}, true
 	case "project":
```

`server/internal/handler/task_lifecycle.go`:

```diff
diff --git a/server/internal/handler/task_lifecycle.go b/server/internal/handler/task_lifecycle.go
--- a/server/internal/handler/task_lifecycle.go
+++ b/server/internal/handler/task_lifecycle.go
@@ -217,6 +217,11 @@ func (h *Handler) RerunIssue(w http.ResponseWriter, r *http.Request) {
 		h.writeDispatchBlocked(w, http.StatusForbidden, ReasonIssueInTriage)
 		return
 	}
+	// ext-workflow: a workflow issue reruns through its run's decisions.
+	if errors.Is(err, service.ErrRerunWorkflowIssue) {
+		writeError(w, http.StatusConflict, err.Error())
+		return
+	}
 	// Not a dispatch refusal: the issue may well be runnable, and only the named
 	// source is ineligible. It falls through to the 400 below with the
 	// sentinel's own sentence, like the sibling "does not belong to this issue".
```

`server/pkg/db/queries/issue.sql`:

```diff
diff --git a/server/pkg/db/queries/issue.sql b/server/pkg/db/queries/issue.sql
--- a/server/pkg/db/queries/issue.sql
+++ b/server/pkg/db/queries/issue.sql
@@ -58,6 +58,12 @@ WHERE i.workspace_id = $1
              AND a.workspace_id = $1
              AND a.owner_id     = sqlc.narg('involves_user_id')::uuid
     ))
+    -- ext-workflow: a workflow the user created involves them.
+    OR (i.assignee_type = 'workflow' AND i.assignee_id IN (
+          SELECT w.id FROM ext_workflow w
+           WHERE w.workspace_id = $1
+             AND w.creator_id   = sqlc.narg('involves_user_id')::uuid
+    ))
   )
 ORDER BY i.position ASC, i.created_at DESC
 LIMIT $2 OFFSET $3;
@@ -609,6 +615,12 @@ WHERE i.workspace_id = $1
              AND a.workspace_id = $1
              AND a.owner_id     = sqlc.narg('involves_user_id')::uuid
     ))
+    -- ext-workflow: a workflow the user created involves them.
+    OR (i.assignee_type = 'workflow' AND i.assignee_id IN (
+          SELECT w.id FROM ext_workflow w
+           WHERE w.workspace_id = $1
+             AND w.creator_id   = sqlc.narg('involves_user_id')::uuid
+    ))
   )
 ORDER BY i.position ASC, i.created_at DESC;
 
@@ -655,6 +667,12 @@ WHERE i.workspace_id = $1
              AND a.workspace_id = $1
              AND a.owner_id     = sqlc.narg('involves_user_id')::uuid
     ))
+    -- ext-workflow: a workflow the user created involves them.
+    OR (i.assignee_type = 'workflow' AND i.assignee_id IN (
+          SELECT w.id FROM ext_workflow w
+           WHERE w.workspace_id = $1
+             AND w.creator_id   = sqlc.narg('involves_user_id')::uuid
+    ))
   );
 
 -- name: ListChildIssues :many
```

Then `make sqlc` (regenerates `server/pkg/db/generated/issue.sql.go`).

- [ ] **Step 4: Run, expect PASS**

Run:
```bash
cd server && gofmt -l internal/handler/ext_workflow_bridge.go internal/handler/ext_workflow_hooks_test.go internal/handler/handler.go internal/handler/issue.go && go build ./... && go vet ./internal/handler/
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/handler/ -run \"TestExtWorkflow|TestIsIssueActorTypeAcceptsWorkflow|TestInvolvesUserIncludesWorkflows\" -count=1 -v'"
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/handler/ -count=1'"
```
Expected: `--- PASS` for `TestExtWorkflowAssignRequiresTheEngine`, `TestExtWorkflowAssignStartsARun`, `TestExtWorkflowBacklogStartsOnPromotion`, `TestExtWorkflowParentCancelStopsTheRun`, `TestExtWorkflowAssignNeedsInvokeOnEveryAgent`, `TestExtWorkflowRerunIsAConflict`, `TestIsIssueActorTypeAcceptsWorkflow`, `TestInvolvesUserIncludesWorkflowsTheyCreated` and Part A's `TestExtWorkflow_*`; the full package run is `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/handler/ext_workflow_bridge.go server/internal/handler/ext_workflow_hooks_test.go server/internal/handler/handler.go server/internal/handler/issue.go server/internal/handler/issue_trigger.go server/internal/handler/issue_table_query.go server/internal/handler/issue_table_group.go server/internal/handler/task_lifecycle.go server/pkg/db/queries/issue.sql server/pkg/db/generated
git commit -m "feat(ext-workflow): accept workflow assignees in issue handlers

Assignment validates through the engine and starts a run; cancelling,
reassigning or deleting the parent stops it; filters, grouping and
rerun understand the workflow assignee type.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


---

### Task B9: Server wiring — kill switch, constraint check, listeners, reconcile job

**Files:**
- Create: `server/internal/scheduler/jobs_ext_workflow.go` (modelled on `jobs_child_done.go`)
- Create: `server/cmd/server/ext_workflow_wiring.go`
- Modify: `server/cmd/server/main.go` — after `h.PRRefresh.SetReadSelector(h.ReadSelector)` (~line 699) and after the `ChildEventSweepJob` registration (~line 836)
- Modify: `.env.example` — after `MULTICA_FEATURE_FLAGS_FILE=` (~line 216)
- Test: `server/internal/scheduler/jobs_ext_workflow_test.go`, `server/cmd/server/ext_workflow_wiring_test.go`

**Interfaces:**
- Consumes: B5/B6 engine, B8 `handler.NewExtWorkflowBridge`, `Handler.ExtWorkflow`, `TaskService.ExtWorkflow`, existing `envBool`, `scheduler.JobSpec`.
- Produces: `scheduler.ExtWorkflowReconciler`, `scheduler.ExtWorkflowReconcileJob(r) JobSpec` (`ext_workflow_reconcile`, 30 s, `CatchUpLatestOnly`, `MaxAttempts 1`, global scope); in package `main`: `extWorkflowEngineEnv = "MULTICA_WORKFLOW_ENGINE"`, `extWorkflowConstraintAdmitsWorkflow(ctx, pool) (bool, error)`, `setupExtWorkflow(ctx, pool, bus, h) *extworkflow.Engine`, `registerExtWorkflowListeners(bus, engine extWorkflowTaskObserver)`.

Behaviour: `MULTICA_WORKFLOW_ENGINE` defaults to true. Off, or when `issue_assignee_type_check` lacks `'workflow'` (logged at error level), no engine is built: `h.ExtWorkflow` and `TaskService.ExtWorkflow` stay nil, so there are no listeners, no reconcile job and no hooks, assignment answers 409 `workflow_engine_disabled`, and Part A's CRUD keeps working. The listeners forward `task:completed`, `task:failed` (skipping `retry_pending: true`) and `task:cancelled` to `OnTaskTerminal`.

- [ ] **Step 1: Write the failing test**

`server/internal/scheduler/jobs_ext_workflow_test.go`:

```go
package scheduler

import (
	"context"
	"testing"
	"time"
)

type countingReconciler struct{ calls int }

func (c *countingReconciler) Reconcile(context.Context) error {
	c.calls++
	return nil
}

func TestExtWorkflowReconcileJob(t *testing.T) {
	r := &countingReconciler{}
	job := ExtWorkflowReconcileJob(r)
	if err := job.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if job.Name != "ext_workflow_reconcile" || job.Cadence != 30*time.Second || job.CatchUpMode != CatchUpLatestOnly || job.MaxAttempts != 1 {
		t.Fatalf("job = %s every %s, catch-up %v, attempts %d", job.Name, job.Cadence, job.CatchUpMode, job.MaxAttempts)
	}
	if _, err := job.Handler(context.Background(), HandlerInput{}); err != nil || r.calls != 1 {
		t.Fatalf("handler err=%v calls=%d", err, r.calls)
	}
}
```

`server/cmd/server/ext_workflow_wiring_test.go`:

```go
package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type recordingTaskObserver struct{ ids []pgtype.UUID }

func (r *recordingTaskObserver) OnTaskTerminal(_ context.Context, id pgtype.UUID) error {
	r.ids = append(r.ids, id)
	return nil
}

func TestExtWorkflowListenersForwardTerminalTasks(t *testing.T) {
	bus := events.New()
	obs := &recordingTaskObserver{}
	registerExtWorkflowListeners(bus, obs)
	completed, failed, retried := dbid.NewV7(), dbid.NewV7(), dbid.NewV7()
	publish := func(eventType string, id pgtype.UUID, extra map[string]any) {
		payload := map[string]any{"task_id": util.UUIDToString(id)}
		for k, v := range extra {
			payload[k] = v
		}
		bus.Publish(events.Event{Type: eventType, Payload: payload})
	}
	publish(protocol.EventTaskCompleted, completed, nil)
	publish(protocol.EventTaskFailed, retried, map[string]any{"retry_pending": true})
	publish(protocol.EventTaskFailed, failed, map[string]any{"retry_pending": false})
	if len(obs.ids) != 2 || obs.ids[0] != completed || obs.ids[1] != failed {
		t.Fatalf("forwarded %v, want completed then failed (retry pending skipped)", obs.ids)
	}
}

func TestExtWorkflowConstraintAdmitsWorkflow(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ok, err := extWorkflowConstraintAdmitsWorkflow(context.Background(), testPool)
	if err != nil || !ok {
		t.Fatalf("constraint admits workflow = %v, %v; want true after the ext migrations", ok, err)
	}
}

func TestSetupExtWorkflowHonoursTheKillSwitch(t *testing.T) {
	t.Setenv(extWorkflowEngineEnv, "false")
	if engine := setupExtWorkflow(context.Background(), nil, events.New(), nil); engine != nil {
		t.Fatal("engine built with the kill switch off")
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `cd server && go vet ./internal/scheduler/ ./cmd/server/`
Expected: build failures — `undefined: ExtWorkflowReconcileJob`; `undefined: registerExtWorkflowListeners`, `undefined: extWorkflowConstraintAdmitsWorkflow`, `undefined: setupExtWorkflow`, `undefined: extWorkflowEngineEnv`.

- [ ] **Step 3: Implement**

`server/internal/scheduler/jobs_ext_workflow.go`:

```go
package scheduler

import (
	"context"
	"time"
)

// ExtWorkflowReconciler is *extworkflow.Engine.
type ExtWorkflowReconciler interface{ Reconcile(context.Context) error }

// ExtWorkflowReconcileJob is the workflow engine's safety net (fork,
// ext-workflow): it re-derives events that a crash or a lost bus event
// dropped, for at most 200 active runs per tick.
func ExtWorkflowReconcileJob(r ExtWorkflowReconciler) JobSpec {
	return JobSpec{
		Name: "ext_workflow_reconcile", Cadence: 30 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 45 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, r.Reconcile(ctx)
		},
	}
}
```

`server/cmd/server/ext_workflow_wiring.go`:

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ext-workflow (fork): engine wiring, kept out of main.go so the upstream
// file only carries two hook lines.

// extWorkflowEngineEnv is the kill switch (default on). Off: no engine, no
// listeners, no reconcile job; workflow CRUD stays available and assigning an
// issue to a workflow answers 409 workflow_engine_disabled.
const extWorkflowEngineEnv = "MULTICA_WORKFLOW_ENGINE"

// extWorkflowConstraintAdmitsWorkflow reports whether issue_assignee_type_check
// still admits 'workflow'. An upstream migration that redefines the shared
// constraint drops it; the engine must then stay off (spec §2.3).
func extWorkflowConstraintAdmitsWorkflow(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var def string
	err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
		WHERE c.conname = 'issue_assignee_type_check' AND c.conrelid = 'issue'::regclass`).Scan(&def)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.Contains(def, "'workflow'"), nil
}

// setupExtWorkflow builds the engine when it is enabled and the schema admits
// it, wires it into the handler and the task service, and subscribes the
// task-terminal listeners. It returns nil when the engine stays off.
func setupExtWorkflow(ctx context.Context, pool *pgxpool.Pool, bus *events.Bus, h *handler.Handler) *extworkflow.Engine {
	if !envBool(extWorkflowEngineEnv, true) {
		slog.Info("ext-workflow: engine disabled by " + extWorkflowEngineEnv)
		return nil
	}
	ok, err := extWorkflowConstraintAdmitsWorkflow(ctx, pool)
	if err != nil || !ok {
		slog.Error("ext-workflow: issue_assignee_type_check does not admit 'workflow'; engine disabled", "error", err)
		return nil
	}
	bridge := handler.NewExtWorkflowBridge(h)
	engine := extworkflow.NewEngine(extworkflow.Deps{
		Pool: pool, Queries: h.Queries, Issues: h.IssueService, Tasks: h.TaskService,
		Access: bridge, Publisher: bridge, Enabled: true,
	})
	h.ExtWorkflow = engine
	h.TaskService.ExtWorkflow = engine
	registerExtWorkflowListeners(bus, engine)
	return engine
}

type extWorkflowTaskObserver interface {
	OnTaskTerminal(ctx context.Context, taskID pgtype.UUID) error
}

// registerExtWorkflowListeners forwards terminal task events to the engine.
// A failure with a retry pending is skipped: the retry carries the same
// workflow stamp and reports again.
func registerExtWorkflowListeners(bus *events.Bus, engine extWorkflowTaskObserver) {
	ctx := context.Background()
	forward := func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		if pending, _ := payload["retry_pending"].(bool); pending {
			return
		}
		raw, _ := payload["task_id"].(string)
		taskID, err := util.ParseUUID(raw)
		if err != nil {
			return
		}
		if err := engine.OnTaskTerminal(ctx, taskID); err != nil {
			slog.Warn("ext-workflow: task terminal handling failed", "task_id", raw, "error", err)
		}
	}
	for _, eventType := range []string{protocol.EventTaskCompleted, protocol.EventTaskFailed, protocol.EventTaskCancelled} {
		bus.Subscribe(eventType, forward)
	}
}
```

`server/cmd/server/main.go`:

```diff
diff --git a/server/cmd/server/main.go b/server/cmd/server/main.go
--- a/server/cmd/server/main.go
+++ b/server/cmd/server/main.go
@@ -702,6 +702,8 @@ func main() {
 	// create a second wrapper around the same primary pool.
 	h.ReadSelector = dbreader.New(h.Queries, replicaQueries, readRecorder)
 	h.PRRefresh.SetReadSelector(h.ReadSelector)
+	// ext-workflow: build the workflow engine before the server accepts work.
+	extWorkflowEngine := setupExtWorkflow(context.Background(), pool, bus, h)
 
 	// Reconciled race recoveries in the batched scheduler reuse the same
 	// daemon:register refresh the sync transition path publishes. Wired before
@@ -834,6 +836,12 @@ func main() {
 	if err := schedulerMgr.Register(scheduler.ChildEventSweepJob(&service.IssueWakeupService{Tasks: taskSvc})); err != nil {
 		slog.Error("scheduler: register child-done sweep", "error", err)
 	}
+	// ext-workflow: safety net for active workflow runs (only with the engine on).
+	if extWorkflowEngine != nil {
+		if err := schedulerMgr.Register(scheduler.ExtWorkflowReconcileJob(extWorkflowEngine)); err != nil {
+			slog.Error("scheduler: register ext workflow reconcile", "error", err)
+		}
+	}
 	if err := schedulerMgr.Register(scheduler.AutopilotScheduleDispatchJob(pool, queries, autopilotSvc)); err != nil {
 		slog.Warn("scheduler: failed to register autopilot_schedule_dispatch job", "error", err)
 	}
```

In `.env.example`, after the line `MULTICA_FEATURE_FLAGS_FILE=`:

```diff
 MULTICA_FEATURE_FLAGS_FILE=
+
+# Ext workflow engine (fork). Off stops workflow runs, their listeners and the
+# reconcile job; workflow CRUD stays available and assigning an issue to a
+# workflow answers 409 workflow_engine_disabled.
+# MULTICA_WORKFLOW_ENGINE=true
```

- [ ] **Step 4: Run, expect PASS**

Run:
```bash
cd server && go build ./... && go vet ./internal/scheduler/ ./cmd/server/
go test ./internal/scheduler/ -run TestExtWorkflowReconcileJob -count=1
make env-exec ARGS="-- bash -c 'cd server && go test ./cmd/server/ -run \"TestExtWorkflow|TestSetupExtWorkflow\" -count=1 -v'"
```
Expected: `ok` for scheduler; `--- PASS` for `TestExtWorkflowListenersForwardTerminalTasks`, `TestExtWorkflowConstraintAdmitsWorkflow` (requires Part A's `ext_0003` applied), `TestSetupExtWorkflowHonoursTheKillSwitch`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/scheduler/jobs_ext_workflow.go server/internal/scheduler/jobs_ext_workflow_test.go server/cmd/server/ext_workflow_wiring.go server/cmd/server/ext_workflow_wiring_test.go server/cmd/server/main.go .env.example
git commit -m "feat(ext-workflow): wire the engine into the server

MULTICA_WORKFLOW_ENGINE kill switch (default on), a startup check that
the assignee constraint still admits workflow, task-terminal listeners
and the ext_workflow_reconcile scheduler job.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


---

### Task B10: Engine end-to-end tests with simulated agents

**Files:**
- Test: `server/internal/extworkflow/e2e_test.go`

**Interfaces:**
- Consumes: the B5/B6 harness (`newEnv`, `workflow`, `parentIssue`, `start`, `run`, `step`, `latestTask`, `countTasks`, `endTask`, `setStatus`), the B7 child-event hook (`IssueWakeupService.ProcessChildEvents` → `OnChildEvents`), `Engine.Advance` for decisions (what Part C's decision API and comment protocol call).
- Produces: new harness helpers `childMoves`, `running`, `decide`, `summaryTask`, `wantRunDone`.

This task adds tests only; it is the regression net for the whole engine. If a scenario fails, fix the engine in the task that owns the behaviour, not the test.

Scenarios: happy path (spec → reviewed build → summary → parent `in_review`); review redo then approve; request-rewind → rewind → downstream rerun; escalate → stale human decision refused → human approve; parent cancel cancels the run's tasks and children; a failed task with a pending platform retry is ignored and the retry finishes the step; reconcile recovers a task failure whose bus event was lost.

- [ ] **Step 1: Write the failing test**

`server/internal/extworkflow/e2e_test.go`:

```go
package extworkflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// End-to-end runs with simulated agents: the test plays every agent by
// moving child issues (through the real child-event hook) and ending tasks
// (through OnTaskTerminal, as the bus listener does). Decisions go straight
// to Advance, the call the decision API and the comment protocol make.

// childMoves sets a child issue's status the way an agent's CLI call does and
// lets the platform process the parent's sub-issue change.
func (e *env) childMoves(t *testing.T, parent, child pgtype.UUID, status string) {
	t.Helper()
	e.setStatus(t, child, status)
	if err := (&service.IssueWakeupService{Tasks: e.tasks}).ProcessChildEvents(context.Background(), parent); err != nil {
		t.Fatalf("ProcessChildEvents: %v", err)
	}
}

func (e *env) running(t *testing.T, task db.AgentTaskQueue) db.AgentTaskQueue {
	t.Helper()
	e.fx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, task.ID)
	task.Status = "running"
	return task
}

func (e *env) decide(t *testing.T, run db.ExtWorkflowRun, key string, d Decision, actor Actor) {
	t.Helper()
	if err := e.engine.Advance(context.Background(), run.ID, AdvanceInput{StepKey: key, Event: Event{Kind: EvDecision, Decision: d}, Actor: actor}); err != nil {
		t.Fatalf("decide %s on %s: %v", d.Action, key, err)
	}
}

func (e *env) summaryTask(t *testing.T, run db.ExtWorkflowRun) db.AgentTaskQueue {
	t.Helper()
	task, err := e.q.GetLatestExtWorkflowSummaryTask(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("summary task: %v", err)
	}
	return task
}

func (e *env) wantRunDone(t *testing.T, parent pgtype.UUID) {
	t.Helper()
	run := e.run(t, parent)
	if RunStatus(run.Status) != RunDone || !run.FinishedAt.Valid {
		t.Fatalf("run = %s (finished=%v), want done", run.Status, run.FinishedAt.Valid)
	}
	if got := e.issue(t, parent).Status; got != "in_review" {
		t.Fatalf("parent = %s, want in_review", got)
	}
}

func TestE2EHappyPath(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, review: true, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)

	spec := e.step(t, run, "spec")
	specTask := e.running(t, e.latestTask(t, spec, RoleStep))
	e.childMoves(t, parent, spec.IssueID, "done")
	e.endTask(t, specTask, "completed")
	wantStepRow(t, e.step(t, run, "spec"), StepDone, 1)

	build := e.step(t, run, "build")
	wantStepRow(t, build, StepRunning, 1)
	buildTask := e.running(t, e.latestTask(t, build, RoleStep))
	e.setStatus(t, build.IssueID, "in_review")
	e.endTask(t, buildTask, "completed")
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)

	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	if review.ExtWorkflowKind.String != KindReview || review.IssueID != parent {
		t.Fatalf("review task kind=%q issue=%v", review.ExtWorkflowKind.String, review.IssueID)
	}
	e.decide(t, run, "build", Decision{Action: ActionApprove}, Actor{Type: "agent", ID: supervisor, TaskID: review.ID})
	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	if got := e.issue(t, build.IssueID).Status; got != "done" {
		t.Fatalf("build child = %s, want done", got)
	}
	e.endTask(t, review, "completed")

	summary := e.summaryTask(t, run)
	if summary.AgentID != supervisor || summary.IssueID != parent || summary.Status != "queued" {
		t.Fatalf("summary task = %s for %v on %v", summary.Status, summary.AgentID, summary.IssueID)
	}
	e.endTask(t, summary, "completed")
	e.wantRunDone(t, parent)
	if n := e.fx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`, parent); n != 2 {
		t.Fatalf("milestone comments = %d, want started + finished", n)
	}
}

func TestE2EReviewRedoThenApprove(t *testing.T) {
	e := newEnv(t)
	supervisor, coder := e.agent(t, "Supervisor"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: coder, review: true})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	build := e.step(t, run, "build")

	first := e.running(t, e.latestTask(t, build, RoleStep))
	e.setStatus(t, build.IssueID, "in_review")
	e.endTask(t, first, "completed")
	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	e.decide(t, run, "build", Decision{Action: ActionRedo, Feedback: "add tests"}, Actor{Type: "agent", ID: supervisor, TaskID: review.ID})
	e.endTask(t, review, "completed")

	build = e.step(t, run, "build")
	wantStepRow(t, build, StepRunning, 2)
	if build.LastFeedback.String != "add tests" || e.issue(t, build.IssueID).Status != "in_progress" {
		t.Fatalf("feedback=%q child=%s", build.LastFeedback.String, e.issue(t, build.IssueID).Status)
	}
	second := e.running(t, e.latestTask(t, build, RoleStep))
	if second.ID == first.ID || !strings.Contains(second.HandoffNote.String, "attempt 2 of 3") {
		t.Fatalf("second attempt task %v note=%q", second.ID, second.HandoffNote.String)
	}

	e.setStatus(t, build.IssueID, "in_review")
	e.endTask(t, second, "completed")
	review = e.running(t, e.latestTask(t, e.step(t, run, "build"), RoleSupervisor))
	e.decide(t, run, "build", Decision{Action: ActionApprove}, Actor{Type: "agent", ID: supervisor, TaskID: review.ID})
	e.endTask(t, review, "completed")
	e.endTask(t, e.summaryTask(t, run), "completed")
	e.wantRunDone(t, parent)
	wantStepRow(t, e.step(t, run, "build"), StepDone, 2)
}

func TestE2ERequestRewindThenRewindRerunsDownstream(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	e.childMoves(t, parent, spec.IssueID, "done")
	e.endTask(t, e.latestTask(t, spec, RoleStep), "completed")

	build := e.step(t, run, "build")
	buildTask := e.running(t, e.latestTask(t, build, RoleStep))
	e.decide(t, run, "build", Decision{Action: ActionRequestRewind, To: "spec", Reason: "the spec assumes pagination"},
		Actor{Type: "agent", ID: coder, TaskID: buildTask.ID})
	e.endTask(t, buildTask, "completed")
	build = e.step(t, run, "build")
	wantStepRow(t, build, StepAwaitingSupervisor, 1)
	arbiter := e.running(t, e.latestTask(t, build, RoleSupervisor))
	if arbiter.ExtWorkflowKind.String != KindRewindRequest {
		t.Fatalf("supervisor kind = %q, want rewind_request", arbiter.ExtWorkflowKind.String)
	}

	e.decide(t, run, "build", Decision{Action: ActionRewind, To: "spec", Feedback: "use cursor pagination"},
		Actor{Type: "agent", ID: supervisor, TaskID: arbiter.ID})
	e.endTask(t, arbiter, "completed")
	if got := e.run(t, parent).RewindsUsed; got != 1 {
		t.Fatalf("rewinds used = %d", got)
	}
	spec = e.step(t, run, "spec")
	wantStepRow(t, spec, StepRunning, 1)
	if spec.LastFeedback.String != "use cursor pagination" {
		t.Fatalf("spec feedback = %q", spec.LastFeedback.String)
	}
	wantStepRow(t, e.step(t, run, "build"), StepPending, 0)
	if got := e.issue(t, build.IssueID).Status; got != "backlog" {
		t.Fatalf("build child = %s, want backlog", got)
	}

	e.childMoves(t, parent, spec.IssueID, "done")
	e.endTask(t, e.latestTask(t, spec, RoleStep), "completed")
	build = e.step(t, run, "build")
	wantStepRow(t, build, StepRunning, 1)
	if n := e.countTasks(t, build, RoleStep); n != 2 {
		t.Fatalf("build step tasks = %d, want the original and the rerun", n)
	}
	e.childMoves(t, parent, build.IssueID, "done")
	e.endTask(t, e.latestTask(t, build, RoleStep), "completed")
	e.endTask(t, e.summaryTask(t, run), "completed")
	e.wantRunDone(t, parent)
}

func TestE2EEscalateThenHumanDecision(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, coder := e.agent(t, "Supervisor"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: coder, review: true})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	build := e.step(t, run, "build")
	e.setStatus(t, build.IssueID, "in_review")
	e.endTask(t, e.latestTask(t, build, RoleStep), "completed")

	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	e.decide(t, run, "build", Decision{Action: ActionEscalate, Reason: "needs a product decision"}, Actor{Type: "agent", ID: supervisor, TaskID: review.ID})
	e.endTask(t, review, "completed")
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingHuman, 1)
	if RunStatus(e.run(t, parent).Status) != RunWaitingHuman {
		t.Fatalf("run = %s, want waiting_human", e.run(t, parent).Status)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND type = $2`, e.user, InboxTypeEscalation); n != 1 {
		t.Fatalf("escalation inbox items = %d", n)
	}

	approve := AdvanceInput{StepKey: "build", Event: Event{Kind: EvDecision, Decision: Decision{Action: ActionApprove}}, Actor: Actor{Type: "member", ID: e.user}}
	stale := approve
	stale.ExpectedStatus = StepAwaitingSupervisor
	if err := e.engine.Advance(ctx, run.ID, stale); !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("stale decision: %v", err)
	}
	approve.ExpectedStatus = StepAwaitingHuman
	if err := e.engine.Advance(ctx, run.ID, approve); err != nil {
		t.Fatalf("human approve: %v", err)
	}
	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	if RunStatus(e.run(t, parent).Status) != RunRunning {
		t.Fatalf("run = %s, want running", e.run(t, parent).Status)
	}
	e.endTask(t, e.summaryTask(t, run), "completed")
	e.wantRunDone(t, parent)
}

func TestE2EParentCancelCancelsTasks(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	task := e.running(t, e.latestTask(t, spec, RoleStep))

	e.setStatus(t, parent, "cancelled")
	if err := e.engine.OnParentChanged(context.Background(), parent); err != nil {
		t.Fatalf("OnParentChanged: %v", err)
	}
	if got := RunStatus(e.run(t, parent).Status); got != RunCancelled {
		t.Fatalf("run = %s, want cancelled", got)
	}
	if got := e.latestTask(t, spec, RoleStep); got.ID != task.ID || got.Status != "cancelled" || got.FailureReason.String != service.ExtWorkflowEngineCancelReason {
		t.Fatalf("spec task = %s (%q)", got.Status, got.FailureReason.String)
	}
	for _, key := range []string{"spec", "build"} {
		s := e.step(t, run, key)
		if StepStatus(s.Status) != StepCancelled || e.issue(t, s.IssueID).Status != "cancelled" {
			t.Fatalf("%s = %s, child %s", key, s.Status, e.issue(t, s.IssueID).Status)
		}
	}
	e.endTask(t, task, "cancelled") // the engine's own cancellation reports back harmlessly
}

func TestE2ERetryPendingIsIgnored(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	first := e.running(t, e.latestTask(t, spec, RoleStep))

	e.fx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', completed_at = now(), failure_reason = 'provider_network' WHERE id = $1`, first.ID)
	retry, err := e.q.CreateRetryTask(ctx, db.CreateRetryTaskParams{ID: first.ID})
	if err != nil {
		t.Fatalf("CreateRetryTask: %v", err)
	}
	if err := e.engine.OnTaskTerminal(ctx, first.ID); err != nil {
		t.Fatalf("OnTaskTerminal: %v", err)
	}
	wantStepRow(t, e.step(t, run, "spec"), StepRunning, 1)
	if e.countTasks(t, spec, RoleSupervisor) != 0 || retry.ExtWorkflowStepID != spec.ID {
		t.Fatalf("retry pending woke the supervisor or lost its stamp")
	}

	e.childMoves(t, parent, spec.IssueID, "done")
	e.endTask(t, e.running(t, retry), "completed")
	wantStepRow(t, e.step(t, run, "spec"), StepDone, 1)
}

func TestE2EReconcileRecoversALostTaskEvent(t *testing.T) {
	e := newEnv(t)
	supervisor, planner := e.agent(t, "Supervisor"), e.agent(t, "Planner")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "spec", title: "Spec", agent: planner})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	task := e.latestTask(t, spec, RoleStep)

	// The task fails, but the bus event never reaches the engine.
	e.fx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', started_at = now(), completed_at = now(), failure_reason = 'agent_error', error = 'crashed' WHERE id = $1`, task.ID)
	wantStepRow(t, e.step(t, run, "spec"), StepRunning, 1)

	if err := e.engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	spec = e.step(t, run, "spec")
	wantStepRow(t, spec, StepAwaitingSupervisor, 1)
	if sup := e.latestTask(t, spec, RoleSupervisor); sup.ExtWorkflowKind.String != KindFailure {
		t.Fatalf("supervisor kind = %q, want failure", sup.ExtWorkflowKind.String)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

These tests pass as soon as they compile when B1–B9 are in place; they are the regression net. Confirm they bite with a throwaway mutation: in `observe.go` `deriveStep`, change `if childStatus == issuestatus.Done || childStatus == issuestatus.InReview {` to `if childStatus == issuestatus.Done {`, then run
`make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -run TestE2EHappyPath -count=1'"`.
Expected: `FAIL` with `review task kind="failure" issue=…` (a child moved to `in_review` was read as "ended without finishing"). Revert the mutation (`git checkout server/internal/extworkflow/observe.go`).

- [ ] **Step 3: Implement**

No production code: B1–B9 implement every behaviour exercised here.

- [ ] **Step 4: Run, expect PASS**

Run:
```bash
cd server && gofmt -l ./internal/extworkflow && go vet ./internal/extworkflow/
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -run TestE2E -count=1 -race -v'"
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ ./internal/service/ ./internal/scheduler/ ./cmd/server/ -count=1'"
```
Expected: seven `--- PASS: TestE2E…` lines (not SKIP); every package `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/extworkflow/e2e_test.go
git commit -m "test(ext-workflow): end-to-end runs with simulated agents

Happy path, review redo, request-rewind and rewind, escalation to a
person, parent cancel, ignored retry-pending failures and reconcile
recovery, driven through the real child-event hook and task listener path.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


# Part C

# Part C: Run API, comment protocol, conversation, claim-time briefing

## Notes for integrator

**Verification status.**
- Every code block below was applied in task order to a scratch copy of `server/`. The copy was Part B's verified tree, which already carries Part A's server side. One thing was added to it first: Part A's one-line `registerExtRoutes(r, h)` hook in `cmd/server/router.go`, which B's scratch copy lacked.
- After applying, `make sqlc` (sqlc v1.31.1), `go build ./...` and `go vet` all ran clean, and gofmt reports nothing for the new and edited files.
- The tree was migrated on Postgres 17 into a throwaway database, `extwf_partc_verify` in the `multica-wf-test-pg` container, which was dropped afterwards.
- These suites pass:
  - `internal/extworkflow`, including under `-race`;
  - `cmd/server`;
  - `internal/service`;
  - `internal/scheduler`.
- The full `internal/handler` package fails only the environmental tests that Part B also reported:
  - the four `TestExamplePlugin*` tests and `TestSchedulePulseExampleManifestDrivesDurableRetry`, which need `examples/` outside `server/`;
  - `TestWorkspaceDeletionManifestCoversPublicSchema`, because Part A's manifest edit was not in the scratch tree.
- Not run: `make check`, the frontend, and Playwright.
- Rebuilding the tree from this file alone reproduces the verified tree byte for byte. The check: write every `server/...` code block, `git apply` every diff, then run `make sqlc`, `go build ./...` and `go vet`. The verified tree is kept at `scratchpad/pc/server`, with one commit per task.

**No migration.**
- Part C adds one query file, `server/pkg/db/queries/ext_workflow_protocol.sql`, with two queries:
  - `HasExtWorkflowConversationTaskForComment`;
  - `GetLatestAgentCommentOnIssue`.
- It adds no table, so the workspace-deletion manifest is unaffected.

**Contract deviations and additions (with reason)**

1. **`DecideInput` has one extra field.** Its fields are `{RunID, StepID, Decision, ActorType, ActorID, OnBehalfOf, TaskID, ExpectedStatus}`. The extra one is `TaskID`, the deciding agent task, which B's `Actor.TaskID` spares from cancellation.
   - New sentinel: `ErrStepNotFound` → 404.
   - New exported names:
     - `Engine.MemberCanDecide(ctx, run, userID)`, which implements spec §7.2: the triggering member, the workflow creator, or an owner/admin;
     - `HumanActions`;
     - `StaleBriefing`;
     - `ProtocolErrorReason = "invalid_block"`.
   - A member may only `approve`, `redo`, `retry`, `skip`, `rewind` or `abort`. A member sending `escalate` or `request-rewind` gets 422. The same set applies to a supervisor acting on someone's behalf.
2. **The comment protocol identifies the deciding task by `comment.source_task_id`.**
   - The spec says "comments carry no source task". v0.6.1 does have one: CreateComment stamps it from the CLI's server-trusted `X-Task-ID`.
   - When it is set, that task must be one of the run's in-flight tasks. Otherwise the author's in-flight tasks are matched by role.
   - A run participant whose block is invalid or misplaced gets a `protocol_error` event and a system reply. Anyone else is ignored.
3. **Who a conversation decision is attributed to.** The commenter is the author of the conversation task's `trigger_comment_id`; nothing else is stored. `Decide` checks permission against that member and records `on_behalf_of`.
4. **The conversation hook has two parts.**
   - The `routeAssigneeFallback` `case "workflow"`, as the spec asks.
   - One marker line in `triggerTasksForComment`. It is needed because `computeCommentAgentTriggers` also runs for two other callers, and only the marked pass may wake the supervisor:
     - the trigger preview, where `editing_comment_id` sets `ExcludeTriggerCommentID`;
     - comment replays: the cancelled-batch retrigger, and the daemon's re-evaluation at the end of a run.
   - `HasExtWorkflowConversationTaskForComment` makes the wake idempotent per comment.
5. **`isNoteComment` calls `isExtWorkflowBlockComment`**, which is defined in `handler/ext_workflow_comment.go`. This keeps `comment.go` free of a new import line.
6. **`GET /api/ext/workflows/{id}/runs` is implemented here.** It is registered inside `registerExtRoutes`. `limit` defaults to 50 and is capped at 200; `offset` defaults to 0.
7. **`GET /api/ext/workflow-runs?issue_id=`** accepts a UUID or an identifier, through `loadIssueForUser`.
   - For a parent issue it returns `{runs, step_of: null}`.
   - For a child issue it returns `{runs: [], step_of: {...}}`.
8. **What the step briefing tells the agent to do when finished.** It says "move this issue to `done` (`in_review` also completes the step)". The platform runtime instructions tell agents to deliver to `in_review`, and the engine accepts both.
9. **Cancelling from the API** uses reason "a person cancelled the run". The parent's status is left as it is, as in B's `endRun`.

**What Part D needs to know**

- **Decision endpoint.** On success it returns 200 with the full `Run` (summary, `steps` and `events`). On failure:
  - 403: not permitted;
  - 404: unknown run or step;
  - 409: the status changed (`expected_status` mismatch, or the step or run moved on), or the engine is off (body `{"error":"workflow_engine_disabled"}`);
  - 422: an illegal decision, a missing `feedback`/`reason`/`to`, an action reserved for agents, or an exhausted attempt or rewind budget.
- **Cancel** returns 204, 403, 404 or 409.
- **Reads** (run, runs by issue, runs by workflow) work while the engine is off.
- **`step_of.index` is 1-based.**
- **The escalation inbox item has `issue_id` = the parent issue.** This was verified in B's `engine.escalate` (`IssueID: parent.ID`), so no fix was needed.
- **Timeline payloads added by Part C:**
  - `protocol_error`: `{reason:"invalid_block", error, comment_id, issue_id, task_id, action?}`, with `actor_type:"agent"`. B's own `protocol_error` payload is `{reason:"no_decision", detail, wake}`.
  - An agent decision is `decision` with `actor_type:"agent"`, plus `on_behalf_of` when it came from a conversation.

**Known limitations (deliberate)**

- **A reply in the supervisor's thread on the parent is not a conversation.** When a person replies inside the thread of a supervisor comment, the platform's reply-to-parent-author route wakes the supervisor. That task is plain: it has no workflow columns and no briefing, and any block it posts is ignored.
  - Only top-level comments, and replies to non-agent threads, reach the workflow fallback.
- **The trigger preview does not list the supervisor** for a workflow parent.
- **A conversation needs the commenter to be allowed to invoke the supervisor.**

**Test harness helpers added**
- `internal/extworkflow`:
  - `e.member(t, role)`, `e.reviewRun(t, starter)`;
  - `block(lines...)`, `e.agentSays(...)`, `e.protocolErrors(...)`;
  - `e.memberSays(...)`, `e.conversationTask(...)`.
- `internal/handler`:
  - `extRunReq`, `startExtRun`, `failExtStep`, `getExtRun`;
  - `postComment`, `conversationTasks`, `forwardExtWorkflowComments`;
  - `extWFAgent{t,id}` with `.comment` and `.moves`;
  - `extTask`, `mustExtTask`, `endExtTask`, `extStep`, `wantExtStep`, `protocolReplies`, `e2eWorkflow`.

**Commands**

DB-backed tests skip silently without a database, so pass `-v` to confirm PASS. Run every command from the repo root:

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -count=1 -v'"
```

**Ordering**

- C1 → C7 are sequential.
- Part C needs all of Part B (B1–B10), and Part A's handler test helpers (A4) and `registerExtRoutes`.

## Tasks

- C1 Engine decisions and cancellation (`decide.go`)
- C2 Run HTTP endpoints (`handler/ext_workflow_run.go`, routes)
- C3 Comment decision protocol (`protocol.go`, `Engine.OnComment`)
- C4 Conversation wake (`conversation.go`, `Engine.OnMemberParentComment`, protocol queries)
- C5 Comment hooks: `isNoteComment`, `routeAssigneeFallback`, `comment:created` listener
- C6 Claim-time briefing (`briefing.go`, daemon claim hook)
- C7 End-to-end agent protocol through the HTTP handlers

---

### Task C1: Engine decisions and cancellation

**Files:**
- Create: `server/internal/extworkflow/decide.go`
- Test: `server/internal/extworkflow/decide_test.go`

**Interfaces:**
- Consumes (Part B): `Engine.Advance`, `AdvanceInput`, `Actor`, `Event{Kind: EvDecision|EvCancelRun}`, `Engine.LoadRun`, `RunSnapshot.KeyOf`, `ValidateDecision`, `ErrEngineDisabled/ErrRunNotFound/ErrForbidden/ErrIllegalDecision/ErrStatusMismatch`; queries `GetMemberByUserAndWorkspace`, `GetExtWorkflowInWorkspace`; harness `newEnv`, `workflow`, `parentIssue`, `run`, `step`, `latestTask`, `running`, `childMoves`, `wantStepRow`.
- Produces: `DecideInput`, `Engine.Decide`, `Engine.CancelRun`, `Engine.MemberCanDecide`, `HumanActions`, `ErrStepNotFound`; test helpers `e.member(t, role)`, `e.reviewRun(t, starter) (run, step, supervisor)`.

- [ ] **Step 1: Write the failing test**

`server/internal/extworkflow/decide_test.go`:

```go
package extworkflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// member adds a workspace member with the given role.
func (e *env) member(t *testing.T, role string) pgtype.UUID {
	t.Helper()
	n := envSerial.Add(1)
	user := e.fx.User(t, "Member "+role, fmt.Sprintf("extwf-member-%d-%d@multica.test", os.Getpid(), n))
	e.fx.Member(t, e.fx.WorkspaceID, user, role)
	return util.MustParseUUID(user)
}

// reviewRun starts a one-step workflow whose step needs review and finishes
// its first attempt, so the step waits for the supervisor.
func (e *env) reviewRun(t *testing.T, starter pgtype.UUID) (db.ExtWorkflowRun, db.ExtWorkflowRunStep, pgtype.UUID) {
	t.Helper()
	supervisor, coder := e.agent(t, "Supervisor"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: coder, review: true})
	parent := e.parentIssue(t, wf, "todo")
	if err := e.engine.StartRun(context.Background(), parent, "member", starter); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	run := e.run(t, parent)
	build := e.step(t, run, "build")
	e.running(t, e.latestTask(t, build, RoleStep))
	e.childMoves(t, parent, build.IssueID, "done")
	build = e.step(t, run, "build")
	wantStepRow(t, build, StepAwaitingSupervisor, 1)
	return run, build, supervisor
}

func TestMemberCanDecide(t *testing.T) {
	e := newEnv(t)
	trigger, creator, admin, plain := e.member(t, "member"), e.member(t, "member"), e.member(t, "admin"), e.member(t, "member")
	run, _, _ := e.reviewRun(t, trigger)
	e.fx.Exec(t, `UPDATE ext_workflow SET creator_id = $1 WHERE id = $2`, creator, run.WorkflowID)
	for name, tc := range map[string]struct {
		user pgtype.UUID
		want bool
	}{
		"trigger": {trigger, true}, "creator": {creator, true}, "admin": {admin, true}, "plain": {plain, false}, "none": {pgtype.UUID{}, false},
	} {
		got, err := e.engine.MemberCanDecide(context.Background(), run, tc.user)
		if err != nil || got != tc.want {
			t.Errorf("%s: MemberCanDecide = %v, %v; want %v", name, got, err, tc.want)
		}
	}
}

func TestDecideChecksPermissionStatusAndAction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plain := e.member(t, "member")
	run, build, supervisor := e.reviewRun(t, e.user)
	approve := Decision{Action: ActionApprove}
	member := func(user pgtype.UUID, d Decision, expected StepStatus) error {
		return e.engine.Decide(ctx, DecideInput{RunID: run.ID, StepID: build.ID, Decision: d, ActorType: "member", ActorID: user, ExpectedStatus: expected})
	}

	if err := member(plain, approve, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("plain member: %v, want ErrForbidden", err)
	}
	if err := e.engine.Decide(ctx, DecideInput{RunID: run.ID, StepID: build.ID, Decision: approve, ActorType: "agent", ActorID: supervisor, OnBehalfOf: plain}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("supervisor for a plain member: %v, want ErrForbidden", err)
	}
	if err := member(e.user, Decision{Action: ActionEscalate, Reason: "x"}, ""); !errors.Is(err, ErrIllegalDecision) {
		t.Fatalf("member escalate: %v, want ErrIllegalDecision", err)
	}
	if err := member(e.user, Decision{Action: ActionRedo}, ""); !errors.Is(err, ErrIllegalDecision) {
		t.Fatalf("redo without feedback: %v, want ErrIllegalDecision", err)
	}
	if err := member(e.user, approve, StepAwaitingHuman); !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("stale expected status: %v, want ErrStatusMismatch", err)
	}
	if err := e.engine.Decide(ctx, DecideInput{RunID: run.ID, StepID: run.ID, Decision: approve, ActorType: "member", ActorID: e.user}); !errors.Is(err, ErrStepNotFound) {
		t.Fatalf("foreign step id: %v, want ErrStepNotFound", err)
	}
	if err := e.engine.Decide(ctx, DecideInput{RunID: build.ID, StepID: build.ID, Decision: approve, ActorType: "member", ActorID: e.user}); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("unknown run: %v, want ErrRunNotFound", err)
	}
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)

	if err := member(e.user, approve, StepAwaitingSupervisor); err != nil {
		t.Fatalf("approve: %v", err)
	}
	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	events, err := e.q.ListExtWorkflowRunEvents(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var decision db.ExtWorkflowRunEvent
	for _, ev := range events {
		if ev.Kind == RunEventDecision {
			decision = ev
		}
	}
	if decision.ActorType != "member" || decision.ActorID != e.user || decision.StepID != build.ID {
		t.Fatalf("decision event = %+v", decision)
	}
	if err := member(e.user, approve, ""); !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("second approve: %v, want ErrStatusMismatch", err)
	}
}

func TestCancelRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plain := e.member(t, "member")
	run, build, _ := e.reviewRun(t, e.user)

	if err := e.engine.CancelRun(ctx, run.ID, "member", plain); !errors.Is(err, ErrForbidden) {
		t.Fatalf("plain member cancel: %v, want ErrForbidden", err)
	}
	if err := e.engine.CancelRun(ctx, run.ID, "member", e.user); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := e.run(t, run.IssueID).Status; got != string(RunCancelled) {
		t.Fatalf("run = %s, want cancelled", got)
	}
	wantStepRow(t, e.step(t, run, "build"), StepCancelled, 1)
	if n := e.fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE ext_workflow_run_id = $1 AND status IN ('queued','dispatched','running')`, run.ID); n != 0 {
		t.Fatalf("%d tasks still active", n)
	}
	if got := e.issue(t, build.IssueID).Status; got != "cancelled" {
		t.Fatalf("child = %s, want cancelled", got)
	}
	if err := e.engine.CancelRun(ctx, run.ID, "member", e.user); !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("second cancel: %v, want ErrStatusMismatch", err)
	}
}

func TestDecideNeedsTheEngine(t *testing.T) {
	off := NewEngine(Deps{})
	if err := off.Decide(context.Background(), DecideInput{Decision: Decision{Action: ActionApprove}}); !errors.Is(err, ErrEngineDisabled) {
		t.Fatalf("Decide = %v, want ErrEngineDisabled", err)
	}
	if err := off.CancelRun(context.Background(), pgtype.UUID{}, "member", pgtype.UUID{}); !errors.Is(err, ErrEngineDisabled) {
		t.Fatalf("CancelRun = %v, want ErrEngineDisabled", err)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -run \"TestMemberCanDecide|TestDecide|TestCancelRun\" -count=1'"
```

Expected: build failure, `e.engine.MemberCanDecide undefined (type *Engine has no field or method MemberCanDecide)` and `e.engine.Decide undefined`.

- [ ] **Step 3: Implement**

`server/internal/extworkflow/decide.go`:

```go
package extworkflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Decisions and cancellation from people (the run API) and from agents (the
// comment protocol). Both end in Advance; this file only resolves the step,
// checks who may act and which actions they may take (spec §6.3, §7.2).

// ErrStepNotFound: the step id does not belong to the run.
var ErrStepNotFound = errors.New("workflow run step not found")

// DecideInput is one decision on a run step.
type DecideInput struct {
	RunID pgtype.UUID
	// StepID may be invalid only for abort.
	StepID   pgtype.UUID
	Decision Decision
	// ActorType is "member" (the run API) or "agent" (the comment protocol).
	ActorType string
	ActorID   pgtype.UUID
	// OnBehalfOf is the member a supervisor acts for in a conversation turn;
	// the permission check is made against that member.
	OnBehalfOf pgtype.UUID
	// TaskID is the agent task that decided; cancellations spare it.
	TaskID pgtype.UUID
	// ExpectedStatus guards against a stale UI ("" = no check).
	ExpectedStatus StepStatus
}

// HumanActions are the decisions a person (or a supervisor acting for one)
// may take. escalate and request-rewind belong to the agents.
var HumanActions = []DecisionAction{ActionApprove, ActionRedo, ActionRetry, ActionSkip, ActionRewind, ActionAbort}

func isHumanAction(a DecisionAction) bool {
	for _, h := range HumanActions {
		if h == a {
			return true
		}
	}
	return false
}

// Decide applies a decision. Errors: ErrEngineDisabled, ErrRunNotFound,
// ErrStepNotFound, ErrForbidden, ErrIllegalDecision, ErrStatusMismatch.
func (e *Engine) Decide(ctx context.Context, in DecideInput) error {
	if !e.Enabled() {
		return ErrEngineDisabled
	}
	if err := ValidateDecision(in.Decision); err != nil {
		return err
	}
	snap, err := e.LoadRun(ctx, in.RunID)
	if err != nil {
		return err
	}
	key := ""
	if in.StepID.Valid {
		k, ok := snap.KeyOf(in.StepID)
		if !ok {
			return ErrStepNotFound
		}
		key = k
	} else if in.Decision.Action != ActionAbort {
		return fmt.Errorf("%w: %s needs a step", ErrIllegalDecision, in.Decision.Action)
	}
	switch in.ActorType {
	case "member":
		if !isHumanAction(in.Decision.Action) {
			return fmt.Errorf("%w: %s is reserved for agents", ErrIllegalDecision, in.Decision.Action)
		}
		if err := e.requireDecider(ctx, snap.Run, in.ActorID); err != nil {
			return err
		}
	case "agent":
		if in.OnBehalfOf.Valid {
			if !isHumanAction(in.Decision.Action) {
				return fmt.Errorf("%w: %s cannot be taken on behalf of a person", ErrIllegalDecision, in.Decision.Action)
			}
			if err := e.requireDecider(ctx, snap.Run, in.OnBehalfOf); err != nil {
				return err
			}
		}
	default:
		return ErrForbidden
	}
	d := in.Decision
	d.Step = key
	return e.Advance(ctx, in.RunID, AdvanceInput{
		StepKey:        key,
		Event:          Event{Kind: EvDecision, Decision: d},
		Actor:          Actor{Type: in.ActorType, ID: in.ActorID, OnBehalfOf: in.OnBehalfOf, TaskID: in.TaskID},
		ExpectedStatus: in.ExpectedStatus,
	})
}

// CancelRun stops an active run for a person (spec §5.6, §7.2).
func (e *Engine) CancelRun(ctx context.Context, runID pgtype.UUID, actorType string, actorID pgtype.UUID) error {
	if !e.Enabled() {
		return ErrEngineDisabled
	}
	snap, err := e.LoadRun(ctx, runID)
	if err != nil {
		return err
	}
	if actorType != "member" {
		return ErrForbidden
	}
	if err := e.requireDecider(ctx, snap.Run, actorID); err != nil {
		return err
	}
	if !RunStatus(snap.Run.Status).Active() {
		return fmt.Errorf("%w: the run is %s", ErrStatusMismatch, snap.Run.Status)
	}
	return e.Advance(ctx, runID, AdvanceInput{
		Event: Event{Kind: EvCancelRun, Reason: "a person cancelled the run"},
		Actor: Actor{Type: "member", ID: actorID},
	})
}

func (e *Engine) requireDecider(ctx context.Context, run db.ExtWorkflowRun, userID pgtype.UUID) error {
	ok, err := e.MemberCanDecide(ctx, run, userID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// MemberCanDecide reports whether a member may decide on, or cancel, a run:
// the member who triggered it, the workflow's creator, or a workspace
// owner/admin (spec §7.2).
func (e *Engine) MemberCanDecide(ctx context.Context, run db.ExtWorkflowRun, userID pgtype.UUID) (bool, error) {
	if !userID.Valid {
		return false, nil
	}
	if run.TriggeredByType == "member" && run.TriggeredByID == userID {
		return true, nil
	}
	member, err := e.q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: userID, WorkspaceID: run.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load member: %w", err)
	}
	if member.Role == "owner" || member.Role == "admin" {
		return true, nil
	}
	wf, err := e.q.GetExtWorkflowInWorkspace(ctx, db.GetExtWorkflowInWorkspaceParams{ID: run.WorkflowID, WorkspaceID: run.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load workflow: %w", err)
	}
	return wf.CreatorID == userID, nil
}
```

- [ ] **Step 4: Run, expect PASS**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -run \"TestMemberCanDecide|TestDecide|TestCancelRun\" -count=1 -v'"
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -count=1'"
```

Expected: `--- PASS` for `TestMemberCanDecide`, `TestDecideChecksPermissionStatusAndAction`, `TestCancelRun` and `TestDecideNeedsTheEngine`; the package is `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -l server/internal/extworkflow
git add server/internal/extworkflow/decide.go server/internal/extworkflow/decide_test.go
git commit -m "feat(ext-workflow): engine decisions and run cancellation with permission checks

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task C2: Run HTTP endpoints

**Files:**
- Create: `server/internal/handler/ext_workflow_run.go`
- Modify: `server/cmd/server/ext_routes.go` (`registerExtRoutes`: a `/runs` route under `/api/ext/workflows/{id}`, and a new `/api/ext/workflow-runs` group)
- Test: `server/internal/handler/ext_workflow_run_test.go`

**Interfaces:**
- Consumes:
  - C1: `Engine.Decide`, `Engine.CancelRun`, `ErrStepNotFound`.
  - Part B queries: `GetExtWorkflowRunSummary`, `ListExtWorkflowRunSummariesByIssue`, `ListExtWorkflowRunSummariesByWorkflow`, `CountExtWorkflowRunsByWorkflow`, `GetExtWorkflowRunInWorkspace`, `ListExtWorkflowRunSteps`, `ListExtWorkflowRunEvents`, `GetExtWorkflowRunStepByIssue`.
  - Part A: `loadExtWorkflowInWorkspace`, `extWFReq`, `hookWorkflow` (B8), `createWorkflowIssue` (B8), `withExtWorkflowEngine` / `setExtWorkflowEngine` (B8).
  - Existing: `loadIssueForUser`, `getIssuePrefix`, `requireWorkspaceMember`, `createPlainMember`.
  - Identifiers are `prefix + "-" + number`, the rule `issueToResponse` uses (`handler/issue.go:472`).
- Produces:
  - Handlers: `ListExtWorkflowRunsForWorkflow`, `ListExtWorkflowRunsForIssue`, `GetExtWorkflowRun`, `CancelExtWorkflowRun`, `DecideExtWorkflowStep`.
  - Types: `ExtWorkflowRunSummaryResponse`, `ExtWorkflowRunResponse`, `ExtWorkflowStepResponse`, `ExtWorkflowRunEventResponse`, `ExtWorkflowStepOfResponse`.
  - Helpers: `writeExtWorkflowEngineError`.
  - Test helpers: `extRunReq`, `startExtRun`, `failExtStep`, `getExtRun`.

JSON shapes are the contract's. `title`, `max_attempts`, `requires_review` and `depends_on` on a step come from the run's definition snapshot. `max_rewinds` on a summary also comes from the snapshot.

- [ ] **Step 1: Write the failing test**

`server/internal/handler/ext_workflow_run_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// extRunReq is extWFReq plus the step URL param of the decision route.
func extRunReq(userID, method, path string, body any, runID, stepID string) *http.Request {
	req := extWFReq(userID, method, path, body, runID)
	if stepID != "" {
		chi.RouteContext(req.Context()).URLParams.Add("stepId", stepID)
	}
	return req
}

// startExtRun assigns a fresh issue to a one-step workflow and returns the
// parent issue and its run id.
func startExtRun(t *testing.T) (IssueResponse, string, string) {
	t.Helper()
	wf := hookWorkflow(t, "")
	var issue IssueResponse
	createWorkflowIssue(t, wf, "todo").Want(http.StatusCreated).JSON(&issue)
	dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1 OR issue_id IN (SELECT id FROM issue WHERE parent_issue_id = $1)`, issue.ID)
	var runID string
	dbfx.QueryRow(t, `SELECT id FROM ext_workflow_run WHERE issue_id = $1`, issue.ID).Scan(&runID)
	return issue, runID, wf
}

// failExtStep ends the step task without the child issue finishing, which
// sends the step to the supervisor with pending_reason=failure.
func failExtStep(t *testing.T, engine *extworkflow.Engine, parentID string) {
	t.Helper()
	var taskID string
	dbfx.QueryRow(t, `
		SELECT t.id FROM agent_task_queue t JOIN issue c ON c.id = t.issue_id
		WHERE c.parent_issue_id = $1 AND t.ext_workflow_role = 'step'
		ORDER BY t.created_at DESC LIMIT 1`, parentID).Scan(&taskID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', started_at = now(), completed_at = now() WHERE id = $1`, taskID)
	if err := engine.OnTaskTerminal(context.Background(), util.MustParseUUID(taskID)); err != nil {
		t.Fatalf("OnTaskTerminal: %v", err)
	}
}

func getExtRun(t *testing.T, runID string) ExtWorkflowRunResponse {
	t.Helper()
	var run ExtWorkflowRunResponse
	testutil.Call(t, testHandler.GetExtWorkflowRun, extRunReq("", "GET", "/api/ext/workflow-runs/"+runID, nil, runID, "")).
		Want(http.StatusOK).JSON(&run)
	return run
}

func TestExtWorkflowRunReads(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	issue, runID, wf := startExtRun(t)
	setExtWorkflowEngine(t, nil) // reads do not need the engine

	var byIssue struct {
		Runs   []ExtWorkflowRunSummaryResponse `json:"runs"`
		StepOf *ExtWorkflowStepOfResponse      `json:"step_of"`
	}
	testutil.Call(t, testHandler.ListExtWorkflowRunsForIssue, extRunReq("", "GET", "/api/ext/workflow-runs?issue_id="+issue.ID, nil, "", "")).
		Want(http.StatusOK).JSON(&byIssue)
	if len(byIssue.Runs) != 1 || byIssue.StepOf != nil {
		t.Fatalf("parent runs = %+v step_of = %+v", byIssue.Runs, byIssue.StepOf)
	}
	sum := byIssue.Runs[0]
	if sum.ID != runID || sum.WorkflowID != wf || sum.WorkflowName != t.Name() || sum.IssueIdentifier != issue.Identifier ||
		sum.IssueTitle != issue.Title || sum.Status != "running" || sum.MaxRewinds != 3 || sum.TriggeredByType != "member" ||
		sum.TriggeredByID != testUserID || sum.FinishedAt != nil {
		t.Fatalf("summary = %+v (issue %s)", sum, issue.Identifier)
	}

	run := getExtRun(t, runID)
	if run.ID != runID || len(run.Steps) != 1 || len(run.Events) == 0 {
		t.Fatalf("run = %+v", run)
	}
	step := run.Steps[0]
	if step.NodeKey != "build" || step.Title != "Title build" || step.Status != "running" || step.Attempts != 1 ||
		step.MaxAttempts != 3 || step.RequiresReview || len(step.DependsOn) != 0 || step.PendingReason != nil || step.StartedAt == nil {
		t.Fatalf("step = %+v", step)
	}
	if run.Events[0].Kind != "run_started" || run.Events[0].ActorType != "member" || string(run.Events[0].Payload) == "" {
		t.Fatalf("first event = %+v", run.Events[0])
	}

	testutil.Call(t, testHandler.ListExtWorkflowRunsForIssue, extRunReq("", "GET", "/api/ext/workflow-runs?issue_id="+step.IssueID, nil, "", "")).
		Want(http.StatusOK).JSON(&byIssue)
	if len(byIssue.Runs) != 0 || byIssue.StepOf == nil {
		t.Fatalf("child runs = %+v step_of = %+v", byIssue.Runs, byIssue.StepOf)
	}
	if got := *byIssue.StepOf; got.RunID != runID || got.StepID != step.ID || got.NodeKey != "build" || got.Index != 1 || got.Total != 1 || got.ParentIssueID != issue.ID {
		t.Fatalf("step_of = %+v", got)
	}

	var page struct {
		Runs  []ExtWorkflowRunSummaryResponse `json:"runs"`
		Total int                             `json:"total"`
	}
	testutil.Call(t, testHandler.ListExtWorkflowRunsForWorkflow, extWFReq("", "GET", "/api/ext/workflows/"+wf+"/runs?limit=10", nil, wf)).
		Want(http.StatusOK).JSON(&page)
	if page.Total != 1 || len(page.Runs) != 1 || page.Runs[0].ID != runID {
		t.Fatalf("workflow runs = %+v", page)
	}

	testutil.Call(t, testHandler.ListExtWorkflowRunsForIssue, extRunReq("", "GET", "/api/ext/workflow-runs", nil, "", "")).Want(http.StatusBadRequest)
	testutil.Call(t, testHandler.GetExtWorkflowRun, extRunReq("", "GET", "/api/ext/workflow-runs/"+issue.ID, nil, issue.ID, "")).Want(http.StatusNotFound)
	testutil.Call(t, testHandler.ListExtWorkflowRunsForWorkflow, extWFReq("", "GET", "/api/ext/workflows/"+wf+"/runs?limit=x", nil, wf)).Want(http.StatusBadRequest)
}

func TestExtWorkflowRunDecision(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	issue, runID, _ := startExtRun(t)
	failExtStep(t, engine, issue.ID)
	stepID := getExtRun(t, runID).Steps[0].ID
	decide := func(userID string, body map[string]any) *testutil.Response {
		path := "/api/ext/workflow-runs/" + runID + "/steps/" + stepID + "/decision"
		return testutil.Call(t, testHandler.DecideExtWorkflowStep, extRunReq(userID, "POST", path, body, runID, stepID))
	}

	plain := createPlainMember(t, "ext-wf-decide-plain@multica.test")
	decide(plain, map[string]any{"action": "retry", "expected_status": "awaiting_supervisor"}).Want(http.StatusForbidden)
	decide("", map[string]any{"action": "retry", "expected_status": "awaiting_human"}).Want(http.StatusConflict)
	decide("", map[string]any{"action": "redo", "expected_status": "awaiting_supervisor"}).Want(http.StatusUnprocessableEntity)
	decide("", map[string]any{"action": "escalate", "reason": "x", "expected_status": "awaiting_supervisor"}).Want(http.StatusUnprocessableEntity)
	testutil.Call(t, testHandler.DecideExtWorkflowStep, extRunReq("", "POST", "/x", map[string]any{"action": "retry"}, runID, runID)).Want(http.StatusNotFound)

	setExtWorkflowEngine(t, nil)
	resp := decide("", map[string]any{"action": "retry", "expected_status": "awaiting_supervisor"}).Want(http.StatusConflict)
	if !strings.Contains(resp.Text(), "workflow_engine_disabled") {
		t.Fatalf("engine off body = %s", resp.Text())
	}
	setExtWorkflowEngine(t, engine)

	var run ExtWorkflowRunResponse
	decide("", map[string]any{"action": "retry", "expected_status": "awaiting_supervisor"}).Want(http.StatusOK).JSON(&run)
	if run.ID != runID || run.Steps[0].Status != "running" || run.Steps[0].Attempts != 2 || run.Steps[0].PendingReason != nil {
		t.Fatalf("after retry: %+v", run.Steps[0])
	}
	last := run.Events[len(run.Events)-1]
	found := false
	for _, ev := range run.Events {
		found = found || (ev.Kind == "decision" && ev.ActorType == "member" && ev.ActorID != nil && *ev.ActorID == testUserID)
	}
	if !found {
		t.Fatalf("no member decision event; last = %+v", last)
	}
	decide("", map[string]any{"action": "retry", "expected_status": "awaiting_supervisor"}).Want(http.StatusConflict)
}

func TestExtWorkflowRunCancel(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	issue, runID, _ := startExtRun(t)
	cancel := func(userID string) *testutil.Response {
		return testutil.Call(t, testHandler.CancelExtWorkflowRun, extRunReq(userID, "POST", "/api/ext/workflow-runs/"+runID+"/cancel", nil, runID, ""))
	}

	plain := createPlainMember(t, "ext-wf-cancel-plain@multica.test")
	cancel(plain).Want(http.StatusForbidden)
	setExtWorkflowEngine(t, nil)
	cancel("").Want(http.StatusConflict)
	setExtWorkflowEngine(t, engine)

	cancel("").Want(http.StatusNoContent)
	if got := activeRunStatus(t, issue.ID); got != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", got)
	}
	cancel("").Want(http.StatusConflict)
	testutil.Call(t, testHandler.CancelExtWorkflowRun, extRunReq("", "POST", "/x", nil, issue.ID, "")).Want(http.StatusNotFound)
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/handler/ -run TestExtWorkflowRun -count=1'"
```

Expected: build failure, `undefined: ExtWorkflowRunResponse` and `testHandler.GetExtWorkflowRun undefined (type *Handler has no field or method GetExtWorkflowRun)`.

- [ ] **Step 3: Implement**

`server/internal/handler/ext_workflow_run.go`:

```go
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow: run endpoints (spec §7.1). Reads work with the engine off;
// decisions and cancellation need it (409 workflow_engine_disabled).

const (
	extWorkflowRunsDefaultLimit = 50
	extWorkflowRunsMaxLimit     = 200
)

// ── Response types ──────────────────────────────────────────────────────────

type ExtWorkflowRunSummaryResponse struct {
	ID              string  `json:"id"`
	WorkspaceID     string  `json:"workspace_id"`
	WorkflowID      string  `json:"workflow_id"`
	WorkflowName    string  `json:"workflow_name"`
	IssueID         string  `json:"issue_id"`
	IssueIdentifier string  `json:"issue_identifier"`
	IssueTitle      string  `json:"issue_title"`
	TriggeredByType string  `json:"triggered_by_type"`
	TriggeredByID   string  `json:"triggered_by_id"`
	Status          string  `json:"status"`
	RewindsUsed     int     `json:"rewinds_used"`
	MaxRewinds      int     `json:"max_rewinds"`
	StartedAt       string  `json:"started_at"`
	FinishedAt      *string `json:"finished_at"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

type ExtWorkflowStepResponse struct {
	ID               string   `json:"id"`
	NodeKey          string   `json:"node_key"`
	Title            string   `json:"title"`
	AgentID          string   `json:"agent_id"`
	IssueID          string   `json:"issue_id"`
	Status           string   `json:"status"`
	Attempts         int      `json:"attempts"`
	MaxAttempts      int      `json:"max_attempts"`
	RequiresReview   bool     `json:"requires_review"`
	DependsOn        []string `json:"depends_on"`
	PendingReason    *string  `json:"pending_reason"`
	LastFeedback     *string  `json:"last_feedback"`
	EscalationReason *string  `json:"escalation_reason"`
	StartedAt        *string  `json:"started_at"`
	FinishedAt       *string  `json:"finished_at"`
}

type ExtWorkflowRunEventResponse struct {
	ID         string          `json:"id"`
	StepID     *string         `json:"step_id"`
	Kind       string          `json:"kind"`
	ActorType  string          `json:"actor_type"`
	ActorID    *string         `json:"actor_id"`
	OnBehalfOf *string         `json:"on_behalf_of"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  string          `json:"created_at"`
}

type ExtWorkflowRunResponse struct {
	ExtWorkflowRunSummaryResponse
	Steps  []ExtWorkflowStepResponse     `json:"steps"`
	Events []ExtWorkflowRunEventResponse `json:"events"`
}

type ExtWorkflowStepOfResponse struct {
	RunID         string `json:"run_id"`
	StepID        string `json:"step_id"`
	NodeKey       string `json:"node_key"`
	Index         int    `json:"index"` // 1-based position in the definition
	Total         int    `json:"total"`
	ParentIssueID string `json:"parent_issue_id"`
}

// ── Converters ──────────────────────────────────────────────────────────────

func decodeExtRunDefinition(raw []byte) extworkflow.Definition {
	var def extworkflow.Definition
	if err := json.Unmarshal(raw, &def); err != nil {
		slog.Warn("ext-workflow: undecodable run definition", "error", err)
	}
	return def
}

// extRunSummaryToResponse converts a summary row. The by-issue and
// by-workflow list rows have the same fields and convert to this type.
func extRunSummaryToResponse(row db.GetExtWorkflowRunSummaryRow, prefix string) ExtWorkflowRunSummaryResponse {
	identifier := ""
	if row.IssueNumber.Valid && prefix != "" {
		identifier = prefix + "-" + strconv.Itoa(int(row.IssueNumber.Int32))
	}
	return ExtWorkflowRunSummaryResponse{
		ID:              uuidToString(row.ID),
		WorkspaceID:     uuidToString(row.WorkspaceID),
		WorkflowID:      uuidToString(row.WorkflowID),
		WorkflowName:    row.WorkflowName,
		IssueID:         uuidToString(row.IssueID),
		IssueIdentifier: identifier,
		IssueTitle:      row.IssueTitle.String,
		TriggeredByType: row.TriggeredByType,
		TriggeredByID:   uuidToString(row.TriggeredByID),
		Status:          row.Status,
		RewindsUsed:     int(row.RewindsUsed),
		MaxRewinds:      decodeExtRunDefinition(row.Definition).MaxRewinds,
		StartedAt:       timestampToString(row.StartedAt),
		FinishedAt:      timestampToPtr(row.FinishedAt),
		CreatedAt:       timestampToString(row.CreatedAt),
		UpdatedAt:       timestampToString(row.UpdatedAt),
	}
}

func extStepToResponse(s db.ExtWorkflowRunStep, def extworkflow.Definition) ExtWorkflowStepResponse {
	resp := ExtWorkflowStepResponse{
		ID:               uuidToString(s.ID),
		NodeKey:          s.NodeKey,
		Title:            s.NodeKey,
		AgentID:          uuidToString(s.AgentID),
		IssueID:          uuidToString(s.IssueID),
		Status:           s.Status,
		Attempts:         int(s.Attempts),
		MaxAttempts:      1,
		DependsOn:        []string{},
		PendingReason:    textToPtr(s.PendingReason),
		LastFeedback:     textToPtr(s.LastFeedback),
		EscalationReason: textToPtr(s.EscalationReason),
		StartedAt:        timestampToPtr(s.StartedAt),
		FinishedAt:       timestampToPtr(s.FinishedAt),
	}
	if n, ok := def.NodeByKey(s.NodeKey); ok {
		resp.Title = n.Title
		resp.MaxAttempts = n.MaxAttempts
		resp.RequiresReview = n.RequiresReview
		if n.DependsOn != nil {
			resp.DependsOn = n.DependsOn
		}
	}
	return resp
}

func extRunEventToResponse(ev db.ExtWorkflowRunEvent) ExtWorkflowRunEventResponse {
	payload := json.RawMessage(ev.Payload)
	if len(payload) == 0 || string(payload) == "null" {
		payload = json.RawMessage("{}")
	}
	return ExtWorkflowRunEventResponse{
		ID:         uuidToString(ev.ID),
		StepID:     uuidToPtr(ev.StepID),
		Kind:       ev.Kind,
		ActorType:  ev.ActorType,
		ActorID:    uuidToPtr(ev.ActorID),
		OnBehalfOf: uuidToPtr(ev.OnBehalfOf),
		Payload:    payload,
		CreatedAt:  timestampToString(ev.CreatedAt),
	}
}

// extRunResponse loads a run's summary, steps and events. found=false means
// the run is not in this workspace.
func (h *Handler) extRunResponse(ctx context.Context, wsUUID, runID pgtype.UUID) (ExtWorkflowRunResponse, bool, error) {
	row, err := h.Queries.GetExtWorkflowRunSummary(ctx, db.GetExtWorkflowRunSummaryParams{ID: runID, WorkspaceID: wsUUID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ExtWorkflowRunResponse{}, false, nil
	}
	if err != nil {
		return ExtWorkflowRunResponse{}, false, err
	}
	steps, err := h.Queries.ListExtWorkflowRunSteps(ctx, runID)
	if err != nil {
		return ExtWorkflowRunResponse{}, false, err
	}
	events, err := h.Queries.ListExtWorkflowRunEvents(ctx, runID)
	if err != nil {
		return ExtWorkflowRunResponse{}, false, err
	}
	def := decodeExtRunDefinition(row.Definition)
	resp := ExtWorkflowRunResponse{
		ExtWorkflowRunSummaryResponse: extRunSummaryToResponse(row, h.getIssuePrefix(ctx, wsUUID)),
		Steps:                         make([]ExtWorkflowStepResponse, 0, len(steps)),
		Events:                        make([]ExtWorkflowRunEventResponse, 0, len(events)),
	}
	for _, s := range steps {
		resp.Steps = append(resp.Steps, extStepToResponse(s, def))
	}
	for _, ev := range events {
		resp.Events = append(resp.Events, extRunEventToResponse(ev))
	}
	return resp, true, nil
}

// extRunStepOf locates the step whose child issue this is.
func (h *Handler) extRunStepOf(ctx context.Context, wsUUID, issueID pgtype.UUID) (*ExtWorkflowStepOfResponse, error) {
	step, err := h.Queries.GetExtWorkflowRunStepByIssue(ctx, db.GetExtWorkflowRunStepByIssueParams{IssueID: issueID, WorkspaceID: wsUUID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run, err := h.Queries.GetExtWorkflowRunInWorkspace(ctx, db.GetExtWorkflowRunInWorkspaceParams{ID: step.RunID, WorkspaceID: wsUUID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	def := decodeExtRunDefinition(run.Definition)
	index := 0
	for i, n := range def.Nodes {
		if n.Key == step.NodeKey {
			index = i + 1
			break
		}
	}
	return &ExtWorkflowStepOfResponse{
		RunID:         uuidToString(run.ID),
		StepID:        uuidToString(step.ID),
		NodeKey:       step.NodeKey,
		Index:         index,
		Total:         len(def.Nodes),
		ParentIssueID: uuidToString(run.IssueID),
	}, nil
}

// writeExtWorkflowEngineError maps engine errors to HTTP statuses.
func writeExtWorkflowEngineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, extworkflow.ErrEngineDisabled):
		writeError(w, http.StatusConflict, "workflow_engine_disabled")
	case errors.Is(err, extworkflow.ErrRunNotFound):
		writeError(w, http.StatusNotFound, "workflow run not found")
	case errors.Is(err, extworkflow.ErrStepNotFound):
		writeError(w, http.StatusNotFound, "workflow run step not found")
	case errors.Is(err, extworkflow.ErrForbidden):
		writeError(w, http.StatusForbidden, "only the member who started the run, the workflow's creator or a workspace admin can do this")
	case errors.Is(err, extworkflow.ErrStatusMismatch):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, extworkflow.ErrIllegalDecision):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		slog.Warn("ext-workflow: run action failed", "error", err)
		writeError(w, http.StatusInternalServerError, "workflow run action failed")
	}
}

// loadExtRunForMutation resolves the member, the workspace and the run id
// for cancel/decision. The engine must be on.
func (h *Handler) loadExtRunForMutation(w http.ResponseWriter, r *http.Request) (db.Member, pgtype.UUID, pgtype.UUID, bool) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return db.Member{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	if !h.ExtWorkflow.Enabled() {
		writeError(w, http.StatusConflict, "workflow_engine_disabled")
		return db.Member{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	runID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "run id")
	if !ok {
		return db.Member{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	if _, err := h.Queries.GetExtWorkflowRunInWorkspace(r.Context(), db.GetExtWorkflowRunInWorkspaceParams{ID: runID, WorkspaceID: member.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "workflow run not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to load workflow run")
		}
		return db.Member{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	return member, member.WorkspaceID, runID, true
}

// ── Handlers ────────────────────────────────────────────────────────────────

// ListExtWorkflowRunsForWorkflow: GET /api/ext/workflows/{id}/runs?limit&offset
func (h *Handler) ListExtWorkflowRunsForWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := h.loadExtWorkflowInWorkspace(w, r)
	if !ok {
		return
	}
	limit, offset := extWorkflowRunsDefaultLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = min(n, extWorkflowRunsMaxLimit)
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		offset = n
	}
	rows, err := h.Queries.ListExtWorkflowRunSummariesByWorkflow(r.Context(), db.ListExtWorkflowRunSummariesByWorkflowParams{
		WorkflowID: wf.ID, WorkspaceID: wf.WorkspaceID, PageLimit: int32(limit), PageOffset: int32(offset),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list workflow runs")
		return
	}
	total, err := h.Queries.CountExtWorkflowRunsByWorkflow(r.Context(), db.CountExtWorkflowRunsByWorkflowParams{WorkflowID: wf.ID, WorkspaceID: wf.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count workflow runs")
		return
	}
	prefix := h.getIssuePrefix(r.Context(), wf.WorkspaceID)
	runs := make([]ExtWorkflowRunSummaryResponse, 0, len(rows))
	for _, row := range rows {
		runs = append(runs, extRunSummaryToResponse(db.GetExtWorkflowRunSummaryRow(row), prefix))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "total": total})
}

// ListExtWorkflowRunsForIssue: GET /api/ext/workflow-runs?issue_id= — the
// runs of a parent issue, newest first, and step_of when the issue is a
// workflow step's child issue.
func (h *Handler) ListExtWorkflowRunsForIssue(w http.ResponseWriter, r *http.Request) {
	issueParam := r.URL.Query().Get("issue_id")
	if issueParam == "" {
		writeError(w, http.StatusBadRequest, "issue_id is required")
		return
	}
	issue, ok := h.loadIssueForUser(w, r, issueParam)
	if !ok {
		return
	}
	rows, err := h.Queries.ListExtWorkflowRunSummariesByIssue(r.Context(), db.ListExtWorkflowRunSummariesByIssueParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list workflow runs")
		return
	}
	prefix := h.getIssuePrefix(r.Context(), issue.WorkspaceID)
	runs := make([]ExtWorkflowRunSummaryResponse, 0, len(rows))
	for _, row := range rows {
		runs = append(runs, extRunSummaryToResponse(db.GetExtWorkflowRunSummaryRow(row), prefix))
	}
	stepOf, err := h.extRunStepOf(r.Context(), issue.WorkspaceID, issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workflow step")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "step_of": stepOf})
}

// GetExtWorkflowRun: GET /api/ext/workflow-runs/{id}
func (h *Handler) GetExtWorkflowRun(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceIDFromURL(r, "workspaceId"), "workspace_id")
	if !ok {
		return
	}
	runID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "run id")
	if !ok {
		return
	}
	resp, found, err := h.extRunResponse(r.Context(), wsUUID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workflow run")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "workflow run not found")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// CancelExtWorkflowRun: POST /api/ext/workflow-runs/{id}/cancel
func (h *Handler) CancelExtWorkflowRun(w http.ResponseWriter, r *http.Request) {
	member, _, runID, ok := h.loadExtRunForMutation(w, r)
	if !ok {
		return
	}
	if err := h.ExtWorkflow.CancelRun(r.Context(), runID, "member", member.UserID); err != nil {
		writeExtWorkflowEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type extWorkflowDecisionRequest struct {
	Action         string `json:"action"`
	To             string `json:"to"`
	Reason         string `json:"reason"`
	Feedback       string `json:"feedback"`
	ExpectedStatus string `json:"expected_status"`
}

// DecideExtWorkflowStep: POST /api/ext/workflow-runs/{id}/steps/{stepId}/decision
// answers the run after the decision.
func (h *Handler) DecideExtWorkflowStep(w http.ResponseWriter, r *http.Request) {
	member, wsUUID, runID, ok := h.loadExtRunForMutation(w, r)
	if !ok {
		return
	}
	stepID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "stepId"), "step id")
	if !ok {
		return
	}
	var req extWorkflowDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	err := h.ExtWorkflow.Decide(r.Context(), extworkflow.DecideInput{
		RunID:  runID,
		StepID: stepID,
		Decision: extworkflow.Decision{
			Action: extworkflow.DecisionAction(req.Action), To: req.To, Reason: req.Reason, Feedback: req.Feedback,
		},
		ActorType:      "member",
		ActorID:        member.UserID,
		ExpectedStatus: extworkflow.StepStatus(req.ExpectedStatus),
	})
	if err != nil {
		writeExtWorkflowEngineError(w, err)
		return
	}
	resp, found, err := h.extRunResponse(r.Context(), wsUUID, runID)
	if err != nil || !found {
		writeError(w, http.StatusInternalServerError, "failed to load workflow run")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
```

Register the routes inside `registerExtRoutes`:

`server/cmd/server/ext_routes.go`:

```diff
diff --git a/server/cmd/server/ext_routes.go b/server/cmd/server/ext_routes.go
--- a/server/cmd/server/ext_routes.go
+++ b/server/cmd/server/ext_routes.go
@@ -17,6 +17,15 @@ func registerExtRoutes(r chi.Router, h *handler.Handler) {
 			r.Get("/", h.GetExtWorkflow)
 			r.Put("/", h.UpdateExtWorkflow)
 			r.Delete("/", h.DeleteExtWorkflow)
+			r.Get("/runs", h.ListExtWorkflowRunsForWorkflow)
+		})
+	})
+	r.Route("/api/ext/workflow-runs", func(r chi.Router) {
+		r.Get("/", h.ListExtWorkflowRunsForIssue)
+		r.Route("/{id}", func(r chi.Router) {
+			r.Get("/", h.GetExtWorkflowRun)
+			r.Post("/cancel", h.CancelExtWorkflowRun)
+			r.Post("/steps/{stepId}/decision", h.DecideExtWorkflowStep)
 		})
 	})
 }
```

- [ ] **Step 4: Run, expect PASS**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/handler/ -run \"TestExtWorkflow\" -count=1 -v'"
make env-exec ARGS="-- bash -c 'cd server && go build ./... && go vet ./internal/handler/ ./cmd/server/'"
```

Expected: `--- PASS` for `TestExtWorkflowRunReads`, `TestExtWorkflowRunDecision` and `TestExtWorkflowRunCancel`, and the Part A/B `TestExtWorkflow*` tests still pass.

- [ ] **Step 5: Commit**

```bash
gofmt -l server/internal/handler/ext_workflow_run.go server/internal/handler/ext_workflow_run_test.go server/cmd/server/ext_routes.go
git add server/internal/handler/ext_workflow_run.go server/internal/handler/ext_workflow_run_test.go server/cmd/server/ext_routes.go
git commit -m "feat(ext-workflow): run read, cancel and decision endpoints

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task C3: Comment decision protocol (`Engine.OnComment`)

**Files:**
- Create: `server/internal/extworkflow/protocol.go`
- Test: `server/internal/extworkflow/protocol_test.go`

**Interfaces:**
- Consumes:
  - C1: `Decide`, `DecideInput`, `ErrStepNotFound`, `MemberCanDecide` (through `Decide`).
  - Part B: `ParseBlock`, `ContainsBlock`, the `ErrBlock*` sentinels, `LoadRun`, `RunSnapshot.Active`, the queries `GetComment`, `GetExtWorkflowRunStepByIssue`, `GetActiveExtWorkflowRunByIssue`, `CreateExtWorkflowRunEvent`, `CreateComment` and `TouchExtWorkflowRun`, plus `TaskService.ExtPublishSystemComment` and `protocol.EventExtWorkflowRunUpdated`.
- Produces:
  - `Engine.OnComment(ctx, commentID) error` and `ProtocolErrorReason`.
  - Test helpers: `block(lines...)`, `e.agentSays(t, issue, agent, task, content)`, `e.protocolErrors(t, run, comment)`.

Authorization follows the table in spec §6.3:
- **A supervisor task (review, failure or rewind_request) focused on step S**, commenting on S's child issue, may use every action except `request-rewind`.
- **A conversation task**, commenting on the parent, may use the human actions and must name `step:` (except for `abort`). Its permission is checked against the commenter. C4 wires the wake and tests this case.
- **A step task of S**, commenting on S's child issue, may only use `request-rewind`.
- **A run participant posting anywhere else** gets a `protocol_error` that says where to post.
- **Anyone else is ignored.**

- [ ] **Step 1: Write the failing test**

`server/internal/extworkflow/protocol_test.go`:

````go
package extworkflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func block(lines ...string) string {
	return "Decision:\n\n```ext-workflow\n" + strings.Join(lines, "\n") + "\n```\n"
}

// agentSays posts an agent comment (stamped with the authoring task, as the
// CLI does) and delivers it to the engine like the comment:created listener.
func (e *env) agentSays(t *testing.T, issueID, agentID pgtype.UUID, task *db.AgentTaskQueue, content string) pgtype.UUID {
	t.Helper()
	cols := testutil.Cols{"author_type": "agent", "author_id": agentID}
	if task != nil {
		cols["source_task_id"] = task.ID
	}
	id := util.MustParseUUID(e.fx.Comment(t, util.UUIDToString(issueID), content, cols))
	if err := e.engine.OnComment(context.Background(), id); err != nil {
		t.Fatalf("OnComment: %v", err)
	}
	return id
}

// protocolErrors counts protocol_error events and the system replies under
// the given comment.
func (e *env) protocolErrors(t *testing.T, run db.ExtWorkflowRun, comment pgtype.UUID) (events, replies int) {
	t.Helper()
	events = e.fx.Count(t, `SELECT count(*) FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'protocol_error' AND payload->>'comment_id' = $2`, run.ID, util.UUIDToString(comment))
	replies = e.fx.Count(t, `SELECT count(*) FROM comment WHERE parent_id = $1 AND author_type = 'system' AND content LIKE '%did not apply%'`, comment)
	return events, replies
}

func TestOnCommentSupervisorApprovesOnTheChild(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)
	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	if review.ExtWorkflowKind.String != KindReview {
		t.Fatalf("supervisor task kind = %q", review.ExtWorkflowKind.String)
	}
	e.agentSays(t, build.IssueID, supervisor, &review, block("action: approve", "reason: looks right"))

	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	var actorType string
	var actorID, stepID pgtype.UUID
	e.fx.QueryRow(t, `SELECT actor_type, actor_id, step_id FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'decision'`, run.ID).Scan(&actorType, &actorID, &stepID)
	if actorType != "agent" || actorID != supervisor || stepID != build.ID {
		t.Fatalf("decision by %s %v on %v", actorType, actorID, stepID)
	}
	if got := e.latestTask(t, build, RoleSupervisor).Status; got != "running" {
		t.Fatalf("deciding task = %s, want it left running", got)
	}
}

func TestOnCommentStepAgentRequestsRewind(t *testing.T) {
	e := newEnv(t)
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	e.childMoves(t, parent, spec.IssueID, "done")
	build := e.step(t, run, "build")
	buildTask := e.running(t, e.latestTask(t, build, RoleStep))

	e.agentSays(t, build.IssueID, coder, &buildTask, block("action: request-rewind", "to: spec", "reason: The spec assumes a paginated API."))

	build = e.step(t, run, "build")
	if StepStatus(build.Status) != StepAwaitingSupervisor || build.PendingReason.String != string(PendingRewindRequest) {
		t.Fatalf("build = %s/%s, want awaiting_supervisor/rewind_request", build.Status, build.PendingReason.String)
	}
	sup := e.latestTask(t, build, RoleSupervisor)
	if sup.ExtWorkflowKind.String != KindRewindRequest || sup.IssueID != parent {
		t.Fatalf("supervisor task kind=%q issue=%v", sup.ExtWorkflowKind.String, sup.IssueID)
	}
}

func TestOnCommentRejectsInvalidBlocks(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)
	review := e.running(t, e.latestTask(t, build, RoleSupervisor))
	for name, tc := range map[string]struct {
		issue   pgtype.UUID
		content string
		want    string
	}{
		"request-rewind from the supervisor": {build.IssueID, block("action: request-rewind", "reason: x"), "for step agents"},
		"unknown field":                      {build.IssueID, block("action: approve", "colour: red"), "not valid"},
		"two blocks":                         {build.IssueID, block("action: approve") + block("action: skip"), "only one"},
		"missing feedback":                   {build.IssueID, block("action: redo"), "needs feedback"},
		"decision on the parent":             {run.IssueID, block("action: approve", "step: build"), "child issue of step"},
	} {
		comment := e.agentSays(t, tc.issue, supervisor, &review, tc.content)
		events, replies := e.protocolErrors(t, run, comment)
		if events != 1 || replies != 1 {
			t.Errorf("%s: protocol_error events=%d replies=%d, want 1/1", name, events, replies)
		}
		var reply string
		e.fx.QueryRow(t, `SELECT content FROM comment WHERE parent_id = $1`, comment).Scan(&reply)
		if !strings.Contains(reply, tc.want) {
			t.Errorf("%s: reply %q does not mention %q", name, reply, tc.want)
		}
	}
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)
}

func TestOnCommentStepAgentMayOnlyRequestRewind(t *testing.T) {
	e := newEnv(t)
	supervisor, coder := e.agent(t, "Supervisor"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3, wfNode{key: "build", title: "Build", agent: coder})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	build := e.step(t, run, "build")
	task := e.running(t, e.latestTask(t, build, RoleStep))

	comment := e.agentSays(t, build.IssueID, coder, &task, block("action: approve"))
	if events, replies := e.protocolErrors(t, run, comment); events != 1 || replies != 1 {
		t.Fatalf("protocol_error events=%d replies=%d, want 1/1", events, replies)
	}
	wantStepRow(t, e.step(t, run, "build"), StepRunning, 1)
}

func TestOnCommentIgnoresNonParticipants(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)
	stranger := e.agent(t, "Stranger")
	review := e.latestTask(t, build, RoleSupervisor)
	cases := map[string]pgtype.UUID{}

	// A person's comment, an agent without a task in the run, a comment with
	// no block, and a comment stamped with an unrelated task.
	cases["member"] = util.MustParseUUID(e.fx.Comment(t, util.UUIDToString(build.IssueID), block("action: approve")))
	cases["stranger"] = e.agentSays(t, build.IssueID, stranger, nil, block("action: approve"))
	cases["no block"] = e.agentSays(t, build.IssueID, supervisor, &review, "Looks fine to me.")
	other := review
	other.ID = util.MustParseUUID(e.fx.Task(t, util.UUIDToString(supervisor), testutil.Cols{"status": "completed", "runtime_id": e.runtime}))
	cases["foreign source task"] = e.agentSays(t, build.IssueID, supervisor, &other, block("action: approve"))
	if err := e.engine.OnComment(context.Background(), cases["member"]); err != nil {
		t.Fatalf("OnComment(member): %v", err)
	}

	for name, comment := range cases {
		if events, replies := e.protocolErrors(t, run, comment); events != 0 || replies != 0 {
			t.Errorf("%s: protocol_error events=%d replies=%d, want none", name, events, replies)
		}
	}
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)
}
````

- [ ] **Step 2: Run, expect FAIL**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -run TestOnComment -count=1'"
```

Expected: build failure, `e.engine.OnComment undefined (type *Engine has no field or method OnComment)`.

- [ ] **Step 3: Implement**

`server/internal/extworkflow/protocol.go`:

```go
package extworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The comment decision protocol (spec §6.3). Agents decide by posting one
// ext-workflow block with the existing CLI. A comment names no workflow task,
// so the engine identifies the deciding task from the comment's source task
// (stamped by the CLI's X-Task-ID) or, failing that, from the author's
// in-flight tasks in the run, and authorizes by role:
//
//	supervisor task focused on step S → on S's child issue: every action but request-rewind
//	supervisor conversation task      → on the parent, naming step: every human action, for the commenter
//	step task of step S               → on S's child issue: request-rewind only
//
// Other authors are ignored. A run participant whose block is invalid gets a
// protocol_error event and a system reply under its comment.

// ProtocolErrorReason is the protocol_error payload reason for a rejected block.
const ProtocolErrorReason = "invalid_block"

// OnComment handles comment:created for an agent comment that carries an
// ext-workflow block.
func (e *Engine) OnComment(ctx context.Context, commentID pgtype.UUID) error {
	if !e.Enabled() {
		return nil
	}
	c, err := e.q.GetComment(ctx, commentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load comment: %w", err)
	}
	if c.AuthorType != "agent" || c.DeletedAt.Valid || !ContainsBlock(c.Content) {
		return nil
	}
	call, ok, err := e.protocolCall(ctx, c)
	if err != nil || !ok {
		return err
	}
	d, _, perr := ParseBlock(c.Content)
	if perr == nil {
		var in DecideInput
		if in, perr = e.decideInput(ctx, call, d); perr == nil {
			perr = e.Decide(ctx, in)
		}
	}
	switch {
	case perr == nil, errors.Is(perr, ErrEngineDisabled):
		return nil
	case isProtocolError(perr):
		return e.rejectBlock(ctx, call, d, perr)
	}
	return perr
}

// protocolCall is a block posted by a run participant.
type protocolCall struct {
	snap    *RunSnapshot
	comment db.Comment
	task    db.AgentTaskQueue // the deciding task
	// childStep is the step whose child issue carries the comment; "" when
	// the comment is on the parent issue.
	childStep string
}

// protocolCall finds the run the commented issue belongs to and the author's
// deciding task. ok=false: not a run participant, ignore the comment.
func (e *Engine) protocolCall(ctx context.Context, c db.Comment) (protocolCall, bool, error) {
	call := protocolCall{comment: c}
	var runID pgtype.UUID
	step, err := e.q.GetExtWorkflowRunStepByIssue(ctx, db.GetExtWorkflowRunStepByIssueParams{IssueID: c.IssueID, WorkspaceID: c.WorkspaceID})
	switch {
	case err == nil:
		runID, call.childStep = step.RunID, step.NodeKey
	case errors.Is(err, pgx.ErrNoRows):
		run, err := e.q.GetActiveExtWorkflowRunByIssue(ctx, c.IssueID)
		if errors.Is(err, pgx.ErrNoRows) {
			return call, false, nil
		}
		if err != nil {
			return call, false, fmt.Errorf("load active run: %w", err)
		}
		runID = run.ID
	default:
		return call, false, fmt.Errorf("load step for issue: %w", err)
	}
	snap, err := e.LoadRun(ctx, runID)
	if errors.Is(err, ErrRunNotFound) {
		return call, false, nil
	}
	if err != nil {
		return call, false, err
	}
	call.snap = snap
	task, ok := pickDecidingTask(snap, c, call.childStep)
	if !ok {
		return call, false, nil
	}
	call.task = task
	return call, true, nil
}

// pickDecidingTask chooses among the author's in-flight tasks of the run: the
// comment's source task when it carries one, else the task whose role fits
// the issue the comment is on.
func pickDecidingTask(snap *RunSnapshot, c db.Comment, childStep string) (db.AgentTaskQueue, bool) {
	var mine []db.AgentTaskQueue
	for _, t := range snap.Active {
		if t.AgentID != c.AuthorID || (c.SourceTaskID.Valid && t.ID != c.SourceTaskID) {
			continue
		}
		mine = append(mine, t)
	}
	if len(mine) == 0 {
		return db.AgentTaskQueue{}, false
	}
	var prefer []func(db.AgentTaskQueue) bool
	if childStep != "" {
		stepID := snap.ByKey[childStep].ID
		prefer = append(prefer,
			func(t db.AgentTaskQueue) bool { return isFocusSupervisorTask(t) && t.ExtWorkflowStepID == stepID },
			func(t db.AgentTaskQueue) bool {
				return t.ExtWorkflowRole.String == RoleStep && t.ExtWorkflowStepID == stepID
			})
	} else {
		prefer = append(prefer, func(t db.AgentTaskQueue) bool { return t.ExtWorkflowKind.String == KindConversation })
	}
	for _, match := range prefer {
		for _, t := range mine {
			if match(t) {
				return t, true
			}
		}
	}
	return mine[0], true
}

func isFocusSupervisorTask(t db.AgentTaskQueue) bool {
	if t.ExtWorkflowRole.String != RoleSupervisor {
		return false
	}
	switch t.ExtWorkflowKind.String {
	case KindReview, KindFailure, KindRewindRequest:
		return true
	}
	return false
}

// decideInput authorizes the block for the deciding task's role (the table
// in spec §6.3) and turns it into a decision.
func (e *Engine) decideInput(ctx context.Context, call protocolCall, d Decision) (DecideInput, error) {
	snap, t := call.snap, call.task
	in := DecideInput{RunID: snap.Run.ID, Decision: d, ActorType: "agent", ActorID: call.comment.AuthorID, TaskID: t.ID}
	illegal := func(format string, args ...any) (DecideInput, error) {
		return in, fmt.Errorf("%w: "+format, append([]any{ErrIllegalDecision}, args...)...)
	}
	stepTitle := func(key string) string {
		if n, ok := snap.Def.NodeByKey(key); ok {
			return fmt.Sprintf("%q (%s)", n.Title, key)
		}
		return fmt.Sprintf("%q", key)
	}
	focus, _ := snap.KeyOf(t.ExtWorkflowStepID)
	key := ""
	switch {
	case call.childStep != "" && t.ExtWorkflowRole.String == RoleStep && focus == call.childStep:
		if d.Action != ActionRequestRewind {
			return illegal("a step agent may only post request-rewind")
		}
		if d.Step != "" && d.Step != call.childStep {
			return illegal("this is the child issue of step %s, not %q", stepTitle(call.childStep), d.Step)
		}
		key = call.childStep
	case call.childStep != "" && isFocusSupervisorTask(t) && focus == call.childStep:
		if d.Action == ActionRequestRewind {
			return illegal("request-rewind is for step agents; the supervisor decides with rewind")
		}
		if d.Step != "" && d.Step != call.childStep {
			return illegal("this is the child issue of step %s, not %q", stepTitle(call.childStep), d.Step)
		}
		key = call.childStep
	case call.childStep == "" && t.ExtWorkflowKind.String == KindConversation:
		if d.Action == ActionRequestRewind {
			return illegal("request-rewind is for step agents")
		}
		if d.Step == "" && d.Action != ActionAbort {
			return illegal("a decision on the parent issue must name the step with `step:`")
		}
		if d.Step != "" {
			if _, ok := snap.ByKey[d.Step]; !ok {
				return illegal("unknown step %q", d.Step)
			}
		}
		commenter, err := e.conversationCommenter(ctx, t)
		if err != nil {
			return in, err
		}
		in.OnBehalfOf = commenter
		key = d.Step
	case t.ExtWorkflowKind.String == KindSummary:
		return illegal("the summary takes no decision block; write a plain comment on the parent issue")
	case t.ExtWorkflowRole.String == RoleStep:
		return illegal("post request-rewind on the child issue of your own step %s", stepTitle(focus))
	case isFocusSupervisorTask(t):
		return illegal("post the decision on the child issue of step %s", stepTitle(focus))
	default:
		return illegal("this turn cannot decide here")
	}
	if key != "" {
		in.StepID = snap.ByKey[key].ID
	}
	return in, nil
}

// conversationCommenter is the member whose comment woke a conversation
// turn; the supervisor acts on that member's behalf.
func (e *Engine) conversationCommenter(ctx context.Context, t db.AgentTaskQueue) (pgtype.UUID, error) {
	if !t.TriggerCommentID.Valid {
		return pgtype.UUID{}, fmt.Errorf("%w: the conversation has no triggering comment", ErrForbidden)
	}
	trigger, err := e.q.GetComment(ctx, t.TriggerCommentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, fmt.Errorf("%w: the triggering comment is gone", ErrForbidden)
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("load triggering comment: %w", err)
	}
	if trigger.AuthorType != "member" {
		return pgtype.UUID{}, fmt.Errorf("%w: the conversation was not started by a person", ErrForbidden)
	}
	return trigger.AuthorID, nil
}

func isProtocolError(err error) bool {
	for _, target := range []error{
		ErrIllegalDecision, ErrStatusMismatch, ErrForbidden, ErrStepNotFound, ErrRunNotFound,
		ErrBlockMultiple, ErrBlockUnclosed, ErrBlockEmpty, ErrBlockInvalid,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// rejectBlock records a protocol_error and replies under the comment.
func (e *Engine) rejectBlock(ctx context.Context, call protocolCall, d Decision, cause error) error {
	c, snap := call.comment, call.snap
	issue, err := e.q.GetIssue(ctx, c.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load issue: %w", err)
	}
	stepKey := call.childStep
	if stepKey == "" {
		stepKey = d.Step
	}
	payload := map[string]any{
		"reason": ProtocolErrorReason, "error": cause.Error(),
		"comment_id": util.UUIDToString(c.ID), "issue_id": util.UUIDToString(c.IssueID),
		"task_id": util.UUIDToString(call.task.ID),
	}
	if d.Action != "" {
		payload["action"] = string(d.Action)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode protocol error: %w", err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := e.q.WithTx(tx)
	if _, err := q.CreateExtWorkflowRunEvent(ctx, db.CreateExtWorkflowRunEventParams{
		ID: dbid.NewV7(), RunID: snap.Run.ID, WorkspaceID: snap.Run.WorkspaceID, StepID: snap.ByKey[stepKey].ID,
		Kind: RunEventProtocolError, ActorType: "agent", ActorID: c.AuthorID, Payload: raw,
	}); err != nil {
		return fmt.Errorf("record protocol error: %w", err)
	}
	reply, err := q.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "system", ParentID: c.ID,
		Content: protocolErrorText(cause),
	})
	if err != nil {
		return fmt.Errorf("create protocol error reply: %w", err)
	}
	if err := q.TouchExtWorkflowRun(ctx, snap.Run.ID); err != nil {
		return fmt.Errorf("touch run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	e.tasks.ExtPublishSystemComment(issue, reply)
	if e.pub != nil {
		e.pub.Publish(protocol.EventExtWorkflowRunUpdated, util.UUIDToString(snap.Run.WorkspaceID), "system", "", map[string]any{
			"run_id": util.UUIDToString(snap.Run.ID), "issue_id": util.UUIDToString(snap.Run.IssueID), "workflow_id": util.UUIDToString(snap.Run.WorkflowID),
		})
	}
	return nil
}

func protocolErrorText(cause error) string {
	return fmt.Sprintf("The workflow did not apply the `%s` block above: %s.\n\nNothing changed. Fix the block and post it again in a new comment.", BlockLang, cause.Error())
}
```

- [ ] **Step 4: Run, expect PASS**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -run TestOnComment -count=1 -v'"
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -count=1'"
```

Expected: `--- PASS` for these five tests, and the package is `ok`:
- `TestOnCommentSupervisorApprovesOnTheChild`
- `TestOnCommentStepAgentRequestsRewind`
- `TestOnCommentRejectsInvalidBlocks`
- `TestOnCommentStepAgentMayOnlyRequestRewind`
- `TestOnCommentIgnoresNonParticipants`

- [ ] **Step 5: Commit**

```bash
gofmt -l server/internal/extworkflow
git add server/internal/extworkflow/protocol.go server/internal/extworkflow/protocol_test.go
git commit -m "feat(ext-workflow): apply agent decision blocks from comments

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task C4: Conversation wake (`Engine.OnMemberParentComment`)

**Files:**
- Create: `server/pkg/db/queries/ext_workflow_protocol.sql`
- Regenerate: `server/pkg/db/generated/ext_workflow_protocol.sql.go` (`make sqlc`)
- Create: `server/internal/extworkflow/conversation.go`
- Test: `server/internal/extworkflow/conversation_test.go`

**Interfaces:**
- Consumes:
  - Part B: `TaskService.EnqueueExtWorkflowTask` with `Kind: "conversation"`, `TriggerCommentID` and `ActorUserID`, plus `PublishExtWorkflowTaskQueued`, `service.ErrExtAgentUnavailable`, `service.ErrExtTaskSlotBusy`, and `AgentAccess.CanInvokeAgent`.
  - C3: the conversation branch of `decideInput`, and `conversationCommenter`.
- Produces:
  - `Engine.OnMemberParentComment(ctx, issueID, commentID, memberID) error`.
  - Query `HasExtWorkflowConversationTaskForComment(commentID) bool`.
  - Test helpers `e.memberSays` and `e.conversationTask`.

**How a later decision block is attributed.** The conversation task stores the member comment as its `trigger_comment_id`. When the supervisor posts a block during that turn, `OnComment` loads that comment's author and passes it as `OnBehalfOf`. `Decide` then checks `MemberCanDecide` against that member, not against the supervisor.

- [ ] **Step 1: Write the failing test**

`server/internal/extworkflow/conversation_test.go`:

```go
package extworkflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// memberSays posts a person's comment on the parent and delivers it to the
// conversation hook, as the comment handler's assignee fallback does.
func (e *env) memberSays(t *testing.T, parent, member pgtype.UUID, content string) pgtype.UUID {
	t.Helper()
	id := util.MustParseUUID(e.fx.Comment(t, util.UUIDToString(parent), content, testutil.Cols{"author_id": member}))
	if err := e.engine.OnMemberParentComment(context.Background(), parent, id, member); err != nil {
		t.Fatalf("OnMemberParentComment: %v", err)
	}
	return id
}

func (e *env) conversationTask(t *testing.T, comment pgtype.UUID) (db.AgentTaskQueue, bool) {
	t.Helper()
	var id pgtype.UUID
	if e.fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1 AND ext_workflow_kind = 'conversation'`, comment) == 0 {
		return db.AgentTaskQueue{}, false
	}
	e.fx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE trigger_comment_id = $1 AND ext_workflow_kind = 'conversation'`, comment).Scan(&id)
	task, err := e.q.GetAgentTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task, true
}

func TestMemberCommentWakesTheSupervisorOnce(t *testing.T) {
	e := newEnv(t)
	run, build, supervisor := e.reviewRun(t, e.user)
	comment := e.memberSays(t, run.IssueID, e.user, "Ship it, the review comments are addressed.")

	task, ok := e.conversationTask(t, comment)
	if !ok {
		t.Fatal("no conversation task")
	}
	if task.AgentID != supervisor || task.IssueID != run.IssueID || task.ExtWorkflowRole.String != RoleSupervisor ||
		task.ExtWorkflowRunID != run.ID || task.ExtWorkflowStepID.Valid || task.Status != "queued" {
		t.Fatalf("conversation task = %+v", task)
	}
	if err := e.engine.OnMemberParentComment(context.Background(), run.IssueID, comment, e.user); err != nil {
		t.Fatal(err)
	}
	if n := e.fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, comment); n != 1 {
		t.Fatalf("conversation tasks = %d, want 1", n)
	}
	// The review the step waits for is untouched.
	if got := e.latestTask(t, build, RoleSupervisor); got.ExtWorkflowKind.String != KindReview || got.Status != "queued" {
		t.Fatalf("review task = %s/%s", got.ExtWorkflowKind.String, got.Status)
	}
}

func TestMemberCommentNeedsAccessToTheSupervisor(t *testing.T) {
	e := newEnv(t)
	run, _, supervisor := e.reviewRun(t, e.user)
	e.access.deny(supervisor)
	comment := e.memberSays(t, run.IssueID, e.user, "Anyone there?")
	if _, ok := e.conversationTask(t, comment); ok {
		t.Fatal("a member without access to the supervisor woke it")
	}
}

func TestConversationActsOnBehalfOfAPermittedMember(t *testing.T) {
	e := newEnv(t)
	run, _, supervisor := e.reviewRun(t, e.user)
	comment := e.memberSays(t, run.IssueID, e.user, "Approve the build step please.")
	task, _ := e.conversationTask(t, comment)
	task = e.running(t, task)

	missing := e.agentSays(t, run.IssueID, supervisor, &task, block("action: approve"))
	if events, replies := e.protocolErrors(t, run, missing); events != 1 || replies != 1 {
		t.Fatalf("block without step: events=%d replies=%d, want 1/1", events, replies)
	}

	e.agentSays(t, run.IssueID, supervisor, &task, block("action: approve", "step: build"))
	wantStepRow(t, e.step(t, run, "build"), StepDone, 1)
	var onBehalf, actor pgtype.UUID
	e.fx.QueryRow(t, `SELECT on_behalf_of, actor_id FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'decision'`, run.ID).Scan(&onBehalf, &actor)
	if onBehalf != e.user || actor != supervisor {
		t.Fatalf("decision by %v on behalf of %v", actor, onBehalf)
	}
}

func TestConversationRefusesAnUnpermittedMember(t *testing.T) {
	e := newEnv(t)
	plain := e.member(t, "member")
	run, _, supervisor := e.reviewRun(t, e.user)
	comment := e.memberSays(t, run.IssueID, plain, "Just approve it.")
	task, ok := e.conversationTask(t, comment)
	if !ok {
		t.Fatal("a member who may invoke the supervisor gets a conversation turn")
	}
	task = e.running(t, task)

	refused := e.agentSays(t, run.IssueID, supervisor, &task, block("action: approve", "step: build"))
	if events, replies := e.protocolErrors(t, run, refused); events != 1 || replies != 1 {
		t.Fatalf("refused decision: events=%d replies=%d, want 1/1", events, replies)
	}
	wantStepRow(t, e.step(t, run, "build"), StepAwaitingSupervisor, 1)
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -run \"Conversation|MemberComment\" -count=1'"
```

Expected: build failure, `e.engine.OnMemberParentComment undefined (type *Engine has no field or method OnMemberParentComment)`.

- [ ] **Step 3: Implement**

Create the query file. C6 appends a second query to it.

`server/pkg/db/queries/ext_workflow_protocol.sql`:

```sql
-- ext-workflow: queries of the comment protocol and the claim-time briefing.

-- name: HasExtWorkflowConversationTaskForComment :one
-- A person's comment wakes the supervisor at most once: the comment
-- re-evaluation paths (cancelled batches, end-of-run replays) reach the
-- conversation hook again for the same comment.
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue
    WHERE trigger_comment_id = @comment_id AND ext_workflow_kind = 'conversation'
) AS woken;
```

Regenerate:

```bash
make sqlc
```

Expected: a new `server/pkg/db/generated/ext_workflow_protocol.sql.go` with `func (q *Queries) HasExtWorkflowConversationTaskForComment(ctx context.Context, commentID pgtype.UUID) (bool, error)`.

`server/internal/extworkflow/conversation.go`:

```go
package extworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// OnMemberParentComment wakes the supervisor with a conversation turn when a
// person comments on a workflow parent without mentioning anyone (spec §6.4).
// As with any comment that wakes an agent, the commenter must be allowed to
// invoke the supervisor. A comment wakes at most one conversation turn.
//
// The turn's task carries the comment as its trigger: a decision block the
// supervisor posts during the turn is attributed to that comment's author
// (OnComment → conversationCommenter), and permission is checked against
// them, not the supervisor.
func (e *Engine) OnMemberParentComment(ctx context.Context, issueID, commentID, memberID pgtype.UUID) error {
	if !e.Enabled() || !commentID.Valid || !memberID.Valid {
		return nil
	}
	run, err := e.q.GetActiveExtWorkflowRunByIssue(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load active run: %w", err)
	}
	woken, err := e.q.HasExtWorkflowConversationTaskForComment(ctx, commentID)
	if err != nil {
		return fmt.Errorf("check conversation task: %w", err)
	}
	if woken {
		return nil
	}
	var def Definition
	if err := json.Unmarshal(run.Definition, &def); err != nil {
		return fmt.Errorf("decode run definition: %w", err)
	}
	supervisor, err := util.ParseUUID(def.SupervisorAgentID)
	if err != nil {
		return nil
	}
	if e.access != nil {
		ok, err := e.access.CanInvokeAgent(ctx, run.WorkspaceID, "member", memberID, supervisor)
		if err != nil {
			return fmt.Errorf("check supervisor access: %w", err)
		}
		if !ok {
			return nil
		}
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	task, err := e.tasks.EnqueueExtWorkflowTask(ctx, tx, service.ExtWorkflowTaskParams{
		IssueID: issueID, AgentID: supervisor, RunID: run.ID, Role: RoleSupervisor, Kind: KindConversation,
		TriggerCommentID: commentID, ActorUserID: memberID,
		HandoffNote: "Workflow supervisor: a person commented on this workflow issue. Your instructions carry the run briefing.",
	})
	if errors.Is(err, service.ErrExtAgentUnavailable) || errors.Is(err, service.ErrExtTaskSlotBusy) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("enqueue conversation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	e.tasks.PublishExtWorkflowTaskQueued(ctx, task)
	return nil
}
```

- [ ] **Step 4: Run, expect PASS**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -run \"Conversation|MemberComment\" -count=1 -v'"
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -count=1'"
```

Expected: `--- PASS` for these four tests, and the package is `ok`:
- `TestMemberCommentWakesTheSupervisorOnce`
- `TestMemberCommentNeedsAccessToTheSupervisor`
- `TestConversationActsOnBehalfOfAPermittedMember`
- `TestConversationRefusesAnUnpermittedMember`

- [ ] **Step 5: Commit**

```bash
gofmt -l server/internal/extworkflow
git add server/pkg/db/queries/ext_workflow_protocol.sql server/pkg/db/generated/ext_workflow_protocol.sql.go server/internal/extworkflow/conversation.go server/internal/extworkflow/conversation_test.go
git commit -m "feat(ext-workflow): wake the supervisor for comments on a workflow issue

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task C5: Comment hooks (`isNoteComment`, assignee fallback, `comment:created` listener)

**Files:**
- Create: `server/internal/handler/ext_workflow_comment.go`
- Modify: `server/internal/handler/comment.go`
  - `isNoteComment` (~:2020): add the block check;
  - `triggerTasksForComment` (~:2041): mark the trigger pass;
  - `routeAssigneeFallback` (~:3063): add `case "workflow"`.
- Create: `server/cmd/server/ext_workflow_comments.go`
- Modify: `server/cmd/server/ext_workflow_wiring.go` (`setupExtWorkflow`: register the comment listener next to B's task listeners, so it exists only when the kill switch leaves the engine on)
- Test: `server/internal/handler/ext_workflow_comment_test.go`, `server/cmd/server/ext_workflow_comments_test.go`

**Interfaces:**
- Consumes:
  - C3: `OnComment`.
  - C4: `OnMemberParentComment`.
  - Part B: `ContainsBlock`, `setupExtWorkflow`, `registerExtWorkflowListeners`.
  - C2 test helpers: `startExtRun`, `getExtRun`.
- Produces:
  - `handler.ExtCommentFromEvent(events.Event) (commentID pgtype.UUID, authorType string, ok bool)`.
  - Unexported: `isExtWorkflowBlockComment`, `withExtWorkflowCommentTrigger`, `(*Handler).extWorkflowParentComment`, and `registerExtWorkflowCommentListener` (in `cmd/server`).
  - Test helpers: `postComment`, `conversationTasks`.

- [ ] **Step 1: Write the failing tests**

`server/internal/handler/ext_workflow_comment_test.go`:

````go
package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

const extWFApproveBlock = "Approving.\n\n```ext-workflow\naction: approve\n```\n"

func TestIsNoteCommentTreatsWorkflowBlocksAsNotes(t *testing.T) {
	for content, want := range map[string]bool{
		extWFApproveBlock:                  true,
		"```ext-workflow\naction: approve": true, // unclosed still counts: it is protocol traffic
		"/note remember this":              true,
		"please look at this":              false,
		"```yaml\naction: approve\n```\n":  false,
	} {
		if got := isNoteComment(content); got != want {
			t.Errorf("isNoteComment(%q) = %v, want %v", content, got, want)
		}
	}
}

// postComment creates a comment through the handler as userID ("" = owner).
func postComment(t *testing.T, userID, issueID string, body map[string]any) CommentResponse {
	t.Helper()
	var req *http.Request
	if userID == "" {
		req = newRequest("POST", "/api/issues/"+issueID+"/comments", body)
	} else {
		req = newRequestAs(userID, "POST", "/api/issues/"+issueID+"/comments", body)
	}
	var c CommentResponse
	testutil.Call(t, testHandler.CreateComment, withURLParam(req, "id", issueID)).Want(http.StatusCreated).JSON(&c)
	return c
}

func conversationTasks(t *testing.T, commentID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1 AND ext_workflow_kind = 'conversation'`, commentID)
}

func TestExtWorkflowMemberCommentWakesTheSupervisor(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	issue, _, _ := startExtRun(t)

	c := postComment(t, "", issue.ID, map[string]any{"content": "How is it going?"})
	if n := conversationTasks(t, c.ID); n != 1 {
		t.Fatalf("conversation tasks = %d, want 1", n)
	}
	var role, agentID string
	dbfx.QueryRow(t, `SELECT ext_workflow_role, agent_id FROM agent_task_queue WHERE trigger_comment_id = $1`, c.ID).Scan(&role, &agentID)
	var supervisor string
	dbfx.QueryRow(t, `SELECT definition->>'supervisor_agent_id' FROM ext_workflow_run WHERE issue_id = $1`, issue.ID).Scan(&supervisor)
	if role != "supervisor" || agentID != supervisor {
		t.Fatalf("conversation task role=%q agent=%s, want the supervisor %s", role, agentID, supervisor)
	}

	// A block from a person is a note; it wakes nobody.
	blocked := postComment(t, "", issue.ID, map[string]any{"content": extWFApproveBlock})
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, blocked.ID); n != 0 {
		t.Fatalf("a block comment enqueued %d tasks", n)
	}

	// Previewing an edit of an older comment must not wake the supervisor.
	older := dbfx.Comment(t, issue.ID, "Written before anyone listened")
	preview := withURLParam(newRequest("POST", "/api/issues/"+issue.ID+"/comment-trigger-preview", map[string]any{
		"content": "Edited text", "editing_comment_id": older,
	}), "id", issue.ID)
	testutil.Call(t, testHandler.PreviewCommentTriggers, preview).Want(http.StatusOK)
	if n := conversationTasks(t, older); n != 0 {
		t.Fatalf("preview woke the supervisor (%d tasks)", n)
	}

	setExtWorkflowEngine(t, nil)
	off := postComment(t, "", issue.ID, map[string]any{"content": "Anyone?"})
	if n := conversationTasks(t, off.ID); n != 0 {
		t.Fatalf("engine off: %d conversation tasks", n)
	}
	setExtWorkflowEngine(t, engine)
}

func TestExtWorkflowBlockOnAChildWakesNobody(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	_, runID, _ := startExtRun(t)
	child := getExtRun(t, runID).Steps[0].IssueID
	helper := createHandlerTestAgent(t, "ext-wf-mentioned-helper", nil)
	mention := "[@Helper](mention://agent/" + helper + ") "

	control := postComment(t, "", child, map[string]any{"content": mention + "please look"})
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, control.ID); n != 1 {
		t.Fatalf("control mention enqueued %d tasks, want 1", n)
	}
	blocked := postComment(t, "", child, map[string]any{"content": mention + extWFApproveBlock})
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1`, blocked.ID); n != 0 {
		t.Fatalf("block comment enqueued %d tasks, want 0", n)
	}
}
````

`server/cmd/server/ext_workflow_comments_test.go`:

```go
package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type recordingCommentObserver struct{ ids []pgtype.UUID }

func (r *recordingCommentObserver) OnComment(_ context.Context, id pgtype.UUID) error {
	r.ids = append(r.ids, id)
	return nil
}

func TestExtWorkflowCommentListenerForwardsAgentComments(t *testing.T) {
	bus := events.New()
	obs := &recordingCommentObserver{}
	registerExtWorkflowCommentListener(bus, obs)
	fromHTTP, fromTask, byMember, bySystem := dbid.NewV7(), dbid.NewV7(), dbid.NewV7(), dbid.NewV7()
	publish := func(comment any) {
		bus.Publish(events.Event{Type: protocol.EventCommentCreated, Payload: map[string]any{"comment": comment}})
	}
	publish(handler.CommentResponse{ID: util.UUIDToString(fromHTTP), AuthorType: "agent"})
	publish(map[string]any{"id": util.UUIDToString(fromTask), "author_type": "agent"})
	publish(handler.CommentResponse{ID: util.UUIDToString(byMember), AuthorType: "member"})
	publish(map[string]any{"id": util.UUIDToString(bySystem), "author_type": "system"})
	publish("not a comment")
	if len(obs.ids) != 2 || obs.ids[0] != fromHTTP || obs.ids[1] != fromTask {
		t.Fatalf("forwarded %v, want the two agent comments", obs.ids)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./cmd/server/ -run TestExtWorkflowCommentListener -count=1'"
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/handler/ -run \"TestIsNoteCommentTreatsWorkflowBlocksAsNotes|TestExtWorkflowMemberComment|TestExtWorkflowBlockOnAChild\" -count=1'"
```

Expected:
- `cmd/server` fails to build: `undefined: registerExtWorkflowCommentListener`.
- `internal/handler` fails:
  - `isNoteComment("Approving.\n\n```ext-workflow...") = false, want true`;
  - `conversation tasks = 0, want 1`;
  - `block comment enqueued 1 tasks, want 0`.

- [ ] **Step 3: Implement**

`server/internal/handler/ext_workflow_comment.go`:

```go
package handler

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/extworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ext-workflow (fork): comment-side glue for the workflow engine (spec §6.3,
// §6.4). The hooks in comment.go stay one line each.

// isExtWorkflowBlockComment: the comment carries an ext-workflow decision
// block (valid or not). isNoteComment treats it as a note, so it wakes no
// agent; the engine reads it from comment:created.
func isExtWorkflowBlockComment(content string) bool { return extworkflow.ContainsBlock(content) }

type extWorkflowCommentKey struct{}

// withExtWorkflowCommentTrigger marks ctx as the trigger pass of a stored
// comment. Only that pass may wake the supervisor: previews and the
// re-evaluation of old comments route the same comment without the mark.
func withExtWorkflowCommentTrigger(ctx context.Context, commentID pgtype.UUID) context.Context {
	return context.WithValue(ctx, extWorkflowCommentKey{}, commentID)
}

// extWorkflowParentComment is the assignee fallback for a workflow assignee:
// a person's comment that names nobody wakes the supervisor with a
// conversation turn. It never returns a platform trigger.
func (h *Handler) extWorkflowParentComment(ctx context.Context, issue db.Issue, authorType, authorID string) {
	commentID, _ := ctx.Value(extWorkflowCommentKey{}).(pgtype.UUID)
	if authorType != "member" || !commentID.Valid || !h.ExtWorkflow.Enabled() {
		return
	}
	memberID, err := util.ParseUUID(authorID)
	if err != nil {
		return
	}
	if err := h.ExtWorkflow.OnMemberParentComment(ctx, issue.ID, commentID, memberID); err != nil {
		slog.Warn("ext-workflow: conversation wake failed", "issue_id", uuidToString(issue.ID), "comment_id", uuidToString(commentID), "error", err)
	}
}

// ExtCommentFromEvent reads the comment id and author type of a
// comment:created event. The payload carries a CommentResponse from the HTTP
// handler and a map from the agent comment path in the task service.
func ExtCommentFromEvent(e events.Event) (pgtype.UUID, string, bool) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return pgtype.UUID{}, "", false
	}
	var id, authorType string
	switch c := payload["comment"].(type) {
	case CommentResponse:
		id, authorType = c.ID, c.AuthorType
	case map[string]any:
		id, _ = c["id"].(string)
		authorType, _ = c["author_type"].(string)
	default:
		return pgtype.UUID{}, "", false
	}
	commentID, err := util.ParseUUID(id)
	if err != nil {
		return pgtype.UUID{}, "", false
	}
	return commentID, authorType, true
}
```

Hook it into `comment.go`. There are three marked edits and no import change:

`server/internal/handler/comment.go`:

```diff
diff --git a/server/internal/handler/comment.go b/server/internal/handler/comment.go
--- a/server/internal/handler/comment.go
+++ b/server/internal/handler/comment.go
@@ -2018,6 +2018,10 @@ const noteCommentPrefix = "/note"
 // "/note check expiry", "  /NOTE", and "/note" all match, while "/notes",
 // "/ note", and "see foo/note" do not.
 func isNoteComment(content string) bool {
+	// ext-workflow: a decision block is workflow protocol traffic; it wakes no agent.
+	if isExtWorkflowBlockComment(content) {
+		return true
+	}
 	trimmed := strings.TrimLeft(content, " \t\r\n")
 	firstToken := trimmed
 	if i := strings.IndexFunc(trimmed, unicode.IsSpace); i >= 0 {
@@ -2038,6 +2042,7 @@ func (h *Handler) triggerTasksForComment(ctx context.Context, issue db.Issue, co
 	if isNoteComment(comment.Content) {
 		return nil
 	}
+	ctx = withExtWorkflowCommentTrigger(ctx, comment.ID) // ext-workflow: only this pass may wake a workflow supervisor
 	triggers, targets := h.computeCommentAgentTriggers(ctx, issue, comment.Content, parentComment, actorType, actorID, commentTriggerComputeOptions{
 		ExcludeTriggerCommentID: comment.ID,
 		AuthoringTaskID:         comment.SourceTaskID,
@@ -3062,6 +3067,10 @@ func (h *Handler) routeAssigneeFallback(ctx context.Context, issue db.Issue, aut
 		return commentAgentTrigger{Agent: agent, Source: commentTriggerSourceIssueAssignee, AlreadyPending: hasPending}, true
 	case "squad":
 		return h.routeAssignedSquadLeaderFallback(ctx, issue, authorType, authorID, opts)
+	case "workflow":
+		// ext-workflow: wake the supervisor with a conversation turn, not a platform run.
+		h.extWorkflowParentComment(ctx, issue, authorType, authorID)
+		return commentAgentTrigger{}, false
 	default:
 		return commentAgentTrigger{}, false
 	}
```

`server/cmd/server/ext_workflow_comments.go`:

```go
package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ext-workflow (fork): the comment protocol listener (spec §6.3).

type extWorkflowCommentObserver interface {
	OnComment(ctx context.Context, commentID pgtype.UUID) error
}

// registerExtWorkflowCommentListener forwards agent comments to the engine,
// which applies the ext-workflow decision block they may carry. People's and
// system comments never decide through this path.
func registerExtWorkflowCommentListener(bus *events.Bus, engine extWorkflowCommentObserver) {
	bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
		commentID, authorType, ok := handler.ExtCommentFromEvent(e)
		if !ok || authorType != "agent" {
			return
		}
		if err := engine.OnComment(context.Background(), commentID); err != nil {
			slog.Warn("ext-workflow: comment protocol failed", "comment_id", util.UUIDToString(commentID), "error", err)
		}
	})
}
```

`server/cmd/server/ext_workflow_wiring.go`:

```diff
diff --git a/server/cmd/server/ext_workflow_wiring.go b/server/cmd/server/ext_workflow_wiring.go
--- a/server/cmd/server/ext_workflow_wiring.go
+++ b/server/cmd/server/ext_workflow_wiring.go
@@ -43,7 +43,7 @@ func extWorkflowConstraintAdmitsWorkflow(ctx context.Context, pool *pgxpool.Pool
 
 // setupExtWorkflow builds the engine when it is enabled and the schema admits
 // it, wires it into the handler and the task service, and subscribes the
-// task-terminal listeners. It returns nil when the engine stays off.
+// task-terminal and comment listeners. It returns nil when the engine stays off.
 func setupExtWorkflow(ctx context.Context, pool *pgxpool.Pool, bus *events.Bus, h *handler.Handler) *extworkflow.Engine {
 	if !envBool(extWorkflowEngineEnv, true) {
 		slog.Info("ext-workflow: engine disabled by " + extWorkflowEngineEnv)
@@ -62,6 +62,7 @@ func setupExtWorkflow(ctx context.Context, pool *pgxpool.Pool, bus *events.Bus,
 	h.ExtWorkflow = engine
 	h.TaskService.ExtWorkflow = engine
 	registerExtWorkflowListeners(bus, engine)
+	registerExtWorkflowCommentListener(bus, engine)
 	return engine
 }
 
```

- [ ] **Step 4: Run, expect PASS**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./cmd/server/ -run \"TestExtWorkflow|TestSetupExtWorkflow\" -count=1 -v'"
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/handler/ -run \"TestExtWorkflow|TestIsNoteComment\" -count=1 -v'"
```

Expected: `--- PASS` for all of the following:
- `TestExtWorkflowCommentListenerForwardsAgentComments`
- `TestIsNoteCommentTreatsWorkflowBlocksAsNotes`
- `TestExtWorkflowMemberCommentWakesTheSupervisor`
- `TestExtWorkflowBlockOnAChildWakesNobody`
- the existing `TestIsNoteComment` and every earlier `TestExtWorkflow*`.

- [ ] **Step 5: Commit**

```bash
gofmt -l server/internal/handler/ext_workflow_comment.go server/internal/handler/ext_workflow_comment_test.go server/cmd/server/ext_workflow_comments.go server/cmd/server/ext_workflow_comments_test.go server/cmd/server/ext_workflow_wiring.go
git add server/internal/handler/ext_workflow_comment.go server/internal/handler/ext_workflow_comment_test.go server/internal/handler/comment.go server/cmd/server/ext_workflow_comments.go server/cmd/server/ext_workflow_comments_test.go server/cmd/server/ext_workflow_wiring.go
git commit -m "feat(ext-workflow): route decision blocks and workflow-issue comments to the engine

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task C6: Claim-time briefing

**Files:**
- Modify: `server/pkg/db/queries/ext_workflow_protocol.sql` (append `GetLatestAgentCommentOnIssue`), then regenerate with `make sqlc`
- Create: `server/internal/extworkflow/briefing.go`
- Create: `server/internal/handler/ext_workflow_briefing.go`
- Modify: `server/internal/handler/daemon.go` `buildClaimedTaskResponse` (right after the squad-leader `if task.IsLeaderTask { … }` block, ~:2739, before `resolveClaimProjectContext`)
- Test: `server/internal/extworkflow/briefing_test.go`, `server/internal/handler/ext_workflow_briefing_test.go`

**Interfaces:**
- Consumes:
  - Part B: `LoadRun`, `RunSnapshot`, `latestTask` (B6's helper in `observe.go`), `Definition.NodeByKey`, and the timeline payloads (`step_failed{reason,error}`, `rewind_requested{to,reason,budget_exhausted}`, `protocol_error{error|detail}`, `decision{action,reason,feedback}`).
  - C1: `MemberCanDecide`, `HumanActions`.
  - Existing queries: `GetAgent`, `GetUser`, `GetComment`.
  - Existing handler test helpers: `claimAgentInstructionsForTest`, `createClaimReclaimRuntime`.
- Produces:
  - `Engine.BuildBriefing(ctx, task) (text string, ok bool, err error)` and `StaleBriefing`.
  - `(*Handler).appendExtWorkflowBriefing`.
  - Query `GetLatestAgentCommentOnIssue{IssueID, AgentID}`.

**Staleness** follows B's rules, and a stale task gets only `StaleBriefing`:
- a step task is current while its step is `running` and it is the step's newest step task;
- a review, failure or rewind_request task is current while its step is `awaiting_supervisor` with `pending_reason` equal to the task's kind;
- a summary task is current while every step has settled;
- a conversation task is current while the run is active.

**Truncation** keeps runes whole:
- each quoted comment is cut to 4 000 runes;
- the upstream comments together are cut to 16 KB (`briefCommentBudget`);
- every truncated or omitted comment carries the `multica issue comment list <id> --tail 5` command to read it in full.

**The daemon does not see these as squad-leader tasks.**
- Workflow tasks never carry `is_leader_task` or `squad_id`, and the claim always sends `leader_role_resolved=true`.
- The briefing never contains the daemon's legacy marker, `## Squad Operating Protocol`.
- Both facts are asserted by tests.

- [ ] **Step 1: Write the failing tests**

`server/internal/extworkflow/briefing_test.go`:

````go
package extworkflow

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// squadMarker is the daemon's legacy squad-leader detection string
// (daemon.squadBriefingMarker). A workflow briefing must never contain it.
const squadMarker = "## Squad Operating Protocol"

func briefDef() Definition {
	return Definition{SupervisorAgentID: "sup", MaxRewinds: 3, Nodes: []Node{
		{Key: "spec", Title: "Spec", MaxAttempts: 3, Prompt: "Write the spec."},
		{Key: "api", Title: "API", MaxAttempts: 3, Prompt: "Build the API."},
		{Key: "build", Title: "Build", MaxAttempts: 3, Prompt: "Build the UI.", DependsOn: []string{"spec", "api"}},
	}}
}

func briefSteps(build briefStep) []briefStep {
	return []briefStep{
		{Key: "spec", Title: "Spec", Agent: "Planner", Status: "done", IssueID: "issue-spec", Attempts: 1, MaxAttempts: 3},
		{Key: "api", Title: "API", Agent: "Backend", Status: "done", IssueID: "issue-api", Attempts: 1, MaxAttempts: 3},
		build,
	}
}

func mustContain(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(text, p) {
			t.Errorf("briefing lacks %q\n---\n%s", p, text)
		}
	}
}

func mustNotContain(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if strings.Contains(text, p) {
			t.Errorf("briefing should not contain %q\n---\n%s", p, text)
		}
	}
}

func TestRenderStepBriefing(t *testing.T) {
	b := briefing{
		Kind: KindStep, Workflow: "Ship", ParentID: "issue-parent", ParentTitle: "Ship feature", ParentDesc: "Make it fast.",
		Def: briefDef(), Focus: "build", LastFeedback: "Use the new tokens.",
		Steps: briefSteps(briefStep{Key: "build", Title: "Build", Status: "running", IssueID: "issue-build", Attempts: 2, MaxAttempts: 3, DependsOn: []string{"spec", "api"}}),
		Upstream: []briefComment{
			{Key: "spec", Title: "Spec", Status: "done", IssueID: "issue-spec", Text: strings.Repeat("界", 5000)},
			{Key: "api", Title: "API", Status: "skipped", IssueID: "issue-api"},
		},
	}
	out := b.render()
	mustContain(t, out,
		"## Workflow step", "step 3 of 3, **Build** (`build`)", "workflow **Ship**", "attempt 2 of 3",
		"3. `build` Build ← spec, api — running **(this step)**",
		"Shared context: the parent issue (issue-parent)", "> Make it fast.",
		"#### `spec` Spec — done", "(truncated; read the whole comment with `multica issue comment list issue-spec --tail 5 --output json`)",
		"#### `api` API — skipped", "Its agent left no comment.",
		"### Feedback on the previous attempt", "> Use the new tokens.",
		"Work only in this child issue (issue-build).", "move this issue to `done`", "Do not modify the parent issue",
		"```ext-workflow\naction: <action>", "- `request-rewind`:", "multica issue comment add issue-build --content-stdin",
	)
	mustNotContain(t, out, "- `approve`", "step: <step key>", squadMarker)
	if got := strings.Count(out, "界"); got != briefCommentRunes {
		t.Errorf("quoted %d runes of the upstream comment, want %d", got, briefCommentRunes)
	}
	if !utf8.ValidString(out) {
		t.Error("briefing is not valid UTF-8")
	}
}

func TestRenderStepBriefingCapsUpstreamComments(t *testing.T) {
	b := briefing{
		Kind: KindStep, Workflow: "Ship", Def: briefDef(), Focus: "build",
		Steps: briefSteps(briefStep{Key: "build", Title: "Build", Status: "running", Attempts: 1, MaxAttempts: 3}),
	}
	for _, key := range []string{"a", "b", "c", "d"} {
		b.Upstream = append(b.Upstream, briefComment{Key: key, Title: key, Status: "done", IssueID: "issue-" + key, Text: strings.Repeat("Ω", 5000)})
	}
	out := b.render()
	// Ω is two bytes: 4000 + 4000 runes, then the 384 bytes left of 16 KB.
	if got, want := strings.Count(out, "Ω"), 4000+4000+(briefCommentBudget-16000)/2; got != want {
		t.Errorf("quoted %d runes, want %d", got, want)
	}
	if got := strings.Count(out, "briefing size limit"); got != 1 {
		t.Errorf("%d upstream comments omitted, want 1", got)
	}
	if !utf8.ValidString(out) {
		t.Error("briefing is not valid UTF-8")
	}
}

func supervisorBriefing(kind string, attempts, rewinds int) briefing {
	return briefing{
		Kind: kind, Workflow: "Ship", ParentID: "issue-parent", ParentTitle: "Ship feature", Def: briefDef(), Focus: "build",
		RewindsUsed: rewinds,
		Steps:       briefSteps(briefStep{Key: "build", Title: "Build", Agent: "Coder", Status: "awaiting_supervisor", IssueID: "issue-build", Attempts: attempts, MaxAttempts: 3}),
		FocusOutput: &briefComment{Key: "build", Text: "Built the UI with the old tokens."},
		Timeline:    []string{"2026-10-07 10:00 · `spec` · supervisor decided `approve`"},
	}
}

func TestRenderSupervisorReviewBriefing(t *testing.T) {
	out := supervisorBriefing(KindReview, 1, 1).render()
	mustContain(t, out,
		"## Workflow supervisor", "| `build` Build | Coder | awaiting_supervisor | 1/3 | issue-build |", "Rewinds used: 1 of 3.",
		"- 2026-10-07 10:00 · `spec` · supervisor decided `approve`",
		"Never do a step's work yourself", "Do not change the status",
		"### Your task: review step `build` Build", "> Build the UI.", "> Built the UI with the old tokens.",
		"Required: exactly one decision block, posted on the step's child issue issue-build.",
		"- `approve`:", "- `redo`:", "- `retry`:", "- `skip`:", "- `rewind`:", "- `escalate`:", "- `abort`:",
		"multica issue comment add issue-build --content-stdin",
	)
	mustNotContain(t, out, "- `request-rewind`:", squadMarker)
}

func TestRenderSupervisorBriefingRespectsBudgets(t *testing.T) {
	out := supervisorBriefing(KindReview, 3, 3).render()
	mustContain(t, out, "- `approve`:", "- `skip`:", "- `escalate`:", "- `abort`:", "The rewind budget (3) is spent.")
	mustNotContain(t, out, "- `redo`:", "- `retry`:", "- `rewind`:")
}

func TestRenderSupervisorFailureAndRewindRequest(t *testing.T) {
	b := supervisorBriefing(KindFailure, 1, 0)
	b.FailureReason = "ended_without_finishing"
	b.ProtocolErrors = []string{"illegal workflow decision: redo needs feedback"}
	mustContain(t, b.render(), "step `build` Build failed", "`ended_without_finishing`", "Your earlier decision attempts were not applied", "- illegal workflow decision: redo needs feedback")

	b = supervisorBriefing(KindRewindRequest, 1, 0)
	b.RewindTo, b.RewindReason = "spec", "The spec assumes pagination."
	mustContain(t, b.render(), "step `build` Build requests a rewind", "Requested target: `spec`.", "> The spec assumes pagination.", "Typical answers")
}

func TestRenderSummaryAndConversationBriefings(t *testing.T) {
	summary := supervisorBriefing(KindSummary, 1, 0)
	summary.Focus = ""
	out := summary.render()
	mustContain(t, out, "### Your task: the run summary", "one plain comment")
	mustNotContain(t, out, "```ext-workflow", "### Decision format")

	conv := supervisorBriefing(KindConversation, 1, 0)
	conv.Focus, conv.TriggerCommentID, conv.TriggerAuthor, conv.TriggerText, conv.TriggerMayDecide = "", "comment-1", "Ada", "Please approve build.", true
	out = conv.render()
	mustContain(t, out, "Ada commented on this issue", "> Please approve build.", "`--parent comment-1`", "step: <step key>", "- `approve`:", "- `abort`:", "multica issue comment add issue-parent")
	mustNotContain(t, out, "- `escalate`:", "- `request-rewind`:")

	conv.TriggerMayDecide = false
	out = conv.render()
	mustContain(t, out, "may not decide on this run")
	mustNotContain(t, out, "```ext-workflow")
}

func TestTruncateBytesKeepsRunesWhole(t *testing.T) {
	got, cut := truncateBytes("aé界", 4) // a(1) é(2) 界(3): the cut falls inside 界
	if got != "aé" || !cut {
		t.Fatalf("truncateBytes = %q, %v", got, cut)
	}
}

// ── DB-backed ───────────────────────────────────────────────────────────────

func TestBuildBriefingForAStepCarriesUpstreamOutput(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	supervisor, planner, coder := e.agent(t, "Supervisor"), e.agent(t, "Planner"), e.agent(t, "Coder")
	wf := e.workflow(t, supervisor, 3,
		wfNode{key: "spec", title: "Spec", agent: planner},
		wfNode{key: "build", title: "Build", agent: coder, deps: []string{"spec"}})
	parent := e.parentIssue(t, wf, "todo")
	run := e.start(t, parent)
	spec := e.step(t, run, "spec")
	e.fx.Comment(t, util.UUIDToString(spec.IssueID), "Spec: use cursor pagination.", testutil.Cols{"author_type": "agent", "author_id": planner})
	e.childMoves(t, parent, spec.IssueID, "done")

	task := e.latestTask(t, e.step(t, run, "build"), RoleStep)
	text, ok, err := e.engine.BuildBriefing(ctx, task)
	if err != nil || !ok {
		t.Fatalf("BuildBriefing = %v, %v", ok, err)
	}
	mustContain(t, text, "step 2 of 2, **Build**", "workflow **Ship**", "> Spec: use cursor pagination.", "#### `spec` Spec — done")

	plain := e.latestTask(t, spec, RoleStep)
	plain.ExtWorkflowRunID.Valid = false
	if _, ok, err := e.engine.BuildBriefing(ctx, plain); ok || err != nil {
		t.Fatalf("a non-workflow task got a briefing (%v, %v)", ok, err)
	}
}

func TestBuildBriefingForAReviewAndStaleTasks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run, build, _ := e.reviewRun(t, e.user)
	e.fx.Comment(t, util.UUIDToString(build.IssueID), "Built it.", testutil.Cols{"author_type": "agent", "author_id": build.AgentID})

	review := e.latestTask(t, build, RoleSupervisor)
	text, ok, err := e.engine.BuildBriefing(ctx, review)
	if err != nil || !ok {
		t.Fatalf("BuildBriefing = %v, %v", ok, err)
	}
	mustContain(t, text, "### Your task: review step `build` Build", "> Built it.", "- `approve`:")

	// The step task finished its turn: the step now waits for review.
	stale, ok, err := e.engine.BuildBriefing(ctx, e.latestTask(t, build, RoleStep))
	if err != nil || !ok || stale != StaleBriefing {
		t.Fatalf("step task after review started: %q, %v, %v", stale, ok, err)
	}
	e.decide(t, run, "build", Decision{Action: ActionApprove}, Actor{Type: "member", ID: e.user})
	stale, _, _ = e.engine.BuildBriefing(ctx, review)
	if stale != StaleBriefing {
		t.Fatalf("review task after approve: %q", stale)
	}
}
````

`server/internal/handler/ext_workflow_briefing_test.go`:

````go
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestExtWorkflowClaimCarriesTheBriefing(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "ext-wf-claim runtime")
	agentOnRuntime := func(name string) string {
		id := dbfx.Agent(t, name, runtimeID, testutil.Cols{
			"visibility": "workspace", "permission_mode": "public_to", "instructions": "Be terse.",
			"custom_env": testutil.Raw("'{}'::jsonb"), "custom_args": testutil.Raw("'[]'::jsonb"),
		})
		dbfx.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'workspace', $2) ON CONFLICT DO NOTHING`, id, testWorkspaceID)
		return id
	}
	supervisor, worker := agentOnRuntime("ext-wf-claim supervisor"), agentOnRuntime("ext-wf-claim worker")
	wf := createExtWorkflowAs(t, "", "Claimed workflow", supervisor)
	updateExtWorkflow(t, "", wf.ID, map[string]any{"nodes": []any{extWFNode("build", worker)}}).Want(http.StatusOK)
	var issue IssueResponse
	createWorkflowIssue(t, wf.ID, "todo").Want(http.StatusCreated).JSON(&issue)
	var stepTaskID string
	dbfx.QueryRow(t, `
		SELECT t.id FROM agent_task_queue t JOIN issue c ON c.id = t.issue_id
		WHERE c.parent_issue_id = $1 AND t.ext_workflow_role = 'step'`, issue.ID).Scan(&stepTaskID)

	taskID, instructions, isLeader, raw := claimAgentInstructionsForTest(t, runtimeID)
	if taskID != stepTaskID {
		t.Fatalf("claimed %q, want the step task %s: %s", taskID, stepTaskID, raw)
	}
	if !strings.HasPrefix(instructions, "Be terse.\n\n## Workflow step") {
		t.Fatalf("instructions do not append the briefing to the agent's own:\n%s", instructions)
	}
	for _, want := range []string{"step 1 of 1, **Title build**", "workflow **Claimed workflow**", "```ext-workflow"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("briefing lacks %q", want)
		}
	}
	if isLeader || strings.Contains(instructions, "## Squad Operating Protocol") {
		t.Fatalf("a workflow step task was delivered as a squad-leader task")
	}
}
````

- [ ] **Step 2: Run, expect FAIL**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -run \"Render|Truncate|BuildBriefing\" -count=1'"
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/handler/ -run TestExtWorkflowClaimCarriesTheBriefing -count=1'"
```

Expected:
- `internal/extworkflow` fails to build: `undefined: briefing`, `undefined: briefStep`, `e.engine.BuildBriefing undefined`.
- The handler test fails with `instructions do not append the briefing to the agent's own:` followed by `Be terse.`.

- [ ] **Step 3: Implement**

Append the query:

`server/pkg/db/queries/ext_workflow_protocol.sql`:

```diff
diff --git a/server/pkg/db/queries/ext_workflow_protocol.sql b/server/pkg/db/queries/ext_workflow_protocol.sql
--- a/server/pkg/db/queries/ext_workflow_protocol.sql
+++ b/server/pkg/db/queries/ext_workflow_protocol.sql
@@ -8,3 +8,11 @@ SELECT EXISTS (
     SELECT 1 FROM agent_task_queue
     WHERE trigger_comment_id = @comment_id AND ext_workflow_kind = 'conversation'
 ) AS woken;
+
+-- name: GetLatestAgentCommentOnIssue :one
+-- The newest comment an agent wrote on an issue: a step's output, as the
+-- briefing hands it to downstream steps and to the supervisor.
+SELECT * FROM comment
+WHERE issue_id = @issue_id AND author_type = 'agent' AND author_id = @agent_id AND deleted_at IS NULL
+ORDER BY created_at DESC, id DESC
+LIMIT 1;
```

```bash
make sqlc
```

`server/internal/extworkflow/briefing.go`:

````go
package extworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Claim-time briefing (spec §6.2). The daemon claim appends it to the agent's
// instructions. It is rebuilt from live state at every claim: a task the run
// has moved past gets StaleBriefing and nothing else.

// StaleBriefing is the whole briefing of a task the run no longer needs.
const StaleBriefing = "## Workflow task\n\nThis workflow run has moved on; do nothing and end."

const (
	briefCommentRunes  = 4000      // per quoted comment
	briefCommentBudget = 16 * 1024 // bytes, all upstream comments together
	briefTimelineMax   = 30        // most recent timeline entries
)

// BuildBriefing returns the briefing for a workflow task. ok=false: the task
// is not a workflow task (or the engine is off) and gets no briefing.
func (e *Engine) BuildBriefing(ctx context.Context, task db.AgentTaskQueue) (string, bool, error) {
	if !task.ExtWorkflowRunID.Valid || !e.Enabled() {
		return "", false, nil
	}
	snap, err := e.LoadRun(ctx, task.ExtWorkflowRunID)
	if errors.Is(err, ErrRunNotFound) {
		return StaleBriefing, true, nil
	}
	if err != nil {
		return "", false, err
	}
	current, err := e.taskIsCurrent(ctx, snap, task)
	if err != nil {
		return "", false, err
	}
	if !current {
		return StaleBriefing, true, nil
	}
	b, err := e.gatherBriefing(ctx, snap, task)
	if err != nil {
		return "", false, err
	}
	return b.render(), true, nil
}

// taskIsCurrent re-validates the task against the run, with the engine's own
// staleness rules.
func (e *Engine) taskIsCurrent(ctx context.Context, snap *RunSnapshot, task db.AgentTaskQueue) (bool, error) {
	if !snap.State.Status.Active() {
		return false, nil
	}
	kind := task.ExtWorkflowKind.String
	switch kind {
	case KindConversation:
		return true, nil
	case KindSummary:
		return snap.State.AllSettled(), nil
	}
	key, ok := snap.KeyOf(task.ExtWorkflowStepID)
	if !ok {
		return false, nil
	}
	st := snap.State.Steps[key]
	switch kind {
	case KindStep:
		if st.Status != StepRunning {
			return false, nil
		}
		latest, found, err := latestTask(ctx, e.q, task.ExtWorkflowStepID, RoleStep)
		if err != nil || !found {
			return false, err
		}
		return latest.ID == task.ID, nil
	case KindReview, KindFailure, KindRewindRequest:
		return st.Status == StepAwaitingSupervisor && string(st.PendingReason) == kind, nil
	}
	return false, nil
}

// briefing is everything the renderer needs; render is pure.
type briefing struct {
	Kind        string
	Workflow    string
	ParentID    string
	ParentTitle string
	ParentDesc  string
	Def         Definition
	Steps       []briefStep // node order
	Focus       string      // the step this task is about ("" for summary/conversation)
	RewindsUsed int
	// Step briefing.
	Upstream     []briefComment
	LastFeedback string
	// Supervisor briefing.
	FocusOutput    *briefComment
	Timeline       []string
	FailureReason  string
	FailureError   string
	RewindTo       string
	RewindReason   string
	BudgetSpent    bool // the step's rewind request found no budget left
	ProtocolErrors []string
	// Conversation.
	TriggerCommentID string
	TriggerAuthor    string
	TriggerText      string
	TriggerMayDecide bool
}

type briefStep struct {
	Key, Title, Agent, Status, IssueID string
	Attempts, MaxAttempts              int
	DependsOn                          []string
}

type briefComment struct {
	Key, Title, Status, IssueID string
	Text                        string // "" when the agent wrote no comment
}

func (e *Engine) gatherBriefing(ctx context.Context, snap *RunSnapshot, task db.AgentTaskQueue) (briefing, error) {
	b := briefing{
		Kind: task.ExtWorkflowKind.String, Workflow: "workflow", Def: snap.Def,
		ParentID: util.UUIDToString(snap.Run.IssueID), RewindsUsed: int(snap.Run.RewindsUsed),
	}
	if wf, err := e.q.GetExtWorkflowInWorkspace(ctx, db.GetExtWorkflowInWorkspaceParams{ID: snap.Run.WorkflowID, WorkspaceID: snap.Run.WorkspaceID}); err == nil {
		b.Workflow = wf.Name
	}
	if parent, err := e.q.GetIssue(ctx, snap.Run.IssueID); err == nil {
		b.ParentTitle, b.ParentDesc = parent.Title, parent.Description.String
	}
	agentNames := map[pgtype.UUID]string{}
	for _, row := range snap.Steps {
		name, ok := agentNames[row.AgentID]
		if !ok {
			if a, err := e.q.GetAgent(ctx, row.AgentID); err == nil {
				name = a.Name
			}
			agentNames[row.AgentID] = name
		}
		n, _ := snap.Def.NodeByKey(row.NodeKey)
		b.Steps = append(b.Steps, briefStep{
			Key: row.NodeKey, Title: n.Title, Agent: name, Status: row.Status, IssueID: util.UUIDToString(row.IssueID),
			Attempts: int(row.Attempts), MaxAttempts: n.MaxAttempts, DependsOn: n.DependsOn,
		})
	}
	b.Focus, _ = snap.KeyOf(task.ExtWorkflowStepID)
	events, err := e.q.ListExtWorkflowRunEvents(ctx, snap.Run.ID)
	if err != nil {
		return b, fmt.Errorf("list run events: %w", err)
	}
	switch b.Kind {
	case KindStep:
		node, _ := snap.Def.NodeByKey(b.Focus)
		for _, dep := range node.DependsOn {
			c, err := e.stepOutput(ctx, snap, dep)
			if err != nil {
				return b, err
			}
			b.Upstream = append(b.Upstream, c)
		}
		b.LastFeedback = snap.ByKey[b.Focus].LastFeedback.String
		return b, nil
	case KindReview, KindFailure, KindRewindRequest:
		c, err := e.stepOutput(ctx, snap, b.Focus)
		if err != nil {
			return b, err
		}
		b.FocusOutput = &c
		b.focusEvents(snap, events)
	case KindConversation:
		if err := e.gatherTrigger(ctx, snap, task, &b); err != nil {
			return b, err
		}
	}
	b.Timeline = timeline(snap, events)
	return b, nil
}

// stepOutput is a step's latest comment by its own agent.
func (e *Engine) stepOutput(ctx context.Context, snap *RunSnapshot, key string) (briefComment, error) {
	row := snap.ByKey[key]
	n, _ := snap.Def.NodeByKey(key)
	c := briefComment{Key: key, Title: n.Title, Status: row.Status, IssueID: util.UUIDToString(row.IssueID)}
	comment, err := e.q.GetLatestAgentCommentOnIssue(ctx, db.GetLatestAgentCommentOnIssueParams{IssueID: row.IssueID, AgentID: row.AgentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("load step output: %w", err)
	}
	c.Text = comment.Content
	return c, nil
}

// focusEvents reads, from the timeline, why the focus step waits: the event
// that sent it to the supervisor and the protocol errors since.
func (b *briefing) focusEvents(snap *RunSnapshot, events []db.ExtWorkflowRunEvent) {
	stepID := snap.ByKey[b.Focus].ID
	start := -1
	for i, ev := range events {
		if ev.StepID != stepID {
			continue
		}
		switch ev.Kind {
		case RunEventStepFinished, RunEventStepFailed, RunEventRewindRequested:
			start = i
		}
	}
	if start < 0 {
		return
	}
	p := eventPayload(events[start])
	switch events[start].Kind {
	case RunEventStepFailed:
		b.FailureReason, b.FailureError = str(p["reason"]), str(p["error"])
	case RunEventRewindRequested:
		b.RewindTo, b.RewindReason = str(p["to"]), str(p["reason"])
		b.BudgetSpent, _ = p["budget_exhausted"].(bool)
	}
	for _, ev := range events[start+1:] {
		if ev.StepID != stepID || ev.Kind != RunEventProtocolError {
			continue
		}
		p := eventPayload(ev)
		msg := str(p["error"])
		if msg == "" {
			msg = str(p["detail"])
		}
		if msg != "" {
			b.ProtocolErrors = append(b.ProtocolErrors, msg)
		}
	}
}

func (e *Engine) gatherTrigger(ctx context.Context, snap *RunSnapshot, task db.AgentTaskQueue, b *briefing) error {
	if !task.TriggerCommentID.Valid {
		return nil
	}
	b.TriggerCommentID = util.UUIDToString(task.TriggerCommentID)
	c, err := e.q.GetComment(ctx, task.TriggerCommentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load triggering comment: %w", err)
	}
	b.TriggerText = c.Content
	b.TriggerAuthor = "a person"
	if c.AuthorType != "member" {
		return nil
	}
	if u, err := e.q.GetUser(ctx, c.AuthorID); err == nil && u.Name != "" {
		b.TriggerAuthor = u.Name
	}
	ok, err := e.MemberCanDecide(ctx, snap.Run, c.AuthorID)
	if err != nil {
		return err
	}
	b.TriggerMayDecide = ok
	return nil
}

func eventPayload(ev db.ExtWorkflowRunEvent) map[string]any {
	p := map[string]any{}
	_ = json.Unmarshal(ev.Payload, &p)
	return p
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// timeline renders the run's past decisions, oldest first.
func timeline(snap *RunSnapshot, events []db.ExtWorkflowRunEvent) []string {
	var out []string
	for _, ev := range events {
		key, _ := snap.KeyOf(ev.StepID)
		p := eventPayload(ev)
		var line string
		switch ev.Kind {
		case RunEventDecision:
			line = fmt.Sprintf("%s decided `%s`", ev.ActorType, str(p["action"]))
			if r := firstNonEmpty(str(p["reason"]), str(p["feedback"])); r != "" {
				line += ": " + oneLine(r)
			}
		case RunEventRewind:
			line = fmt.Sprintf("rewound to `%s`", str(p["to"]))
		case RunEventRewindRequested:
			line = fmt.Sprintf("step agent requested a rewind to `%s`: %s", firstNonEmpty(str(p["to"]), key), oneLine(str(p["reason"])))
		case RunEventEscalated:
			line = "escalated to a person: " + oneLine(str(p["reason"]))
		case RunEventStepFailed:
			line = "failed: " + str(p["reason"])
		default:
			continue
		}
		if key != "" {
			line = fmt.Sprintf("`%s` · %s", key, line)
		}
		out = append(out, ev.CreatedAt.Time.UTC().Format("2006-01-02 15:04")+" · "+line)
	}
	if len(out) > briefTimelineMax {
		out = out[len(out)-briefTimelineMax:]
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	t, cut := truncateRunes(s, 200)
	if cut {
		t += "…"
	}
	return t
}

// truncateRunes keeps at most n runes.
func truncateRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	return string([]rune(s)[:n]), true
}

// truncateBytes keeps at most n bytes without splitting a rune.
func truncateBytes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	if n <= 0 {
		return "", true
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// ── Rendering (pure) ────────────────────────────────────────────────────────

func (b briefing) render() string {
	var w strings.Builder
	switch b.Kind {
	case KindStep:
		b.renderStep(&w)
	default:
		b.renderSupervisor(&w)
	}
	return strings.TrimRight(w.String(), "\n")
}

func (b briefing) stepIndex(key string) int {
	for i, n := range b.Def.Nodes {
		if n.Key == key {
			return i + 1
		}
	}
	return 0
}

func (b briefing) step(key string) briefStep {
	for _, s := range b.Steps {
		if s.Key == key {
			return s
		}
	}
	return briefStep{Key: key, Title: key}
}

func (b briefing) renderStep(w *strings.Builder) {
	focus := b.step(b.Focus)
	fmt.Fprintf(w, "## Workflow step\n\n")
	fmt.Fprintf(w, "You are running step %d of %d, **%s** (`%s`), of the workflow **%s**. This issue (%s) is that step's child issue; attempt %d of %d.\n\n",
		b.stepIndex(b.Focus), len(b.Def.Nodes), focus.Title, focus.Key, b.Workflow, focus.IssueID, focus.Attempts, focus.MaxAttempts)
	w.WriteString("### Workflow outline\n\n")
	for i, n := range b.Def.Nodes {
		s := b.step(n.Key)
		line := fmt.Sprintf("%d. `%s` %s", i+1, n.Key, n.Title)
		if len(n.DependsOn) > 0 {
			line += " ← " + strings.Join(n.DependsOn, ", ")
		}
		line += " — " + s.Status
		if n.Key == b.Focus {
			line += " **(this step)**"
		}
		w.WriteString(line + "\n")
	}
	fmt.Fprintf(w, "\n### Shared context: the parent issue (%s)\n\n**%s**\n\n", b.ParentID, b.ParentTitle)
	if desc, cut := truncateRunes(strings.TrimSpace(b.ParentDesc), briefCommentRunes); desc != "" {
		w.WriteString(quote(desc) + "\n")
		if cut {
			fmt.Fprintf(w, "\n(truncated; read it with `multica issue get %s --output json`)\n", b.ParentID)
		}
		w.WriteString("\n")
	}
	if len(b.Upstream) > 0 {
		w.WriteString("### Upstream results\n\n")
		budget := briefCommentBudget
		for _, u := range b.Upstream {
			fmt.Fprintf(w, "#### `%s` %s — %s\n\nChild issue: %s\n\n", u.Key, u.Title, u.Status, u.IssueID)
			full := fmt.Sprintf("`multica issue comment list %s --tail 5 --output json`", u.IssueID)
			switch {
			case strings.TrimSpace(u.Text) == "":
				w.WriteString("Its agent left no comment.\n\n")
			case budget <= 0:
				fmt.Fprintf(w, "Its last comment is not included here (briefing size limit); read it with %s.\n\n", full)
			default:
				text, cut := truncateRunes(u.Text, briefCommentRunes)
				if t, over := truncateBytes(text, budget); over {
					text, cut = t, true
				}
				budget -= len(text)
				w.WriteString("Last comment by its agent:\n\n" + quote(text) + "\n\n")
				if cut {
					fmt.Fprintf(w, "(truncated; read the whole comment with %s)\n\n", full)
				}
			}
		}
	}
	if b.LastFeedback != "" {
		w.WriteString("### Feedback on the previous attempt\n\n" + quote(b.LastFeedback) + "\n\n")
	}
	w.WriteString("### Rules\n\n")
	fmt.Fprintf(w, "- Work only in this child issue (%s).\n", focus.IssueID)
	w.WriteString("- When you are finished, post your result as one comment on this issue, then move this issue to `done` (`in_review` also completes the step).\n")
	w.WriteString("- Do not modify the parent issue or the other steps' issues.\n")
	w.WriteString("- Use `request-rewind` only when an upstream result is wrong or makes this step infeasible, never because the work is hard.\n\n")
	b.renderDecisionFormat(w, []DecisionAction{ActionRequestRewind}, focus.IssueID, false)
}

func (b briefing) renderSupervisor(w *strings.Builder) {
	fmt.Fprintf(w, "## Workflow supervisor\n\n")
	fmt.Fprintf(w, "You supervise the workflow **%s** on this issue (%s), **%s**. A deterministic engine runs the steps; you handle what it cannot decide.\n\n",
		b.Workflow, b.ParentID, b.ParentTitle)
	w.WriteString("### Run overview\n\n| Step | Agent | Status | Attempts | Child issue |\n|---|---|---|---|---|\n")
	for _, s := range b.Steps {
		fmt.Fprintf(w, "| `%s` %s | %s | %s | %d/%d | %s |\n", s.Key, s.Title, s.Agent, s.Status, s.Attempts, s.MaxAttempts, s.IssueID)
	}
	fmt.Fprintf(w, "\nRewinds used: %d of %d.\n\n", b.RewindsUsed, b.Def.MaxRewinds)
	if len(b.Timeline) > 0 {
		w.WriteString("### Timeline\n\n")
		for _, line := range b.Timeline {
			w.WriteString("- " + line + "\n")
		}
		w.WriteString("\n")
	}
	w.WriteString("### Rules\n\n")
	w.WriteString("- Never do a step's work yourself; decide, and let the step's agent do the work.\n")
	w.WriteString("- Do not change the status of this issue or of any step's issue; the engine owns them.\n\n")

	switch b.Kind {
	case KindReview, KindFailure, KindRewindRequest:
		b.renderFocus(w)
	case KindSummary:
		w.WriteString("### Your task: the run summary\n\n")
		w.WriteString("Every step has settled. Post exactly one plain comment on this issue that summarizes each step's outcome, with links to the steps' issues. Do not post a decision block.\n")
	case KindConversation:
		b.renderConversation(w)
	}
}

func (b briefing) renderFocus(w *strings.Builder) {
	focus := b.step(b.Focus)
	n, _ := b.Def.NodeByKey(b.Focus)
	switch b.Kind {
	case KindReview:
		fmt.Fprintf(w, "### Your task: review step `%s` %s\n\nThe step finished and needs your review.\n\n", focus.Key, focus.Title)
	case KindFailure:
		fmt.Fprintf(w, "### Your task: step `%s` %s failed\n\n", focus.Key, focus.Title)
		switch {
		case b.BudgetSpent:
			fmt.Fprintf(w, "The step's agent asked for a rewind to `%s` (%s), but the rewind budget is spent.\n\n", firstNonEmpty(b.RewindTo, focus.Key), oneLine(b.RewindReason))
		case b.FailureReason == "ended_without_finishing":
			w.WriteString("Failure reason: `ended_without_finishing`: the agent's turn ended without moving the issue to done.\n\n")
		case b.FailureReason != "":
			fmt.Fprintf(w, "Failure reason: `%s`.\n\n", b.FailureReason)
		}
		if b.FailureError != "" {
			w.WriteString("Error:\n\n" + quote(b.FailureError) + "\n\n")
		}
	case KindRewindRequest:
		fmt.Fprintf(w, "### Your task: step `%s` %s requests a rewind\n\n", focus.Key, focus.Title)
		fmt.Fprintf(w, "Requested target: `%s`.\n\nReason:\n\n%s\n\n", firstNonEmpty(b.RewindTo, focus.Key), quote(b.RewindReason))
		w.WriteString("Typical answers: `rewind` (with feedback for the target step), or `redo` with feedback explaining why the request is rejected.\n\n")
	}
	fmt.Fprintf(w, "Step prompt:\n\n%s\n\nChild issue: %s (attempt %d of %d).\n\n", quote(n.Prompt), focus.IssueID, focus.Attempts, focus.MaxAttempts)
	if b.FocusOutput != nil && strings.TrimSpace(b.FocusOutput.Text) != "" {
		text, cut := truncateRunes(b.FocusOutput.Text, briefCommentRunes)
		w.WriteString("Last comment by the step's agent:\n\n" + quote(text) + "\n\n")
		if cut {
			fmt.Fprintf(w, "(truncated; read it with `multica issue comment list %s --tail 5 --output json`)\n\n", focus.IssueID)
		}
	} else {
		w.WriteString("The step's agent left no comment.\n\n")
	}
	if len(b.ProtocolErrors) > 0 {
		w.WriteString("Your earlier decision attempts were not applied:\n\n")
		for _, msg := range b.ProtocolErrors {
			w.WriteString("- " + oneLine(msg) + "\n")
		}
		w.WriteString("\n")
	}
	fmt.Fprintf(w, "Required: exactly one decision block, posted on the step's child issue %s.\n\n", focus.IssueID)
	b.renderDecisionFormat(w, b.allowedFor(focus), focus.IssueID, false)
}

func (b briefing) renderConversation(w *strings.Builder) {
	w.WriteString("### Your task: answer a person\n\n")
	fmt.Fprintf(w, "%s commented on this issue:\n\n", b.TriggerAuthor)
	text, _ := truncateRunes(b.TriggerText, briefCommentRunes)
	w.WriteString(quote(text) + "\n\n")
	fmt.Fprintf(w, "Reply under that comment (`--parent %s`). Answer questions about the run from the overview and the timeline.\n\n", b.TriggerCommentID)
	if !b.TriggerMayDecide {
		w.WriteString("This person may not decide on this run (only the member who started it, the workflow's creator or a workspace admin can), so do not post a decision block; tell them who can.\n")
		return
	}
	w.WriteString("If they ask for a decision, you may act on their behalf with one decision block in your reply. Name the step with `step:` (not needed for `abort`). The engine checks their permission, not yours.\n\n")
	b.renderDecisionFormat(w, HumanActions, b.ParentID, true)
}

// allowedFor is what a supervisor may decide on a waiting step now.
func (b briefing) allowedFor(s briefStep) []DecisionAction {
	out := []DecisionAction{ActionApprove}
	if s.Attempts < s.MaxAttempts {
		out = append(out, ActionRedo, ActionRetry)
	}
	out = append(out, ActionSkip)
	if b.RewindsUsed < b.Def.MaxRewinds {
		out = append(out, ActionRewind)
	}
	return append(out, ActionEscalate, ActionAbort)
}

var actionHelp = map[DecisionAction]string{
	ActionApprove:       "accept the step's result; the step is done.",
	ActionRedo:          "run the step again with `feedback` on what must change.",
	ActionRetry:         "run the step again unchanged (for transient failures).",
	ActionSkip:          "give up on the step; the steps after it go ahead without it.",
	ActionRewind:        "reset step `to` (this step or one upstream of it) and everything after it, then re-run from there with `feedback`.",
	ActionEscalate:      "hand the decision to a person, with `reason`.",
	ActionAbort:         "stop the whole run, with `reason`.",
	ActionRequestRewind: "ask the supervisor to rewind to upstream step `to` (optional), with `reason`.",
}

func (b briefing) renderDecisionFormat(w *strings.Builder, allowed []DecisionAction, issueID string, onParent bool) {
	w.WriteString("### Decision format\n\n")
	fmt.Fprintf(w, "Decide by posting one comment on issue %s that contains exactly one fenced block:\n\n", issueID)
	w.WriteString("```" + BlockLang + "\n")
	w.WriteString("action: <action>\n")
	if onParent {
		w.WriteString("step: <step key>      # required, except for abort\n")
	}
	w.WriteString("to: <step key>        # rewind (required), request-rewind (optional)\n")
	w.WriteString("reason: <one line>    # required for escalate, abort, request-rewind\n")
	w.WriteString("feedback: |           # required for redo and rewind\n")
	w.WriteString("  <what must change>\n")
	w.WriteString("```\n\n")
	w.WriteString("Allowed now:\n\n")
	for _, a := range allowed {
		fmt.Fprintf(w, "- `%s`: %s\n", a, actionHelp[a])
	}
	if b.RewindsUsed >= b.Def.MaxRewinds {
		fmt.Fprintf(w, "\nThe rewind budget (%d) is spent.\n", b.Def.MaxRewinds)
	}
	fmt.Fprintf(w, "\nPost it with `multica issue comment add %s --content-stdin` so the block keeps its line breaks. Unknown fields, a second block or a missing required field are rejected with a reply on the issue; fix the block and post it again.\n", issueID)
}

func quote(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}
````

`server/internal/handler/ext_workflow_briefing.go`:

```go
package handler

import (
	"context"
	"log/slog"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// appendExtWorkflowBriefing appends the workflow briefing to a claimed
// workflow task's instructions, after any squad briefing. Workflow tasks
// never carry is_leader_task or squad_id, so the daemon runs them as plain
// agent turns. A briefing that cannot be built leaves the claim untouched.
func (h *Handler) appendExtWorkflowBriefing(ctx context.Context, task *db.AgentTaskQueue, resp *AgentTaskResponse) {
	if task == nil || !task.ExtWorkflowRunID.Valid || resp.Agent == nil || !h.ExtWorkflow.Enabled() {
		return
	}
	text, ok, err := h.ExtWorkflow.BuildBriefing(ctx, *task)
	if err != nil {
		slog.Warn("ext-workflow: briefing failed", "task_id", uuidToString(task.ID), "error", err)
		return
	}
	if !ok || text == "" {
		return
	}
	if strings.TrimSpace(resp.Agent.Instructions) == "" {
		resp.Agent.Instructions = text
	} else {
		resp.Agent.Instructions += "\n\n" + text
	}
}
```

The claim hook, placed after the squad-leader block:

`server/internal/handler/daemon.go`:

```diff
diff --git a/server/internal/handler/daemon.go b/server/internal/handler/daemon.go
--- a/server/internal/handler/daemon.go
+++ b/server/internal/handler/daemon.go
@@ -2737,6 +2737,8 @@ func (h *Handler) buildClaimedTaskResponse(r *http.Request, task *db.AgentTaskQu
 				)
 			}
 		}
+		// ext-workflow: a workflow task carries its run briefing (spec §6.2).
+		h.appendExtWorkflowBriefing(r.Context(), task, &resp)
 
 		projectCtx, projectErr := h.resolveClaimProjectContext(r.Context(), issue.ProjectID, issue.WorkspaceID)
 		if projectErr != nil {
```

- [ ] **Step 4: Run, expect PASS**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -count=1 -v -run \"Render|Truncate|BuildBriefing\"'"
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -count=1'"
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/handler/ -run \"TestExtWorkflow|TestClaim_\" -count=1'"
```

Expected:
- `--- PASS` for the seven pure renderer and truncation tests:
  - `TestRenderStepBriefing`
  - `TestRenderStepBriefingCapsUpstreamComments`
  - `TestRenderSupervisorReviewBriefing`
  - `TestRenderSupervisorBriefingRespectsBudgets`
  - `TestRenderSupervisorFailureAndRewindRequest`
  - `TestRenderSummaryAndConversationBriefings`
  - `TestTruncateBytesKeepsRunesWhole`
- `--- PASS` for the two DB-backed tests, `TestBuildBriefingForAStepCarriesUpstreamOutput` and `TestBuildBriefingForAReviewAndStaleTasks`.
- The handler run is `ok`; it includes `TestExtWorkflowClaimCarriesTheBriefing` and the existing squad-briefing claim tests.

- [ ] **Step 5: Commit**

```bash
gofmt -l server/internal/extworkflow server/internal/handler/ext_workflow_briefing.go server/internal/handler/ext_workflow_briefing_test.go
git add server/pkg/db/queries/ext_workflow_protocol.sql server/pkg/db/generated/ext_workflow_protocol.sql.go server/internal/extworkflow/briefing.go server/internal/extworkflow/briefing_test.go server/internal/handler/ext_workflow_briefing.go server/internal/handler/ext_workflow_briefing_test.go server/internal/handler/daemon.go
git commit -m "feat(ext-workflow): claim-time briefing for workflow step and supervisor tasks

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task C7: End-to-end agent protocol through the HTTP handlers

**Files:**
- Test: `server/internal/handler/ext_workflow_protocol_e2e_test.go`

**Interfaces:**
- Consumes:
  - C2–C6.
  - `CreateComment` and `UpdateIssue` called with agent headers (`X-Agent-ID` + `X-Task-ID`, as the CLI sends them), and the real child-event hook inside `UpdateIssue`.
  - `ExtCommentFromEvent` + `OnComment`, subscribed to the test bus exactly as `cmd/server` subscribes them.
  - `Engine.OnTaskTerminal`, which plays the task-terminal listener.
- Produces: test helpers `forwardExtWorkflowComments`, `extWFAgent`, `extTask`, `mustExtTask`, `endExtTask`, `extStep`, `wantExtStep`, `protocolReplies`, `e2eWorkflow`.

**Scenarios.** Both tests use a supervisor and the nodes `spec` → `build`, where `build` needs review.

1. **Spec, request-rewind, rewind, re-run, approve.**
   1. The spec agent comments and moves its child to `done`.
   2. The build agent posts a `request-rewind` block.
   3. The supervisor posts an invalid `rewind` with no feedback. It gets a `protocol_error` event and a system reply, and nothing else changes.
   4. The supervisor posts a valid `rewind`. The spec step re-runs with `last_feedback`, the build step resets to `pending`, and `rewinds_used` becomes 1.
   5. The spec agent re-runs. The build step re-runs, and its briefing quotes the new spec.
   6. The build agent moves its child to `in_review`, which starts the review.
   7. The supervisor posts `approve` on the child issue. The step is done and the child is `done`; no platform task woke on the child.
   8. The deciding turn ends and the summary task is enqueued.
2. **Conversation.**
   - A plain member comments on the parent. The supervisor's on-behalf `approve` is refused, with a reply.
   - The member who started the run comments. The same block applies, with `on_behalf_of` set to that member.

This task adds tests only.

- [ ] **Step 1: Write the test**

`server/internal/handler/ext_workflow_protocol_e2e_test.go`:

````go
package handler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// End-to-end agent protocol through the HTTP handlers: agents comment and
// move issues with their task headers, people comment and decide, and the
// test only plays the daemon (a task starts running, a task ends).

var extWFCommentForwardOnce sync.Once

// forwardExtWorkflowComments subscribes the comment protocol listener to the
// shared test bus, as cmd/server does, delivering to whichever engine the
// current test wired.
func forwardExtWorkflowComments() {
	extWFCommentForwardOnce.Do(func() {
		testHandler.Bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
			commentID, authorType, ok := ExtCommentFromEvent(e)
			if !ok || authorType != "agent" || testHandler.ExtWorkflow == nil {
				return
			}
			_ = testHandler.ExtWorkflow.OnComment(context.Background(), commentID)
		})
	})
}

type extWFAgent struct {
	t  *testing.T
	id string
}

func (a extWFAgent) request(method, path string, body any, taskID string) *http.Request {
	req := newRequest(method, path, body)
	req.Header.Set("X-Agent-ID", a.id)
	req.Header.Set("X-Task-ID", taskID)
	return req
}

// comment posts as the agent from inside taskID; parentID may be "".
func (a extWFAgent) comment(taskID, issueID, content, parentID string) CommentResponse {
	a.t.Helper()
	body := map[string]any{"content": content}
	if parentID != "" {
		body["parent_id"] = parentID
	}
	var c CommentResponse
	testutil.Call(a.t, testHandler.CreateComment, withURLParam(a.request("POST", "/api/issues/"+issueID+"/comments", body, taskID), "id", issueID)).
		Want(http.StatusCreated).JSON(&c)
	return c
}

// moves sets an issue's status as the agent, like `multica issue status`.
func (a extWFAgent) moves(taskID, issueID, status string) {
	a.t.Helper()
	testutil.Call(a.t, testHandler.UpdateIssue, withURLParam(a.request("PUT", "/api/issues/"+issueID, map[string]any{"status": status}, taskID), "id", issueID)).
		Want(http.StatusOK)
}

// extTask is the newest workflow task of a kind on an issue, marked running
// as if a daemon had claimed it.
func extTask(t *testing.T, issueID, kind string) string {
	t.Helper()
	var id string
	dbfx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE issue_id = $1 AND ext_workflow_kind = $2 ORDER BY created_at DESC, id DESC LIMIT 1`, issueID, kind).Scan(&id)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, id)
	return id
}

func mustExtTask(t *testing.T, taskID string) db.AgentTaskQueue {
	t.Helper()
	task, err := testHandler.Queries.GetAgentTask(context.Background(), util.MustParseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func endExtTask(t *testing.T, taskID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, taskID)
	if err := testHandler.ExtWorkflow.OnTaskTerminal(context.Background(), util.MustParseUUID(taskID)); err != nil {
		t.Fatalf("OnTaskTerminal: %v", err)
	}
}

func extStep(t *testing.T, runID, key string) ExtWorkflowStepResponse {
	t.Helper()
	for _, s := range getExtRun(t, runID).Steps {
		if s.NodeKey == key {
			return s
		}
	}
	t.Fatalf("no step %q", key)
	return ExtWorkflowStepResponse{}
}

func wantExtStep(t *testing.T, runID, key, status string, attempts int) ExtWorkflowStepResponse {
	t.Helper()
	s := extStep(t, runID, key)
	if s.Status != status || s.Attempts != attempts {
		t.Fatalf("step %s = %s/%d, want %s/%d", key, s.Status, s.Attempts, status, attempts)
	}
	return s
}

func protocolReplies(t *testing.T, commentID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM comment WHERE parent_id = $1 AND author_type = 'system'`, commentID)
}

const extWFBlockFence = "```ext-workflow\n"

// e2eWorkflow builds supervisor → spec → build (build needs review) and
// assigns a fresh issue to it.
func e2eWorkflow(t *testing.T) (supervisor, planner, coder extWFAgent, issue IssueResponse, runID string) {
	t.Helper()
	supervisor = extWFAgent{t, createHandlerTestAgent(t, t.Name()+" supervisor", nil)}
	planner = extWFAgent{t, createHandlerTestAgent(t, t.Name()+" planner", nil)}
	coder = extWFAgent{t, createHandlerTestAgent(t, t.Name()+" coder", nil)}
	wf := createExtWorkflowAs(t, "", t.Name(), supervisor.id)
	build := extWFNode("build", coder.id, "spec")
	build["requires_review"] = true
	updateExtWorkflow(t, "", wf.ID, map[string]any{"nodes": []any{extWFNode("spec", planner.id), build}}).Want(http.StatusOK)
	createWorkflowIssue(t, wf.ID, "todo").Want(http.StatusCreated).JSON(&issue)
	dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1 OR issue_id IN (SELECT id FROM issue WHERE parent_issue_id = $1)`, issue.ID)
	dbfx.QueryRow(t, `SELECT id FROM ext_workflow_run WHERE issue_id = $1`, issue.ID).Scan(&runID)
	return
}

func TestExtWorkflowAgentProtocolThroughComments(t *testing.T) {
	requireExtWorkflowDB(t)
	engine := withExtWorkflowEngine(t)
	forwardExtWorkflowComments()
	supervisor, planner, coder, issue, runID := e2eWorkflow(t)
	spec, build := extStep(t, runID, "spec"), extStep(t, runID, "build")

	// spec runs and finishes; build starts.
	t1 := extTask(t, spec.IssueID, "step")
	planner.comment(t1, spec.IssueID, "Spec v1: offset pagination.", "")
	planner.moves(t1, spec.IssueID, "done")
	endExtTask(t, t1)
	wantExtStep(t, runID, "spec", "done", 1)
	wantExtStep(t, runID, "build", "running", 1)

	// The build agent finds the spec wrong and asks for a rewind.
	t2 := extTask(t, build.IssueID, "step")
	coder.comment(t2, build.IssueID, "The API has no offsets.\n\n"+extWFBlockFence+"action: request-rewind\nto: spec\nreason: The spec assumes offset pagination; the API only has cursors.\n```\n", "")
	if s := wantExtStep(t, runID, "build", "awaiting_supervisor", 1); s.PendingReason == nil || *s.PendingReason != "rewind_request" {
		t.Fatalf("build pending_reason = %v, want rewind_request", s.PendingReason)
	}
	endExtTask(t, t2)

	// The supervisor's first answer misses feedback: a protocol error and a
	// reply, nothing else. The second rewinds spec.
	rr := extTask(t, issue.ID, "rewind_request")
	bad := supervisor.comment(rr, build.IssueID, extWFBlockFence+"action: rewind\nto: spec\n```\n", "")
	if n := protocolReplies(t, bad.ID); n != 1 {
		t.Fatalf("protocol error replies = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'protocol_error'`, runID); n != 1 {
		t.Fatalf("protocol_error events = %d, want 1", n)
	}
	wantExtStep(t, runID, "build", "awaiting_supervisor", 1)
	supervisor.comment(rr, build.IssueID, extWFBlockFence+"action: rewind\nto: spec\nfeedback: |\n  Switch to cursor pagination.\n```\n", "")
	if s := wantExtStep(t, runID, "spec", "running", 1); s.LastFeedback == nil || *s.LastFeedback != "Switch to cursor pagination." {
		t.Fatalf("spec last_feedback = %v", s.LastFeedback)
	}
	wantExtStep(t, runID, "build", "pending", 0)
	if run := getExtRun(t, runID); run.RewindsUsed != 1 {
		t.Fatalf("rewinds_used = %d, want 1", run.RewindsUsed)
	}
	endExtTask(t, rr)

	// spec re-runs; build re-runs on the new spec, which its briefing carries.
	t3 := extTask(t, spec.IssueID, "step")
	planner.comment(t3, spec.IssueID, "Spec v2: cursor pagination.", "")
	planner.moves(t3, spec.IssueID, "done")
	endExtTask(t, t3)
	wantExtStep(t, runID, "build", "running", 1)
	t4 := extTask(t, build.IssueID, "step")
	if text, ok, err := engine.BuildBriefing(context.Background(), mustExtTask(t, t4)); err != nil || !ok || !strings.Contains(text, "> Spec v2: cursor pagination.") {
		t.Fatalf("build briefing (ok=%v err=%v) lacks the new spec:\n%s", ok, err, text)
	}
	coder.comment(t4, build.IssueID, "Build v2 done.", "")
	coder.moves(t4, build.IssueID, "in_review")
	endExtTask(t, t4)
	if s := wantExtStep(t, runID, "build", "awaiting_supervisor", 1); *s.PendingReason != "review" {
		t.Fatalf("build pending_reason = %s, want review", *s.PendingReason)
	}

	// The supervisor reviews on the child issue and approves.
	review := extTask(t, issue.ID, "review")
	supervisor.comment(review, build.IssueID, "Looks right.\n\n"+extWFBlockFence+"action: approve\n```\n", "")
	wantExtStep(t, runID, "build", "done", 1)
	var childStatus string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, build.IssueID).Scan(&childStatus)
	if childStatus != "done" {
		t.Fatalf("build child = %s, want done", childStatus)
	}
	// The block on the child woke nobody: the only tasks there are the step's.
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND ext_workflow_run_id IS NULL`, build.IssueID); n != 0 {
		t.Fatalf("%d platform tasks on the build child", n)
	}
	// The summary waits for the deciding review turn to end.
	endExtTask(t, review)
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND ext_workflow_kind = 'summary'`, issue.ID); n != 1 {
		t.Fatalf("summary tasks = %d, want 1", n)
	}
}

func TestExtWorkflowConversationThroughComments(t *testing.T) {
	requireExtWorkflowDB(t)
	withExtWorkflowEngine(t)
	forwardExtWorkflowComments()
	supervisor, planner, coder, issue, runID := e2eWorkflow(t)
	spec, build := extStep(t, runID, "spec"), extStep(t, runID, "build")
	t1 := extTask(t, spec.IssueID, "step")
	planner.moves(t1, spec.IssueID, "done")
	endExtTask(t, t1)
	t2 := extTask(t, build.IssueID, "step")
	coder.moves(t2, build.IssueID, "in_review")
	endExtTask(t, t2)
	wantExtStep(t, runID, "build", "awaiting_supervisor", 1)
	approve := extWFBlockFence + "action: approve\nstep: build\n```\n"

	// A member who may not decide asks; the supervisor's block for them is refused.
	plain := createPlainMember(t, "ext-wf-conversation-plain@multica.test")
	asked := postComment(t, plain, issue.ID, map[string]any{"content": "Can we just approve the build?"})
	c1 := extTask(t, issue.ID, "conversation")
	refused := supervisor.comment(c1, issue.ID, "On your behalf:\n\n"+approve, asked.ID)
	if n := protocolReplies(t, refused.ID); n != 1 {
		t.Fatalf("refused decision replies = %d, want 1", n)
	}
	wantExtStep(t, runID, "build", "awaiting_supervisor", 1)
	endExtTask(t, c1)

	// The member who started the run asks; the same block applies for them.
	asked = postComment(t, "", issue.ID, map[string]any{"content": "Approve the build, please."})
	c2 := extTask(t, issue.ID, "conversation")
	supervisor.comment(c2, issue.ID, "Approving for you:\n\n"+approve, asked.ID)
	wantExtStep(t, runID, "build", "done", 1)
	var onBehalf, actor string
	dbfx.QueryRow(t, `SELECT on_behalf_of::text, actor_id::text FROM ext_workflow_run_event WHERE run_id = $1 AND kind = 'decision'`, runID).Scan(&onBehalf, &actor)
	if onBehalf != testUserID || actor != supervisor.id {
		t.Fatalf("decision by %s on behalf of %s, want the supervisor for %s", actor, onBehalf, testUserID)
	}
}
````

- [ ] **Step 2: Run, expect PASS (coverage-only task), and check that it can fail**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/handler/ -run \"TestExtWorkflowAgentProtocolThroughComments|TestExtWorkflowConversationThroughComments\" -count=1 -v'"
```

Expected: `--- PASS` for both tests. C2–C6 already implement the behaviour.

To check that the test can fail, temporarily replace the body of `forwardExtWorkflowComments` with `return` and rerun. It fails with `step build = running/1, want awaiting_supervisor/1`. Then restore the body.

- [ ] **Step 3: Implement**

Nothing to implement. If a scenario fails, fix the behaviour in the task that owns it (C3 for the protocol, C4 for the conversation, C5 for the hooks, C6 for the briefing), not in this test.

- [ ] **Step 4: Run the ext suites**

```bash
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/extworkflow/ -count=1 -race'"
make env-exec ARGS="-- bash -c 'cd server && go test ./internal/handler/ -run \"TestExtWorkflow|TestIsNoteComment|TestClaim_\" -count=1'"
make env-exec ARGS="-- bash -c 'cd server && go test ./cmd/server/ ./internal/service/ ./internal/scheduler/ -count=1'"
make env-exec ARGS="-- bash -c 'cd server && go vet ./internal/extworkflow/ ./internal/handler/ ./cmd/server/'"
```

Expected: every listed package is `ok`. The full `internal/handler` package (`make test`) is green once Part A's workspace-deletion manifest edit is in the tree.

- [ ] **Step 5: Commit**

```bash
gofmt -l server/internal/handler/ext_workflow_protocol_e2e_test.go
git add server/internal/handler/ext_workflow_protocol_e2e_test.go
git commit -m "test(ext-workflow): end-to-end agent protocol through comments

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

# Part D

# Part D: Web and desktop views for ext workflows

Branch `feat/ext-workflow`. Every command runs from the repo root `/mnt/a0df7f69-337a-4cb4-a10a-f48243e3ddfc/Code/multica` unless stated. Parts A to C must be merged first (backend, and `packages/core/ext-workflows/`).

## Notes for integrator

**Names this part assumes from core (the contract does not pin them down). Part A must match them, or tell Part D.**

1. Mutation hook signatures. Every mutation hook takes `wsId` as its first argument, like `useDeleteIssueWakeup(workspaceId, issueId)`. Variables:
   - `useCreateExtWorkflow(wsId).mutateAsync({ name, description, supervisor_agent_id }) -> ExtWorkflow`
   - `useUpdateExtWorkflow(wsId).mutateAsync({ id, name?, description?, supervisor_agent_id?, max_rewinds?, nodes?: ExtWorkflowNodeInput[] }) -> ExtWorkflow` (with `nodes`)
   - `useArchiveExtWorkflow(wsId).mutateAsync(id: string)`
   - `useDecideExtWorkflowStep(wsId).mutateAsync({ runId, stepId, action, to?, reason?, feedback?, expected_status })`
   - `useCancelExtWorkflowRun(wsId).mutateAsync(runId: string)`
2. Query data shapes. `extWorkflowListOptions(wsId)` resolves to `ExtWorkflow[]` (already unwrapped from `{workflows}`). `extWorkflowDetailOptions(wsId, id)` resolves to `ExtWorkflow` with `nodes`. `extWorkflowRunsOptions(wsId, id)` resolves to `{ runs: ExtWorkflowRunSummary[]; total: number }` (no paging argument; the Runs tab shows what the server returns). `extWorkflowIssueRunsOptions(wsId, issueId)` resolves to `ExtWorkflowIssueRuns` = `{ runs: ExtWorkflowRunSummary[]; step_of: null | { run_id; step_id; node_key; index; total; parent_issue_id } }`. `extWorkflowRunOptions(wsId, runId)` resolves to `ExtWorkflowRun`.
3. `step_of.index` is displayed as given, so it is assumed to be 1-based. If Part C emits 0-based, add 1 in `ExtWorkflowStepLine`.
4. Run statuses: `running | waiting_human | done | failed | cancelled | unknown`. Step statuses: the eight spec values plus `unknown`. Types `ExtWorkflowRunStatus` / `ExtWorkflowStepStatus` must include `"unknown"`.
5. The 422 error is detected by duck typing (an object with `errors: {field, message}[]`), in `extractValidationErrors`, so Part D does not import `ExtWorkflowValidationFailed`. It must carry `.errors`. HTTP status of decision failures is read the same way (`errorStatus`: a numeric `.status` property, as `ApiError` has), because `issue-detail.test.tsx` replaces `@multica/core/api` with an object that has no `ApiError`.
6. `ExtWorkflow` list items carry `node_count`, `active_run_count`, `last_run_at`, `creator_id`, `avatar_url`, `archived_at`; the detail response also carries `nodes` (with `position`). `ExtWorkflowRunSummary` carries `workflow_name`, `issue_identifier`, `issue_title`. A decision on an inbox item's parent uses `issue_id`.
7. Everything is imported from `@multica/core/ext-workflows` (the package `index.ts`): option functions, hooks, and types.

**Contract deviations**

- Create-dialog copy and the "Workflows" assignee-group copy: the dialog copy lives in `ext-workflows.json` (`create.*`), not `modals.json`. Reason: one namespace, fewer cross-file edits. Only `modals.json` `run_confirm.create_will_start_workflow` and `issues.json` `pickers.assignee.workflows_group` / `workflow_needs_nodes` are added to existing namespaces.
- `quick-create-issue.tsx` is NOT changed. A workflow is not a valid quick-create "actor" (that flow asks one agent to turn a prompt into an issue, and §9 says quick action stays untouched). The `ActorSelection` union stays `agent | squad`, so adding `"workflow"` to `IssueAssigneeType` does not break it (verified: `QuickCreateActorType` is its own union).
- `InboxItemType` in `packages/core/types/inbox.ts` is NOT widened. `apps/mobile/components/inbox/detail-label.tsx` has a total `Record<InboxItemType, string>`, so adding a member would break the mobile typecheck, and mobile is out of scope. Instead `useTypeLabels()` returns `Record<InboxItemType | "ext_workflow_escalation", string>`; `InboxDetailLabel` already has a `default:` branch that renders `typeLabels[item.type] ?? item.type`, so the escalation item shows its label with no other change. Clicking it opens the parent issue through the item's existing `issue_id` (Part B/C must set `issue_id` to the parent issue on the inbox item).
- Unsaved-changes guard: draft state lives in the page, so switching tabs does not lose it. The squad-style `AlertDialog` guard is applied to tab switches (Keep editing / Discard), plus a `beforeunload` listener. There is no in-app navigation blocker in this codebase, so sidebar navigation away from a dirty page is not intercepted.
- Core path edits that the brief marked "if needed" are needed: `TabActorType` gains `"workflow"` (`tab-subject.ts`, `tab-presentation.ts`), plus `TabLabelKey` `"workflow"`.

**Cross-part ordering constraints**

- `packages/views/search/search-command.tsx` has a total `Record<WorkspacePageKey, string[]>`. The moment Part A adds `workflows` to `WorkspacePageKey` in `packages/core/paths/route-icons.ts`, `pnpm --filter @multica/views typecheck` fails until Task D2 adds the keywords. Land D2 in the same PR, or have Part A's route-icons commit include the one-line `workflows:` entry in `PAGE_KEYWORDS` (D2 Step 3 shows it).
- Same for `ROUTE_ICON_COMPONENTS` in `route-icon-components.tsx` (needs `Workflow` once `RouteIconName` has it) and for `NavLabelKey` (`layout.json` `nav.workflows`).
- `reserved_slugs.json` / `reserved-slugs.ts` `workflows`: Part A. The e2e in D8 needs it.
- Backend flag `MULTICA_WORKFLOW_ENGINE` must not be `false` for the D8 e2e.
- Existing suites with hand-written `@multica/core/api` mocks (`issue-detail.test.tsx` at minimum) need stubs for `listExtWorkflows` (D6) and `getIssueExtWorkflowRuns` (D7). The tasks add them; D6 and D7 Step 4 run the whole views suite to catch any other.
- Method names assumed on `ApiClient` (only used by those stubs): `listExtWorkflows`, `getIssueExtWorkflowRuns`, as in the contract.

**Task list** (each task leaves `pnpm typecheck`, `pnpm lint`, `pnpm test` green)

| Task | Content |
|---|---|
| D1 | `ext-workflows.json` x5, registration, small keys in layout/settings/issues/modals/inbox |
| D2 | Nav scaffolding: sidebar item, icon, shortcut, search keywords, tab subject and presentation |
| D3 | Pure helpers, DAG layout, status components, agent select |
| D4 | List page, create dialog, registry, web and desktop list routes, package exports |
| D5 | Node editor, DAG preview, runs tab, detail page, detail routes |
| D6 | `ActorAvatar`, assignee picker, run-confirm gate, create-issue copy, batch toolbar, inbox label |
| D7 | Issue run section, step line, mount in issue-detail |
| D8 | Playwright e2e |

Test commands used throughout:

- single views test: `pnpm --filter @multica/views exec vitest run <path>`
- single core test: `pnpm --filter @multica/core exec vitest run <path>`
- all: `pnpm typecheck && pnpm lint && pnpm test`

Shared throwaway helper for JSON edits (used in D1/D2/D6; never committed). Save it once to the scratchpad:

```bash
cat > "$CLAUDE_SCRATCH/add-key.mjs" <<'JS'
// usage: node add-key.mjs <file> <dot.path.to.object|""> <afterKey|""> <newKey> <jsonValue>
import fs from "node:fs";
const [file, path, afterKey, newKey, raw] = process.argv.slice(2);
const root = JSON.parse(fs.readFileSync(file, "utf8"));
let target = root;
for (const seg of path ? path.split(".") : []) target = target[seg];
if (!target || typeof target !== "object") throw new Error(`no object at ${path} in ${file}`);
if (newKey in target) { console.log(`skip ${file}: ${newKey} exists`); process.exit(0); }
const next = {};
let placed = false;
for (const [k, v] of Object.entries(target)) {
  next[k] = v;
  if (k === afterKey) { next[newKey] = JSON.parse(raw); placed = true; }
}
if (!placed) next[newKey] = JSON.parse(raw);
for (const k of Object.keys(target)) delete target[k];
Object.assign(target, next);
fs.writeFileSync(file, JSON.stringify(root, null, 2) + "\n");
JS
```

`CLAUDE_SCRATCH` is any writable temp dir (`export CLAUDE_SCRATCH=$(mktemp -d)`).
After each use, `git diff --stat` must show only small additions per file (the files are already 2-space JSON with a trailing newline).


---

### Task D1: i18n namespace `ext-workflows` and small cross-namespace keys

**Files:**
- Create: `packages/views/locales/en/ext-workflows.json`, `packages/views/locales/zh-Hans/ext-workflows.json`, `packages/views/locales/ko/ext-workflows.json`, `packages/views/locales/ja/ext-workflows.json`, `packages/views/locales/fr/ext-workflows.json`
- Create (test): `packages/views/locales/ext-workflows.test.ts`
- Modify: `packages/views/locales/index.ts` (import block after each `...Squads` import at lines 25, 51, 77, 103, 129; `RESOURCES` entries after each `squads:` line); `packages/views/i18n/resources-types.ts` (`import type` after line 28, interface entry after line 69)
- Modify (keys via the add-key helper): `locales/*/layout.json` (`nav.workflows`, `tab.workflow`), `locales/*/settings.json` (`shortcuts.actions.goWorkflows`), `locales/*/issues.json` (`pickers.assignee.workflows_group`, `pickers.assignee.workflow_needs_nodes`), `locales/*/modals.json` (`run_confirm.create_will_start_workflow`), `locales/*/inbox.json` (`types.ext_workflow_escalation`)

**Interfaces:** Produces the `ext-workflows` namespace (consumed by D3 to D7 through `useT("ext-workflows")`), and the keys listed above.

- [ ] **Step 1: Write the failing test**

Create `packages/views/locales/ext-workflows.test.ts`:

```ts
// @vitest-environment node
import { describe, expect, it } from "vitest";
import { RESOURCES } from "./index";

const LOCALES = ["en", "zh-Hans", "ko", "ja", "fr"] as const;

describe("ext-workflows locale wiring", () => {
  for (const locale of LOCALES) {
    it(`${locale}: registers the ext-workflows namespace`, () => {
      const ns = RESOURCES[locale]["ext-workflows"] as Record<string, unknown> | undefined;
      expect(ns).toBeDefined();
      expect(ns).toHaveProperty("page.title");
      expect(ns).toHaveProperty("run_section.action_approve");
      expect(ns).toHaveProperty("step_status.awaiting_human");
    });

    it(`${locale}: adds the cross-namespace keys`, () => {
      const r = RESOURCES[locale] as Record<string, Record<string, unknown>>;
      expect(r.layout).toHaveProperty("nav.workflows");
      expect(r.layout).toHaveProperty("tab.workflow");
      expect(r.settings).toHaveProperty("shortcuts.actions.goWorkflows.label");
      expect(r.issues).toHaveProperty("pickers.assignee.workflows_group");
      expect(r.issues).toHaveProperty("pickers.assignee.workflow_needs_nodes");
      expect(r.modals).toHaveProperty("run_confirm.create_will_start_workflow");
      expect(r.inbox).toHaveProperty("types.ext_workflow_escalation");
    });
  }
});
```

- [ ] **Step 2: Run, expect FAIL**

```bash
pnpm --filter @multica/views exec vitest run locales/ext-workflows.test.ts locales/parity.test.ts
```
Expected: FAIL (namespace undefined).

- [ ] **Step 3: Implement**

3a. Create the five JSON files with exactly this content.

`packages/views/locales/en/ext-workflows.json`:

```json
{
  "page": {
    "title": "Workflows",
    "new_button": "New workflow",
    "empty": "No workflows yet.",
    "row_menu": "Workflow actions",
    "archive_action": "Archive",
    "never_run": "Never",
    "table": {
      "name": "Workflow",
      "supervisor": "Supervisor",
      "nodes": "Nodes",
      "active_runs": "Active runs",
      "last_run": "Last run"
    }
  },
  "archive_dialog": {
    "title": "Archive this workflow?",
    "description": "\"{{name}}\" will be archived. Runs in progress continue, but it can't be assigned to new issues.",
    "cancel": "Cancel",
    "confirm": "Archive",
    "archiving": "Archiving…",
    "success": "Workflow archived",
    "failed": "Failed to archive workflow"
  },
  "agent_select": {
    "search_placeholder": "Search agents…",
    "group_my_agents": "My agents",
    "group_workspace_agents": "Workspace agents",
    "no_agents": "No agents available. Create an agent first.",
    "archived_suffix": "(archived)"
  },
  "create": {
    "title": "New workflow",
    "name_label": "Name",
    "name_placeholder": "e.g. Release pipeline",
    "description_label": "Description",
    "description_placeholder": "What this workflow is for",
    "supervisor_label": "Supervisor agent",
    "supervisor_hint": "Reviews each step and decides what happens next.",
    "supervisor_placeholder": "Select a supervisor agent",
    "cancel": "Cancel",
    "submit": "Create",
    "submitting": "Creating…",
    "toast_created": "Workflow created",
    "toast_failed": "Failed to create workflow"
  },
  "detail": {
    "not_found": "Workflow not found",
    "tabs": {
      "nodes": "Nodes",
      "runs": "Runs"
    },
    "save": "Save",
    "saving": "Saving…",
    "saved": "Workflow saved",
    "save_failed": "Failed to save workflow",
    "discard": "Discard changes",
    "unsaved": "Unsaved changes",
    "validation_failed": "Fix the highlighted problems and save again.",
    "archive_button": "Archive",
    "discard_dialog": {
      "title": "Discard unsaved changes?",
      "description": "Your edits to this workflow haven't been saved.",
      "keep_editing": "Keep editing",
      "discard": "Discard"
    }
  },
  "inspector": {
    "name": "Name",
    "description": "Description",
    "description_placeholder": "No description",
    "supervisor": "Supervisor",
    "max_rewinds": "Max rewinds",
    "max_rewinds_hint": "How many times a run may return to an earlier step.",
    "created_by": "Created by",
    "updated": "Updated"
  },
  "nodes": {
    "add": "Add node",
    "empty": "No nodes yet. Add the first step.",
    "active_runs_note_one": "{{count}} run in progress keeps using the previous definition.",
    "active_runs_note_other": "{{count}} runs in progress keep using the previous definition.",
    "node_n": "Node {{n}}",
    "title_label": "Title",
    "title_placeholder": "Step title",
    "key_label": "Key",
    "agent_label": "Agent",
    "agent_placeholder": "Select agent",
    "depends_label": "Depends on",
    "depends_none": "None",
    "depends_no_options": "No other node can be added without creating a cycle.",
    "review_label": "Review",
    "max_attempts_label": "Max attempts",
    "prompt_label": "Prompt",
    "prompt_placeholder": "What should the agent do in this step?",
    "expand": "Show prompt",
    "collapse": "Hide prompt",
    "move_up": "Move up",
    "move_down": "Move down",
    "delete": "Delete node",
    "archived_agent": "This agent is archived. Choose another agent."
  },
  "errors": {
    "key_required": "Key is required",
    "key_invalid": "Use 1 to 40 lowercase letters, digits, - or _",
    "key_duplicate": "Key is already used",
    "title_required": "Title is required",
    "agent_required": "Choose an agent",
    "prompt_too_long": "Prompt is too long (max 20,000 bytes)",
    "max_attempts_range": "Max attempts must be 1 to 10",
    "self_dependency": "A node can't depend on itself",
    "unknown_dependency": "Depends on a node that doesn't exist",
    "cycle": "Dependencies form a cycle",
    "too_many_nodes": "At most 50 nodes",
    "no_nodes": "Add at least one node"
  },
  "dag": {
    "aria_label": "Workflow graph",
    "empty": "Add nodes to see the flow."
  },
  "runs": {
    "empty": "No runs yet. Assign an issue to this workflow to start one.",
    "col_issue": "Issue",
    "col_status": "Status",
    "col_started": "Started",
    "col_finished": "Finished",
    "showing": "Showing {{shown}} of {{total}}"
  },
  "run_status": {
    "running": "Running",
    "waiting_human": "Needs you",
    "done": "Done",
    "failed": "Failed",
    "cancelled": "Cancelled",
    "unknown": "Unknown"
  },
  "step_status": {
    "pending": "Pending",
    "running": "Running",
    "awaiting_supervisor": "Supervisor reviewing",
    "awaiting_human": "Needs you",
    "done": "Done",
    "skipped": "Skipped",
    "failed": "Failed",
    "cancelled": "Cancelled",
    "unknown": "Unknown"
  },
  "run_section": {
    "title": "Workflow run",
    "rewinds": "Rewinds {{used}}/{{max}}",
    "attempts": "Attempt {{attempts}}/{{max}}",
    "open_child": "Open step issue",
    "open_workflow": "Open workflow",
    "needs_decision": "Needs your decision",
    "action_approve": "Approve",
    "action_redo": "Redo…",
    "action_retry": "Retry",
    "action_skip": "Skip",
    "action_rewind": "Rewind to…",
    "action_abort": "Abort…",
    "attempt_limit": "Attempt limit reached",
    "rewind_limit": "Rewind limit reached",
    "cancel_run": "Cancel run",
    "timeline": "Timeline",
    "timeline_empty": "No events yet",
    "actor_engine": "Workflow",
    "step_line": "Workflow step {{index}} of {{total}}",
    "step_line_with_parent": "Workflow step {{index}} of {{total}} · {{parent}}",
    "redo_dialog": {
      "title": "Redo this step",
      "feedback_label": "Feedback for the agent",
      "feedback_placeholder": "What should change?",
      "confirm": "Redo",
      "cancel": "Cancel"
    },
    "rewind_dialog": {
      "title": "Rewind",
      "target_label": "Return to",
      "target_self": "{{title}} (this step)",
      "feedback_label": "Feedback for the agent",
      "feedback_placeholder": "What should change?",
      "confirm": "Rewind",
      "cancel": "Cancel"
    },
    "abort_dialog": {
      "title": "Abort the run?",
      "reason_label": "Reason",
      "reason_placeholder": "Why is this run being stopped?",
      "confirm": "Abort run",
      "cancel": "Cancel"
    },
    "cancel_dialog": {
      "title": "Cancel this run?",
      "description": "Running steps stop and open step issues are cancelled.",
      "keep": "Keep running",
      "confirm": "Cancel run"
    },
    "toast": {
      "decided": "Decision sent",
      "mismatch": "This step changed. Review its new state and try again.",
      "forbidden": "You can't decide on this run",
      "failed": "Failed to send decision",
      "cancelled": "Run cancelled",
      "cancel_failed": "Failed to cancel run"
    }
  },
  "events": {
    "run_started": "Run started",
    "step_started": "Step started",
    "step_finished": "Step finished",
    "step_failed": "Step failed",
    "decision": "Decision",
    "rewind": "Rewind",
    "rewind_requested": "Rewind requested",
    "escalated": "Escalated to a human",
    "run_finished": "Run finished",
    "run_cancelled": "Run cancelled",
    "protocol_error": "Protocol error",
    "unknown": "Event"
  }
}
```

`packages/views/locales/zh-Hans/ext-workflows.json`:

```json
{
  "page": {
    "title": "工作流",
    "new_button": "新建工作流",
    "empty": "还没有工作流。",
    "row_menu": "工作流操作",
    "archive_action": "归档",
    "never_run": "从未运行",
    "table": {
      "name": "工作流",
      "supervisor": "监督者",
      "nodes": "节点",
      "active_runs": "进行中的运行",
      "last_run": "最近运行"
    }
  },
  "archive_dialog": {
    "title": "归档此工作流？",
    "description": "“{{name}}”将被归档。进行中的运行会继续，但无法再分配给新任务。",
    "cancel": "取消",
    "confirm": "归档",
    "archiving": "归档中…",
    "success": "工作流已归档",
    "failed": "归档工作流失败"
  },
  "agent_select": {
    "search_placeholder": "搜索智能体…",
    "group_my_agents": "我的智能体",
    "group_workspace_agents": "工作区智能体",
    "no_agents": "没有可用的智能体，请先创建一个。",
    "archived_suffix": "（已归档）"
  },
  "create": {
    "title": "新建工作流",
    "name_label": "名称",
    "name_placeholder": "例如：发布流水线",
    "description_label": "描述",
    "description_placeholder": "这个工作流用来做什么",
    "supervisor_label": "监督者智能体",
    "supervisor_hint": "审核每个步骤，并决定下一步怎么走。",
    "supervisor_placeholder": "选择监督者智能体",
    "cancel": "取消",
    "submit": "创建",
    "submitting": "创建中…",
    "toast_created": "工作流已创建",
    "toast_failed": "创建工作流失败"
  },
  "detail": {
    "not_found": "未找到工作流",
    "tabs": {
      "nodes": "节点",
      "runs": "运行"
    },
    "save": "保存",
    "saving": "保存中…",
    "saved": "工作流已保存",
    "save_failed": "保存工作流失败",
    "discard": "放弃修改",
    "unsaved": "有未保存的修改",
    "validation_failed": "请修正标出的问题后再保存。",
    "archive_button": "归档",
    "discard_dialog": {
      "title": "放弃未保存的修改？",
      "description": "你对此工作流所做的修改尚未保存。",
      "keep_editing": "继续编辑",
      "discard": "放弃"
    }
  },
  "inspector": {
    "name": "名称",
    "description": "描述",
    "description_placeholder": "暂无描述",
    "supervisor": "监督者",
    "max_rewinds": "最大回退次数",
    "max_rewinds_hint": "一次运行最多可以退回到之前步骤的次数。",
    "created_by": "创建者",
    "updated": "更新于"
  },
  "nodes": {
    "add": "添加节点",
    "empty": "还没有节点，添加第一个步骤。",
    "active_runs_note_other": "有 {{count}} 个进行中的运行仍使用旧的定义。",
    "node_n": "节点 {{n}}",
    "title_label": "标题",
    "title_placeholder": "步骤标题",
    "key_label": "标识",
    "agent_label": "智能体",
    "agent_placeholder": "选择智能体",
    "depends_label": "依赖于",
    "depends_none": "无",
    "depends_no_options": "没有其他节点可以在不形成环的情况下添加。",
    "review_label": "审核",
    "max_attempts_label": "最大尝试次数",
    "prompt_label": "提示词",
    "prompt_placeholder": "智能体在这一步要做什么？",
    "expand": "显示提示词",
    "collapse": "隐藏提示词",
    "move_up": "上移",
    "move_down": "下移",
    "delete": "删除节点",
    "archived_agent": "该智能体已归档，请选择其他智能体。"
  },
  "errors": {
    "key_required": "标识不能为空",
    "key_invalid": "请使用 1 到 40 个小写字母、数字、- 或 _",
    "key_duplicate": "标识已被使用",
    "title_required": "标题不能为空",
    "agent_required": "请选择智能体",
    "prompt_too_long": "提示词过长（最多 20,000 字节）",
    "max_attempts_range": "最大尝试次数须为 1 到 10",
    "self_dependency": "节点不能依赖自身",
    "unknown_dependency": "依赖的节点不存在",
    "cycle": "依赖关系形成了环",
    "too_many_nodes": "最多 50 个节点",
    "no_nodes": "请至少添加一个节点"
  },
  "dag": {
    "aria_label": "工作流图",
    "empty": "添加节点后即可看到流程。"
  },
  "runs": {
    "empty": "还没有运行。把任务分配给此工作流即可开始一次运行。",
    "col_issue": "任务",
    "col_status": "状态",
    "col_started": "开始时间",
    "col_finished": "结束时间",
    "showing": "显示 {{shown}} / {{total}}"
  },
  "run_status": {
    "running": "运行中",
    "waiting_human": "等你处理",
    "done": "已完成",
    "failed": "失败",
    "cancelled": "已取消",
    "unknown": "未知"
  },
  "step_status": {
    "pending": "待开始",
    "running": "运行中",
    "awaiting_supervisor": "监督者审核中",
    "awaiting_human": "等你处理",
    "done": "已完成",
    "skipped": "已跳过",
    "failed": "失败",
    "cancelled": "已取消",
    "unknown": "未知"
  },
  "run_section": {
    "title": "工作流运行",
    "rewinds": "回退 {{used}}/{{max}}",
    "attempts": "第 {{attempts}}/{{max}} 次尝试",
    "open_child": "打开步骤任务",
    "open_workflow": "打开工作流",
    "needs_decision": "需要你来决定",
    "action_approve": "通过",
    "action_redo": "重做…",
    "action_retry": "重试",
    "action_skip": "跳过",
    "action_rewind": "回退到…",
    "action_abort": "中止…",
    "attempt_limit": "已达尝试次数上限",
    "rewind_limit": "已达回退次数上限",
    "cancel_run": "取消运行",
    "timeline": "时间线",
    "timeline_empty": "暂无事件",
    "actor_engine": "工作流",
    "step_line": "工作流步骤 {{index}}/{{total}}",
    "step_line_with_parent": "工作流步骤 {{index}}/{{total}} · {{parent}}",
    "redo_dialog": {
      "title": "重做此步骤",
      "feedback_label": "给智能体的反馈",
      "feedback_placeholder": "需要改什么？",
      "confirm": "重做",
      "cancel": "取消"
    },
    "rewind_dialog": {
      "title": "回退",
      "target_label": "退回到",
      "target_self": "{{title}}（当前步骤）",
      "feedback_label": "给智能体的反馈",
      "feedback_placeholder": "需要改什么？",
      "confirm": "回退",
      "cancel": "取消"
    },
    "abort_dialog": {
      "title": "中止这次运行？",
      "reason_label": "原因",
      "reason_placeholder": "为什么要停止这次运行？",
      "confirm": "中止运行",
      "cancel": "取消"
    },
    "cancel_dialog": {
      "title": "取消这次运行？",
      "description": "运行中的步骤会停止，未完成的步骤任务会被取消。",
      "keep": "继续运行",
      "confirm": "取消运行"
    },
    "toast": {
      "decided": "决定已发送",
      "mismatch": "该步骤的状态已变化，请查看最新状态后重试。",
      "forbidden": "你无权处理这次运行",
      "failed": "发送决定失败",
      "cancelled": "运行已取消",
      "cancel_failed": "取消运行失败"
    }
  },
  "events": {
    "run_started": "运行开始",
    "step_started": "步骤开始",
    "step_finished": "步骤结束",
    "step_failed": "步骤失败",
    "decision": "决定",
    "rewind": "回退",
    "rewind_requested": "请求回退",
    "escalated": "已转交人工处理",
    "run_finished": "运行结束",
    "run_cancelled": "运行已取消",
    "protocol_error": "协议错误",
    "unknown": "事件"
  }
}
```

`packages/views/locales/ko/ext-workflows.json`:

```json
{
  "page": {
    "title": "워크플로",
    "new_button": "새 워크플로",
    "empty": "아직 워크플로가 없습니다.",
    "row_menu": "워크플로 작업",
    "archive_action": "보관",
    "never_run": "실행 없음",
    "table": {
      "name": "워크플로",
      "supervisor": "감독자",
      "nodes": "노드",
      "active_runs": "진행 중인 실행",
      "last_run": "마지막 실행"
    }
  },
  "archive_dialog": {
    "title": "이 워크플로를 보관할까요?",
    "description": "\"{{name}}\"을(를) 보관합니다. 진행 중인 실행은 계속되지만 새 태스크에는 지정할 수 없습니다.",
    "cancel": "취소",
    "confirm": "보관",
    "archiving": "보관 중…",
    "success": "워크플로를 보관했습니다",
    "failed": "워크플로를 보관하지 못했습니다"
  },
  "agent_select": {
    "search_placeholder": "에이전트 검색…",
    "group_my_agents": "내 에이전트",
    "group_workspace_agents": "워크스페이스 에이전트",
    "no_agents": "사용할 수 있는 에이전트가 없습니다. 먼저 에이전트를 만드세요.",
    "archived_suffix": "(보관됨)"
  },
  "create": {
    "title": "새 워크플로",
    "name_label": "이름",
    "name_placeholder": "예: 릴리스 파이프라인",
    "description_label": "설명",
    "description_placeholder": "이 워크플로의 용도",
    "supervisor_label": "감독자 에이전트",
    "supervisor_hint": "각 단계를 검토하고 다음에 할 일을 결정합니다.",
    "supervisor_placeholder": "감독자 에이전트 선택",
    "cancel": "취소",
    "submit": "만들기",
    "submitting": "만드는 중…",
    "toast_created": "워크플로를 만들었습니다",
    "toast_failed": "워크플로를 만들지 못했습니다"
  },
  "detail": {
    "not_found": "워크플로를 찾을 수 없습니다",
    "tabs": {
      "nodes": "노드",
      "runs": "실행"
    },
    "save": "저장",
    "saving": "저장 중…",
    "saved": "워크플로를 저장했습니다",
    "save_failed": "워크플로를 저장하지 못했습니다",
    "discard": "변경 취소",
    "unsaved": "저장하지 않은 변경 사항",
    "validation_failed": "표시된 문제를 고친 뒤 다시 저장하세요.",
    "archive_button": "보관",
    "discard_dialog": {
      "title": "저장하지 않은 변경 사항을 버릴까요?",
      "description": "이 워크플로에 한 수정이 아직 저장되지 않았습니다.",
      "keep_editing": "계속 편집",
      "discard": "버리기"
    }
  },
  "inspector": {
    "name": "이름",
    "description": "설명",
    "description_placeholder": "설명 없음",
    "supervisor": "감독자",
    "max_rewinds": "최대 되돌리기",
    "max_rewinds_hint": "한 번의 실행이 이전 단계로 돌아갈 수 있는 횟수입니다.",
    "created_by": "만든 사람",
    "updated": "업데이트"
  },
  "nodes": {
    "add": "노드 추가",
    "empty": "아직 노드가 없습니다. 첫 단계를 추가하세요.",
    "active_runs_note_other": "진행 중인 실행 {{count}}개는 이전 정의를 계속 사용합니다.",
    "node_n": "노드 {{n}}",
    "title_label": "제목",
    "title_placeholder": "단계 제목",
    "key_label": "키",
    "agent_label": "에이전트",
    "agent_placeholder": "에이전트 선택",
    "depends_label": "선행 노드",
    "depends_none": "없음",
    "depends_no_options": "순환을 만들지 않고 추가할 수 있는 노드가 없습니다.",
    "review_label": "검토",
    "max_attempts_label": "최대 시도",
    "prompt_label": "프롬프트",
    "prompt_placeholder": "이 단계에서 에이전트가 무엇을 해야 하나요?",
    "expand": "프롬프트 보기",
    "collapse": "프롬프트 숨기기",
    "move_up": "위로 이동",
    "move_down": "아래로 이동",
    "delete": "노드 삭제",
    "archived_agent": "보관된 에이전트입니다. 다른 에이전트를 선택하세요."
  },
  "errors": {
    "key_required": "키를 입력하세요",
    "key_invalid": "영문 소문자, 숫자, - 또는 _ 를 1~40자로 입력하세요",
    "key_duplicate": "이미 사용 중인 키입니다",
    "title_required": "제목을 입력하세요",
    "agent_required": "에이전트를 선택하세요",
    "prompt_too_long": "프롬프트가 너무 깁니다 (최대 20,000바이트)",
    "max_attempts_range": "최대 시도는 1~10이어야 합니다",
    "self_dependency": "노드는 자기 자신에 의존할 수 없습니다",
    "unknown_dependency": "존재하지 않는 노드에 의존합니다",
    "cycle": "의존 관계가 순환합니다",
    "too_many_nodes": "노드는 최대 50개입니다",
    "no_nodes": "노드를 하나 이상 추가하세요"
  },
  "dag": {
    "aria_label": "워크플로 그래프",
    "empty": "노드를 추가하면 흐름이 표시됩니다."
  },
  "runs": {
    "empty": "아직 실행이 없습니다. 이 워크플로에 태스크를 지정하면 실행이 시작됩니다.",
    "col_issue": "태스크",
    "col_status": "상태",
    "col_started": "시작",
    "col_finished": "종료",
    "showing": "{{total}}개 중 {{shown}}개 표시"
  },
  "run_status": {
    "running": "실행 중",
    "waiting_human": "확인 필요",
    "done": "완료",
    "failed": "실패",
    "cancelled": "취소됨",
    "unknown": "알 수 없음"
  },
  "step_status": {
    "pending": "대기 중",
    "running": "실행 중",
    "awaiting_supervisor": "감독자 검토 중",
    "awaiting_human": "확인 필요",
    "done": "완료",
    "skipped": "건너뜀",
    "failed": "실패",
    "cancelled": "취소됨",
    "unknown": "알 수 없음"
  },
  "run_section": {
    "title": "워크플로 실행",
    "rewinds": "되돌리기 {{used}}/{{max}}",
    "attempts": "시도 {{attempts}}/{{max}}",
    "open_child": "단계 태스크 열기",
    "open_workflow": "워크플로 열기",
    "needs_decision": "결정이 필요합니다",
    "action_approve": "승인",
    "action_redo": "다시 하기…",
    "action_retry": "재시도",
    "action_skip": "건너뛰기",
    "action_rewind": "되돌리기…",
    "action_abort": "중단…",
    "attempt_limit": "시도 한도에 도달했습니다",
    "rewind_limit": "되돌리기 한도에 도달했습니다",
    "cancel_run": "실행 취소",
    "timeline": "타임라인",
    "timeline_empty": "아직 이벤트가 없습니다",
    "actor_engine": "워크플로",
    "step_line": "워크플로 단계 {{index}}/{{total}}",
    "step_line_with_parent": "워크플로 단계 {{index}}/{{total}} · {{parent}}",
    "redo_dialog": {
      "title": "이 단계 다시 하기",
      "feedback_label": "에이전트에게 전달할 피드백",
      "feedback_placeholder": "무엇을 바꿔야 하나요?",
      "confirm": "다시 하기",
      "cancel": "취소"
    },
    "rewind_dialog": {
      "title": "되돌리기",
      "target_label": "돌아갈 단계",
      "target_self": "{{title}} (현재 단계)",
      "feedback_label": "에이전트에게 전달할 피드백",
      "feedback_placeholder": "무엇을 바꿔야 하나요?",
      "confirm": "되돌리기",
      "cancel": "취소"
    },
    "abort_dialog": {
      "title": "실행을 중단할까요?",
      "reason_label": "사유",
      "reason_placeholder": "이 실행을 중단하는 이유는 무엇인가요?",
      "confirm": "실행 중단",
      "cancel": "취소"
    },
    "cancel_dialog": {
      "title": "이 실행을 취소할까요?",
      "description": "실행 중인 단계는 중지되고 열려 있는 단계 태스크는 취소됩니다.",
      "keep": "계속 실행",
      "confirm": "실행 취소"
    },
    "toast": {
      "decided": "결정을 보냈습니다",
      "mismatch": "이 단계가 변경되었습니다. 새 상태를 확인한 뒤 다시 시도하세요.",
      "forbidden": "이 실행에 대해 결정할 권한이 없습니다",
      "failed": "결정을 보내지 못했습니다",
      "cancelled": "실행을 취소했습니다",
      "cancel_failed": "실행을 취소하지 못했습니다"
    }
  },
  "events": {
    "run_started": "실행 시작",
    "step_started": "단계 시작",
    "step_finished": "단계 종료",
    "step_failed": "단계 실패",
    "decision": "결정",
    "rewind": "되돌리기",
    "rewind_requested": "되돌리기 요청",
    "escalated": "사람에게 전달됨",
    "run_finished": "실행 종료",
    "run_cancelled": "실행 취소됨",
    "protocol_error": "프로토콜 오류",
    "unknown": "이벤트"
  }
}
```

`packages/views/locales/ja/ext-workflows.json`:

```json
{
  "page": {
    "title": "ワークフロー",
    "new_button": "新しいワークフロー",
    "empty": "ワークフローはまだありません。",
    "row_menu": "ワークフローの操作",
    "archive_action": "アーカイブ",
    "never_run": "未実行",
    "table": {
      "name": "ワークフロー",
      "supervisor": "スーパーバイザー",
      "nodes": "ノード",
      "active_runs": "実行中",
      "last_run": "最終実行"
    }
  },
  "archive_dialog": {
    "title": "このワークフローをアーカイブしますか？",
    "description": "「{{name}}」をアーカイブします。実行中のものは継続しますが、新しいタスクには割り当てられなくなります。",
    "cancel": "キャンセル",
    "confirm": "アーカイブ",
    "archiving": "アーカイブ中…",
    "success": "ワークフローをアーカイブしました",
    "failed": "ワークフローをアーカイブできませんでした"
  },
  "agent_select": {
    "search_placeholder": "エージェントを検索…",
    "group_my_agents": "自分のエージェント",
    "group_workspace_agents": "ワークスペースのエージェント",
    "no_agents": "使えるエージェントがありません。先にエージェントを作成してください。",
    "archived_suffix": "（アーカイブ済み）"
  },
  "create": {
    "title": "新しいワークフロー",
    "name_label": "名前",
    "name_placeholder": "例：リリースパイプライン",
    "description_label": "説明",
    "description_placeholder": "このワークフローの用途",
    "supervisor_label": "スーパーバイザーのエージェント",
    "supervisor_hint": "各ステップをレビューし、次に何をするかを決めます。",
    "supervisor_placeholder": "スーパーバイザーのエージェントを選択",
    "cancel": "キャンセル",
    "submit": "作成",
    "submitting": "作成中…",
    "toast_created": "ワークフローを作成しました",
    "toast_failed": "ワークフローを作成できませんでした"
  },
  "detail": {
    "not_found": "ワークフローが見つかりません",
    "tabs": {
      "nodes": "ノード",
      "runs": "実行"
    },
    "save": "保存",
    "saving": "保存中…",
    "saved": "ワークフローを保存しました",
    "save_failed": "ワークフローを保存できませんでした",
    "discard": "変更を破棄",
    "unsaved": "未保存の変更があります",
    "validation_failed": "強調表示された問題を直してから、もう一度保存してください。",
    "archive_button": "アーカイブ",
    "discard_dialog": {
      "title": "未保存の変更を破棄しますか？",
      "description": "このワークフローへの編集はまだ保存されていません。",
      "keep_editing": "編集を続ける",
      "discard": "破棄"
    }
  },
  "inspector": {
    "name": "名前",
    "description": "説明",
    "description_placeholder": "説明なし",
    "supervisor": "スーパーバイザー",
    "max_rewinds": "最大巻き戻し回数",
    "max_rewinds_hint": "1 回の実行が以前のステップへ戻れる回数です。",
    "created_by": "作成者",
    "updated": "更新"
  },
  "nodes": {
    "add": "ノードを追加",
    "empty": "ノードはまだありません。最初のステップを追加してください。",
    "active_runs_note_other": "実行中の {{count}} 件は、以前の定義を使い続けます。",
    "node_n": "ノード {{n}}",
    "title_label": "タイトル",
    "title_placeholder": "ステップのタイトル",
    "key_label": "キー",
    "agent_label": "エージェント",
    "agent_placeholder": "エージェントを選択",
    "depends_label": "依存先",
    "depends_none": "なし",
    "depends_no_options": "循環を作らずに追加できるノードはありません。",
    "review_label": "レビュー",
    "max_attempts_label": "最大試行回数",
    "prompt_label": "プロンプト",
    "prompt_placeholder": "このステップでエージェントに何をさせますか？",
    "expand": "プロンプトを表示",
    "collapse": "プロンプトを隠す",
    "move_up": "上へ移動",
    "move_down": "下へ移動",
    "delete": "ノードを削除",
    "archived_agent": "このエージェントはアーカイブ済みです。別のエージェントを選んでください。"
  },
  "errors": {
    "key_required": "キーを入力してください",
    "key_invalid": "小文字の英字、数字、- または _ を 1〜40 文字で入力してください",
    "key_duplicate": "このキーはすでに使われています",
    "title_required": "タイトルを入力してください",
    "agent_required": "エージェントを選択してください",
    "prompt_too_long": "プロンプトが長すぎます（最大 20,000 バイト）",
    "max_attempts_range": "最大試行回数は 1〜10 にしてください",
    "self_dependency": "ノードは自分自身に依存できません",
    "unknown_dependency": "存在しないノードに依存しています",
    "cycle": "依存関係が循環しています",
    "too_many_nodes": "ノードは最大 50 個です",
    "no_nodes": "ノードを 1 つ以上追加してください"
  },
  "dag": {
    "aria_label": "ワークフローの図",
    "empty": "ノードを追加すると流れが表示されます。"
  },
  "runs": {
    "empty": "実行はまだありません。このワークフローにタスクを割り当てると開始されます。",
    "col_issue": "タスク",
    "col_status": "ステータス",
    "col_started": "開始",
    "col_finished": "終了",
    "showing": "{{total}} 件中 {{shown}} 件を表示"
  },
  "run_status": {
    "running": "実行中",
    "waiting_human": "対応が必要",
    "done": "完了",
    "failed": "失敗",
    "cancelled": "キャンセル済み",
    "unknown": "不明"
  },
  "step_status": {
    "pending": "待機中",
    "running": "実行中",
    "awaiting_supervisor": "スーパーバイザーがレビュー中",
    "awaiting_human": "対応が必要",
    "done": "完了",
    "skipped": "スキップ",
    "failed": "失敗",
    "cancelled": "キャンセル済み",
    "unknown": "不明"
  },
  "run_section": {
    "title": "ワークフローの実行",
    "rewinds": "巻き戻し {{used}}/{{max}}",
    "attempts": "試行 {{attempts}}/{{max}}",
    "open_child": "ステップのタスクを開く",
    "open_workflow": "ワークフローを開く",
    "needs_decision": "判断が必要です",
    "action_approve": "承認",
    "action_redo": "やり直し…",
    "action_retry": "再試行",
    "action_skip": "スキップ",
    "action_rewind": "巻き戻し先…",
    "action_abort": "中止…",
    "attempt_limit": "試行回数の上限に達しました",
    "rewind_limit": "巻き戻し回数の上限に達しました",
    "cancel_run": "実行をキャンセル",
    "timeline": "タイムライン",
    "timeline_empty": "イベントはまだありません",
    "actor_engine": "ワークフロー",
    "step_line": "ワークフローのステップ {{index}}/{{total}}",
    "step_line_with_parent": "ワークフローのステップ {{index}}/{{total}} · {{parent}}",
    "redo_dialog": {
      "title": "このステップをやり直す",
      "feedback_label": "エージェントへのフィードバック",
      "feedback_placeholder": "何を変えるべきですか？",
      "confirm": "やり直す",
      "cancel": "キャンセル"
    },
    "rewind_dialog": {
      "title": "巻き戻し",
      "target_label": "戻り先",
      "target_self": "{{title}}（このステップ）",
      "feedback_label": "エージェントへのフィードバック",
      "feedback_placeholder": "何を変えるべきですか？",
      "confirm": "巻き戻す",
      "cancel": "キャンセル"
    },
    "abort_dialog": {
      "title": "実行を中止しますか？",
      "reason_label": "理由",
      "reason_placeholder": "この実行を止める理由は何ですか？",
      "confirm": "実行を中止",
      "cancel": "キャンセル"
    },
    "cancel_dialog": {
      "title": "この実行をキャンセルしますか？",
      "description": "実行中のステップは停止し、未完了のステップのタスクはキャンセルされます。",
      "keep": "実行を続ける",
      "confirm": "実行をキャンセル"
    },
    "toast": {
      "decided": "判断を送信しました",
      "mismatch": "このステップは変更されました。新しい状態を確認してからもう一度お試しください。",
      "forbidden": "この実行について判断する権限がありません",
      "failed": "判断を送信できませんでした",
      "cancelled": "実行をキャンセルしました",
      "cancel_failed": "実行をキャンセルできませんでした"
    }
  },
  "events": {
    "run_started": "実行開始",
    "step_started": "ステップ開始",
    "step_finished": "ステップ終了",
    "step_failed": "ステップ失敗",
    "decision": "判断",
    "rewind": "巻き戻し",
    "rewind_requested": "巻き戻しをリクエスト",
    "escalated": "人に引き継ぎ",
    "run_finished": "実行終了",
    "run_cancelled": "実行キャンセル",
    "protocol_error": "プロトコルエラー",
    "unknown": "イベント"
  }
}
```

`packages/views/locales/fr/ext-workflows.json`:

```json
{
  "page": {
    "title": "Workflows",
    "new_button": "Nouveau workflow",
    "empty": "Aucun workflow pour le moment.",
    "row_menu": "Actions du workflow",
    "archive_action": "Archiver",
    "never_run": "Jamais",
    "table": {
      "name": "Workflow",
      "supervisor": "Superviseur",
      "nodes": "Nœuds",
      "active_runs": "Exécutions actives",
      "last_run": "Dernière exécution"
    }
  },
  "archive_dialog": {
    "title": "Archiver ce workflow ?",
    "description": "« {{name}} » sera archivé. Les exécutions en cours continuent, mais il ne pourra plus être assigné à de nouvelles tâches.",
    "cancel": "Annuler",
    "confirm": "Archiver",
    "archiving": "Archivage…",
    "success": "Workflow archivé",
    "failed": "Impossible d'archiver le workflow"
  },
  "agent_select": {
    "search_placeholder": "Rechercher des agents…",
    "group_my_agents": "Mes agents",
    "group_workspace_agents": "Agents de l'espace de travail",
    "no_agents": "Aucun agent disponible. Créez d'abord un agent.",
    "archived_suffix": "(archivé)"
  },
  "create": {
    "title": "Nouveau workflow",
    "name_label": "Nom",
    "name_placeholder": "p. ex. Pipeline de publication",
    "description_label": "Description",
    "description_placeholder": "À quoi sert ce workflow",
    "supervisor_label": "Agent superviseur",
    "supervisor_hint": "Examine chaque étape et décide de la suite.",
    "supervisor_placeholder": "Choisir un agent superviseur",
    "cancel": "Annuler",
    "submit": "Créer",
    "submitting": "Création…",
    "toast_created": "Workflow créé",
    "toast_failed": "Impossible de créer le workflow"
  },
  "detail": {
    "not_found": "Workflow introuvable",
    "tabs": {
      "nodes": "Nœuds",
      "runs": "Exécutions"
    },
    "save": "Enregistrer",
    "saving": "Enregistrement…",
    "saved": "Workflow enregistré",
    "save_failed": "Impossible d'enregistrer le workflow",
    "discard": "Annuler les modifications",
    "unsaved": "Modifications non enregistrées",
    "validation_failed": "Corrigez les problèmes signalés, puis enregistrez à nouveau.",
    "archive_button": "Archiver",
    "discard_dialog": {
      "title": "Abandonner les modifications non enregistrées ?",
      "description": "Vos modifications de ce workflow n'ont pas été enregistrées.",
      "keep_editing": "Continuer la modification",
      "discard": "Abandonner"
    }
  },
  "inspector": {
    "name": "Nom",
    "description": "Description",
    "description_placeholder": "Aucune description",
    "supervisor": "Superviseur",
    "max_rewinds": "Retours en arrière max.",
    "max_rewinds_hint": "Nombre de fois où une exécution peut revenir à une étape précédente.",
    "created_by": "Créé par",
    "updated": "Mis à jour"
  },
  "nodes": {
    "add": "Ajouter un nœud",
    "empty": "Aucun nœud pour le moment. Ajoutez la première étape.",
    "active_runs_note_one": "{{count}} exécution en cours continue d'utiliser l'ancienne définition.",
    "active_runs_note_other": "{{count}} exécutions en cours continuent d'utiliser l'ancienne définition.",
    "node_n": "Nœud {{n}}",
    "title_label": "Titre",
    "title_placeholder": "Titre de l'étape",
    "key_label": "Clé",
    "agent_label": "Agent",
    "agent_placeholder": "Choisir un agent",
    "depends_label": "Dépend de",
    "depends_none": "Aucun",
    "depends_no_options": "Aucun autre nœud ne peut être ajouté sans créer de cycle.",
    "review_label": "Revue",
    "max_attempts_label": "Tentatives max.",
    "prompt_label": "Prompt",
    "prompt_placeholder": "Que doit faire l'agent à cette étape ?",
    "expand": "Afficher le prompt",
    "collapse": "Masquer le prompt",
    "move_up": "Monter",
    "move_down": "Descendre",
    "delete": "Supprimer le nœud",
    "archived_agent": "Cet agent est archivé. Choisissez un autre agent."
  },
  "errors": {
    "key_required": "La clé est obligatoire",
    "key_invalid": "Utilisez 1 à 40 lettres minuscules, chiffres, - ou _",
    "key_duplicate": "Cette clé est déjà utilisée",
    "title_required": "Le titre est obligatoire",
    "agent_required": "Choisissez un agent",
    "prompt_too_long": "Le prompt est trop long (20 000 octets max.)",
    "max_attempts_range": "Les tentatives max. doivent être comprises entre 1 et 10",
    "self_dependency": "Un nœud ne peut pas dépendre de lui-même",
    "unknown_dependency": "Dépend d'un nœud qui n'existe pas",
    "cycle": "Les dépendances forment un cycle",
    "too_many_nodes": "50 nœuds au maximum",
    "no_nodes": "Ajoutez au moins un nœud"
  },
  "dag": {
    "aria_label": "Graphe du workflow",
    "empty": "Ajoutez des nœuds pour voir le déroulement."
  },
  "runs": {
    "empty": "Aucune exécution. Assignez une tâche à ce workflow pour en lancer une.",
    "col_issue": "Tâche",
    "col_status": "Statut",
    "col_started": "Début",
    "col_finished": "Fin",
    "showing": "{{shown}} sur {{total}} affichées"
  },
  "run_status": {
    "running": "En cours",
    "waiting_human": "Votre action requise",
    "done": "Terminée",
    "failed": "Échouée",
    "cancelled": "Annulée",
    "unknown": "Inconnu"
  },
  "step_status": {
    "pending": "En attente",
    "running": "En cours",
    "awaiting_supervisor": "Revue par le superviseur",
    "awaiting_human": "Votre action requise",
    "done": "Terminée",
    "skipped": "Ignorée",
    "failed": "Échouée",
    "cancelled": "Annulée",
    "unknown": "Inconnu"
  },
  "run_section": {
    "title": "Exécution du workflow",
    "rewinds": "Retours en arrière {{used}}/{{max}}",
    "attempts": "Tentative {{attempts}}/{{max}}",
    "open_child": "Ouvrir la tâche de l'étape",
    "open_workflow": "Ouvrir le workflow",
    "needs_decision": "Une décision de votre part est nécessaire",
    "action_approve": "Approuver",
    "action_redo": "Refaire…",
    "action_retry": "Réessayer",
    "action_skip": "Ignorer",
    "action_rewind": "Revenir à…",
    "action_abort": "Abandonner…",
    "attempt_limit": "Limite de tentatives atteinte",
    "rewind_limit": "Limite de retours en arrière atteinte",
    "cancel_run": "Annuler l'exécution",
    "timeline": "Chronologie",
    "timeline_empty": "Aucun événement pour le moment",
    "actor_engine": "Workflow",
    "step_line": "Étape {{index}} sur {{total}} du workflow",
    "step_line_with_parent": "Étape {{index}} sur {{total}} du workflow · {{parent}}",
    "redo_dialog": {
      "title": "Refaire cette étape",
      "feedback_label": "Retour pour l'agent",
      "feedback_placeholder": "Que faut-il changer ?",
      "confirm": "Refaire",
      "cancel": "Annuler"
    },
    "rewind_dialog": {
      "title": "Revenir en arrière",
      "target_label": "Revenir à",
      "target_self": "{{title}} (cette étape)",
      "feedback_label": "Retour pour l'agent",
      "feedback_placeholder": "Que faut-il changer ?",
      "confirm": "Revenir en arrière",
      "cancel": "Annuler"
    },
    "abort_dialog": {
      "title": "Abandonner l'exécution ?",
      "reason_label": "Motif",
      "reason_placeholder": "Pourquoi cette exécution est-elle arrêtée ?",
      "confirm": "Abandonner l'exécution",
      "cancel": "Annuler"
    },
    "cancel_dialog": {
      "title": "Annuler cette exécution ?",
      "description": "Les étapes en cours s'arrêtent et les tâches d'étape ouvertes sont annulées.",
      "keep": "Continuer l'exécution",
      "confirm": "Annuler l'exécution"
    },
    "toast": {
      "decided": "Décision envoyée",
      "mismatch": "Cette étape a changé. Consultez son nouvel état puis réessayez.",
      "forbidden": "Vous ne pouvez pas décider pour cette exécution",
      "failed": "Impossible d'envoyer la décision",
      "cancelled": "Exécution annulée",
      "cancel_failed": "Impossible d'annuler l'exécution"
    }
  },
  "events": {
    "run_started": "Exécution démarrée",
    "step_started": "Étape démarrée",
    "step_finished": "Étape terminée",
    "step_failed": "Étape échouée",
    "decision": "Décision",
    "rewind": "Retour en arrière",
    "rewind_requested": "Retour en arrière demandé",
    "escalated": "Transmis à un humain",
    "run_finished": "Exécution terminée",
    "run_cancelled": "Exécution annulée",
    "protocol_error": "Erreur de protocole",
    "unknown": "Événement"
  }
}
```

3b. Register in `packages/views/locales/index.ts`. Directly after each locale's `Squads` import add the matching line:

```ts
import enExtWorkflows from "./en/ext-workflows.json";
import zhHansExtWorkflows from "./zh-Hans/ext-workflows.json";
import koExtWorkflows from "./ko/ext-workflows.json";
import jaExtWorkflows from "./ja/ext-workflows.json";
import frExtWorkflows from "./fr/ext-workflows.json";
```

In each locale object of `RESOURCES`, directly after `squads: <x>Squads,` add:

```ts
    "ext-workflows": enExtWorkflows,
```
(and `zhHansExtWorkflows`, `koExtWorkflows`, `jaExtWorkflows`, `frExtWorkflows` in their own blocks).

3c. `packages/views/i18n/resources-types.ts`: after `import type squads from "../locales/en/squads.json";` add

```ts
import type extWorkflows from "../locales/en/ext-workflows.json";
```

and in `interface I18nResources`, after `squads: typeof squads;` add

```ts
    "ext-workflows": typeof extWorkflows;
```

3d. Cross-namespace keys (from the repo root; `export CLAUDE_SCRATCH=...` and the helper from the notes section must exist):

```bash
A=$CLAUDE_SCRATCH/add-key.mjs; L=packages/views/locales
# layout.json: nav.workflows after squads, tab.workflow after squad
node $A $L/en/layout.json nav squads workflows '"Workflows"'
node $A $L/zh-Hans/layout.json nav squads workflows '"工作流"'
node $A $L/ko/layout.json nav squads workflows '"워크플로"'
node $A $L/ja/layout.json nav squads workflows '"ワークフロー"'
node $A $L/fr/layout.json nav squads workflows '"Workflows"'
node $A $L/en/layout.json tab squad workflow '"Workflow"'
node $A $L/zh-Hans/layout.json tab squad workflow '"工作流"'
node $A $L/ko/layout.json tab squad workflow '"워크플로"'
node $A $L/ja/layout.json tab squad workflow '"ワークフロー"'
node $A $L/fr/layout.json tab squad workflow '"Workflow"'
# settings.json: shortcuts.actions.goWorkflows after goSquads
node $A $L/en/settings.json shortcuts.actions goSquads goWorkflows '{"label":"Go to Workflows","description":"Open the workflow list."}'
node $A $L/zh-Hans/settings.json shortcuts.actions goSquads goWorkflows '{"label":"前往工作流","description":"打开工作流列表。"}'
node $A $L/ko/settings.json shortcuts.actions goSquads goWorkflows '{"label":"워크플로로 이동","description":"워크플로 목록을 엽니다."}'
node $A $L/ja/settings.json shortcuts.actions goSquads goWorkflows '{"label":"ワークフローへ移動","description":"ワークフロー一覧を開きます。"}'
node $A $L/fr/settings.json shortcuts.actions goSquads goWorkflows '{"label":"Aller aux workflows","description":"Ouvrir la liste des workflows."}'
# issues.json: pickers.assignee
node $A $L/en/issues.json pickers.assignee squads_group workflows_group '"Workflows"'
node $A $L/zh-Hans/issues.json pickers.assignee squads_group workflows_group '"工作流"'
node $A $L/ko/issues.json pickers.assignee squads_group workflows_group '"워크플로"'
node $A $L/ja/issues.json pickers.assignee squads_group workflows_group '"ワークフロー"'
node $A $L/fr/issues.json pickers.assignee squads_group workflows_group '"Workflows"'
node $A $L/en/issues.json pickers.assignee squad_runtime_required workflow_needs_nodes '"Add nodes to this workflow before assigning work"'
node $A $L/zh-Hans/issues.json pickers.assignee squad_runtime_required workflow_needs_nodes '"请先为该工作流添加节点，再分配工作"'
node $A $L/ko/issues.json pickers.assignee squad_runtime_required workflow_needs_nodes '"작업을 할당하기 전에 이 워크플로에 노드를 추가하세요"'
node $A $L/ja/issues.json pickers.assignee squad_runtime_required workflow_needs_nodes '"作業を割り当てる前に、このワークフローにノードを追加してください"'
node $A $L/fr/issues.json pickers.assignee squad_runtime_required workflow_needs_nodes '"Ajoutez des nœuds à ce workflow avant d'"'"'assigner du travail"'
# modals.json: run_confirm
node $A $L/en/modals.json run_confirm create_will_start_squad create_will_start_workflow '"{{name}} will run its steps right after creation."'
node $A $L/zh-Hans/modals.json run_confirm create_will_start_squad create_will_start_workflow '"创建后将立即运行 {{name}} 的各个步骤。"'
node $A $L/ko/modals.json run_confirm create_will_start_squad create_will_start_workflow '"생성 직후 {{name}}의 단계가 실행됩니다."'
node $A $L/ja/modals.json run_confirm create_will_start_squad create_will_start_workflow '"作成後すぐに {{name}} のステップが実行されます。"'
node $A $L/fr/modals.json run_confirm create_will_start_squad create_will_start_workflow '"Les étapes de {{name}} s'"'"'exécuteront juste après la création."'
# inbox.json: types.ext_workflow_escalation after children_done
node $A $L/en/inbox.json types children_done ext_workflow_escalation '"Workflow needs a decision"'
node $A $L/zh-Hans/inbox.json types children_done ext_workflow_escalation '"工作流需要你来决定"'
node $A $L/ko/inbox.json types children_done ext_workflow_escalation '"워크플로에 결정이 필요합니다"'
node $A $L/ja/inbox.json types children_done ext_workflow_escalation '"ワークフローの判断が必要です"'
node $A $L/fr/inbox.json types children_done ext_workflow_escalation '"Un workflow attend une décision"'
git diff --stat packages/views/locales
```

Expected from `git diff --stat`: only a few inserted lines per existing JSON file, plus `index.ts` and `resources-types.ts`. If a file shows large churn, `git checkout -- <file>` and re-apply.

- [ ] **Step 4: Run, expect PASS**

```bash
pnpm --filter @multica/views exec vitest run locales/ext-workflows.test.ts locales/parity.test.ts
pnpm --filter @multica/views typecheck
```

- [ ] **Step 5: Commit**

```bash
git add packages/views/locales packages/views/i18n/resources-types.ts
git commit -m "feat(ext-workflow): add ext-workflows i18n namespace and nav/picker/inbox keys

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D2: Navigation scaffolding (sidebar, icon, shortcut, search, tab presentation)

**Files:**
- Modify: `packages/views/layout/app-sidebar.tsx` (`NavKey` at line 122, `NavLabelKey` at line 138, `aiTeamNav` at line 161)
- Modify: `packages/views/layout/route-icon-components.tsx` (lucide import list; `ROUTE_ICON_COMPONENTS` record)
- Modify: `packages/core/shortcuts/definitions.ts` (`ShortcutActionId` union at line 26; array entry at line 118)
- Modify: `packages/views/layout/global-shortcuts.tsx` (`GLOBAL_ACTIONS` at line 36; `destinations` at line 67)
- Modify: `packages/views/search/search-command.tsx` (`PAGE_KEYWORDS` at line 100)
- Modify: `packages/core/paths/tab-subject.ts` (`TabActorType` line 18; `switch` case near line 95)
- Modify: `packages/core/paths/tab-presentation.ts` (`TabLabelKey` near line 48; `ACTOR_LABEL` line 104)
- Modify: `packages/views/layout/tab-presentation.tsx` (imports ~line 14 to 35; `PENDING_RESOURCE_KEYS` line 72; `useTabEntityData` ~lines 153 and 183; `safeVisual` ~line 295)
- Test: `packages/views/layout/app-sidebar.test.tsx`, `packages/views/layout/route-icon-components.test.tsx`, `packages/views/search/search-command.test.tsx`, `packages/views/layout/tab-presentation.test.tsx`, `packages/views/layout/global-shortcuts.chat.test.tsx`, `packages/views/layout/global-shortcuts.history.test.tsx`, `packages/core/paths/tab-subject.test.ts`, `packages/core/paths/tab-presentation.test.ts`

**Interfaces:**
- Consumes (Part A): `WorkspacePageKey` `"workflows"`, `NavLabelKey` `"workflows"` and `RouteIconName` `"Workflow"` in `packages/core/paths/route-icons.ts` (`WORKSPACE_PAGES.workflows = { segment: "workflows", icon: "Workflow", navKey: "workflows" }`), `paths.workflows()`, `paths.workflowDetail(id)`, `extWorkflowListOptions(wsId)` from `@multica/core/ext-workflows`. Verify before starting: `grep -n workflows packages/core/paths/route-icons.ts packages/core/paths/paths.ts`.
- Produces: shortcut action id `goWorkflows`; tab subject `{ kind: "actor", actorType: "workflow", id }`; tab label key `workflow`.

- [ ] **Step 1: Write the failing tests**

(a) `packages/core/paths/tab-subject.test.ts`: in the `cases` table, after the `["/acme/squads/sq1", ...]` row add

```ts
    ["/acme/workflows", { kind: "page", page: "workflows" }],
    ["/acme/workflows/wf1", { kind: "actor", actorType: "workflow", id: "wf1" }],
```

(b) `packages/core/paths/tab-presentation.test.ts`: in `it("actor shows avatar visual and resolved name", ...)` append before the closing `});`

```ts
    expect(present("/acme/workflows/wf1")).toEqual({
      visual: { kind: "actor", actorType: "workflow", id: "wf1" },
      title: { kind: "tab", tabKey: "workflow" },
    });
    expect(present("/acme/workflows/wf1", { actorName: "Release flow" }).title).toEqual({
      kind: "text",
      text: "Release flow",
    });
```

(c) `packages/views/layout/route-icon-components.test.tsx`: add `import { Workflow } from "lucide-react";` and change the first loop list to `[p.projects(), p.autopilots(), p.chat(), p.squads(), p.workflows(), p.usage()]`, then add

```ts
  it("gives Workflows the Workflow glyph, in the sidebar and in tabs", () => {
    const p = paths.workspace("acme");
    expect(routeIconForPath(p.workflows())).toBe(Workflow);
    expect(routeIconForPath(p.workflowDetail("wf1"))).toBe(Workflow);
  });
```

(d) `packages/views/layout/app-sidebar.test.tsx`: in the `useWorkspacePaths` mock (line ~146) add `workflows: () => "/acme/workflows",` after `squads:`. Add this describe at the end of the file:

```tsx
describe("workflows nav entry", () => {
  beforeEach(() => {
    navigation.current = { pathname: "/acme/issues" };
    summary.current = [];
    workspaces.current = [];
  });

  it("shows Workflows in the AI Team group, right after Squads", () => {
    const { container } = renderWithI18n(<AppSidebar />);
    const squads = container.querySelector('button[data-href="/acme/squads"]');
    const workflows = container.querySelector('button[data-href="/acme/workflows"]');
    expect(workflows).not.toBeNull();
    expect(workflows).toHaveTextContent("Workflows");
    expect(
      squads!.compareDocumentPosition(workflows!) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("marks Workflows active on a workflow detail route", () => {
    navigation.current = { pathname: "/acme/workflows/wf1" };
    const { container } = renderWithI18n(<AppSidebar />);
    expect(container.querySelector('button[data-href="/acme/workflows"]')).toHaveAttribute(
      "data-active",
      "true",
    );
  });
});
```

(e) `packages/views/search/search-command.test.tsx`: in the `useWorkspacePaths` mock add `workflows: () => "/ws-test/workflows",` after `squads:` and `workflowDetail: (id: string) => \`/ws-test/workflows/${id}\`,` after `squadDetail`. Add after the "navigates to a page whose label differs" test:

```tsx
it("finds Workflows through its keyword aliases", async () => {
  const user = userEvent.setup();
  renderSearch();
  const input = screen.getByPlaceholderText("Type a command or search...");
  await user.type(input, "pipeline");
  await user.click(await screen.findByText("Workflows"));
  expect(mockPush).toHaveBeenCalledWith("/ws-test/workflows");
});
```

(f) `packages/views/layout/tab-presentation.test.tsx`: add `import { extWorkflowListOptions } from "@multica/core/ext-workflows";`. In `seed()` after the `agentListOptions` seed add

```ts
  qc.setQueryData(extWorkflowListOptions("ws1").queryKey, [
    { id: "wf1", name: "Release flow" },
  ] as never);
```
and after the `"actor: avatar visual + resolved name"` test add

```tsx
  it("workflow actor: avatar visual + workflow name from the list cache", () => {
    expect(presentationOf("/acme/workflows/wf1")).toEqual({
      visual: { kind: "actor", actorType: "workflow", id: "wf1" },
      title: "Release flow",
    });
  });
```

(g) `packages/views/layout/global-shortcuts.chat.test.tsx` and `global-shortcuts.history.test.tsx`: their `useWorkspacePaths` mocks list every nav path, and `GlobalShortcuts` now also calls `workspacePaths.workflows()`. Add `workflows: () => "/acme/workflows",` right after the `squads:` entry in each mock (keep the file's own slug if it is not `acme`).

- [ ] **Step 2: Run, expect FAIL**

```bash
pnpm --filter @multica/core exec vitest run paths/tab-subject.test.ts paths/tab-presentation.test.ts
pnpm --filter @multica/views exec vitest run layout search
```
Expected: FAIL (no workflows nav item, no `workflow` actor type, typecheck-level errors do not matter in vitest).

- [ ] **Step 3: Implement**

`packages/views/layout/app-sidebar.tsx`. Three one-line additions, each commented:

```ts
type NavKey =
  | ...
  | "squads"
  | "workflows" // ext-workflow: nav entry
  | "usage"
```
```ts
type NavLabelKey =
  | ...
  | "squads"
  | "workflows" // ext-workflow: nav entry
  | "usage"
```
```ts
const aiTeamNav: { key: NavKey; labelKey: NavLabelKey }[] = [
  { key: "agents", labelKey: "agents" },
  { key: "squads", labelKey: "squads" },
  { key: "workflows", labelKey: "workflows" }, // ext-workflow: nav entry
  { key: "skills", labelKey: "skills" },
  { key: "runtimes", labelKey: "runtimes" },
];
```

`packages/views/layout/route-icon-components.tsx`: add `Workflow,` to the `lucide-react` import (after `Users,`) and `Workflow,` to `ROUTE_ICON_COMPONENTS` (after `Users,`), with a `// ext-workflow:` comment on the record line.

`packages/core/shortcuts/definitions.ts`: in the `ShortcutActionId` union after `| "goSquads"` add `| "goWorkflows"`; in the definitions array after the `goSquads` row add

```ts
  { id: "goWorkflows", category: "navigation", defaultShortcut: null, allowInEditable: false }, // ext-workflow: nav shortcut
```

`packages/views/layout/global-shortcuts.tsx`: add `"goWorkflows",` after `"goSquads",` in `GLOBAL_ACTIONS`, and `goWorkflows: workspacePaths.workflows(),` after `goSquads: workspacePaths.squads(),` in `destinations`.

`packages/views/search/search-command.tsx` (`PAGE_KEYWORDS`), after the `squads:` line:

```ts
  workflows: ["workflows", "workflow", "pipeline", "flows", "工作流", "流程"], // ext-workflow
```

`packages/core/paths/tab-subject.ts`:

```ts
export type TabActorType = "agent" | "member" | "squad" | "workflow"; // ext-workflow
```
and in the `switch`, after the `case "squads":` block:

```ts
    case "workflows": // ext-workflow
      return id
        ? { kind: "actor", actorType: "workflow", id }
        : { kind: "page", page: "workflows" };
```
Update the doc comment on the `actor` variant to `agent / member / squad / workflow`.

`packages/core/paths/tab-presentation.ts`: add `| "workflow"` to `TabLabelKey` after `| "squad"`, and `workflow: "workflow",` to `ACTOR_LABEL`.

`packages/views/layout/tab-presentation.tsx`:

```ts
import { extWorkflowListOptions } from "@multica/core/ext-workflows"; // ext-workflow
```
`PENDING_RESOURCE_KEYS`: add `"workflow",` after `"squad",`. In `useTabEntityData`, after the `squads` query:

```ts
  const workflows = useQuery({ ...extWorkflowListOptions(wsId), enabled: false }).data; // ext-workflow
```
and replace the actor name expression:

```ts
    case "actor": {
      const name =
        subject.actorType === "agent"
          ? agents?.find((a) => a.id === subject.id)?.name
          : subject.actorType === "member"
            ? members?.find((m) => m.user_id === subject.id)?.name
            : subject.actorType === "workflow" // ext-workflow
              ? workflows?.find((w) => w.id === subject.id)?.name
              : squads?.find((s) => s.id === subject.id)?.name;
```
and the `safeVisual` fallback icon:

```ts
          icon:
            visual.actorType === "squad"
              ? "Users"
              : visual.actorType === "workflow" // ext-workflow
                ? "Workflow"
                : visual.actorType === "member"
                  ? "CircleUser"
                  : "Bot",
```

- [ ] **Step 4: Run, expect PASS**

```bash
pnpm --filter @multica/core exec vitest run paths shortcuts
pnpm --filter @multica/views exec vitest run layout search locales
pnpm --filter @multica/core typecheck && pnpm --filter @multica/views typecheck
```

- [ ] **Step 5: Commit**

```bash
git add packages/views/layout packages/views/search packages/core/shortcuts packages/core/paths
git commit -m "feat(ext-workflow): add workflows nav entry, shortcut, search alias and tab presentation

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D3: Pure editor helpers, DAG layout, status components, agent select

**Files:**
- Create: `packages/views/ext-workflows/node-editor-helpers.ts`
- Create: `packages/views/ext-workflows/status-keys.ts`
- Create: `packages/views/ext-workflows/components/status.tsx`
- Create: `packages/views/ext-workflows/components/agent-select.tsx`
- Test: `packages/views/ext-workflows/node-editor-helpers.test.ts`, `packages/views/ext-workflows/status-keys.test.ts`

**Interfaces:**
- Consumes (core): types `ExtWorkflowNode`, `ExtWorkflowNodeInput`, `ExtWorkflowValidationError` from `@multica/core/ext-workflows` (type-only imports, erased at runtime so the tests can run in the node environment).
- Produces: `NodeRow`, `slugify`, `uniqueKey`, `createRow`, `setRowTitle`, `setRowKey`, `removeRow`, `moveRow`, `descendantIds`, `dependencyOptions`, `validateRows`, `mapServerErrors`, `extractValidationErrors`, `nodesToRows`, `rowsToInput`, `serializeRows`, `computeDepths`, `layoutDag`, `stepStatusKey`, `runStatusKey`, `StepStatusIcon`, `RunStatusBadge`, `AgentSelect`.

Design note: inside the editor, `depends_on` holds **row ids** (not keys), so editing a key never has to rewrite other rows. `rowsToInput` converts back to keys for the API; `nodesToRows` converts keys to row ids.

- [ ] **Step 1: Write the failing tests**

`packages/views/ext-workflows/node-editor-helpers.test.ts`:

```ts
// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  EXT_MAX_NODES,
  computeDepths,
  createRow,
  dependencyOptions,
  descendantIds,
  extractValidationErrors,
  layoutDag,
  mapServerErrors,
  moveRow,
  nodesToRows,
  removeRow,
  rowsToInput,
  serializeRows,
  setRowKey,
  setRowTitle,
  slugify,
  uniqueKey,
  validateRows,
  type NodeRow,
} from "./node-editor-helpers";

function row(id: string, key: string, over: Partial<NodeRow> = {}): NodeRow {
  return {
    rowId: id,
    key,
    keyTouched: true,
    title: key.toUpperCase(),
    agent_id: "agent-1",
    prompt: "",
    requires_review: false,
    max_attempts: 3,
    depends_on: [],
    ...over,
  };
}

describe("slugify / uniqueKey", () => {
  it("lowercases, strips accents and collapses punctuation", () => {
    expect(slugify("Review PR #12!")).toBe("review_pr_12");
    expect(slugify("Café résumé")).toBe("cafe_resume");
    expect(slugify("  --Hello   World--  ")).toBe("hello_world");
  });
  it("returns an empty string when nothing ASCII survives", () => {
    expect(slugify("设计方案")).toBe("");
  });
  it("caps at 40 characters without a trailing underscore", () => {
    const key = slugify(`${"a".repeat(39)} b`);
    expect(key.length).toBeLessThanOrEqual(40);
    expect(key.endsWith("_")).toBe(false);
  });
  it("uniquifies against taken keys and keeps the 40 character cap", () => {
    expect(uniqueKey("plan", new Set(["plan"]))).toBe("plan_2");
    expect(uniqueKey("plan", new Set(["plan", "plan_2"]))).toBe("plan_3");
    const long = "x".repeat(40);
    const next = uniqueKey(long, new Set([long]));
    expect(next.length).toBe(40);
    expect(next.endsWith("_2")).toBe(true);
  });
});

describe("row editing", () => {
  it("derives the key from the title until the key is edited by hand", () => {
    let rows = [createRow([])];
    rows[0] = { ...rows[0]!, keyTouched: false };
    const id = rows[0]!.rowId;
    rows = setRowTitle(rows, id, "Write the Spec");
    expect(rows[0]!.key).toBe("write_the_spec");
    rows = setRowKey(rows, id, "spec");
    rows = setRowTitle(rows, id, "Write the Spec v2");
    expect(rows[0]!.key).toBe("spec");
    expect(rows[0]!.keyTouched).toBe(true);
  });
  it("keeps derived keys unique across rows and falls back to node_N", () => {
    const a = { ...row("a", "plan"), keyTouched: true };
    const b = { ...createRow([a]), keyTouched: false };
    const rows = setRowTitle([a, b], b.rowId, "Plan");
    expect(rows[1]!.key).toBe("plan_2");
    const c = { ...createRow(rows), keyTouched: false };
    const rows2 = setRowTitle([...rows, c], c.rowId, "设计");
    expect(rows2[2]!.key).toMatch(/^node_3/);
  });
  it("removing a row strips it from every depends_on", () => {
    const rows = [row("a", "a"), row("b", "b", { depends_on: ["a"] }), row("c", "c", { depends_on: ["a", "b"] })];
    const next = removeRow(rows, "a");
    expect(next.map((r) => r.rowId)).toEqual(["b", "c"]);
    expect(next[0]!.depends_on).toEqual([]);
    expect(next[1]!.depends_on).toEqual(["b"]);
  });
  it("moves rows up and down and ignores out-of-range moves", () => {
    const rows = [row("a", "a"), row("b", "b"), row("c", "c")];
    expect(moveRow(rows, 1, -1).map((r) => r.rowId)).toEqual(["b", "a", "c"]);
    expect(moveRow(rows, 1, 1).map((r) => r.rowId)).toEqual(["a", "c", "b"]);
    expect(moveRow(rows, 0, -1)).toBe(rows);
    expect(moveRow(rows, 2, 1)).toBe(rows);
  });
});

describe("cycle-safe dependency options", () => {
  // a <- b <- c  (b depends on a, c depends on b); d is independent.
  const rows = [
    row("a", "a"),
    row("b", "b", { depends_on: ["a"] }),
    row("c", "c", { depends_on: ["b"] }),
    row("d", "d"),
  ];
  it("descendantIds returns everything downstream, transitively", () => {
    expect([...descendantIds(rows, "a")].sort()).toEqual(["b", "c"]);
    expect([...descendantIds(rows, "b")]).toEqual(["c"]);
    expect([...descendantIds(rows, "d")]).toEqual([]);
  });
  it("excludes self and every downstream node, keeps the rest", () => {
    expect(dependencyOptions(rows, "a").map((r) => r.rowId)).toEqual(["d"]);
    expect(dependencyOptions(rows, "b").map((r) => r.rowId)).toEqual(["a", "d"]);
    expect(dependencyOptions(rows, "c").map((r) => r.rowId)).toEqual(["a", "b", "d"]);
    expect(dependencyOptions(rows, "d").map((r) => r.rowId)).toEqual(["a", "b", "c"]);
  });
  it("never offers an option that would close a cycle", () => {
    for (const target of rows) {
      for (const option of dependencyOptions(rows, target.rowId)) {
        const next = rows.map((r) =>
          r.rowId === target.rowId ? { ...r, depends_on: [...r.depends_on, option.rowId] } : r,
        );
        expect(validateRows(next).rows[target.rowId]?.depends_on).toBeUndefined();
      }
    }
  });
  it("terminates on an already-cyclic graph", () => {
    const cyclic = [row("a", "a", { depends_on: ["b"] }), row("b", "b", { depends_on: ["a"] })];
    expect(() => descendantIds(cyclic, "a")).not.toThrow();
  });
});

describe("validateRows", () => {
  it("accepts a valid graph", () => {
    const rows = [row("a", "a"), row("b", "b", { depends_on: ["a"] })];
    const v = validateRows(rows);
    expect(v.valid).toBe(true);
    expect(v.rows).toEqual({});
    expect(v.general).toEqual([]);
  });
  it("flags key problems", () => {
    const v = validateRows([
      row("a", ""),
      row("b", "Bad Key"),
      row("c", "dup"),
      row("d", "dup"),
    ]);
    expect(v.rows.a?.key).toBe("key_required");
    expect(v.rows.b?.key).toBe("key_invalid");
    expect(v.rows.c?.key).toBe("key_duplicate");
    expect(v.rows.d?.key).toBe("key_duplicate");
    expect(v.valid).toBe(false);
  });
  it("flags title, agent, attempts and prompt size", () => {
    const v = validateRows([
      row("a", "a", { title: "  ", agent_id: "", max_attempts: 11, prompt: "x".repeat(20001) }),
      row("b", "b", { max_attempts: 0 }),
      row("c", "c", { max_attempts: 2.5 }),
      row("d", "d", { prompt: "é".repeat(10001) }),
    ]);
    expect(v.rows.a).toMatchObject({
      title: "title_required",
      agent_id: "agent_required",
      max_attempts: "max_attempts_range",
      prompt: "prompt_too_long",
    });
    expect(v.rows.b?.max_attempts).toBe("max_attempts_range");
    expect(v.rows.c?.max_attempts).toBe("max_attempts_range");
    expect(v.rows.d?.prompt).toBe("prompt_too_long");
  });
  it("flags self, unknown and cyclic dependencies", () => {
    const v = validateRows([
      row("a", "a", { depends_on: ["a"] }),
      row("b", "b", { depends_on: ["ghost"] }),
      row("c", "c", { depends_on: ["d"] }),
      row("d", "d", { depends_on: ["c"] }),
    ]);
    expect(v.rows.a?.depends_on).toBe("self_dependency");
    expect(v.rows.b?.depends_on).toBe("unknown_dependency");
    expect(v.rows.c?.depends_on).toBe("cycle");
    expect(v.rows.d?.depends_on).toBe("cycle");
  });
  it("flags node-count problems", () => {
    expect(validateRows([]).general).toEqual(["no_nodes"]);
    const many = Array.from({ length: EXT_MAX_NODES + 1 }, (_, i) => row(`r${i}`, `n${i}`));
    expect(validateRows(many).general).toEqual(["too_many_nodes"]);
  });
});

describe("server error mapping", () => {
  const rows = [row("a", "plan"), row("b", "build")];
  it("maps node errors onto rows by key and known field", () => {
    const mapped = mapServerErrors(
      [
        { node_key: "build", field: "agent_id", message: "agent is archived" },
        { node_key: "plan", field: "depends_on", message: "cycle: plan -> build -> plan" },
      ],
      rows,
    );
    expect(mapped.byRow.b).toEqual({ agent_id: "agent is archived" });
    expect(mapped.byRow.a).toEqual({ depends_on: "cycle: plan -> build -> plan" });
    expect(mapped.general).toEqual([]);
  });
  it("puts unknown fields on the row and keyless errors in general", () => {
    const mapped = mapServerErrors(
      [
        { node_key: "plan", field: "something_new", message: "nope" },
        { field: "supervisor_agent_id", message: "supervisor is archived" },
        { node_key: "gone", field: "key", message: "stale" },
      ],
      rows,
    );
    expect(mapped.byRow.a).toEqual({ row: "nope" });
    expect(mapped.general).toEqual(["supervisor is archived", "stale"]);
  });
  it("extractValidationErrors duck-types the typed 422 error", () => {
    const err = Object.assign(new Error("validation_failed"), {
      errors: [{ node_key: "plan", field: "key", message: "bad" }],
    });
    expect(extractValidationErrors(err)).toEqual([{ node_key: "plan", field: "key", message: "bad" }]);
    expect(extractValidationErrors(new Error("boom"))).toBeNull();
    expect(extractValidationErrors({ errors: "nope" })).toBeNull();
    expect(extractValidationErrors(null)).toBeNull();
  });
});

describe("API round trip", () => {
  const nodes = [
    { id: "n2", key: "build", title: "Build", agent_id: "ag", prompt: "p2", requires_review: true, max_attempts: 2, depends_on: ["plan"], position: 1 },
    { id: "n1", key: "plan", title: "Plan", agent_id: "ag", prompt: "p1", requires_review: false, max_attempts: 3, depends_on: [], position: 0 },
  ];
  it("sorts by position, resolves dependencies to row ids and back to keys", () => {
    const rows = nodesToRows(nodes);
    expect(rows.map((r) => r.key)).toEqual(["plan", "build"]);
    expect(rows[1]!.depends_on).toEqual([rows[0]!.rowId]);
    expect(rowsToInput(rows)).toEqual([
      { key: "plan", title: "Plan", agent_id: "ag", prompt: "p1", requires_review: false, max_attempts: 3, depends_on: [] },
      { key: "build", title: "Build", agent_id: "ag", prompt: "p2", requires_review: true, max_attempts: 2, depends_on: ["plan"] },
    ]);
  });
  it("keeps an unknown dependency visible so validation can flag it", () => {
    const rows = nodesToRows([{ ...nodes[1]!, depends_on: ["ghost"] }]);
    expect(validateRows(rows).rows[rows[0]!.rowId]?.depends_on).toBe("unknown_dependency");
    expect(rowsToInput(rows)[0]!.depends_on).toEqual(["ghost"]);
  });
  it("serializeRows ignores row ids, so a fresh load is not dirty", () => {
    expect(serializeRows(nodesToRows(nodes))).toBe(serializeRows(nodesToRows(nodes)));
  });
});

describe("DAG layout", () => {
  const rows = [
    row("a", "a"),
    row("b", "b", { depends_on: ["a"] }),
    row("c", "c", { depends_on: ["a"] }),
    row("d", "d", { depends_on: ["b", "c"] }),
  ];
  it("computes longest-path depth", () => {
    const depths = computeDepths(rows);
    expect([depths.get("a"), depths.get("b"), depths.get("c"), depths.get("d")]).toEqual([0, 1, 1, 2]);
  });
  it("does not loop on cycles", () => {
    const depths = computeDepths([row("a", "a", { depends_on: ["b"] }), row("b", "b", { depends_on: ["a"] })]);
    expect(depths.size).toBe(2);
  });
  it("places nodes in columns by depth and emits one edge per dependency", () => {
    const layout = layoutDag(rows);
    const byId = new Map(layout.nodes.map((n) => [n.rowId, n]));
    expect(byId.get("a")!.x).toBeLessThan(byId.get("b")!.x);
    expect(byId.get("b")!.x).toBe(byId.get("c")!.x);
    expect(byId.get("b")!.y).not.toBe(byId.get("c")!.y);
    expect(byId.get("b")!.x).toBeLessThan(byId.get("d")!.x);
    expect(layout.edges).toHaveLength(4);
    expect(layout.edges.every((e) => e.d.startsWith("M "))).toBe(true);
    expect(layout.width).toBeGreaterThan(byId.get("d")!.x);
    expect(layout.height).toBeGreaterThan(byId.get("c")!.y);
  });
  it("labels with the title, then the key, and skips edges to missing nodes", () => {
    const layout = layoutDag([
      row("a", "plan", { title: "" }),
      row("b", "build", { depends_on: ["a", "ghost"] }),
    ]);
    expect(layout.nodes[0]!.label).toBe("plan");
    expect(layout.edges).toHaveLength(1);
  });
  it("returns an empty layout for no rows", () => {
    expect(layoutDag([])).toEqual({ nodes: [], edges: [], width: 0, height: 0 });
  });
});
```

`packages/views/ext-workflows/status-keys.test.ts`:

```ts
// @vitest-environment node
import { describe, expect, it } from "vitest";
import { runStatusKey, stepStatusKey } from "./status-keys";

describe("status keys", () => {
  it("passes known statuses through", () => {
    expect(stepStatusKey("awaiting_human")).toBe("awaiting_human");
    expect(runStatusKey("waiting_human")).toBe("waiting_human");
  });
  it("downgrades unknown server enums to unknown", () => {
    expect(stepStatusKey("some_future_status")).toBe("unknown");
    expect(runStatusKey("paused")).toBe("unknown");
    expect(stepStatusKey(undefined)).toBe("unknown");
  });
});
```

- [ ] **Step 2: Run, expect FAIL**

```bash
pnpm --filter @multica/views exec vitest run ext-workflows
```
Expected: FAIL (modules do not exist).

- [ ] **Step 3: Implement**

`packages/views/ext-workflows/status-keys.ts`:

```ts
// Pure status helpers. Unknown server enums downgrade to "unknown" (AGENTS.md:
// API Compatibility), so a newer backend never crashes an older client.

export const STEP_STATUS_KEYS = [
  "pending",
  "running",
  "awaiting_supervisor",
  "awaiting_human",
  "done",
  "skipped",
  "failed",
  "cancelled",
] as const;
export type StepStatusKey = (typeof STEP_STATUS_KEYS)[number] | "unknown";

export const RUN_STATUS_KEYS = ["running", "waiting_human", "done", "failed", "cancelled"] as const;
export type RunStatusKey = (typeof RUN_STATUS_KEYS)[number] | "unknown";

export function stepStatusKey(status: string | null | undefined): StepStatusKey {
  return (STEP_STATUS_KEYS as readonly string[]).includes(status ?? "")
    ? (status as StepStatusKey)
    : "unknown";
}

export function runStatusKey(status: string | null | undefined): RunStatusKey {
  return (RUN_STATUS_KEYS as readonly string[]).includes(status ?? "")
    ? (status as RunStatusKey)
    : "unknown";
}

/** A run is "active" while it can still change: running or waiting for a human. */
export function isActiveRunStatus(status: string | null | undefined): boolean {
  const key = runStatusKey(status);
  return key === "running" || key === "waiting_human";
}
```

`packages/views/ext-workflows/node-editor-helpers.ts`:

```ts
import type {
  ExtWorkflowNode,
  ExtWorkflowNodeInput,
  ExtWorkflowValidationError,
} from "@multica/core/ext-workflows";

export const EXT_MAX_NODES = 50;
export const EXT_MAX_PROMPT_BYTES = 20000;
export const EXT_KEY_PATTERN = /^[a-z0-9_-]{1,40}$/;
const DEFAULT_MAX_ATTEMPTS = 3;
const MISSING_PREFIX = "missing:";

/** Editor row. `depends_on` holds ROW IDS (not keys): renaming a key never rewrites other rows. */
export interface NodeRow {
  rowId: string;
  key: string;
  /** false while the key still follows the title; true once edited or loaded from the server. */
  keyTouched: boolean;
  title: string;
  agent_id: string;
  prompt: string;
  requires_review: boolean;
  max_attempts: number;
  depends_on: string[];
}

export type NodeField =
  | "key"
  | "title"
  | "agent_id"
  | "prompt"
  | "max_attempts"
  | "depends_on"
  | "row";

export type RowErrorCode =
  | "key_required"
  | "key_invalid"
  | "key_duplicate"
  | "title_required"
  | "agent_required"
  | "prompt_too_long"
  | "max_attempts_range"
  | "self_dependency"
  | "unknown_dependency"
  | "cycle";

export type GeneralErrorCode = "too_many_nodes" | "no_nodes";
export type RowErrors = Record<string, Partial<Record<NodeField, RowErrorCode>>>;

export interface RowValidation {
  rows: RowErrors;
  general: GeneralErrorCode[];
  valid: boolean;
}

let rowSeq = 0;
export function newRowId(): string {
  rowSeq += 1;
  return `row-${rowSeq}`;
}

// ---------------------------------------------------------------- keys

export function slugify(title: string): string {
  return title
    .normalize("NFKD")
    .replace(/[̀-ͯ]/g, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "")
    .slice(0, 40)
    .replace(/_+$/, "");
}

export function uniqueKey(base: string, taken: ReadonlySet<string>): string {
  if (!taken.has(base)) return base;
  for (let i = 2; ; i += 1) {
    const suffix = `_${i}`;
    const candidate = base.slice(0, 40 - suffix.length) + suffix;
    if (!taken.has(candidate)) return candidate;
  }
}

function deriveKey(title: string, position: number, taken: ReadonlySet<string>): string {
  return uniqueKey(slugify(title) || `node_${position}`, taken);
}

// ---------------------------------------------------------------- row editing

export function createRow(existing: readonly NodeRow[], agentId = ""): NodeRow {
  const taken = new Set(existing.map((r) => r.key));
  return {
    rowId: newRowId(),
    key: uniqueKey(`node_${existing.length + 1}`, taken),
    keyTouched: false,
    title: "",
    agent_id: agentId,
    prompt: "",
    requires_review: false,
    max_attempts: DEFAULT_MAX_ATTEMPTS,
    depends_on: [],
  };
}

export function setRowTitle(rows: NodeRow[], rowId: string, title: string): NodeRow[] {
  const index = rows.findIndex((r) => r.rowId === rowId);
  if (index < 0) return rows;
  const current = rows[index]!;
  if (current.keyTouched) {
    return rows.map((r) => (r.rowId === rowId ? { ...r, title } : r));
  }
  const taken = new Set(rows.filter((r) => r.rowId !== rowId).map((r) => r.key));
  const key = deriveKey(title, index + 1, taken);
  return rows.map((r) => (r.rowId === rowId ? { ...r, title, key } : r));
}

export function setRowKey(rows: NodeRow[], rowId: string, key: string): NodeRow[] {
  return rows.map((r) => (r.rowId === rowId ? { ...r, key, keyTouched: true } : r));
}

export function patchRow(rows: NodeRow[], rowId: string, patch: Partial<NodeRow>): NodeRow[] {
  return rows.map((r) => (r.rowId === rowId ? { ...r, ...patch } : r));
}

export function removeRow(rows: NodeRow[], rowId: string): NodeRow[] {
  return rows
    .filter((r) => r.rowId !== rowId)
    .map((r) =>
      r.depends_on.includes(rowId)
        ? { ...r, depends_on: r.depends_on.filter((d) => d !== rowId) }
        : r,
    );
}

export function moveRow(rows: NodeRow[], index: number, delta: -1 | 1): NodeRow[] {
  const target = index + delta;
  if (index < 0 || index >= rows.length || target < 0 || target >= rows.length) return rows;
  const next = [...rows];
  const [moved] = next.splice(index, 1);
  next.splice(target, 0, moved!);
  return next;
}

// ---------------------------------------------------------------- dependencies

/** Rows that depend (transitively) on `rowId`, excluding itself. Terminates on cyclic input. */
export function descendantIds(rows: readonly NodeRow[], rowId: string): Set<string> {
  const children = new Map<string, string[]>();
  for (const r of rows) {
    for (const d of r.depends_on) {
      const list = children.get(d);
      if (list) list.push(r.rowId);
      else children.set(d, [r.rowId]);
    }
  }
  const out = new Set<string>();
  const stack = [rowId];
  while (stack.length > 0) {
    const id = stack.pop()!;
    for (const child of children.get(id) ?? []) {
      if (!out.has(child)) {
        out.add(child);
        stack.push(child);
      }
    }
  }
  out.delete(rowId);
  return out;
}

/**
 * Rows `rowId` may depend on. Excludes itself and every downstream row:
 * depending on a descendant would close a cycle.
 */
export function dependencyOptions(rows: readonly NodeRow[], rowId: string): NodeRow[] {
  const blocked = descendantIds(rows, rowId);
  return rows.filter((r) => r.rowId !== rowId && !blocked.has(r.rowId));
}

// ---------------------------------------------------------------- validation

function utf8Length(value: string): number {
  return new TextEncoder().encode(value).length;
}

function cyclicIds(rows: readonly NodeRow[]): Set<string> {
  const ids = new Set(rows.map((r) => r.rowId));
  const indegree = new Map<string, number>();
  const out = new Map<string, string[]>();
  for (const r of rows) indegree.set(r.rowId, 0);
  for (const r of rows) {
    for (const d of new Set(r.depends_on)) {
      if (d === r.rowId || !ids.has(d)) continue;
      indegree.set(r.rowId, (indegree.get(r.rowId) ?? 0) + 1);
      const list = out.get(d);
      if (list) list.push(r.rowId);
      else out.set(d, [r.rowId]);
    }
  }
  const queue = rows.filter((r) => (indegree.get(r.rowId) ?? 0) === 0).map((r) => r.rowId);
  const done = new Set<string>();
  while (queue.length > 0) {
    const id = queue.shift()!;
    done.add(id);
    for (const next of out.get(id) ?? []) {
      const left = (indegree.get(next) ?? 0) - 1;
      indegree.set(next, left);
      if (left === 0) queue.push(next);
    }
  }
  return new Set(rows.filter((r) => !done.has(r.rowId)).map((r) => r.rowId));
}

export function validateRows(rows: readonly NodeRow[]): RowValidation {
  const errors: RowErrors = {};
  const general: GeneralErrorCode[] = [];
  const set = (rowId: string, field: NodeField, code: RowErrorCode) => {
    errors[rowId] = { ...errors[rowId], [field]: code };
  };

  if (rows.length === 0) general.push("no_nodes");
  if (rows.length > EXT_MAX_NODES) general.push("too_many_nodes");

  const keyCount = new Map<string, number>();
  for (const r of rows) keyCount.set(r.key, (keyCount.get(r.key) ?? 0) + 1);
  const ids = new Set(rows.map((r) => r.rowId));
  const cyclic = cyclicIds(rows);

  for (const r of rows) {
    if (r.key === "") set(r.rowId, "key", "key_required");
    else if (!EXT_KEY_PATTERN.test(r.key)) set(r.rowId, "key", "key_invalid");
    else if ((keyCount.get(r.key) ?? 0) > 1) set(r.rowId, "key", "key_duplicate");

    if (r.title.trim() === "") set(r.rowId, "title", "title_required");
    if (r.agent_id === "") set(r.rowId, "agent_id", "agent_required");
    if (utf8Length(r.prompt) > EXT_MAX_PROMPT_BYTES) set(r.rowId, "prompt", "prompt_too_long");
    if (!Number.isInteger(r.max_attempts) || r.max_attempts < 1 || r.max_attempts > 10) {
      set(r.rowId, "max_attempts", "max_attempts_range");
    }

    if (r.depends_on.includes(r.rowId)) set(r.rowId, "depends_on", "self_dependency");
    else if (r.depends_on.some((d) => !ids.has(d))) set(r.rowId, "depends_on", "unknown_dependency");
    else if (cyclic.has(r.rowId)) set(r.rowId, "depends_on", "cycle");
  }

  return { rows: errors, general, valid: Object.keys(errors).length === 0 && general.length === 0 };
}

// ---------------------------------------------------------------- server errors

const NODE_FIELDS: ReadonlySet<string> = new Set([
  "key",
  "title",
  "agent_id",
  "prompt",
  "max_attempts",
  "depends_on",
]);

export interface MappedServerErrors {
  byRow: Record<string, Partial<Record<NodeField, string>>>;
  general: string[];
}

/** Maps 422 `errors[]` onto rows by `node_key`. Anything that has no row goes to `general`. */
export function mapServerErrors(
  errors: readonly ExtWorkflowValidationError[],
  rows: readonly NodeRow[],
): MappedServerErrors {
  const byRow: MappedServerErrors["byRow"] = {};
  const general: string[] = [];
  for (const e of errors) {
    const row = e.node_key ? rows.find((r) => r.key === e.node_key) : undefined;
    if (!row) {
      general.push(e.message);
      continue;
    }
    const field = (NODE_FIELDS.has(e.field) ? e.field : "row") as NodeField;
    byRow[row.rowId] = { ...byRow[row.rowId], [field]: e.message };
  }
  return { byRow, general };
}

/** Duck-types the typed 422 error (`ExtWorkflowValidationFailed`) so views need not import the class. */
export function extractValidationErrors(err: unknown): ExtWorkflowValidationError[] | null {
  if (!err || typeof err !== "object") return null;
  const errors = (err as { errors?: unknown }).errors;
  if (!Array.isArray(errors)) return null;
  const valid = errors.filter(
    (e): e is ExtWorkflowValidationError =>
      !!e &&
      typeof e === "object" &&
      typeof (e as { field?: unknown }).field === "string" &&
      typeof (e as { message?: unknown }).message === "string",
  );
  return valid.length > 0 ? valid : null;
}

// ---------------------------------------------------------------- API mapping

export function nodesToRows(nodes: readonly ExtWorkflowNode[]): NodeRow[] {
  const sorted = [...nodes].sort((a, b) => a.position - b.position);
  const idByKey = new Map<string, string>();
  const rows: NodeRow[] = sorted.map((n) => {
    const rowId = newRowId();
    idByKey.set(n.key, rowId);
    return {
      rowId,
      key: n.key,
      keyTouched: true,
      title: n.title,
      agent_id: n.agent_id,
      prompt: n.prompt,
      requires_review: n.requires_review,
      max_attempts: n.max_attempts,
      depends_on: [],
    };
  });
  rows.forEach((row, i) => {
    row.depends_on = sorted[i]!.depends_on.map((key) => idByKey.get(key) ?? `${MISSING_PREFIX}${key}`);
  });
  return rows;
}

export function rowsToInput(rows: readonly NodeRow[]): ExtWorkflowNodeInput[] {
  const keyById = new Map(rows.map((r) => [r.rowId, r.key]));
  const keyOf = (id: string) =>
    keyById.get(id) ?? (id.startsWith(MISSING_PREFIX) ? id.slice(MISSING_PREFIX.length) : "");
  return rows.map((r) => ({
    key: r.key,
    title: r.title.trim(),
    agent_id: r.agent_id,
    prompt: r.prompt,
    requires_review: r.requires_review,
    max_attempts: r.max_attempts,
    depends_on: r.depends_on.map(keyOf),
  }));
}

export function serializeRows(rows: readonly NodeRow[]): string {
  return JSON.stringify(rowsToInput(rows));
}

// ---------------------------------------------------------------- DAG layout

export const DAG_NODE_W = 168;
export const DAG_NODE_H = 40;
export const DAG_COL_GAP = 56;
export const DAG_ROW_GAP = 14;
export const DAG_PAD = 12;

/** Longest-path depth per row id. Dependencies on missing rows are ignored; cycles do not loop. */
export function computeDepths(
  rows: readonly Pick<NodeRow, "rowId" | "depends_on">[],
): Map<string, number> {
  const byId = new Map(rows.map((r) => [r.rowId, r]));
  const memo = new Map<string, number>();
  const visiting = new Set<string>();
  const depth = (id: string): number => {
    const known = memo.get(id);
    if (known !== undefined) return known;
    if (visiting.has(id)) return 0;
    visiting.add(id);
    let d = 0;
    for (const dep of byId.get(id)?.depends_on ?? []) {
      if (dep !== id && byId.has(dep)) d = Math.max(d, depth(dep) + 1);
    }
    visiting.delete(id);
    memo.set(id, d);
    return d;
  };
  for (const r of rows) depth(r.rowId);
  return memo;
}

export interface DagNode {
  rowId: string;
  label: string;
  x: number;
  y: number;
  w: number;
  h: number;
}
export interface DagEdge {
  from: string;
  to: string;
  d: string;
}
export interface DagLayout {
  nodes: DagNode[];
  edges: DagEdge[];
  width: number;
  height: number;
}

export function layoutDag(
  rows: readonly Pick<NodeRow, "rowId" | "key" | "title" | "depends_on">[],
): DagLayout {
  if (rows.length === 0) return { nodes: [], edges: [], width: 0, height: 0 };
  const depths = computeDepths(rows);
  const perColumn = new Map<number, number>();
  const nodes: DagNode[] = rows.map((r) => {
    const col = depths.get(r.rowId) ?? 0;
    const slot = perColumn.get(col) ?? 0;
    perColumn.set(col, slot + 1);
    return {
      rowId: r.rowId,
      label: r.title.trim() || r.key,
      x: DAG_PAD + col * (DAG_NODE_W + DAG_COL_GAP),
      y: DAG_PAD + slot * (DAG_NODE_H + DAG_ROW_GAP),
      w: DAG_NODE_W,
      h: DAG_NODE_H,
    };
  });
  const byId = new Map(nodes.map((n) => [n.rowId, n]));
  const edges: DagEdge[] = [];
  for (const r of rows) {
    const to = byId.get(r.rowId)!;
    for (const dep of r.depends_on) {
      const from = byId.get(dep);
      if (!from || dep === r.rowId) continue;
      const x1 = from.x + from.w;
      const y1 = from.y + from.h / 2;
      const x2 = to.x;
      const y2 = to.y + to.h / 2;
      const mid = DAG_COL_GAP / 2;
      edges.push({
        from: dep,
        to: r.rowId,
        d: `M ${x1} ${y1} C ${x1 + mid} ${y1}, ${x2 - mid} ${y2}, ${x2} ${y2}`,
      });
    }
  }
  const columns = Math.max(...depths.values()) + 1;
  const tallest = Math.max(...perColumn.values());
  return {
    nodes,
    edges,
    width: DAG_PAD * 2 + columns * DAG_NODE_W + (columns - 1) * DAG_COL_GAP,
    height: DAG_PAD * 2 + tallest * DAG_NODE_H + (tallest - 1) * DAG_ROW_GAP,
  };
}
```

`packages/views/ext-workflows/components/status.tsx`:

```tsx
"use client";

import {
  Ban,
  CheckCircle2,
  Circle,
  Eye,
  HelpCircle,
  Loader2,
  SkipForward,
  UserRound,
  XCircle,
} from "lucide-react";
import { Badge } from "@multica/ui/components/ui/badge";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { runStatusKey, stepStatusKey, type RunStatusKey, type StepStatusKey } from "../status-keys";

const STEP_ICON: Record<StepStatusKey, { Icon: typeof Circle; className: string }> = {
  pending: { Icon: Circle, className: "text-muted-foreground" },
  running: { Icon: Loader2, className: "text-brand animate-spin" },
  awaiting_supervisor: { Icon: Eye, className: "text-warning" },
  awaiting_human: { Icon: UserRound, className: "text-warning" },
  done: { Icon: CheckCircle2, className: "text-success" },
  skipped: { Icon: SkipForward, className: "text-muted-foreground" },
  failed: { Icon: XCircle, className: "text-destructive" },
  cancelled: { Icon: Ban, className: "text-muted-foreground" },
  unknown: { Icon: HelpCircle, className: "text-muted-foreground" },
};

export function StepStatusIcon({ status, className }: { status: string; className?: string }) {
  const { t } = useT("ext-workflows");
  const key = stepStatusKey(status);
  const { Icon, className: tone } = STEP_ICON[key];
  return (
    <Icon
      role="img"
      aria-label={t(($) => $.step_status[key])}
      className={cn("size-3.5 shrink-0", tone, className)}
    />
  );
}

const RUN_VARIANT: Record<RunStatusKey, { variant: "secondary" | "destructive" | "outline"; className?: string }> = {
  running: { variant: "secondary", className: "text-brand" },
  waiting_human: { variant: "secondary", className: "text-warning" },
  done: { variant: "secondary", className: "text-success" },
  failed: { variant: "destructive" },
  cancelled: { variant: "outline", className: "text-muted-foreground" },
  unknown: { variant: "outline", className: "text-muted-foreground" },
};

export function RunStatusBadge({ status, className }: { status: string; className?: string }) {
  const { t } = useT("ext-workflows");
  const key = runStatusKey(status);
  const { variant, className: tone } = RUN_VARIANT[key];
  return (
    <Badge variant={variant} className={cn(tone, className)}>
      {t(($) => $.run_status[key])}
    </Badge>
  );
}

export function useStepStatusLabel(): (status: string) => string {
  const { t } = useT("ext-workflows");
  return (status) => {
    const key = stepStatusKey(status);
    return t(($) => $.step_status[key]);
  };
}
```

`packages/views/ext-workflows/components/agent-select.tsx` (single-select agent picker modelled on `LeaderPicker` in `modals/create-squad.tsx`):

```tsx
"use client";

import { useMemo, useState } from "react";
import { ChevronDown, UserPlus } from "lucide-react";
import { useAuthStore } from "@multica/core/auth";
import type { Agent } from "@multica/core/types";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { cn } from "@multica/ui/lib/utils";
import { ActorAvatar } from "../../common/actor-avatar";
import { matchesPinyin } from "../../editor/extensions/pinyin-match";
import { useT } from "../../i18n";
import {
  PickerEmpty,
  PickerItem,
  PickerSection,
} from "../../issues/components/pickers/property-picker";

interface AgentSelectProps {
  /** Every agent, archived included, so an archived current value can still show its name. */
  agents: Agent[];
  value: string;
  onChange: (agentId: string) => void;
  /** Accessible name of the trigger (the visible label lives outside). */
  ariaLabel: string;
  placeholder: string;
  /** Supervisors need a bound runtime; node agents only need to be non-archived. */
  requireRuntime?: boolean;
  disabled?: boolean;
  invalid?: boolean;
  className?: string;
}

export function AgentSelect({
  agents,
  value,
  onChange,
  ariaLabel,
  placeholder,
  requireRuntime = false,
  disabled = false,
  invalid = false,
  className,
}: AgentSelectProps) {
  const { t } = useT("ext-workflows");
  const currentUserId = useAuthStore((s) => s.user?.id ?? null);
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState("");

  const selectable = useMemo(
    () => agents.filter((a) => !a.archived_at && (!requireRuntime || !!a.runtime_id)),
    [agents, requireRuntime],
  );
  const q = filter.trim().toLowerCase();
  const matches = (a: Agent) =>
    !q || a.name.toLowerCase().includes(q) || matchesPinyin(a.name, q);
  const mine = selectable.filter((a) => currentUserId && a.owner_id === currentUserId).filter(matches);
  const others = selectable.filter((a) => !currentUserId || a.owner_id !== currentUserId).filter(matches);

  const selected = agents.find((a) => a.id === value) ?? null;
  const selectedLabel = selected
    ? selected.archived_at
      ? `${selected.name} ${t(($) => $.agent_select.archived_suffix)}`
      : selected.name
    : null;

  const pick = (id: string) => {
    onChange(id);
    setOpen(false);
    setFilter("");
  };

  const rows = (list: Agent[]) =>
    list.map((a) => (
      <PickerItem key={a.id} selected={value === a.id} onClick={() => pick(a.id)}>
        <ActorAvatar actorType="agent" actorId={a.id} size="sm" showStatusDot />
        <span className="truncate">{a.name}</span>
      </PickerItem>
    ));

  if (selectable.length === 0 && !selected) {
    return (
      <div
        className={cn(
          "rounded-lg border border-dashed bg-muted/30 px-3 py-2 text-body text-muted-foreground",
          className,
        )}
      >
        {t(($) => $.agent_select.no_agents)}
      </div>
    );
  }

  return (
    <Popover
      open={open}
      onOpenChange={(v) => {
        if (disabled) return;
        setOpen(v);
        if (!v) setFilter("");
      }}
    >
      <PopoverTrigger
        disabled={disabled}
        aria-label={ariaLabel}
        aria-invalid={invalid || undefined}
        className={cn(
          "flex w-full min-w-0 items-center gap-2 rounded-lg border bg-background px-2.5 py-1.5 text-left text-body transition-colors hover:bg-muted disabled:cursor-not-allowed disabled:opacity-60",
          invalid ? "border-destructive" : "border-border",
          className,
        )}
      >
        {selected ? (
          <ActorAvatar actorType="agent" actorId={selected.id} size="sm" showStatusDot />
        ) : (
          <UserPlus className="size-4 shrink-0 text-muted-foreground" />
        )}
        <span className={cn("min-w-0 flex-1 truncate", !selected && "text-muted-foreground")}>
          {selectedLabel ?? placeholder}
        </span>
        <ChevronDown
          className={cn("size-4 shrink-0 text-muted-foreground transition-transform", open && "rotate-180")}
        />
      </PopoverTrigger>
      <PopoverContent align="start" className="w-[var(--anchor-width)] min-w-56 p-0">
        <div className="border-b px-2 py-1.5">
          <input
            autoFocus
            type="text"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder={t(($) => $.agent_select.search_placeholder)}
            className="w-full bg-transparent text-body outline-none placeholder:text-muted-foreground"
          />
        </div>
        <div className="max-h-72 overflow-y-auto p-1">
          {mine.length > 0 && (
            <PickerSection label={t(($) => $.agent_select.group_my_agents)}>{rows(mine)}</PickerSection>
          )}
          {others.length > 0 && (
            <PickerSection label={t(($) => $.agent_select.group_workspace_agents)}>
              {rows(others)}
            </PickerSection>
          )}
          {mine.length === 0 && others.length === 0 && <PickerEmpty />}
        </div>
      </PopoverContent>
    </Popover>
  );
}
```

- [ ] **Step 4: Run, expect PASS**

```bash
pnpm --filter @multica/views exec vitest run ext-workflows
pnpm --filter @multica/views typecheck
pnpm --filter @multica/views lint
```

- [ ] **Step 5: Commit**

```bash
git add packages/views/ext-workflows
git commit -m "feat(ext-workflow): add node editor helpers, status components and agent select

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D4: Workflows list page, create dialog, registry, list routes

**Files:**
- Modify: `packages/ui/components/common/actor-avatar.tsx` (add `isWorkflow`; lines 3, 20, 31, 79)
- Create: `packages/views/ext-workflows/components/workflows-page.tsx`
- Create: `packages/views/ext-workflows/index.ts`
- Create: `packages/views/modals/create-ext-workflow.tsx`
- Modify: `packages/views/modals/registry.tsx` (import near line 6; `case` after `create-squad` at line 46)
- Modify: `packages/views/package.json` (`exports`, after line 40)
- Create: `apps/web/app/[workspaceSlug]/(dashboard)/workflows/page.tsx`
- Modify: `apps/desktop/src/renderer/src/routes.tsx` (import near line 29; route after the `squads/:id` entry at line 222)
- Test: `packages/views/ext-workflows/components/workflows-page.test.tsx`, `packages/views/modals/create-ext-workflow.test.tsx`

**Interfaces:**
- Consumes (core): `extWorkflowListOptions(wsId)` (data: `ExtWorkflow[]`), `useArchiveExtWorkflow(wsId)` (`mutateAsync(id)`), `useCreateExtWorkflow(wsId)` (`mutateAsync({name, description, supervisor_agent_id}) -> ExtWorkflow`), `paths.workflows()`, `paths.workflowDetail(id)`, modal key `"create-ext-workflow"`.
- Produces: `WorkflowsPage`, `CreateExtWorkflowModal`, `AgentSelect` reuse, `@multica/views/ext-workflows` export path.

- [ ] **Step 1: Write the failing tests**

`packages/views/ext-workflows/components/workflows-page.test.tsx`:

```tsx
// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { renderWithI18n } from "../../test/i18n";
import { WorkflowsPage } from "./workflows-page";

const WORKFLOWS = [
  {
    id: "wf-1",
    name: "Release pipeline",
    description: "Ship it",
    supervisor_agent_id: "ag-1",
    creator_id: "user-me",
    avatar_url: null,
    node_count: 4,
    active_run_count: 2,
    last_run_at: "2026-10-01T00:00:00Z",
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  },
  {
    id: "wf-2",
    name: "Docs sweep",
    description: "",
    supervisor_agent_id: "ag-1",
    creator_id: "user-other",
    avatar_url: null,
    node_count: 1,
    active_run_count: 0,
    last_run_at: null,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  },
];

const mocks = vi.hoisted(() => ({
  archive: vi.fn(),
  openModal: vi.fn(),
  role: "member" as "member" | "admin",
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: ({ queryKey }: { queryKey: string[] }) => {
    if (queryKey[0] === "ext-workflows") return { data: WORKFLOWS, isLoading: false };
    if (queryKey[0] === "agents") return { data: [{ id: "ag-1", name: "Supervisor Bot" }] };
    if (queryKey[0] === "members")
      return { data: [{ user_id: "user-me", name: "Me", role: mocks.role }] };
    return { data: [] };
  },
}));
vi.mock("@multica/core/ext-workflows", () => ({
  extWorkflowListOptions: () => ({ queryKey: ["ext-workflows", "ws-1", "list"] }),
  useArchiveExtWorkflow: () => ({ mutateAsync: mocks.archive, isPending: false }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: () => ({ queryKey: ["agents"] }),
  memberListOptions: () => ({ queryKey: ["members"] }),
}));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) => selector({ user: { id: "user-me" } }),
}));
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "ws-1" }),
  useWorkspacePaths: () => ({ workflowDetail: (id: string) => `/acme/workflows/${id}` }),
}));
vi.mock("@multica/core/modals", () => ({
  useModalStore: { getState: () => ({ open: mocks.openModal }) },
}));
vi.mock("../../navigation", () => ({
  useRowLink: () => (path: string) => ({ "data-href": path }),
  useIntentNavigate: () => vi.fn(),
}));
vi.mock("../../layout/collection-page", () => ({
  CollectionPageHeader: ({ title, actions }: { title: ReactNode; actions?: ReactNode }) => (
    <header>
      <h1>{title}</h1>
      {actions}
    </header>
  ),
  CollectionPageHeaderAction: ({ label, onClick }: { label: string; onClick: () => void }) => (
    <button onClick={onClick}>{label}</button>
  ),
  CollectionPageState: ({ title }: { title: ReactNode }) => <div>{title}</div>,
}));
vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({ actorId }: { actorId: string }) => <span data-testid={`avatar-${actorId}`} />,
}));

beforeEach(() => {
  mocks.archive.mockReset().mockResolvedValue(undefined);
  mocks.openModal.mockReset();
  mocks.role = "member";
});

describe("WorkflowsPage", () => {
  it("lists workflows with supervisor, node count, active runs and last run", () => {
    renderWithI18n(<WorkflowsPage />);
    const row = screen.getByText("Release pipeline").closest("[data-href]") as HTMLElement;
    expect(row).toHaveAttribute("data-href", "/acme/workflows/wf-1");
    expect(within(row).getByText("Supervisor Bot")).toBeInTheDocument();
    expect(within(row).getByText("4")).toBeInTheDocument();
    expect(within(row).getByText("2")).toBeInTheDocument();
    const other = screen.getByText("Docs sweep").closest("[data-href]") as HTMLElement;
    expect(within(other).getByText("Never")).toBeInTheDocument();
  });

  it("opens the create dialog from the header button", async () => {
    renderWithI18n(<WorkflowsPage />);
    await userEvent.click(screen.getByRole("button", { name: "New workflow" }));
    expect(mocks.openModal).toHaveBeenCalledWith("create-ext-workflow");
  });

  it("offers archive only on workflows the viewer can manage, and confirms first", async () => {
    renderWithI18n(<WorkflowsPage />);
    // Member: only the workflow they created has the row menu.
    expect(screen.getAllByRole("button", { name: "Workflow actions" })).toHaveLength(1);
    await userEvent.click(screen.getByRole("button", { name: "Workflow actions" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Archive" }));
    expect(mocks.archive).not.toHaveBeenCalled();
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Archive this workflow?")).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Archive" }));
    expect(mocks.archive).toHaveBeenCalledWith("wf-1");
  });

  it("lets workspace admins archive any workflow", () => {
    mocks.role = "admin";
    renderWithI18n(<WorkflowsPage />);
    expect(screen.getAllByRole("button", { name: "Workflow actions" })).toHaveLength(2);
  });
});
```

`packages/views/modals/create-ext-workflow.test.tsx`:

```tsx
// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithI18n } from "../test/i18n";
import { CreateExtWorkflowModal } from "./create-ext-workflow";

const mocks = vi.hoisted(() => ({
  create: vi.fn(),
  push: vi.fn(),
  toastSuccess: vi.fn(),
  toastError: vi.fn(),
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: [{ id: "ag-1", name: "Supervisor Bot" }] }),
}));
vi.mock("@multica/core/ext-workflows", () => ({
  useCreateExtWorkflow: () => ({ mutateAsync: mocks.create, isPending: false }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: () => ({ queryKey: ["agents"] }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ workflowDetail: (id: string) => `/acme/workflows/${id}` }),
}));
vi.mock("@multica/core/utils", () => ({ isImeComposing: () => false }));
vi.mock("sonner", () => ({
  toast: { success: mocks.toastSuccess, error: mocks.toastError },
}));
vi.mock("../navigation", () => ({ useNavigation: () => ({ push: mocks.push }) }));
// The picker is covered by its own usage in the node editor; here it is a stub.
vi.mock("../ext-workflows/components/agent-select", () => ({
  AgentSelect: ({ ariaLabel, onChange }: { ariaLabel: string; onChange: (id: string) => void }) => (
    <button type="button" aria-label={ariaLabel} onClick={() => onChange("ag-1")}>
      pick supervisor
    </button>
  ),
}));

beforeEach(() => {
  mocks.create.mockReset().mockResolvedValue({ id: "wf-9" });
  mocks.push.mockReset();
  mocks.toastSuccess.mockReset();
  mocks.toastError.mockReset();
});

describe("CreateExtWorkflowModal", () => {
  it("keeps Create disabled until a name and a supervisor are set", async () => {
    renderWithI18n(<CreateExtWorkflowModal onClose={vi.fn()} />);
    const submit = screen.getByRole("button", { name: "Create" });
    expect(submit).toBeDisabled();
    await userEvent.type(screen.getByLabelText("Name"), "Release pipeline");
    expect(submit).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Supervisor agent" }));
    expect(submit).toBeEnabled();
  });

  it("creates the workflow and opens its detail page", async () => {
    const onClose = vi.fn();
    renderWithI18n(<CreateExtWorkflowModal onClose={onClose} />);
    await userEvent.type(screen.getByLabelText("Name"), "  Release pipeline ");
    await userEvent.type(screen.getByLabelText("Description"), "Ship it");
    await userEvent.click(screen.getByRole("button", { name: "Supervisor agent" }));
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() =>
      expect(mocks.create).toHaveBeenCalledWith({
        name: "Release pipeline",
        description: "Ship it",
        supervisor_agent_id: "ag-1",
      }),
    );
    expect(onClose).toHaveBeenCalled();
    expect(mocks.push).toHaveBeenCalledWith("/acme/workflows/wf-9");
    expect(mocks.toastSuccess).toHaveBeenCalledWith("Workflow created");
  });

  it("stays open and shows the error when creation fails", async () => {
    mocks.create.mockRejectedValue(new Error("supervisor has no runtime"));
    const onClose = vi.fn();
    renderWithI18n(<CreateExtWorkflowModal onClose={onClose} />);
    await userEvent.type(screen.getByLabelText("Name"), "X");
    await userEvent.click(screen.getByRole("button", { name: "Supervisor agent" }));
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(mocks.toastError).toHaveBeenCalledWith("supervisor has no runtime"));
    expect(onClose).not.toHaveBeenCalled();
    expect(mocks.push).not.toHaveBeenCalled();
  });
});
```

- [ ] **Step 2: Run, expect FAIL**

```bash
pnpm --filter @multica/views exec vitest run ext-workflows/components/workflows-page.test.tsx modals/create-ext-workflow.test.tsx
```
Expected: FAIL (modules missing).

- [ ] **Step 3: Implement**

3a. `packages/ui/components/common/actor-avatar.tsx`: add `Workflow` to the lucide import (`import { Bot, Users, Workflow } from "lucide-react";`), add the prop and the branch:

```tsx
interface ActorAvatarProps {
  ...
  isSquad?: boolean;
  isWorkflow?: boolean; // ext-workflow: workflow actor glyph
  ...
}
function ActorAvatar({ ..., isSquad, isWorkflow, ... }) {
  ...
      ) : isSquad ? (
        <Users style={{ width: px * 0.55, height: px * 0.55 }} />
      ) : isWorkflow ? (
        <Workflow style={{ width: px * 0.55, height: px * 0.55 }} />
      ) : (
        initials
      )}
```

3b. `packages/views/ext-workflows/components/workflows-page.tsx`:

```tsx
"use client";

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ExternalLink, Loader2, MoreHorizontal, Plus, Trash2, Workflow as WorkflowIcon } from "lucide-react";
import { toast } from "sonner";
import { useAuthStore } from "@multica/core/auth";
import {
  extWorkflowListOptions,
  useArchiveExtWorkflow,
  type ExtWorkflow,
} from "@multica/core/ext-workflows";
import { useModalStore } from "@multica/core/modals";
import { useCurrentWorkspace, useWorkspacePaths } from "@multica/core/paths";
import { resolvePublicFileUrl } from "@multica/core/workspace/avatar-url";
import { agentListOptions, memberListOptions } from "@multica/core/workspace/queries";
import type { Agent, MemberWithUser } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import {
  ListGrid,
  ListGridCell,
  ListGridHeader,
  ListGridHeaderCell,
  ListGridRow,
  LIST_GRID_BOTTOM_CLEARANCE,
} from "@multica/ui/components/ui/list-grid";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { ActorAvatar as ActorAvatarBase } from "@multica/ui/components/common/actor-avatar";
import { ActorAvatar } from "../../common/actor-avatar";
import { useT, useTimeAgo } from "../../i18n";
import {
  CollectionPageHeader,
  CollectionPageHeaderAction,
  CollectionPageState,
} from "../../layout/collection-page";
import { useIntentNavigate, useRowLink } from "../../navigation";

// Name + supervisor are the core set (< @2xl); nodes / active runs / last run
// appear from @2xl. The kebab track is constant: it is empty for rows the
// viewer cannot manage.
const GRID_COLS =
  "grid-cols-[0.75rem_minmax(120px,1fr)_160px_1.75rem_0.75rem] " +
  "@2xl:grid-cols-[0.75rem_minmax(200px,1fr)_160px_88px_104px_128px_1.75rem_0.75rem]";

function initialsOf(name: string): string {
  return name
    .split(" ")
    .map((w) => w[0])
    .join("")
    .toUpperCase()
    .slice(0, 2);
}

function WorkflowAvatar({ workflow }: { workflow: ExtWorkflow }) {
  return (
    <ActorAvatarBase
      name={workflow.name}
      initials={initialsOf(workflow.name)}
      avatarUrl={workflow.avatar_url ? resolvePublicFileUrl(workflow.avatar_url) : undefined}
      isWorkflow
      size="lg"
      className="shrink-0"
    />
  );
}

function ArchiveWorkflowDialog({
  workflow,
  open,
  onOpenChange,
}: {
  workflow: ExtWorkflow;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useT("ext-workflows");
  const wsId = useCurrentWorkspace()?.id ?? "";
  const archive = useArchiveExtWorkflow(wsId);
  const confirm = async () => {
    try {
      await archive.mutateAsync(workflow.id);
      onOpenChange(false);
      toast.success(t(($) => $.archive_dialog.success));
    } catch (err) {
      toast.error(err instanceof Error && err.message ? err.message : t(($) => $.archive_dialog.failed));
    }
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t(($) => $.archive_dialog.title)}</DialogTitle>
          <DialogDescription>
            {t(($) => $.archive_dialog.description, { name: workflow.name })}
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={archive.isPending}
            onClick={() => onOpenChange(false)}
          >
            {t(($) => $.archive_dialog.cancel)}
          </Button>
          <Button
            type="button"
            variant="destructive"
            size="sm"
            disabled={archive.isPending}
            aria-busy={archive.isPending}
            onClick={() => void confirm()}
          >
            {archive.isPending ? (
              <>
                <Loader2 className="mr-1 size-3.5 animate-spin" />
                {t(($) => $.archive_dialog.archiving)}
              </>
            ) : (
              t(($) => $.archive_dialog.confirm)
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function WorkflowRowActions({ workflow }: { workflow: ExtWorkflow }) {
  const { t } = useT("ext-workflows");
  const { t: tCommon } = useT("common");
  const p = useWorkspacePaths();
  const intentNavigate = useIntentNavigate();
  const [archiveOpen, setArchiveOpen] = useState(false);
  return (
    <span onClick={(e) => e.stopPropagation()} className="flex items-center">
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <button
              type="button"
              aria-label={t(($) => $.page.row_menu)}
              className="flex size-7 items-center justify-center rounded-md text-muted-foreground opacity-0 transition-opacity hover:bg-accent hover:text-accent-foreground group-hover/row:opacity-100 focus-visible:opacity-100 data-popup-open:bg-accent data-popup-open:opacity-100 data-popup-open:text-accent-foreground"
            >
              <MoreHorizontal className="size-4" />
            </button>
          }
        />
        <DropdownMenuContent align="end" className="w-40">
          <DropdownMenuItem
            onClick={() => intentNavigate(p.workflowDetail(workflow.id), "foreground-tab", workflow.name)}
          >
            <ExternalLink className="size-3.5" />
            {tCommon(($) => $.navigation.open_in_new_tab)}
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem variant="destructive" onClick={() => setArchiveOpen(true)}>
            <Trash2 className="size-3.5" />
            {t(($) => $.page.archive_action)}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <ArchiveWorkflowDialog workflow={workflow} open={archiveOpen} onOpenChange={setArchiveOpen} />
    </span>
  );
}

export function WorkflowsPage() {
  const { t } = useT("ext-workflows");
  const timeAgo = useTimeAgo();
  const wsId = useCurrentWorkspace()?.id ?? "";
  const p = useWorkspacePaths();
  const rowLink = useRowLink();
  const currentUser = useAuthStore((s) => s.user);

  const { data: workflows = [], isLoading } = useQuery({
    ...extWorkflowListOptions(wsId),
    enabled: !!wsId,
  });
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: members = [] } = useQuery(memberListOptions(wsId));

  const agentsById = useMemo(() => new Map(agents.map((a: Agent) => [a.id, a])), [agents]);
  const isWorkspaceAdmin = useMemo(() => {
    const me = members.find((m: MemberWithUser) => m.user_id === currentUser?.id);
    return me?.role === "owner" || me?.role === "admin";
  }, [members, currentUser]);
  // Mirrors canManageSquad: workspace admins manage all, creators manage their own.
  const canManage = (w: ExtWorkflow) =>
    isWorkspaceAdmin || (!!currentUser && w.creator_id === currentUser.id);

  const openCreate = () => useModalStore.getState().open("create-ext-workflow");

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <CollectionPageHeader
        icon={WorkflowIcon}
        title={t(($) => $.page.title)}
        count={workflows.length}
        actions={
          <CollectionPageHeaderAction icon={Plus} label={t(($) => $.page.new_button)} onClick={openCreate} />
        }
      />

      {isLoading ? (
        <LoadingSkeleton />
      ) : workflows.length === 0 ? (
        <CollectionPageState
          icon={WorkflowIcon}
          title={t(($) => $.page.empty)}
          actions={
            <Button size="sm" onClick={openCreate}>
              <Plus aria-hidden="true" className="size-3.5" />
              {t(($) => $.page.new_button)}
            </Button>
          }
        />
      ) : (
        <div className="@container min-h-0 flex-1 overflow-auto">
          <ListGrid className={GRID_COLS} style={{ paddingBottom: LIST_GRID_BOTTOM_CLEARANCE }}>
            <ListGridHeader>
              <ListGridHeaderCell>{t(($) => $.page.table.name)}</ListGridHeaderCell>
              <ListGridHeaderCell>{t(($) => $.page.table.supervisor)}</ListGridHeaderCell>
              <ListGridHeaderCell className="hidden @2xl:flex">{t(($) => $.page.table.nodes)}</ListGridHeaderCell>
              <ListGridHeaderCell className="hidden @2xl:flex">
                {t(($) => $.page.table.active_runs)}
              </ListGridHeaderCell>
              <ListGridHeaderCell className="hidden @2xl:flex">{t(($) => $.page.table.last_run)}</ListGridHeaderCell>
              <span aria-hidden="true" />
            </ListGridHeader>
            {workflows.map((w: ExtWorkflow) => {
              const supervisor = agentsById.get(w.supervisor_agent_id);
              return (
                <ListGridRow key={w.id} className="cursor-pointer" {...rowLink(p.workflowDetail(w.id), w.name)}>
                  <ListGridCell className="gap-3">
                    <WorkflowAvatar workflow={w} />
                    <div className="min-w-0 flex-1">
                      <span className="block min-w-0 truncate text-body font-medium">{w.name}</span>
                      {w.description ? (
                        <span className="block min-w-0 truncate text-caption text-muted-foreground">
                          {w.description}
                        </span>
                      ) : null}
                    </div>
                  </ListGridCell>
                  <ListGridCell className="gap-1.5">
                    <ActorAvatar actorType="agent" actorId={w.supervisor_agent_id} size="sm" />
                    <span className="min-w-0 truncate text-caption text-muted-foreground">
                      {supervisor?.name ?? w.supervisor_agent_id.slice(0, 8)}
                    </span>
                  </ListGridCell>
                  <ListGridCell className="hidden text-caption tabular-nums text-muted-foreground @2xl:flex">
                    {w.node_count}
                  </ListGridCell>
                  <ListGridCell className="hidden text-caption tabular-nums text-muted-foreground @2xl:flex">
                    {w.active_run_count}
                  </ListGridCell>
                  <ListGridCell className="hidden whitespace-nowrap text-caption text-muted-foreground @2xl:flex">
                    {w.last_run_at ? timeAgo(w.last_run_at) : t(($) => $.page.never_run)}
                  </ListGridCell>
                  <ListGridCell className="justify-end px-0">
                    {canManage(w) ? <WorkflowRowActions workflow={w} /> : null}
                  </ListGridCell>
                </ListGridRow>
              );
            })}
          </ListGrid>
        </div>
      )}
    </div>
  );
}

function LoadingSkeleton() {
  return (
    <div className="@container min-h-0 flex-1 overflow-auto">
      <ListGrid className={GRID_COLS}>
        <ListGridHeader>
          <ListGridHeaderCell>
            <Skeleton className="h-3 w-12" />
          </ListGridHeaderCell>
          <ListGridHeaderCell>
            <Skeleton className="h-3 w-12" />
          </ListGridHeaderCell>
          <ListGridHeaderCell className="hidden @2xl:flex" />
          <ListGridHeaderCell className="hidden @2xl:flex" />
          <ListGridHeaderCell className="hidden @2xl:flex" />
          <span aria-hidden="true" />
        </ListGridHeader>
        {Array.from({ length: 3 }).map((_, i) => (
          <ListGridRow key={i} className="h-16 hover:bg-transparent">
            <ListGridCell className="gap-3">
              <Skeleton className="size-8 rounded-full" />
              <Skeleton className="h-3.5 w-32 max-w-full" />
            </ListGridCell>
            <ListGridCell className="gap-1.5">
              <Skeleton className="size-5 rounded-full" />
              <Skeleton className="h-3 w-16" />
            </ListGridCell>
            <ListGridCell className="hidden @2xl:flex" />
            <ListGridCell className="hidden @2xl:flex" />
            <ListGridCell className="hidden @2xl:flex" />
            <span aria-hidden="true" />
          </ListGridRow>
        ))}
      </ListGrid>
    </div>
  );
}
```

3c. `packages/views/ext-workflows/index.ts`:

```ts
export { WorkflowsPage } from "./components/workflows-page";
```

3d. `packages/views/modals/create-ext-workflow.tsx`:

```tsx
"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { useCreateExtWorkflow } from "@multica/core/ext-workflows";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { agentListOptions } from "@multica/core/workspace/queries";
import { isImeComposing } from "@multica/core/utils";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { AgentSelect } from "../ext-workflows/components/agent-select";
import { useT } from "../i18n";
import { useNavigation } from "../navigation";

export function CreateExtWorkflowModal({ onClose }: { onClose: () => void }) {
  const { t } = useT("ext-workflows");
  const router = useNavigation();
  const wsPaths = useWorkspacePaths();
  const wsId = useWorkspaceId();
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const create = useCreateExtWorkflow(wsId);

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [supervisorId, setSupervisorId] = useState("");

  const canSubmit = !!name.trim() && !!supervisorId && !create.isPending;

  const submit = async () => {
    if (!canSubmit) return;
    try {
      const workflow = await create.mutateAsync({
        name: name.trim(),
        description: description.trim(),
        supervisor_agent_id: supervisorId,
      });
      onClose();
      toast.success(t(($) => $.create.toast_created));
      // The detail page opens on the Nodes tab, where a new workflow starts.
      router.push(wsPaths.workflowDetail(workflow.id));
    } catch (err) {
      toast.error(err instanceof Error && err.message ? err.message : t(($) => $.create.toast_failed));
    }
  };

  return (
    <Dialog open onOpenChange={(v) => { if (!v) onClose(); }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t(($) => $.create.title)}</DialogTitle>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-name" className="text-caption text-muted-foreground">
              {t(($) => $.create.name_label)}
            </Label>
            <Input
              id="ext-workflow-name"
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t(($) => $.create.name_placeholder)}
              onKeyDown={(e) => {
                if (isImeComposing(e)) return;
                if (e.key === "Enter") void submit();
              }}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-description" className="text-caption text-muted-foreground">
              {t(($) => $.create.description_label)}
            </Label>
            <Textarea
              id="ext-workflow-description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder={t(($) => $.create.description_placeholder)}
              rows={2}
            />
          </div>
          <div className="space-y-1.5">
            <Label className="text-caption text-muted-foreground">
              {t(($) => $.create.supervisor_label)}
            </Label>
            <p className="text-caption text-muted-foreground">{t(($) => $.create.supervisor_hint)}</p>
            <AgentSelect
              agents={agents}
              value={supervisorId}
              onChange={setSupervisorId}
              ariaLabel={t(($) => $.create.supervisor_label)}
              placeholder={t(($) => $.create.supervisor_placeholder)}
              requireRuntime
            />
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            {t(($) => $.create.cancel)}
          </Button>
          <Button type="button" onClick={() => void submit()} disabled={!canSubmit} aria-busy={create.isPending}>
            {create.isPending ? t(($) => $.create.submitting) : t(($) => $.create.submit)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
```

The `Description` label is bound to the textarea through `htmlFor`, which is what `getByLabelText("Description")` in the test relies on.

3e. `packages/views/modals/registry.tsx`:

```tsx
import { CreateExtWorkflowModal } from "./create-ext-workflow"; // ext-workflow
...
    case "create-ext-workflow": // ext-workflow
      activeModal = <CreateExtWorkflowModal onClose={close} />;
      break;
```
(import after `import { CreateSquadModal } ...`; case after the `create-squad` case.)

3f. `packages/views/package.json` exports, after `"./squads/components": ...`:

```json
    "./ext-workflows": "./ext-workflows/index.ts",
```

3g. `apps/web/app/[workspaceSlug]/(dashboard)/workflows/page.tsx`:

```tsx
export { WorkflowsPage as default } from "@multica/views/ext-workflows";
```

3h. `apps/desktop/src/renderer/src/routes.tsx`: add after the squads import

```tsx
import { WorkflowsPage } from "@multica/views/ext-workflows"; // ext-workflow
```
and after the `squads/:id` route object:

```tsx
          { path: "workflows", element: <WorkflowsPage />, handle: { title: "Workflows" } }, // ext-workflow
```

- [ ] **Step 4: Run, expect PASS**

```bash
pnpm --filter @multica/views exec vitest run ext-workflows modals/create-ext-workflow.test.tsx
pnpm typecheck
pnpm --filter @multica/views lint
```
(`pnpm typecheck` covers web and desktop, which import `@multica/views/ext-workflows`.)

- [ ] **Step 5: Commit**

```bash
git add packages/ui/components/common/actor-avatar.tsx packages/views/ext-workflows packages/views/modals packages/views/package.json "apps/web/app/[workspaceSlug]/(dashboard)/workflows" apps/desktop/src/renderer/src/routes.tsx
git commit -m "feat(ext-workflow): add workflows list page and create dialog

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D5: Node editor, DAG preview, runs tab, workflow detail page, detail routes

**Files:**
- Create: `packages/views/ext-workflows/components/dag-preview.tsx`
- Create: `packages/views/ext-workflows/components/node-editor.tsx`
- Create: `packages/views/ext-workflows/components/workflow-runs-tab.tsx`
- Create: `packages/views/ext-workflows/components/workflow-detail-page.tsx`
- Modify: `packages/views/ext-workflows/index.ts`
- Create: `apps/web/app/[workspaceSlug]/(dashboard)/workflows/[id]/page.tsx`
- Modify: `apps/desktop/src/renderer/src/routes.tsx` (import from D4; route after the `workflows` entry)
- Test: `packages/views/ext-workflows/components/dag-preview.test.tsx`, `node-editor.test.tsx`, `workflow-runs-tab.test.tsx`, `workflow-detail-page.test.tsx` (same directory)

**Interfaces:**
- Consumes (core): `extWorkflowDetailOptions(wsId, id)` (data: `ExtWorkflow` with `nodes`), `extWorkflowRunsOptions(wsId, id)` (data: `{runs, total}`), `useUpdateExtWorkflow(wsId)` (`mutateAsync({id, name?, description?, supervisor_agent_id?, max_rewinds?, nodes?})` returning the saved `ExtWorkflow`), `useArchiveExtWorkflow(wsId)`; errors with `.errors: {node_key?, field, message}[]` (typed `ExtWorkflowValidationFailed`).
- Consumes (D3): helpers, `AgentSelect`, `RunStatusBadge`.
- Produces: `NodeEditor`, `DagPreview`, `WorkflowRunsTab`, `WorkflowDetailPage`.

- [ ] **Step 1: Write the failing tests**

`packages/views/ext-workflows/components/dag-preview.test.tsx`:

```tsx
// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";
import { DagPreview } from "./dag-preview";

const rows = [
  { rowId: "a", key: "plan", title: "Plan", depends_on: [] },
  { rowId: "b", key: "build", title: "Build", depends_on: ["a"] },
  { rowId: "c", key: "ship", title: "Ship", depends_on: ["a", "b"] },
];

describe("DagPreview", () => {
  it("draws one box per node and one edge per dependency", () => {
    const { container } = renderWithI18n(<DagPreview rows={rows} />);
    expect(screen.getByRole("img", { name: "Workflow graph" })).toBeInTheDocument();
    expect(container.querySelectorAll("[data-dag-node]")).toHaveLength(3);
    expect(container.querySelectorAll("[data-dag-edge]")).toHaveLength(3);
    expect(screen.getByText("Plan")).toBeInTheDocument();
  });

  it("shows a hint instead of an empty graph", () => {
    renderWithI18n(<DagPreview rows={[]} />);
    expect(screen.getByText("Add nodes to see the flow.")).toBeInTheDocument();
  });
});
```

`packages/views/ext-workflows/components/node-editor.test.tsx`:

```tsx
// @vitest-environment jsdom
import { useState, type ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Agent } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import type { NodeRow } from "../node-editor-helpers";
import { NodeEditor, type NodeEditorProps } from "./node-editor";

// Render the popup inline so dependency options are queryable without a portal.
vi.mock("@multica/ui/components/ui/popover", () => ({
  Popover: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  PopoverTrigger: ({ children, ...props }: { children: ReactNode }) => (
    <button type="button" {...props}>
      {children}
    </button>
  ),
  PopoverContent: ({ children }: { children: ReactNode }) => <div>{children}</div>,
}));
vi.mock("./agent-select", () => ({
  AgentSelect: ({
    ariaLabel,
    value,
    onChange,
  }: {
    ariaLabel: string;
    value: string;
    onChange: (id: string) => void;
  }) => (
    <button type="button" aria-label={ariaLabel} onClick={() => onChange("ag-2")}>
      {value || "no agent"}
    </button>
  ),
}));

const AGENTS = [
  { id: "ag-1", name: "Planner", archived_at: null },
  { id: "ag-2", name: "Builder", archived_at: null },
  { id: "ag-old", name: "Retired", archived_at: "2026-01-01T00:00:00Z" },
] as unknown as Agent[];

function mk(id: string, key: string, title: string, deps: string[] = [], over: Partial<NodeRow> = {}): NodeRow {
  return {
    rowId: id,
    key,
    keyTouched: true,
    title,
    agent_id: "ag-1",
    prompt: "",
    requires_review: false,
    max_attempts: 3,
    depends_on: deps,
    ...over,
  };
}

const CHAIN = [mk("a", "plan", "Plan"), mk("b", "build", "Build", ["a"]), mk("c", "ship", "Ship", ["b"])];

function Harness({
  initial,
  onRows,
  ...rest
}: { initial: NodeRow[]; onRows?: (rows: NodeRow[]) => void } & Partial<NodeEditorProps>) {
  const [rows, setRows] = useState(initial);
  return (
    <NodeEditor
      rows={rows}
      onChange={(next) => {
        setRows(next);
        onRows?.(next);
      }}
      agents={AGENTS}
      validation={null}
      serverErrors={null}
      activeRunCount={0}
      {...rest}
    />
  );
}

const node = (n: number) => screen.getByRole("listitem", { name: `Node ${n}` });

describe("NodeEditor dependency options", () => {
  it("offers nothing that would create a cycle", () => {
    renderWithI18n(<Harness initial={CHAIN} />);
    // Plan is upstream of both others, so every other node is downstream of it.
    expect(
      within(node(1)).getByText("No other node can be added without creating a cycle."),
    ).toBeInTheDocument();
    // Build may depend on Plan, never on Ship (which depends on Build).
    expect(within(node(2)).getAllByText("Plan").length).toBeGreaterThan(0);
    expect(within(node(2)).queryByText("Ship")).not.toBeInTheDocument();
    // Ship may depend on Plan or Build.
    expect(within(node(3)).getAllByText("Plan").length).toBeGreaterThan(0);
    expect(within(node(3)).getAllByText("Build").length).toBeGreaterThan(0);
  });

  it("toggles a dependency on and off", async () => {
    const onRows = vi.fn();
    renderWithI18n(<Harness initial={CHAIN} onRows={onRows} />);
    await userEvent.click(within(node(3)).getByRole("button", { name: "Plan" }));
    expect(onRows).toHaveBeenLastCalledWith(
      expect.arrayContaining([expect.objectContaining({ rowId: "c", depends_on: ["b", "a"] })]),
    );
    await userEvent.click(within(node(3)).getByRole("button", { name: "Plan" }));
    expect(onRows).toHaveBeenLastCalledWith(
      expect.arrayContaining([expect.objectContaining({ rowId: "c", depends_on: ["b"] })]),
    );
  });
});

describe("NodeEditor row editing", () => {
  it("derives the key from the title until the key is edited", async () => {
    renderWithI18n(<Harness initial={[]} />);
    await userEvent.click(screen.getByRole("button", { name: "Add node" }));
    await userEvent.type(within(node(1)).getByLabelText("Title"), "Write docs");
    expect(within(node(1)).getByLabelText("Key")).toHaveValue("write_docs");
    await userEvent.clear(within(node(1)).getByLabelText("Key"));
    await userEvent.type(within(node(1)).getByLabelText("Key"), "docs");
    await userEvent.type(within(node(1)).getByLabelText("Title"), " v2");
    expect(within(node(1)).getByLabelText("Key")).toHaveValue("docs");
  });

  it("reorders and deletes rows, dropping dangling dependencies", async () => {
    const onRows = vi.fn();
    renderWithI18n(<Harness initial={CHAIN} onRows={onRows} />);
    await userEvent.click(within(node(1)).getByRole("button", { name: "Move down" }));
    expect(onRows.mock.lastCall![0].map((r: NodeRow) => r.rowId)).toEqual(["b", "a", "c"]);
    expect(within(node(1)).getByLabelText("Title")).toHaveValue("Build");
    await userEvent.click(within(node(2)).getByRole("button", { name: "Delete node" }));
    const rows = onRows.mock.lastCall![0] as NodeRow[];
    expect(rows.map((r) => r.rowId)).toEqual(["b", "c"]);
    expect(rows[0]!.depends_on).toEqual([]);
  });

  it("shows the prompt only when expanded", async () => {
    renderWithI18n(<Harness initial={CHAIN} />);
    expect(within(node(1)).queryByLabelText("Prompt")).not.toBeInTheDocument();
    await userEvent.click(within(node(1)).getByRole("button", { name: "Show prompt" }));
    expect(within(node(1)).getByLabelText("Prompt")).toBeInTheDocument();
    await userEvent.click(within(node(1)).getByRole("button", { name: "Hide prompt" }));
    expect(within(node(1)).queryByLabelText("Prompt")).not.toBeInTheDocument();
  });

  it("warns when a row's agent is archived", () => {
    renderWithI18n(<Harness initial={[mk("a", "plan", "Plan", [], { agent_id: "ag-old" })]} />);
    expect(
      within(node(1)).getByText("This agent is archived. Choose another agent."),
    ).toBeInTheDocument();
  });
});

describe("NodeEditor error display", () => {
  it("shows client validation on the row that has the problem", () => {
    renderWithI18n(
      <Harness
        initial={CHAIN}
        validation={{ rows: { b: { title: "title_required" } }, general: ["no_nodes"] }}
      />,
    );
    expect(within(node(2)).getByText("Title is required")).toBeInTheDocument();
    expect(within(node(1)).queryByText("Title is required")).not.toBeInTheDocument();
    expect(screen.getByText("Add at least one node")).toBeInTheDocument();
  });

  it("maps server 422 errors onto rows and lists keyless ones above the rows", () => {
    renderWithI18n(
      <Harness
        initial={CHAIN}
        serverErrors={{
          byRow: { c: { agent_id: "agent is archived", depends_on: "cycle: ship -> build -> ship" } },
          general: ["supervisor is archived"],
        }}
      />,
    );
    expect(within(node(3)).getByText("agent is archived")).toBeInTheDocument();
    expect(within(node(3)).getByText("cycle: ship -> build -> ship")).toBeInTheDocument();
    expect(within(node(1)).queryByText("agent is archived")).not.toBeInTheDocument();
    expect(screen.getByText("supervisor is archived")).toBeInTheDocument();
  });

  it("notes that runs in progress keep the old definition", () => {
    renderWithI18n(<Harness initial={CHAIN} activeRunCount={2} />);
    expect(screen.getByText("2 runs in progress keep using the previous definition.")).toBeInTheDocument();
  });

  it("is read-only for viewers who cannot manage the workflow", () => {
    renderWithI18n(<Harness initial={CHAIN} readOnly />);
    expect(screen.queryByRole("button", { name: "Add node" })).not.toBeInTheDocument();
    expect(within(node(1)).getByLabelText("Title")).toBeDisabled();
    expect(within(node(1)).queryByRole("button", { name: "Delete node" })).not.toBeInTheDocument();
  });
});
```

`packages/views/ext-workflows/components/workflow-runs-tab.test.tsx`:

```tsx
// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";
import { WorkflowRunsTab } from "./workflow-runs-tab";

const state = vi.hoisted(() => ({ runs: [] as unknown[], total: 0 }));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: { runs: state.runs, total: state.total }, isLoading: false }),
}));
vi.mock("@multica/core/ext-workflows", () => ({
  extWorkflowRunsOptions: () => ({ queryKey: ["ext-workflows", "ws-1", "runs", "wf-1"] }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ issueDetail: (id: string) => `/acme/issues/${id}` }),
}));
vi.mock("../../navigation", () => ({
  useRowLink: () => (path: string) => ({ "data-href": path }),
}));

describe("WorkflowRunsTab", () => {
  it("lists runs with issue, status and times; rows open the issue", () => {
    state.total = 1;
    state.runs = [
      {
        id: "run-1",
        issue_id: "iss-1",
        issue_identifier: "MUL-12",
        issue_title: "Ship the thing",
        status: "waiting_human",
        started_at: "2026-10-01T00:00:00Z",
        finished_at: null,
      },
    ];
    renderWithI18n(<WorkflowRunsTab workflowId="wf-1" />);
    const row = screen.getByText("Ship the thing").closest("[data-href]") as HTMLElement;
    expect(row).toHaveAttribute("data-href", "/acme/issues/iss-1");
    expect(within(row).getByText("MUL-12")).toBeInTheDocument();
    expect(within(row).getByText("Needs you")).toBeInTheDocument();
  });

  it("explains how to start a run when there are none", () => {
    state.total = 0;
    state.runs = [];
    renderWithI18n(<WorkflowRunsTab workflowId="wf-1" />);
    expect(
      screen.getByText("No runs yet. Assign an issue to this workflow to start one."),
    ).toBeInTheDocument();
  });

  it("falls back to the unknown status for a future server enum", () => {
    state.total = 1;
    state.runs = [
      { id: "r", issue_id: "i", issue_identifier: "MUL-1", issue_title: "T", status: "paused", started_at: "2026-10-01T00:00:00Z", finished_at: null },
    ];
    renderWithI18n(<WorkflowRunsTab workflowId="wf-1" />);
    expect(screen.getByText("Unknown")).toBeInTheDocument();
  });
});
```

`packages/views/ext-workflows/components/workflow-detail-page.test.tsx`:

```tsx
// @vitest-environment jsdom
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithI18n } from "../../test/i18n";
import { WorkflowDetailPage } from "./workflow-detail-page";

const mocks = vi.hoisted(() => ({
  update: vi.fn(),
  archive: vi.fn(),
  push: vi.fn(),
  toastError: vi.fn(),
  toastSuccess: vi.fn(),
}));

const WORKFLOW = {
  id: "wf-1",
  name: "Release pipeline",
  description: "",
  supervisor_agent_id: "ag-1",
  max_rewinds: 3,
  creator_id: "user-me",
  avatar_url: null,
  node_count: 2,
  active_run_count: 0,
  last_run_at: null,
  created_at: "2026-09-01T00:00:00Z",
  updated_at: "2026-09-02T00:00:00Z",
  nodes: [
    { id: "n1", key: "plan", title: "Plan", agent_id: "ag-1", prompt: "", requires_review: false, max_attempts: 3, depends_on: [], position: 0 },
    { id: "n2", key: "build", title: "Build", agent_id: "ag-1", prompt: "", requires_review: true, max_attempts: 2, depends_on: ["plan"], position: 1 },
  ],
};

vi.mock("@tanstack/react-query", () => ({
  useQuery: ({ queryKey }: { queryKey: string[] }) => {
    if (queryKey[0] === "ext-workflows") return { data: WORKFLOW, isError: false };
    if (queryKey[0] === "agents") return { data: [{ id: "ag-1", name: "Planner", archived_at: null, runtime_id: "rt" }] };
    if (queryKey[0] === "members") return { data: [{ user_id: "user-me", name: "Me", role: "member" }] };
    return { data: undefined };
  },
}));
vi.mock("@multica/core/ext-workflows", () => ({
  extWorkflowDetailOptions: () => ({ queryKey: ["ext-workflows", "ws-1", "detail", "wf-1"] }),
  extWorkflowRunsOptions: () => ({ queryKey: ["ext-workflows", "ws-1", "runs", "wf-1"] }),
  useUpdateExtWorkflow: () => ({ mutateAsync: mocks.update, isPending: false }),
  useArchiveExtWorkflow: () => ({ mutateAsync: mocks.archive, isPending: false }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: () => ({ queryKey: ["agents"] }),
  memberListOptions: () => ({ queryKey: ["members"] }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) => selector({ user: { id: "user-me" } }),
}));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ workflows: () => "/acme/workflows", issueDetail: (id: string) => `/acme/issues/${id}` }),
}));
vi.mock("sonner", () => ({ toast: { error: mocks.toastError, success: mocks.toastSuccess } }));
vi.mock("../../navigation", () => ({
  useNavigation: () => ({ pathname: "/acme/workflows/wf-1", push: mocks.push }),
  useRowLink: () => () => ({}),
}));
vi.mock("../../layout/breadcrumb-header", () => ({
  BreadcrumbHeader: ({ leaf, actions }: { leaf: ReactNode; actions?: ReactNode }) => (
    <header>
      {leaf}
      {actions}
    </header>
  ),
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));
vi.mock("./agent-select", () => ({
  AgentSelect: ({ ariaLabel, value }: { ariaLabel: string; value: string }) => (
    <button type="button" aria-label={ariaLabel}>
      {value}
    </button>
  ),
}));
vi.mock("./workflow-runs-tab", () => ({ WorkflowRunsTab: () => <div>runs tab</div> }));

beforeEach(() => {
  mocks.update.mockReset();
  mocks.archive.mockReset();
  mocks.push.mockReset();
  mocks.toastError.mockReset();
  mocks.toastSuccess.mockReset();
});

const node = (n: number) => screen.getByRole("listitem", { name: `Node ${n}` });

describe("WorkflowDetailPage", () => {
  it("starts clean: Save is disabled until something changes", () => {
    renderWithI18n(<WorkflowDetailPage />);
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    expect(screen.queryByText("Unsaved changes")).not.toBeInTheDocument();
  });

  it("saves only the changed node set and fields", async () => {
    mocks.update.mockResolvedValue({ ...WORKFLOW, name: "Release v2" });
    renderWithI18n(<WorkflowDetailPage />);
    const name = screen.getByLabelText("Name");
    await userEvent.clear(name);
    await userEvent.type(name, "Release v2");
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(mocks.update).toHaveBeenCalled());
    const payload = mocks.update.mock.calls[0]![0];
    expect(payload).toMatchObject({ id: "wf-1", name: "Release v2", supervisor_agent_id: "ag-1", max_rewinds: 3 });
    // Nodes were not touched, so they are not resent.
    expect(payload).not.toHaveProperty("nodes");
  });

  it("maps a 422 onto the offending row and keeps the draft", async () => {
    mocks.update.mockRejectedValue(
      Object.assign(new Error("validation_failed"), {
        errors: [{ node_key: "build", field: "agent_id", message: "agent is archived" }],
      }),
    );
    renderWithI18n(<WorkflowDetailPage />);
    await userEvent.type(within(node(1)).getByLabelText("Title"), " step");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await within(node(2)).findByText("agent is archived")).toBeInTheDocument();
    expect(mocks.toastError).toHaveBeenCalledWith("Fix the highlighted problems and save again.");
    expect(within(node(1)).getByLabelText("Title")).toHaveValue("Plan step");
    expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
  });

  it("blocks saving a node set that fails local validation", async () => {
    renderWithI18n(<WorkflowDetailPage />);
    await userEvent.clear(within(node(2)).getByLabelText("Title"));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await within(node(2)).findByText("Title is required")).toBeInTheDocument();
    expect(mocks.update).not.toHaveBeenCalled();
  });

  it("guards tab switches while dirty, and warns before the page unloads", async () => {
    renderWithI18n(<WorkflowDetailPage />);
    await userEvent.type(within(node(1)).getByLabelText("Title"), "!");

    const unload = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(unload);
    expect(unload.defaultPrevented).toBe(true);

    await userEvent.click(screen.getByRole("button", { name: "Runs" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Discard unsaved changes?")).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));
    expect(screen.queryByText("runs tab")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Runs" }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Discard" }));
    expect(await screen.findByText("runs tab")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Nodes" }));
    expect(within(node(1)).getByLabelText("Title")).toHaveValue("Plan");
  });

  it("does not warn on unload when nothing changed", () => {
    renderWithI18n(<WorkflowDetailPage />);
    const unload = new Event("beforeunload", { cancelable: true });
    fireEvent(window, unload);
    expect(unload.defaultPrevented).toBe(false);
  });

  it("archives after confirmation and returns to the list", async () => {
    mocks.archive.mockResolvedValue(undefined);
    renderWithI18n(<WorkflowDetailPage />);
    await userEvent.click(screen.getAllByRole("button", { name: "Archive" })[0]!);
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Archive" }));
    await waitFor(() => expect(mocks.archive).toHaveBeenCalledWith("wf-1"));
    expect(mocks.push).toHaveBeenCalledWith("/acme/workflows");
  });
});
```

- [ ] **Step 2: Run, expect FAIL**

```bash
pnpm --filter @multica/views exec vitest run ext-workflows/components
```
Expected: FAIL (components missing).

- [ ] **Step 3: Implement**

`packages/views/ext-workflows/components/dag-preview.tsx`:

```tsx
"use client";

import { useId } from "react";
import { useT } from "../../i18n";
import { layoutDag, type NodeRow } from "../node-editor-helpers";

const MAX_LABEL = 22;

function clip(label: string): string {
  return label.length > MAX_LABEL ? `${label.slice(0, MAX_LABEL - 1)}…` : label;
}

/** Read-only graph: nodes in columns by depth, plain SVG edges. No dependency. */
export function DagPreview({
  rows,
}: {
  rows: readonly Pick<NodeRow, "rowId" | "key" | "title" | "depends_on">[];
}) {
  const { t } = useT("ext-workflows");
  const markerId = `dag-arrow-${useId().replace(/:/g, "")}`;
  const layout = layoutDag(rows);

  if (layout.nodes.length === 0) {
    return <p className="text-caption text-muted-foreground">{t(($) => $.dag.empty)}</p>;
  }

  return (
    <div className="overflow-x-auto rounded-lg border bg-muted/20">
      <svg
        role="img"
        aria-label={t(($) => $.dag.aria_label)}
        width={layout.width}
        height={layout.height}
        viewBox={`0 0 ${layout.width} ${layout.height}`}
      >
        <defs>
          <marker
            id={markerId}
            viewBox="0 0 8 8"
            refX="7"
            refY="4"
            markerWidth="7"
            markerHeight="7"
            orient="auto-start-reverse"
          >
            <path d="M 0 0 L 8 4 L 0 8 z" className="fill-muted-foreground" />
          </marker>
        </defs>
        {layout.edges.map((edge) => (
          <path
            key={`${edge.from}->${edge.to}`}
            data-dag-edge=""
            d={edge.d}
            fill="none"
            strokeWidth={1.5}
            className="stroke-muted-foreground/60"
            markerEnd={`url(#${markerId})`}
          />
        ))}
        {layout.nodes.map((n) => (
          <g key={n.rowId} data-dag-node={n.rowId}>
            <title>{n.label}</title>
            <rect x={n.x} y={n.y} width={n.w} height={n.h} rx={8} className="fill-background stroke-border" />
            <text
              x={n.x + n.w / 2}
              y={n.y + n.h / 2}
              textAnchor="middle"
              dominantBaseline="central"
              fontSize={12}
              className="fill-foreground"
            >
              {clip(n.label)}
            </text>
          </g>
        ))}
      </svg>
    </div>
  );
}
```

`packages/views/ext-workflows/components/node-editor.tsx`:

```tsx
"use client";

import { useState } from "react";
import { ArrowDown, ArrowUp, ChevronDown, ChevronRight, Plus, Trash2 } from "lucide-react";
import type { Agent } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { Switch } from "@multica/ui/components/ui/switch";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { PickerItem } from "../../issues/components/pickers/property-picker";
import {
  createRow,
  dependencyOptions,
  moveRow,
  patchRow,
  removeRow,
  setRowKey,
  setRowTitle,
  type GeneralErrorCode,
  type MappedServerErrors,
  type NodeField,
  type NodeRow,
  type RowErrorCode,
  type RowErrors,
} from "../node-editor-helpers";
import { AgentSelect } from "./agent-select";
import { DagPreview } from "./dag-preview";

export interface NodeEditorProps {
  rows: NodeRow[];
  onChange: (rows: NodeRow[]) => void;
  agents: Agent[];
  /** Client validation, passed only once the user has tried to save. */
  validation: { rows: RowErrors; general: GeneralErrorCode[] } | null;
  serverErrors: MappedServerErrors | null;
  /** Runs in progress keep using the previous definition. */
  activeRunCount: number;
  /** New rows start with this agent (the supervisor), which is usually a sensible default. */
  defaultAgentId?: string;
  readOnly?: boolean;
}

const labelOf = (r: Pick<NodeRow, "title" | "key">) => r.title.trim() || r.key;

export function NodeEditor({
  rows,
  onChange,
  agents,
  validation,
  serverErrors,
  activeRunCount,
  defaultAgentId = "",
  readOnly = false,
}: NodeEditorProps) {
  const { t } = useT("ext-workflows");
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());

  const toggleExpanded = (rowId: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(rowId)) next.delete(rowId);
      else next.add(rowId);
      return next;
    });

  const messagesFor = (row: NodeRow): Partial<Record<NodeField, string>> => {
    const out: Partial<Record<NodeField, string>> = {};
    const client = validation?.rows[row.rowId];
    if (client) {
      for (const [field, code] of Object.entries(client)) {
        out[field as NodeField] = t(($) => $.errors[code as RowErrorCode]);
      }
    }
    const server = serverErrors?.byRow[row.rowId];
    if (server) {
      for (const [field, message] of Object.entries(server)) {
        out[field as NodeField] ??= message;
      }
    }
    return out;
  };

  const general = [
    ...(validation?.general.map((code) => t(($) => $.errors[code])) ?? []),
    ...(serverErrors?.general ?? []),
  ];

  return (
    <div className="space-y-4">
      {activeRunCount > 0 && (
        <p className="rounded-md border border-warning/40 bg-warning/10 px-3 py-2 text-caption">
          {t(($) => $.nodes.active_runs_note, { count: activeRunCount })}
        </p>
      )}

      {general.length > 0 && (
        <ul role="alert" className="space-y-1 text-caption text-destructive">
          {general.map((message, i) => (
            <li key={`${i}-${message}`}>{message}</li>
          ))}
        </ul>
      )}

      <DagPreview rows={rows} />

      {rows.length === 0 ? (
        <p className="rounded-lg border border-dashed px-4 py-6 text-center text-body text-muted-foreground">
          {t(($) => $.nodes.empty)}
        </p>
      ) : (
        <ol className="space-y-2">
          {rows.map((row, index) => (
            <NodeRowEditor
              key={row.rowId}
              row={row}
              index={index}
              total={rows.length}
              rows={rows}
              agents={agents}
              messages={messagesFor(row)}
              isExpanded={expanded.has(row.rowId) || !!messagesFor(row).prompt}
              readOnly={readOnly}
              onToggleExpanded={() => toggleExpanded(row.rowId)}
              onTitle={(title) => onChange(setRowTitle(rows, row.rowId, title))}
              onKey={(key) => onChange(setRowKey(rows, row.rowId, key))}
              onPatch={(patch) => onChange(patchRow(rows, row.rowId, patch))}
              onMove={(delta) => onChange(moveRow(rows, index, delta))}
              onDelete={() => onChange(removeRow(rows, row.rowId))}
            />
          ))}
        </ol>
      )}

      {!readOnly && (
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => onChange([...rows, createRow(rows, defaultAgentId)])}
        >
          <Plus className="size-3.5" />
          {t(($) => $.nodes.add)}
        </Button>
      )}
    </div>
  );
}

function NodeRowEditor({
  row,
  index,
  total,
  rows,
  agents,
  messages,
  isExpanded,
  readOnly,
  onToggleExpanded,
  onTitle,
  onKey,
  onPatch,
  onMove,
  onDelete,
}: {
  row: NodeRow;
  index: number;
  total: number;
  rows: NodeRow[];
  agents: Agent[];
  messages: Partial<Record<NodeField, string>>;
  isExpanded: boolean;
  readOnly: boolean;
  onToggleExpanded: () => void;
  onTitle: (title: string) => void;
  onKey: (key: string) => void;
  onPatch: (patch: Partial<NodeRow>) => void;
  onMove: (delta: -1 | 1) => void;
  onDelete: () => void;
}) {
  const { t } = useT("ext-workflows");
  const archivedAgent = !!agents.find((a) => a.id === row.agent_id)?.archived_at;
  const messageList = Object.values(messages);
  const hasError = (field: NodeField) => !!messages[field];

  return (
    <li aria-label={t(($) => $.nodes.node_n, { n: index + 1 })} className="space-y-2 rounded-lg border bg-background p-3">
      <div className="flex items-center gap-2">
        <span className="w-5 shrink-0 text-center text-micro tabular-nums text-muted-foreground">{index + 1}</span>
        <Input
          aria-label={t(($) => $.nodes.title_label)}
          aria-invalid={hasError("title") || undefined}
          value={row.title}
          disabled={readOnly}
          placeholder={t(($) => $.nodes.title_placeholder)}
          onChange={(e) => onTitle(e.target.value)}
          className="min-w-0 flex-1"
        />
        <Input
          aria-label={t(($) => $.nodes.key_label)}
          aria-invalid={hasError("key") || undefined}
          value={row.key}
          disabled={readOnly}
          onChange={(e) => onKey(e.target.value)}
          className="w-36 shrink-0 font-mono text-caption"
        />
        {!readOnly && (
          <div className="flex shrink-0 items-center">
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              aria-label={t(($) => $.nodes.move_up)}
              disabled={index === 0}
              onClick={() => onMove(-1)}
            >
              <ArrowUp />
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              aria-label={t(($) => $.nodes.move_down)}
              disabled={index === total - 1}
              onClick={() => onMove(1)}
            >
              <ArrowDown />
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              aria-label={t(($) => $.nodes.delete)}
              onClick={onDelete}
            >
              <Trash2 />
            </Button>
          </div>
        )}
      </div>

      <div className="flex flex-wrap items-center gap-2 pl-7">
        <AgentSelect
          agents={agents}
          value={row.agent_id}
          onChange={(agent_id) => onPatch({ agent_id })}
          ariaLabel={t(($) => $.nodes.agent_label)}
          placeholder={t(($) => $.nodes.agent_placeholder)}
          disabled={readOnly}
          invalid={hasError("agent_id") || archivedAgent}
          className="w-52"
        />
        <DependsPicker row={row} rows={rows} readOnly={readOnly} invalid={hasError("depends_on")} onChange={(depends_on) => onPatch({ depends_on })} />
        <label className="flex items-center gap-1.5 text-caption">
          <Switch
            size="sm"
            checked={row.requires_review}
            disabled={readOnly}
            onCheckedChange={(requires_review) => onPatch({ requires_review })}
          />
          {t(($) => $.nodes.review_label)}
        </label>
        <label className="flex items-center gap-1.5 text-caption text-muted-foreground">
          {t(($) => $.nodes.max_attempts_label)}
          <Input
            type="number"
            min={1}
            max={10}
            value={row.max_attempts}
            disabled={readOnly}
            aria-invalid={hasError("max_attempts") || undefined}
            onChange={(e) => onPatch({ max_attempts: e.target.valueAsNumber })}
            className="w-16"
          />
        </label>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          aria-expanded={isExpanded}
          aria-label={isExpanded ? t(($) => $.nodes.collapse) : t(($) => $.nodes.expand)}
          onClick={onToggleExpanded}
        >
          {isExpanded ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
          {t(($) => $.nodes.prompt_label)}
        </Button>
      </div>

      {isExpanded && (
        <div className="pl-7">
          <Textarea
            aria-label={t(($) => $.nodes.prompt_label)}
            aria-invalid={hasError("prompt") || undefined}
            value={row.prompt}
            disabled={readOnly}
            placeholder={t(($) => $.nodes.prompt_placeholder)}
            rows={6}
            onChange={(e) => onPatch({ prompt: e.target.value })}
          />
        </div>
      )}

      {(messageList.length > 0 || archivedAgent) && (
        <ul className="space-y-0.5 pl-7 text-caption text-destructive">
          {archivedAgent && <li>{t(($) => $.nodes.archived_agent)}</li>}
          {messageList.map((m) => (
            <li key={m}>{m}</li>
          ))}
        </ul>
      )}
    </li>
  );
}

function DependsPicker({
  row,
  rows,
  readOnly,
  invalid,
  onChange,
}: {
  row: NodeRow;
  rows: NodeRow[];
  readOnly: boolean;
  invalid: boolean;
  onChange: (dependsOn: string[]) => void;
}) {
  const { t } = useT("ext-workflows");
  const [open, setOpen] = useState(false);
  // Only choices that cannot close a cycle are offered.
  const options = dependencyOptions(rows, row.rowId);
  const selected = row.depends_on
    .map((id) => rows.find((r) => r.rowId === id))
    .filter((r): r is NodeRow => !!r);
  const toggle = (id: string) =>
    onChange(row.depends_on.includes(id) ? row.depends_on.filter((d) => d !== id) : [...row.depends_on, id]);

  return (
    <Popover open={open} onOpenChange={readOnly ? undefined : setOpen}>
      <PopoverTrigger
        disabled={readOnly}
        aria-label={t(($) => $.nodes.depends_label)}
        className={cn(
          "flex max-w-64 min-w-32 items-center gap-1.5 rounded-lg border bg-background px-2.5 py-1.5 text-left text-caption hover:bg-muted disabled:opacity-60",
          invalid ? "border-destructive" : "border-border",
        )}
      >
        <span className="shrink-0 text-muted-foreground">{t(($) => $.nodes.depends_label)}:</span>
        <span className="min-w-0 truncate">
          {selected.length === 0 ? t(($) => $.nodes.depends_none) : selected.map(labelOf).join(", ")}
        </span>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-64 p-1">
        {options.length === 0 ? (
          <p className="px-2 py-1.5 text-caption text-muted-foreground">{t(($) => $.nodes.depends_no_options)}</p>
        ) : (
          options.map((option) => (
            <PickerItem key={option.rowId} selected={row.depends_on.includes(option.rowId)} onClick={() => toggle(option.rowId)}>
              <span className="truncate">{labelOf(option)}</span>
            </PickerItem>
          ))
        )}
      </PopoverContent>
    </Popover>
  );
}
```

Note: the test's inline Popover mock renders `PopoverTrigger` as a `<button aria-label="Depends on">`, and `PickerItem` renders a real `<button>`, so the toggle test finds each option by `getByRole("button", { name: "Plan" })` inside the row.

`packages/views/ext-workflows/components/workflow-runs-tab.tsx`:

```tsx
"use client";

import { useQuery } from "@tanstack/react-query";
import { extWorkflowRunsOptions } from "@multica/core/ext-workflows";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@multica/ui/components/ui/table";
import { useT, useTimeAgo } from "../../i18n";
import { useRowLink } from "../../navigation";
import { RunStatusBadge } from "./status";

export function WorkflowRunsTab({ workflowId }: { workflowId: string }) {
  const { t } = useT("ext-workflows");
  const timeAgo = useTimeAgo();
  const wsId = useWorkspaceId();
  const p = useWorkspacePaths();
  const rowLink = useRowLink();
  const { data, isLoading } = useQuery({
    ...extWorkflowRunsOptions(wsId, workflowId),
    enabled: !!wsId && !!workflowId,
  });
  const runs = data?.runs ?? [];

  if (isLoading) return <Skeleton className="h-24 w-full" />;
  if (runs.length === 0) {
    return <p className="py-8 text-center text-body text-muted-foreground">{t(($) => $.runs.empty)}</p>;
  }

  return (
    <div className="space-y-2">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t(($) => $.runs.col_issue)}</TableHead>
            <TableHead>{t(($) => $.runs.col_status)}</TableHead>
            <TableHead>{t(($) => $.runs.col_started)}</TableHead>
            <TableHead>{t(($) => $.runs.col_finished)}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {runs.map((run) => (
            <TableRow
              key={run.id}
              className="cursor-pointer"
              {...rowLink(p.issueDetail(run.issue_id), run.issue_identifier)}
            >
              <TableCell className="max-w-0">
                <div className="flex min-w-0 items-center gap-2">
                  <span className="shrink-0 text-caption text-muted-foreground">{run.issue_identifier}</span>
                  <span className="truncate">{run.issue_title}</span>
                </div>
              </TableCell>
              <TableCell>
                <RunStatusBadge status={run.status} />
              </TableCell>
              <TableCell className="whitespace-nowrap text-caption text-muted-foreground">
                {timeAgo(run.started_at)}
              </TableCell>
              <TableCell className="whitespace-nowrap text-caption text-muted-foreground">
                {run.finished_at ? timeAgo(run.finished_at) : "—"}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {data && data.total > runs.length && (
        <p className="text-caption text-muted-foreground">
          {t(($) => $.runs.showing, { shown: runs.length, total: data.total })}
        </p>
      )}
    </div>
  );
}
```

`packages/views/ext-workflows/components/workflow-detail-page.tsx`:

```tsx
"use client";

import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { GitBranch, History, Save, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { useAuthStore } from "@multica/core/auth";
import {
  extWorkflowDetailOptions,
  useArchiveExtWorkflow,
  useUpdateExtWorkflow,
  type ExtWorkflow,
} from "@multica/core/ext-workflows";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { resolvePublicFileUrl } from "@multica/core/workspace/avatar-url";
import { agentListOptions, memberListOptions } from "@multica/core/workspace/queries";
import type { Agent, MemberWithUser } from "@multica/core/types";
import { ActorAvatar as ActorAvatarBase } from "@multica/ui/components/common/actor-avatar";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { ActorAvatar } from "../../common/actor-avatar";
import { useT, useTimeAgo } from "../../i18n";
import { BreadcrumbHeader } from "../../layout/breadcrumb-header";
import { useNavigation } from "../../navigation";
import {
  extractValidationErrors,
  mapServerErrors,
  nodesToRows,
  rowsToInput,
  serializeRows,
  validateRows,
  type MappedServerErrors,
  type NodeRow,
} from "../node-editor-helpers";
import { AgentSelect } from "./agent-select";
import { NodeEditor } from "./node-editor";
import { WorkflowRunsTab } from "./workflow-runs-tab";

type DetailTab = "nodes" | "runs";
const DETAIL_TABS: { id: DetailTab; icon: typeof GitBranch }[] = [
  { id: "nodes", icon: GitBranch },
  { id: "runs", icon: History },
];

interface Draft {
  name: string;
  description: string;
  supervisorId: string;
  maxRewinds: number;
  rows: NodeRow[];
}

function draftFrom(w: ExtWorkflow): Draft {
  return {
    name: w.name,
    description: w.description ?? "",
    supervisorId: w.supervisor_agent_id,
    maxRewinds: w.max_rewinds,
    rows: nodesToRows(w.nodes ?? []),
  };
}

function signature(d: Draft): string {
  return JSON.stringify([d.name, d.description, d.supervisorId, d.maxRewinds, serializeRows(d.rows)]);
}

export function WorkflowDetailPage() {
  const { t } = useT("ext-workflows");
  const wsId = useWorkspaceId();
  const p = useWorkspacePaths();
  const { pathname, push } = useNavigation();
  const workflowId = pathname.split("/").pop() ?? "";
  const currentUser = useAuthStore((s) => s.user);

  const { data: workflow, isError } = useQuery({
    ...extWorkflowDetailOptions(wsId, workflowId),
    enabled: !!wsId && !!workflowId,
  });
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const update = useUpdateExtWorkflow(wsId);
  const archive = useArchiveExtWorkflow(wsId);

  const [baseline, setBaseline] = useState<Draft | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [tab, setTab] = useState<DetailTab>("nodes");
  const [pendingTab, setPendingTab] = useState<DetailTab | null>(null);
  const [showErrors, setShowErrors] = useState(false);
  const [serverErrors, setServerErrors] = useState<MappedServerErrors | null>(null);
  const [confirmArchive, setConfirmArchive] = useState(false);

  const dirty = !!draft && !!baseline && signature(draft) !== signature(baseline);
  const serverDraft = workflow ? draftFrom(workflow) : null;
  // Adopt server state while the user has no unsaved edits (first load, a
  // realtime update, or right after Discard). Never overwrite a dirty draft.
  if (serverDraft && !dirty && (!baseline || signature(serverDraft) !== signature(baseline))) {
    setBaseline(serverDraft);
    setDraft(serverDraft);
  }

  useEffect(() => {
    if (!dirty) return;
    const warn = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  const myRole = useMemo(
    () => members.find((m: MemberWithUser) => m.user_id === currentUser?.id)?.role ?? null,
    [members, currentUser],
  );
  // Mirrors canManageSquad: workspace admins, or the creator.
  const canManage =
    myRole === "owner" || myRole === "admin" || (!!currentUser && workflow?.creator_id === currentUser.id);

  const nodesChanged = !!draft && !!baseline && serializeRows(draft.rows) !== serializeRows(baseline.rows);
  const validation = useMemo(
    () => (draft && showErrors ? validateRows(draft.rows) : null),
    [draft, showErrors],
  );

  if (isError) {
    return <p className="p-6 text-body text-muted-foreground">{t(($) => $.detail.not_found)}</p>;
  }
  if (!workflow || !draft || !baseline) return <DetailSkeleton />;

  const patch = (next: Partial<Draft>) => setDraft({ ...draft, ...next });
  const canSave = canManage && dirty && !!draft.name.trim() && !!draft.supervisorId && !update.isPending;

  const requestTab = (next: DetailTab) => {
    if (next === tab) return;
    if (dirty) {
      setPendingTab(next);
      return;
    }
    setTab(next);
  };

  const discard = () => {
    setDraft(baseline);
    setShowErrors(false);
    setServerErrors(null);
  };

  const save = async () => {
    if (!canSave) return;
    if (nodesChanged) {
      setShowErrors(true);
      if (!validateRows(draft.rows).valid) return;
    }
    try {
      const saved = await update.mutateAsync({
        id: workflowId,
        name: draft.name.trim(),
        description: draft.description.trim(),
        supervisor_agent_id: draft.supervisorId,
        max_rewinds: draft.maxRewinds,
        ...(nodesChanged ? { nodes: rowsToInput(draft.rows) } : {}),
      });
      const next = draftFrom(saved);
      setBaseline(next);
      setDraft(next);
      setShowErrors(false);
      setServerErrors(null);
      toast.success(t(($) => $.detail.saved));
    } catch (err) {
      const errors = extractValidationErrors(err);
      if (errors) {
        setServerErrors(mapServerErrors(errors, draft.rows));
        toast.error(t(($) => $.detail.validation_failed));
      } else {
        toast.error(err instanceof Error && err.message ? err.message : t(($) => $.detail.save_failed));
      }
    }
  };

  const doArchive = async () => {
    try {
      await archive.mutateAsync(workflowId);
      toast.success(t(($) => $.archive_dialog.success));
      push(p.workflows());
    } catch (err) {
      toast.error(err instanceof Error && err.message ? err.message : t(($) => $.archive_dialog.failed));
    }
  };

  const creatorName = members.find((m: MemberWithUser) => m.user_id === workflow.creator_id)?.name;
  const initials = workflow.name
    .split(" ")
    .map((w) => w[0])
    .join("")
    .toUpperCase()
    .slice(0, 2);

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <BreadcrumbHeader
        segments={[{ href: p.workflows(), label: t(($) => $.page.title) }]}
        leaf={
          <>
            <ActorAvatarBase
              name={workflow.name}
              initials={initials}
              avatarUrl={workflow.avatar_url ? resolvePublicFileUrl(workflow.avatar_url) : undefined}
              isWorkflow
              size="sm"
            />
            <h1 className="truncate text-body font-medium text-foreground">{workflow.name}</h1>
          </>
        }
        actions={
          canManage ? (
            <>
              {dirty && <span className="text-caption text-muted-foreground">{t(($) => $.detail.unsaved)}</span>}
              {dirty && (
                <Button size="sm" variant="ghost" onClick={discard}>
                  {t(($) => $.detail.discard)}
                </Button>
              )}
              <Button size="sm" onClick={() => void save()} disabled={!canSave} aria-busy={update.isPending}>
                <Save className="size-3.5" />
                {update.isPending ? t(($) => $.detail.saving) : t(($) => $.detail.save)}
              </Button>
              <Button
                size="sm"
                variant="ghost"
                className="text-destructive hover:text-destructive"
                onClick={() => setConfirmArchive(true)}
              >
                <Trash2 className="size-3.5" />
                {t(($) => $.detail.archive_button)}
              </Button>
            </>
          ) : null
        }
      />

      <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3 md:grid md:grid-cols-[minmax(0,1fr)_280px] md:gap-4 md:overflow-hidden md:p-6 lg:grid-cols-[minmax(0,1fr)_320px]">
        <div className="flex min-h-[60vh] flex-col overflow-hidden rounded-lg border bg-background md:h-full md:min-h-0">
          <div className="flex shrink-0 items-center gap-0 overflow-x-auto border-b px-2 md:px-4">
            {DETAIL_TABS.map((entry) => (
              <button
                key={entry.id}
                type="button"
                onClick={() => requestTab(entry.id)}
                className={`flex shrink-0 items-center gap-1.5 whitespace-nowrap border-b-2 px-3 py-2.5 text-caption font-medium transition-colors ${
                  tab === entry.id
                    ? "border-foreground text-foreground"
                    : "border-transparent text-muted-foreground hover:text-foreground"
                }`}
              >
                <entry.icon className="size-3.5" />
                {t(($) => $.detail.tabs[entry.id])}
              </button>
            ))}
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto p-4 md:p-6">
            {tab === "nodes" ? (
              <NodeEditor
                rows={draft.rows}
                onChange={(rows) => {
                  setServerErrors(null);
                  patch({ rows });
                }}
                agents={agents as Agent[]}
                validation={validation}
                serverErrors={serverErrors}
                activeRunCount={workflow.active_run_count}
                defaultAgentId={draft.supervisorId}
                readOnly={!canManage}
              />
            ) : (
              <WorkflowRunsTab workflowId={workflowId} />
            )}
          </div>
        </div>

        <aside className="flex w-full flex-col gap-4 rounded-lg border bg-background p-5 md:h-full md:min-h-0 md:overflow-y-auto">
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-inspector-name" className="text-caption text-muted-foreground">
              {t(($) => $.inspector.name)}
            </Label>
            <Input
              id="ext-workflow-inspector-name"
              value={draft.name}
              disabled={!canManage}
              onChange={(e) => patch({ name: e.target.value })}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-inspector-description" className="text-caption text-muted-foreground">
              {t(($) => $.inspector.description)}
            </Label>
            <Textarea
              id="ext-workflow-inspector-description"
              value={draft.description}
              disabled={!canManage}
              placeholder={t(($) => $.inspector.description_placeholder)}
              rows={3}
              onChange={(e) => patch({ description: e.target.value })}
            />
          </div>
          <div className="space-y-1.5">
            <Label className="text-caption text-muted-foreground">{t(($) => $.inspector.supervisor)}</Label>
            <AgentSelect
              agents={agents as Agent[]}
              value={draft.supervisorId}
              onChange={(supervisorId) => patch({ supervisorId })}
              ariaLabel={t(($) => $.inspector.supervisor)}
              placeholder={t(($) => $.create.supervisor_placeholder)}
              requireRuntime
              disabled={!canManage}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-inspector-rewinds" className="text-caption text-muted-foreground">
              {t(($) => $.inspector.max_rewinds)}
            </Label>
            <Input
              id="ext-workflow-inspector-rewinds"
              type="number"
              min={0}
              max={10}
              value={draft.maxRewinds}
              disabled={!canManage}
              onChange={(e) => {
                const n = e.target.valueAsNumber;
                patch({ maxRewinds: Number.isNaN(n) ? 0 : Math.min(10, Math.max(0, Math.trunc(n))) });
              }}
              className="w-24"
            />
            <p className="text-caption text-muted-foreground">{t(($) => $.inspector.max_rewinds_hint)}</p>
          </div>
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 border-t pt-4 text-caption">
            <PropRow label={t(($) => $.inspector.created_by)}>
              <span className="flex min-w-0 items-center gap-1.5">
                <ActorAvatar actorType="member" actorId={workflow.creator_id} size="xs" />
                <span className="truncate">{creatorName ?? workflow.creator_id.slice(0, 8)}</span>
              </span>
            </PropRow>
            <PropRow label={t(($) => $.inspector.updated)}>
              <UpdatedAt value={workflow.updated_at} />
            </PropRow>
          </dl>
        </aside>
      </div>

      {pendingTab !== null && (
        <AlertDialog open onOpenChange={(v) => { if (!v) setPendingTab(null); }}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t(($) => $.detail.discard_dialog.title)}</AlertDialogTitle>
              <AlertDialogDescription>{t(($) => $.detail.discard_dialog.description)}</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>{t(($) => $.detail.discard_dialog.keep_editing)}</AlertDialogCancel>
              <AlertDialogAction
                variant="destructive"
                onClick={() => {
                  discard();
                  setTab(pendingTab);
                  setPendingTab(null);
                }}
              >
                {t(($) => $.detail.discard_dialog.discard)}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}

      {confirmArchive && (
        <AlertDialog open onOpenChange={(v) => { if (!v && !archive.isPending) setConfirmArchive(false); }}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t(($) => $.archive_dialog.title)}</AlertDialogTitle>
              <AlertDialogDescription>
                {t(($) => $.archive_dialog.description, { name: workflow.name })}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel disabled={archive.isPending}>{t(($) => $.archive_dialog.cancel)}</AlertDialogCancel>
              <AlertDialogAction
                onClick={() => void doArchive()}
                disabled={archive.isPending}
                className="bg-destructive text-white hover:bg-destructive/90"
              >
                {archive.isPending ? t(($) => $.archive_dialog.archiving) : t(($) => $.archive_dialog.confirm)}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  );
}

function PropRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
  );
}

function UpdatedAt({ value }: { value: string }) {
  const timeAgo = useTimeAgo();
  return <span className="text-muted-foreground">{timeAgo(value)}</span>;
}

function DetailSkeleton() {
  return (
    <div className="flex flex-1 flex-col gap-3 p-6 md:grid md:grid-cols-[minmax(0,1fr)_320px]">
      <Skeleton className="h-64 w-full" />
      <Skeleton className="h-64 w-full" />
    </div>
  );
}
```

Notes for the implementer of this step: the render-time `setBaseline` / `setDraft` block is the React "adjust state while rendering" pattern (it is guarded by the `!dirty` and signature comparison, so it settles after one extra render). The hooks `useMemo` and `useEffect` all sit above the early returns. `AlertDialogAction variant="destructive"` is the same prop squads uses.

In the test for "archives after confirmation", the first "Archive" button is the header action (the only one before the dialog opens).

`packages/views/ext-workflows/index.ts`:

```ts
export { WorkflowsPage } from "./components/workflows-page";
export { WorkflowDetailPage } from "./components/workflow-detail-page";
```

`apps/web/app/[workspaceSlug]/(dashboard)/workflows/[id]/page.tsx`:

```tsx
export { WorkflowDetailPage as default } from "@multica/views/ext-workflows";
```

`apps/desktop/src/renderer/src/routes.tsx`: change the D4 import to `import { WorkflowsPage, WorkflowDetailPage } from "@multica/views/ext-workflows"; // ext-workflow` and add after the `workflows` route:

```tsx
          {
            path: "workflows/:id", // ext-workflow
            element: <WorkflowDetailPage />,
            handle: { title: "Workflow" },
          },
```

- [ ] **Step 4: Run, expect PASS**

```bash
pnpm --filter @multica/views exec vitest run ext-workflows
pnpm typecheck
pnpm --filter @multica/views lint
```

- [ ] **Step 5: Commit**

```bash
git add packages/views/ext-workflows "apps/web/app/[workspaceSlug]/(dashboard)/workflows" apps/desktop/src/renderer/src/routes.tsx
git commit -m "feat(ext-workflow): add workflow detail page with node editor, DAG preview and runs tab

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D6: Issue surfaces: workflow avatar, assignee picker group, run-confirm gate, create-issue hint, batch toolbar, inbox label

**Files:**
- Modify: `packages/views/common/actor-avatar.tsx` (base props at line 121; `shouldLinkToProfile` at line 143; `profileHref` at line 149)
- Modify: `packages/views/issues/components/pickers/assignee-picker.tsx` (imports at line 9 to 13; queries at line 107; filters at line 131; new section after the Squads section ending at line 280; empty check at line 283)
- Modify: `packages/views/issues/actions/run-confirm-gate.ts` (lines 16, 23, 81, 99)
- Modify: `packages/views/issues/components/batch-action-toolbar.tsx` (line 128)
- Modify: `packages/views/modals/run-confirm.tsx` (line 135 to 136)
- Modify: `packages/views/modals/create-issue.tsx` (`CreateRunHint`, lines 130, 143, 157)
- Modify: `packages/views/inbox/components/inbox-detail-label.tsx` (`useTypeLabels`, lines 16 to 41)
- Test: `packages/views/common/actor-avatar-profile-link.test.tsx`, `packages/views/issues/components/pickers/assignee-picker.workflows.test.tsx` (new), `packages/views/issues/actions/run-confirm-gate.test.ts`, `packages/views/inbox/components/inbox-detail-label.test.tsx`

**Interfaces:**
- Consumes (core): `IssueAssigneeType` includes `"workflow"` and `getActorName` / `getActorAvatarUrl` / `hasActor` resolve workflows (Part A, `packages/core/types/issue.ts`, `packages/core/workspace/hooks.ts`); `extWorkflowListOptions(wsId)`; `paths.workflowDetail(id)`; `ExtWorkflow.node_count`, `.archived_at`.
- Consumes (D4): `isWorkflow` prop on the base avatar. Consumes (D1): `pickers.assignee.workflows_group`, `workflow_needs_nodes`, `run_confirm.create_will_start_workflow`, inbox `types.ext_workflow_escalation`.
- Produces: `ActorAvatar actorType="workflow"` (glyph, profile link to the workflow page); `Workflows` group in the assignee picker; gate and hint copy for workflow assignees.

Not changed on purpose: `modals/quick-create-issue.tsx` (see notes), board/swimlane assignee grouping (`actorRef.type` filters), mobile.

- [ ] **Step 1: Write the failing tests**

(a) `packages/views/common/actor-avatar-profile-link.test.tsx`: add `workflowDetail: (id: string) => \`/acme/workflows/${id}\`,` to the `useWorkspacePaths` mock (after `squadDetail`), and add inside `describe("ActorAvatar profile link", ...)`:

```tsx
  it("renders a workflow actor with the workflow glyph and links to its page", () => {
    const push = vi.fn();
    const { container } = render(
      <NavigationProvider value={makeAdapter({ push })}>
        <ActorAvatar actorType="workflow" actorId="wf1" />
      </NavigationProvider>,
    );
    // The base avatar draws a lucide glyph (an svg) instead of initials for workflows.
    expect(container.querySelector('[data-slot="avatar"] svg')).not.toBeNull();
    fireEvent.click(screen.getByRole("link"));
    expect(push).toHaveBeenCalledWith("/acme/workflows/wf1");
  });
```

(b) `packages/views/issues/actions/run-confirm-gate.test.ts`: add

```ts
describe("runConfirmIntent — workflow owners", () => {
  it("confirms assigning a workflow like an agent or squad", () => {
    expect(
      runConfirmIntent(issue({ status: "todo" }), { assignee_type: "workflow", assignee_id: "wf-1" }, CATALOG),
    ).toEqual({ issueIds: ["issue-1"], mode: "assign", assigneeType: "workflow", assigneeId: "wf-1" });
  });

  it("applies directly when the issue is parked in backlog", () => {
    expect(
      runConfirmIntent(issue({ status: "backlog" }), { assignee_type: "workflow", assignee_id: "wf-1" }, CATALOG),
    ).toBeNull();
  });

  it("confirms promoting a workflow-owned issue out of backlog", () => {
    expect(
      runConfirmIntent(
        issue({ status: "backlog", assignee_type: "workflow", assignee_id: "wf-1" }),
        { status: "todo" },
        CATALOG,
      ),
    ).toEqual({
      issueIds: ["issue-1"],
      mode: "promote",
      status: "todo",
      assigneeType: "workflow",
      assigneeId: "wf-1",
    });
  });
});
```

(c) `packages/views/inbox/components/inbox-detail-label.test.tsx`: add inside the main `describe` (use the file's existing `item()` helper):

```tsx
  it("labels an ext workflow escalation", () => {
    const { container } = render(
      <InboxDetailLabel item={item({ type: "ext_workflow_escalation" as InboxItem["type"] })} />,
    );
    expect(container).toHaveTextContent("Workflow needs a decision");
  });
```

(d) New `packages/views/issues/components/pickers/assignee-picker.workflows.test.tsx` (mocks mirror `assignee-picker.keyboard.test.tsx`):

```tsx
// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enIssues from "../../../locales/en/issues.json";
import { AssigneePicker } from "./assignee-picker";

const WORKFLOWS = [
  { id: "wf-1", name: "Release pipeline", node_count: 3, archived_at: null },
  { id: "wf-2", name: "Empty flow", node_count: 0, archived_at: null },
  { id: "wf-3", name: "Retired flow", node_count: 2, archived_at: "2026-01-01T00:00:00Z" },
];

vi.mock("@tanstack/react-query", () => ({
  useQuery: ({ queryKey }: { queryKey: string[] }) => {
    if (queryKey[0] === "members") return { data: [{ user_id: "user-1", name: "Ada Lovelace", role: "member" }] };
    if (queryKey[0] === "agents") return { data: [{ id: "ag-1", name: "Planner", archived_at: null }] };
    if (queryKey[0] === "squads") return { data: [{ id: "sq-1", name: "Core squad", leader_id: "ag-1", archived_at: null }] };
    if (queryKey[0] === "ext-workflows") return { data: WORKFLOWS };
    return { data: [] };
  },
}));
vi.mock("@multica/core/ext-workflows", () => ({
  extWorkflowListOptions: () => ({ queryKey: ["ext-workflows", "workspace-1", "list"] }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));
vi.mock("@multica/core/auth", () => ({ useAuthStore: () => ({ id: "user-1" }) }));
vi.mock("@multica/core/agents", () => ({ isAgentRuntimeBound: () => true }));
vi.mock("@multica/core/permissions", () => ({ canAssignAgentToIssue: () => ({ allowed: true }) }));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Someone" }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"] }),
  agentListOptions: () => ({ queryKey: ["agents"] }),
  squadListOptions: () => ({ queryKey: ["squads"] }),
  assigneeFrequencyOptions: () => ({ queryKey: ["frequency"] }),
}));
vi.mock("../../../common/actor-avatar", () => ({
  ActorAvatar: () => <span data-testid="actor-avatar" />,
}));

function renderPicker(onUpdate: () => void, props: Record<string, unknown> = {}) {
  return render(
    <I18nProvider locale="en" resources={{ en: { issues: enIssues } }}>
      <AssigneePicker assigneeType={null} assigneeId={null} onUpdate={onUpdate} open onOpenChange={() => {}} {...props} />
    </I18nProvider>,
  );
}

describe("AssigneePicker workflows group", () => {
  it("lists non-archived workflows in a group after Squads", async () => {
    renderPicker(vi.fn());
    const squads = await screen.findByText("Squads");
    const workflows = screen.getByText("Workflows");
    expect(squads.compareDocumentPosition(workflows) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole("button", { name: "Release pipeline" })).toBeInTheDocument();
    expect(screen.queryByText("Retired flow")).not.toBeInTheDocument();
  });

  it("assigns the issue to a workflow", async () => {
    const onUpdate = vi.fn();
    renderPicker(onUpdate);
    await userEvent.click(await screen.findByRole("button", { name: "Release pipeline" }));
    expect(onUpdate).toHaveBeenCalledWith({ assignee_type: "workflow", assignee_id: "wf-1" });
  });

  it("disables a workflow that has no nodes yet", async () => {
    const onUpdate = vi.fn();
    renderPicker(onUpdate);
    const row = await screen.findByRole("button", { name: "Empty flow" });
    expect(row).toBeDisabled();
    await userEvent.click(row);
    expect(onUpdate).not.toHaveBeenCalled();
  });

  it("filters workflows by the search box", async () => {
    renderPicker(vi.fn());
    await userEvent.type(await screen.findByPlaceholderText("Assign to..."), "release");
    expect(screen.getByRole("button", { name: "Release pipeline" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Empty flow" })).not.toBeInTheDocument();
  });
});
```

- [ ] **Step 2: Run, expect FAIL**

```bash
pnpm --filter @multica/views exec vitest run common/actor-avatar-profile-link.test.tsx issues/actions/run-confirm-gate.test.ts inbox/components/inbox-detail-label.test.tsx issues/components/pickers
```
Expected: FAIL (no workflow glyph or link, gate returns null, label missing, no Workflows group).

- [ ] **Step 3: Implement**

3a. `packages/views/common/actor-avatar.tsx`

```tsx
    <ActorAvatarBase
      ...
      isSquad={actorType === "squad"}
      isWorkflow={actorType === "workflow"} // ext-workflow: workflow glyph
      ...
    />
```
```tsx
  const shouldLinkToProfile =
    profileAvailable &&
    (profileLink ??
      (actorType === "member" ||
        actorType === "agent" ||
        actorType === "squad" ||
        actorType === "workflow")); // ext-workflow
  const profileHref = shouldLinkToProfile
    ? actorType === "member"
      ? paths.memberDetail(actorId)
      : actorType === "agent"
        ? paths.agentDetail(actorId)
        : actorType === "squad"
          ? paths.squadDetail(actorId)
          : actorType === "workflow" // ext-workflow
            ? paths.workflowDetail(actorId)
            : null
    : null;
```
No hover card for workflows: the existing chain falls through to `return content`.

3b. `packages/views/issues/components/pickers/assignee-picker.tsx`

Imports: add `import { extWorkflowListOptions } from "@multica/core/ext-workflows"; // ext-workflow` next to the workspace queries import.

After `const { data: squads = [] } = useQuery(squadListOptions(wsId));`:

```ts
  const { data: workflows = [] } = useQuery(extWorkflowListOptions(wsId)); // ext-workflow
```

After `filteredSquads`:

```ts
  // ext-workflow: non-archived workflows, most-used first.
  const filteredWorkflows = workflows
    .filter((w) => !w.archived_at && (w.name.toLowerCase().includes(query) || matchesPinyin(w.name, query)))
    .sort((a, b) => getFreq("workflow", b.id) - getFreq("workflow", a.id));
```

After the Squads `PickerSection` block, before the empty-state check:

```tsx
      {/* Workflows (ext-workflow) — assigning starts a run that fans the issue
          out into step issues. A workflow with no nodes cannot start one. */}
      {filteredWorkflows.length > 0 && (
        <PickerSection label={t(($) => $.pickers.assignee.workflows_group)}>
          {filteredWorkflows.map((w) => {
            const startable = w.node_count > 0;
            return (
              <PickerItem
                key={w.id}
                selected={isSelected("workflow", w.id)}
                disabled={!startable}
                tooltip={startable ? undefined : t(($) => $.pickers.assignee.workflow_needs_nodes)}
                onClick={() => {
                  if (!startable) return;
                  onUpdate({ assignee_type: "workflow", assignee_id: w.id });
                  setOpen(false);
                }}
              >
                <ActorAvatar actorType="workflow" actorId={w.id} size="sm" />
                <span className="truncate">{w.name}</span>
              </PickerItem>
            );
          })}
        </PickerSection>
      )}
```
and extend the empty condition:

```tsx
      {filteredMembers.length === 0 &&
        filteredAgents.length === 0 &&
        filteredSquads.length === 0 &&
        filteredWorkflows.length === 0 && // ext-workflow
        filter && <PickerEmpty />}
```

3c. `packages/views/issues/actions/run-confirm-gate.ts`

```ts
      assigneeType: "agent" | "squad" | "workflow"; // ext-workflow (both union members, lines 16 and 23)
```
```ts
  if (
    (updates.assignee_type === "agent" ||
      updates.assignee_type === "squad" ||
      updates.assignee_type === "workflow") && // ext-workflow: a workflow owner starts a run like an agent/squad
    updates.assignee_id &&
    !parked
  ) {
```
```ts
    (owner === "agent" || owner === "squad" || owner === "workflow") && // ext-workflow
```
Update the doc comment on `assign` (`an agent/squad/workflow owner`).

3d. `packages/views/issues/components/batch-action-toolbar.tsx` line 128:

```ts
    if ((updates.assignee_type === "agent" || updates.assignee_type === "squad" || updates.assignee_type === "workflow") && updates.assignee_id) { // ext-workflow
```

3e. `packages/views/modals/run-confirm.tsx`

```ts
  const assigneeName =
    d.assigneeName ??
    getActorName(
      d.assigneeType === "squad" || d.assigneeType === "workflow" ? d.assigneeType : "agent", // ext-workflow
      d.assigneeId ?? "",
    );
```
and update the comment above it ("for a squad or workflow that is the squad or workflow itself").

3f. `packages/views/modals/create-issue.tsx` (`CreateRunHint`):

```ts
  const isAgentLike =
    assigneeType === "agent" || assigneeType === "squad" || assigneeType === "workflow"; // ext-workflow
```
after `const isSquad = ...`:

```ts
  const isWorkflow = assigneeType === "workflow"; // ext-workflow
```
and a branch after the `isSquad` branch:

```ts
  } else if (isWorkflow) {
    // ext-workflow: the supervisor and step agents work through the workflow,
    // so the workflow stays the subject.
    avatarType = "workflow";
    avatarId = assigneeId;
    text = t(($) => $.run_confirm.create_will_start_workflow, {
      name: getActorName("workflow", assigneeId ?? ""),
    });
  } else {
```
The existing `else` (single agent) stays as the last branch. The flip-to-agent-mode block at line 827 keeps `agent | squad` only: a workflow cannot be an agent-mode actor.

3g. `packages/views/inbox/components/inbox-detail-label.tsx`:

```ts
// ext-workflow: server-side only type. It is NOT added to InboxItemType
// (apps/mobile keeps a total Record over that union), so it is carried here.
type InboxLabelType = InboxItemType | "ext_workflow_escalation";

export function useTypeLabels(): Record<InboxLabelType, string> {
  ...
    children_done: t(($) => $.types.children_done),
    ext_workflow_escalation: t(($) => $.types.ext_workflow_escalation), // ext-workflow
  };
}
```
No other change: `InboxDetailLabel`'s `default:` branch already renders `typeLabels[item.type] ?? item.type`, which resolves the escalation label at runtime.

3h. Existing suites that mount `AssigneePicker` with a hand-written `api` mock now also issue the workflows list query. In `packages/views/issues/components/issue-detail.test.tsx`, add to the hoisted `mockApiObj` (line 308), next to `listTasksByIssue`:

```ts
  listExtWorkflows: vi.fn().mockResolvedValue([]), // ext-workflow: AssigneePicker lists workflows
```
Any other suite that fails after this task with `api.listExtWorkflows is not a function` gets the same one-line stub (found by Step 4b).

- [ ] **Step 4: Run, expect PASS**

```bash
pnpm --filter @multica/views exec vitest run common issues inbox modals
pnpm typecheck
pnpm --filter @multica/views lint
```

Step 4b: run the whole views suite once (`pnpm --filter @multica/views test`) and stub `listExtWorkflows` in any other hand-written `api` mock that fails.
Existing `assignee-picker.keyboard.test.tsx` must still pass: its `useQuery` mock returns `{ data: [] }` for the new `ext-workflows` key.

- [ ] **Step 5: Commit**

```bash
git add packages/views/common packages/views/issues packages/views/modals packages/views/inbox
git commit -m "feat(ext-workflow): add workflow actor avatar, assignee picker group and run-confirm handling

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D7: Issue run section, child-issue step line, mount in issue detail

**Files:**
- Create: `packages/views/ext-workflows/run-utils.ts`
- Create: `packages/views/issues/components/ext-workflow-run-section.tsx` (exports `ExtWorkflowRunSection` and `ExtWorkflowStepLine`)
- Modify: `packages/views/issues/components/issue-detail.tsx` (import near line 111; `<ExtWorkflowStepLine>` before the parent-issue block at ~line 2877; `<ExtWorkflowRunSection>` right before `<ExecutionLogSection>` at ~line 2943)
- Modify (test stub): `packages/views/issues/components/issue-detail.test.tsx` (`mockApiObj`, line 308)
- Test: `packages/views/ext-workflows/run-utils.test.ts`, `packages/views/issues/components/ext-workflow-run-section.test.tsx`

**Interfaces:**
- Consumes (core): `extWorkflowIssueRunsOptions(wsId, issueId)` (data `{ runs, step_of }`), `extWorkflowRunOptions(wsId, runId)` (data `ExtWorkflowRun` with `steps`, `events`), `useDecideExtWorkflowStep(wsId)` (`mutateAsync({ runId, stepId, action, to?, reason?, feedback?, expected_status })`), `useCancelExtWorkflowRun(wsId)` (`mutateAsync(runId)`), `issueDetailOptions` from `@multica/core/issues/queries`, `useActorName`, `paths.workflowDetail`, `paths.issueDetail`.
- Consumes (D3): `StepStatusIcon`, `RunStatusBadge`, `useStepStatusLabel`, `isActiveRunStatus`.
- Produces: `ExtWorkflowRunSection({ issueId })`, `ExtWorkflowStepLine({ issueId })`, `stepAncestors`, `rewindTargets`, `eventKey`, `eventDetail`, `errorStatus`.

Behaviour:
- The section renders only when the issue has at least one run (newest run shown), and carries: run status badge, rewinds used/max, a step list (status icon, title, agent, "Attempt n/max", link to the child issue), and a collapsible timeline.
- Decision buttons exist **only** for a step in `awaiting_human`: Approve, Redo (feedback dialog), Retry, Skip, Rewind to (target + feedback dialog), Abort (reason dialog). Every call sends `expected_status: step.status`. Redo and Retry are disabled when `attempts >= max_attempts`; Rewind is disabled when `rewinds_used >= max_rewinds`. Dialogs close only after the call succeeds; a 409 shows the "step changed" toast, a 403 the "can't decide" toast.
- "Cancel run" (confirm dialog) shows while the run is `running` or `waiting_human`.
- A child issue shows one line: "Workflow step k of n · MUL-123", linking to the parent.

- [ ] **Step 1: Write the failing tests**

`packages/views/ext-workflows/run-utils.test.ts`:

```ts
// @vitest-environment node
import { describe, expect, it } from "vitest";
import { errorStatus, eventDetail, eventKey, rewindTargets, stepAncestors } from "./run-utils";

const steps = [
  { node_key: "plan", title: "Plan", depends_on: [] as string[] },
  { node_key: "build", title: "Build", depends_on: ["plan"] },
  { node_key: "docs", title: "Docs", depends_on: ["plan"] },
  { node_key: "ship", title: "Ship", depends_on: ["build", "docs"] },
];

describe("stepAncestors", () => {
  it("returns every transitive upstream key in step order, excluding the step itself", () => {
    expect(stepAncestors(steps, "ship")).toEqual(["plan", "build", "docs"]);
    expect(stepAncestors(steps, "build")).toEqual(["plan"]);
    expect(stepAncestors(steps, "plan")).toEqual([]);
  });
  it("terminates on malformed cyclic input", () => {
    const cyclic = [
      { node_key: "a", title: "A", depends_on: ["b"] },
      { node_key: "b", title: "B", depends_on: ["a"] },
    ];
    expect(() => stepAncestors(cyclic, "a")).not.toThrow();
  });
});

describe("rewindTargets", () => {
  it("offers the upstream steps and the step itself, in step order", () => {
    expect(rewindTargets(steps, "build").map((t) => t.key)).toEqual(["plan", "build"]);
    expect(rewindTargets(steps, "plan").map((t) => t.key)).toEqual(["plan"]);
  });
  it("marks which target is the step itself", () => {
    expect(rewindTargets(steps, "ship").map((t) => t.isSelf)).toEqual([false, false, false, true]);
  });
});

describe("event helpers", () => {
  it("passes known kinds through and downgrades unknown ones", () => {
    expect(eventKey("escalated")).toBe("escalated");
    expect(eventKey("brand_new")).toBe("unknown");
  });
  it("shows the most useful payload text", () => {
    expect(eventDetail({ reason: "no budget", feedback: "x" })).toBe("no budget");
    expect(eventDetail({ feedback: "tighten it" })).toBe("tighten it");
    expect(eventDetail({ action: "approve" })).toBeNull();
    expect(eventDetail(null)).toBeNull();
  });
});

describe("errorStatus", () => {
  it("reads an HTTP status off an ApiError-like value", () => {
    expect(errorStatus({ status: 409 })).toBe(409);
    expect(errorStatus(new Error("x"))).toBeUndefined();
    expect(errorStatus(null)).toBeUndefined();
  });
});
```

`packages/views/issues/components/ext-workflow-run-section.test.tsx`:

```tsx
// @vitest-environment jsdom
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithI18n } from "../../test/i18n";
import { ExtWorkflowRunSection, ExtWorkflowStepLine } from "./ext-workflow-run-section";

const mocks = vi.hoisted(() => ({
  decide: vi.fn(),
  cancel: vi.fn(),
  toastError: vi.fn(),
  toastSuccess: vi.fn(),
  issueRuns: { runs: [] as unknown[], step_of: null as unknown },
  run: undefined as unknown,
}));

function step(over: Record<string, unknown>) {
  return {
    id: "step-x",
    node_key: "x",
    title: "X",
    agent_id: "ag-1",
    issue_id: "child-x",
    status: "pending",
    attempts: 0,
    max_attempts: 3,
    requires_review: false,
    depends_on: [],
    pending_reason: null,
    last_feedback: null,
    escalation_reason: null,
    started_at: null,
    finished_at: null,
    ...over,
  };
}

function makeRun(over: Record<string, unknown> = {}) {
  return {
    id: "run-1",
    workspace_id: "ws-1",
    workflow_id: "wf-1",
    workflow_name: "Release pipeline",
    issue_id: "issue-1",
    issue_identifier: "MUL-1",
    issue_title: "Ship it",
    triggered_by_type: "member",
    triggered_by_id: "user-1",
    status: "waiting_human",
    rewinds_used: 0,
    max_rewinds: 2,
    started_at: "2026-10-01T00:00:00Z",
    finished_at: null,
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-01T00:00:00Z",
    steps: [
      step({ id: "s1", node_key: "plan", title: "Plan", status: "done", attempts: 1, issue_id: "child-1" }),
      step({ id: "s2", node_key: "build", title: "Build", status: "awaiting_human", attempts: 2, depends_on: ["plan"], escalation_reason: "Tests keep failing", issue_id: "child-2" }),
      step({ id: "s3", node_key: "review", title: "Review", status: "awaiting_supervisor", attempts: 1, depends_on: ["build"], issue_id: "child-3" }),
      step({ id: "s4", node_key: "ship", title: "Ship", status: "pending", depends_on: ["review"], issue_id: "child-4" }),
    ],
    events: [
      { id: "e1", step_id: null, kind: "run_started", actor_type: "engine", actor_id: null, on_behalf_of: null, payload: {}, created_at: "2026-10-01T00:00:00Z" },
      { id: "e2", step_id: "s2", kind: "escalated", actor_type: "agent", actor_id: "ag-1", on_behalf_of: null, payload: { reason: "Tests keep failing" }, created_at: "2026-10-01T01:00:00Z" },
    ],
    ...over,
  };
}

vi.mock("@tanstack/react-query", () => ({
  useQuery: ({ queryKey }: { queryKey: string[] }) => {
    if (queryKey[0] === "issue-runs") return { data: mocks.issueRuns };
    if (queryKey[0] === "run") return { data: mocks.run };
    if (queryKey[0] === "issue") return { data: { id: "issue-9", identifier: "MUL-9", title: "Parent" } };
    return { data: undefined };
  },
}));
vi.mock("@multica/core/ext-workflows", () => ({
  extWorkflowIssueRunsOptions: () => ({ queryKey: ["issue-runs"] }),
  extWorkflowRunOptions: () => ({ queryKey: ["run"] }),
  useDecideExtWorkflowStep: () => ({ mutateAsync: mocks.decide, isPending: false }),
  useCancelExtWorkflowRun: () => ({ mutateAsync: mocks.cancel, isPending: false }),
}));
vi.mock("@multica/core/issues/queries", () => ({
  issueDetailOptions: () => ({ queryKey: ["issue"] }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    issueDetail: (id: string) => `/acme/issues/${id}`,
    workflowDetail: (id: string) => `/acme/workflows/${id}`,
  }),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (type: string, id: string) => `${type}:${id}` }),
}));
vi.mock("sonner", () => ({ toast: { error: mocks.toastError, success: mocks.toastSuccess } }));
vi.mock("../../navigation", () => ({
  AppLink: ({ children, href, ...props }: { children: ReactNode; href: string }) => (
    <a href={href} {...props}>
      {children}
    </a>
  ),
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));

beforeEach(() => {
  mocks.decide.mockReset().mockResolvedValue(undefined);
  mocks.cancel.mockReset().mockResolvedValue(undefined);
  mocks.toastError.mockReset();
  mocks.toastSuccess.mockReset();
  const run = makeRun();
  mocks.run = run;
  mocks.issueRuns = { runs: [run], step_of: null };
});

describe("ExtWorkflowRunSection", () => {
  it("renders nothing when the issue has no run", () => {
    mocks.issueRuns = { runs: [], step_of: null };
    const { container } = renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the run status, rewinds and every step with agent, attempts and child link", () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.getByText("Workflow run")).toBeInTheDocument();
    expect(screen.getAllByText("Needs you").length).toBeGreaterThan(0);
    expect(screen.getByText("Rewinds 0/2")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Release pipeline" })).toHaveAttribute("href", "/acme/workflows/wf-1");
    for (const title of ["Plan", "Build", "Review", "Ship"]) expect(screen.getByText(title)).toBeInTheDocument();
    expect(screen.getByText("Attempt 2/3")).toBeInTheDocument();
    const links = screen.getAllByRole("link", { name: "Open step issue" });
    expect(links.map((l) => l.getAttribute("href"))).toEqual([
      "/acme/issues/child-1",
      "/acme/issues/child-2",
      "/acme/issues/child-3",
      "/acme/issues/child-4",
    ]);
    expect(screen.getByText("Tests keep failing")).toBeInTheDocument();
  });

  it("offers decisions only for the step awaiting a human", () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    for (const name of ["Approve", "Redo…", "Retry", "Skip", "Rewind to…", "Abort…"]) {
      expect(screen.getAllByRole("button", { name })).toHaveLength(1);
    }
    // The supervisor-review, done and pending steps get no buttons.
    expect(screen.getByText("Review").closest("li")!.querySelectorAll("button")).toHaveLength(0);
    expect(screen.getByText("Plan").closest("li")!.querySelectorAll("button")).toHaveLength(0);
  });

  it("offers no decisions when no step awaits a human", () => {
    const run = makeRun({
      status: "running",
      steps: [step({ id: "s1", node_key: "plan", title: "Plan", status: "awaiting_supervisor" })],
    });
    mocks.run = run;
    mocks.issueRuns = { runs: [run], step_of: null };
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
  });

  it("approves with the step's current status as expected_status", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(mocks.decide).toHaveBeenCalledWith({
      runId: "run-1",
      stepId: "s2",
      action: "approve",
      expected_status: "awaiting_human",
    });
    expect(mocks.toastSuccess).toHaveBeenCalledWith("Decision sent");
  });

  it("retries and skips directly", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(mocks.decide).toHaveBeenLastCalledWith(
      expect.objectContaining({ action: "retry", stepId: "s2", expected_status: "awaiting_human" }),
    );
    await userEvent.click(screen.getByRole("button", { name: "Skip" }));
    expect(mocks.decide).toHaveBeenLastCalledWith(
      expect.objectContaining({ action: "skip", stepId: "s2", expected_status: "awaiting_human" }),
    );
  });

  it("redo asks for feedback first", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Redo…" }));
    const dialog = await screen.findByRole("dialog");
    const confirm = within(dialog).getByRole("button", { name: "Redo" });
    expect(confirm).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText("Feedback for the agent"), "Cover the edge cases");
    await userEvent.click(confirm);
    await waitFor(() =>
      expect(mocks.decide).toHaveBeenCalledWith({
        runId: "run-1",
        stepId: "s2",
        action: "redo",
        feedback: "Cover the edge cases",
        expected_status: "awaiting_human",
      }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("rewind offers the upstream steps and the step itself", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Rewind to…" }));
    const dialog = await screen.findByRole("dialog");
    const select = within(dialog).getByLabelText("Return to") as HTMLSelectElement;
    expect([...select.options].map((o) => o.value)).toEqual(["plan", "build"]);
    expect(within(dialog).getByRole("option", { name: "Build (this step)" })).toBeInTheDocument();
    await userEvent.selectOptions(select, "plan");
    await userEvent.type(within(dialog).getByLabelText("Feedback for the agent"), "Redo the plan");
    await userEvent.click(within(dialog).getByRole("button", { name: "Rewind" }));
    await waitFor(() =>
      expect(mocks.decide).toHaveBeenCalledWith({
        runId: "run-1",
        stepId: "s2",
        action: "rewind",
        to: "plan",
        feedback: "Redo the plan",
        expected_status: "awaiting_human",
      }),
    );
  });

  it("abort requires a reason", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Abort…" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Reason"), "Out of scope");
    await userEvent.click(within(dialog).getByRole("button", { name: "Abort run" }));
    await waitFor(() =>
      expect(mocks.decide).toHaveBeenCalledWith({
        runId: "run-1",
        stepId: "s2",
        action: "abort",
        reason: "Out of scope",
        expected_status: "awaiting_human",
      }),
    );
  });

  it("disables redo and retry at the attempt limit and rewind at the rewind limit", () => {
    const run = makeRun({
      rewinds_used: 2,
      steps: [
        step({ id: "s2", node_key: "build", title: "Build", status: "awaiting_human", attempts: 3, max_attempts: 3 }),
      ],
    });
    mocks.run = run;
    mocks.issueRuns = { runs: [run], step_of: null };
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.getByRole("button", { name: "Redo…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Retry" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Rewind to…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
    expect(screen.getByText("Attempt limit reached")).toBeInTheDocument();
    expect(screen.getByText("Rewind limit reached")).toBeInTheDocument();
  });

  it("explains a 409 status mismatch and a 403", async () => {
    mocks.decide.mockRejectedValueOnce(Object.assign(new Error("conflict"), { status: 409 }));
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(mocks.toastError).toHaveBeenCalledWith("This step changed. Review its new state and try again."),
    );
    mocks.decide.mockRejectedValueOnce(Object.assign(new Error("forbidden"), { status: 403 }));
    await userEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(mocks.toastError).toHaveBeenCalledWith("You can't decide on this run"));
  });

  it("keeps a dialog open when the decision fails", async () => {
    mocks.decide.mockRejectedValueOnce(Object.assign(new Error("conflict"), { status: 409 }));
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Abort…" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Reason"), "x");
    await userEvent.click(within(dialog).getByRole("button", { name: "Abort run" }));
    await waitFor(() => expect(mocks.toastError).toHaveBeenCalled());
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("cancels an active run after confirmation, and hides Cancel once it finished", async () => {
    const { unmount } = renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Cancel run" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(mocks.cancel).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel run" }));
    await waitFor(() => expect(mocks.cancel).toHaveBeenCalledWith("run-1"));
    unmount();

    const done = makeRun({ status: "done", finished_at: "2026-10-02T00:00:00Z", steps: [] });
    mocks.run = done;
    mocks.issueRuns = { runs: [done], step_of: null };
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.queryByRole("button", { name: "Cancel run" })).not.toBeInTheDocument();
  });

  it("shows the timeline only when expanded", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.queryByText("Run started")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Timeline" }));
    expect(screen.getByText("Run started")).toBeInTheDocument();
    expect(screen.getByText("Escalated to a human")).toBeInTheDocument();
    expect(screen.getAllByText("Tests keep failing").length).toBeGreaterThan(1);
  });
});

describe("ExtWorkflowStepLine", () => {
  it("renders nothing for an issue that is not a workflow step", () => {
    const { container } = renderWithI18n(<ExtWorkflowStepLine issueId="child-1" />);
    expect(container).toBeEmptyDOMElement();
  });

  it("shows 'Workflow step k of n · parent' linking to the parent issue", () => {
    mocks.issueRuns = {
      runs: [],
      step_of: { run_id: "run-1", step_id: "s2", node_key: "build", index: 2, total: 4, parent_issue_id: "issue-9" },
    };
    renderWithI18n(<ExtWorkflowStepLine issueId="child-2" />);
    const link = screen.getByRole("link", { name: /Workflow step 2 of 4/ });
    expect(link).toHaveAttribute("href", "/acme/issues/issue-9");
    expect(link).toHaveTextContent("Workflow step 2 of 4 · MUL-9");
  });
});
```

- [ ] **Step 2: Run, expect FAIL**

```bash
pnpm --filter @multica/views exec vitest run ext-workflows/run-utils.test.ts issues/components/ext-workflow-run-section.test.tsx
```
Expected: FAIL (modules missing).

- [ ] **Step 3: Implement**

`packages/views/ext-workflows/run-utils.ts`:

```ts
// Pure helpers for the issue run section (no React, no core imports).

interface StepLike {
  node_key: string;
  title: string;
  depends_on: string[];
}

/** Transitive upstream node keys of `nodeKey`, in step order, excluding itself. */
export function stepAncestors(steps: readonly StepLike[], nodeKey: string): string[] {
  const byKey = new Map(steps.map((s) => [s.node_key, s]));
  const seen = new Set<string>();
  const stack = [...(byKey.get(nodeKey)?.depends_on ?? [])];
  while (stack.length > 0) {
    const key = stack.pop()!;
    if (seen.has(key) || key === nodeKey) continue;
    seen.add(key);
    stack.push(...(byKey.get(key)?.depends_on ?? []));
  }
  return steps.map((s) => s.node_key).filter((k) => seen.has(k));
}

export interface RewindTarget {
  key: string;
  title: string;
  isSelf: boolean;
}

/** A rewind may return to any upstream step, or to the step itself. */
export function rewindTargets(steps: readonly StepLike[], nodeKey: string): RewindTarget[] {
  const allowed = new Set([...stepAncestors(steps, nodeKey), nodeKey]);
  return steps
    .filter((s) => allowed.has(s.node_key))
    .map((s) => ({ key: s.node_key, title: s.title, isSelf: s.node_key === nodeKey }));
}

const EVENT_KINDS = [
  "run_started",
  "step_started",
  "step_finished",
  "step_failed",
  "decision",
  "rewind",
  "rewind_requested",
  "escalated",
  "run_finished",
  "run_cancelled",
  "protocol_error",
] as const;
export type EventKey = (typeof EVENT_KINDS)[number] | "unknown";

export function eventKey(kind: string): EventKey {
  return (EVENT_KINDS as readonly string[]).includes(kind) ? (kind as EventKey) : "unknown";
}

/** The most informative free text in an event payload, if any. */
export function eventDetail(payload: Record<string, unknown> | null | undefined): string | null {
  for (const field of ["reason", "feedback"]) {
    const value = payload?.[field];
    if (typeof value === "string" && value.trim() !== "") return value;
  }
  return null;
}

/** HTTP status of an ApiError-like value, without importing the class (it may be mocked). */
export function errorStatus(err: unknown): number | undefined {
  if (err && typeof err === "object" && "status" in err) {
    const status = (err as { status?: unknown }).status;
    return typeof status === "number" ? status : undefined;
  }
  return undefined;
}
```

`packages/views/issues/components/ext-workflow-run-section.tsx`:

```tsx
"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, ExternalLink } from "lucide-react";
import { toast } from "sonner";
import {
  extWorkflowIssueRunsOptions,
  extWorkflowRunOptions,
  useCancelExtWorkflowRun,
  useDecideExtWorkflowStep,
  type ExtWorkflowDecisionAction,
  type ExtWorkflowRun,
  type ExtWorkflowRunSummary,
  type ExtWorkflowStep,
} from "@multica/core/ext-workflows";
import { useWorkspaceId } from "@multica/core/hooks";
import { issueDetailOptions } from "@multica/core/issues/queries";
import { useWorkspacePaths } from "@multica/core/paths";
import { useActorName } from "@multica/core/workspace/hooks";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Label } from "@multica/ui/components/ui/label";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { cn } from "@multica/ui/lib/utils";
import { ActorAvatar } from "../../common/actor-avatar";
import { RunStatusBadge, StepStatusIcon, useStepStatusLabel } from "../../ext-workflows/components/status";
import { errorStatus, eventDetail, eventKey, rewindTargets } from "../../ext-workflows/run-utils";
import { isActiveRunStatus } from "../../ext-workflows/status-keys";
import { useT, useTimeAgo } from "../../i18n";
import { AppLink } from "../../navigation";

// ---------------------------------------------------------------- run section

/** Parent-issue sidebar section: the newest workflow run on this issue. */
export function ExtWorkflowRunSection({ issueId }: { issueId: string }) {
  const wsId = useWorkspaceId();
  const { data } = useQuery({
    ...extWorkflowIssueRunsOptions(wsId, issueId),
    enabled: !!wsId && !!issueId,
  });
  const latest = data?.runs?.[0];
  if (!latest) return null;
  return <RunPanel wsId={wsId} summary={latest} />;
}

function RunPanel({ wsId, summary }: { wsId: string; summary: ExtWorkflowRunSummary }) {
  const { t } = useT("ext-workflows");
  const p = useWorkspacePaths();
  const [open, setOpen] = useState(true);
  const [timelineOpen, setTimelineOpen] = useState(false);
  const [cancelOpen, setCancelOpen] = useState(false);
  const { data: detail } = useQuery({ ...extWorkflowRunOptions(wsId, summary.id), enabled: !!wsId });
  const decide = useDecideExtWorkflowStep(wsId);
  const cancel = useCancelExtWorkflowRun(wsId);

  const run: ExtWorkflowRunSummary | ExtWorkflowRun = detail ?? summary;
  const steps = detail?.steps ?? [];
  const events = detail?.events ?? [];

  // Returns true when the decision was accepted, so dialogs close only on success.
  const send = async (
    step: ExtWorkflowStep,
    action: ExtWorkflowDecisionAction,
    extra: { to?: string; reason?: string; feedback?: string } = {},
  ): Promise<boolean> => {
    try {
      await decide.mutateAsync({
        runId: summary.id,
        stepId: step.id,
        action,
        ...extra,
        expected_status: step.status,
      });
      toast.success(t(($) => $.run_section.toast.decided));
      return true;
    } catch (err) {
      const status = errorStatus(err);
      if (status === 409) toast.error(t(($) => $.run_section.toast.mismatch));
      else if (status === 403) toast.error(t(($) => $.run_section.toast.forbidden));
      else toast.error(err instanceof Error && err.message ? err.message : t(($) => $.run_section.toast.failed));
      return false;
    }
  };

  const cancelRun = async () => {
    try {
      await cancel.mutateAsync(summary.id);
      toast.success(t(($) => $.run_section.toast.cancelled));
      setCancelOpen(false);
    } catch (err) {
      toast.error(err instanceof Error && err.message ? err.message : t(($) => $.run_section.toast.cancel_failed));
    }
  };

  return (
    <div>
      <button
        type="button"
        className={cn(
          "mb-2 flex w-full items-center gap-1 rounded-md px-2 py-1 text-caption font-medium transition-colors hover:bg-accent/70",
          !open && "text-muted-foreground hover:text-foreground",
        )}
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        {t(($) => $.run_section.title)}
        <ChevronRight
          className={cn("!size-3 shrink-0 stroke-[2.5] text-muted-foreground transition-transform", open && "rotate-90")}
        />
        <span className="ml-auto">
          <RunStatusBadge status={run.status} />
        </span>
      </button>

      {open && (
        <div className="space-y-2 pl-2">
          <div className="flex items-center gap-2 px-2 text-micro text-muted-foreground">
            <AppLink href={p.workflowDetail(summary.workflow_id)} className="min-w-0 truncate hover:text-foreground">
              {summary.workflow_name}
            </AppLink>
            <span className="shrink-0 tabular-nums">
              {t(($) => $.run_section.rewinds, { used: run.rewinds_used, max: run.max_rewinds })}
            </span>
          </div>

          <ul className="space-y-1">
            {steps.map((step) => (
              <StepRow
                key={step.id}
                step={step}
                run={run}
                steps={steps}
                busy={decide.isPending}
                onSend={send}
              />
            ))}
          </ul>

          <div className="px-2">
            <button
              type="button"
              className="flex items-center gap-1 text-micro font-medium text-muted-foreground hover:text-foreground"
              aria-expanded={timelineOpen}
              onClick={() => setTimelineOpen(!timelineOpen)}
            >
              <ChevronRight className={cn("!size-3 transition-transform", timelineOpen && "rotate-90")} />
              {t(($) => $.run_section.timeline)}
            </button>
            {timelineOpen && <Timeline events={events} />}
          </div>

          {isActiveRunStatus(run.status) && (
            <div className="px-2">
              <Button
                type="button"
                variant="ghost"
                size="xs"
                className="text-destructive hover:text-destructive"
                onClick={() => setCancelOpen(true)}
              >
                {t(($) => $.run_section.cancel_run)}
              </Button>
            </div>
          )}
        </div>
      )}

      {cancelOpen && (
        <AlertDialog open onOpenChange={(v) => { if (!v && !cancel.isPending) setCancelOpen(false); }}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t(($) => $.run_section.cancel_dialog.title)}</AlertDialogTitle>
              <AlertDialogDescription>{t(($) => $.run_section.cancel_dialog.description)}</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel disabled={cancel.isPending}>
                {t(($) => $.run_section.cancel_dialog.keep)}
              </AlertDialogCancel>
              <AlertDialogAction
                onClick={() => void cancelRun()}
                disabled={cancel.isPending}
                className="bg-destructive text-white hover:bg-destructive/90"
              >
                {t(($) => $.run_section.cancel_dialog.confirm)}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- steps

type DialogKind = "redo" | "rewind" | "abort";

function StepRow({
  step,
  run,
  steps,
  busy,
  onSend,
}: {
  step: ExtWorkflowStep;
  run: Pick<ExtWorkflowRunSummary, "rewinds_used" | "max_rewinds">;
  steps: ExtWorkflowStep[];
  busy: boolean;
  onSend: (
    step: ExtWorkflowStep,
    action: ExtWorkflowDecisionAction,
    extra?: { to?: string; reason?: string; feedback?: string },
  ) => Promise<boolean>;
}) {
  const { t } = useT("ext-workflows");
  const p = useWorkspacePaths();
  const { getActorName } = useActorName();
  const statusLabel = useStepStatusLabel();
  const [dialog, setDialog] = useState<DialogKind | null>(null);

  const awaitingHuman = step.status === "awaiting_human";
  const canRetry = step.attempts < step.max_attempts;
  const canRewind = run.rewinds_used < run.max_rewinds;

  return (
    <li className="rounded-md px-2 py-1.5 hover:bg-accent/40">
      <div className="flex items-center gap-2">
        <StepStatusIcon status={step.status} />
        <span className="min-w-0 flex-1 truncate text-caption font-medium">{step.title}</span>
        <span className="shrink-0 text-micro tabular-nums text-muted-foreground">
          {t(($) => $.run_section.attempts, { attempts: step.attempts, max: step.max_attempts })}
        </span>
        <AppLink
          href={p.issueDetail(step.issue_id)}
          aria-label={t(($) => $.run_section.open_child)}
          className="shrink-0 rounded-xs p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground"
        >
          <ExternalLink className="size-3" />
        </AppLink>
      </div>
      <div className="mt-0.5 flex min-w-0 items-center gap-1.5 pl-5 text-micro text-muted-foreground">
        <ActorAvatar actorType="agent" actorId={step.agent_id} size="xs" />
        <span className="truncate">{getActorName("agent", step.agent_id)}</span>
        <span aria-hidden="true">·</span>
        <span className="shrink-0">{statusLabel(step.status)}</span>
      </div>

      {awaitingHuman && (
        <div className="mt-2 space-y-2 rounded-md border border-warning/40 bg-warning/5 p-2">
          <p className="text-caption font-medium">{t(($) => $.run_section.needs_decision)}</p>
          {step.escalation_reason && (
            <p className="whitespace-pre-wrap text-caption text-muted-foreground">{step.escalation_reason}</p>
          )}
          <div className="flex flex-wrap gap-1.5">
            <Button type="button" size="xs" disabled={busy} onClick={() => void onSend(step, "approve")}>
              {t(($) => $.run_section.action_approve)}
            </Button>
            <Button
              type="button"
              size="xs"
              variant="outline"
              disabled={busy || !canRetry}
              onClick={() => setDialog("redo")}
            >
              {t(($) => $.run_section.action_redo)}
            </Button>
            <Button
              type="button"
              size="xs"
              variant="outline"
              disabled={busy || !canRetry}
              onClick={() => void onSend(step, "retry")}
            >
              {t(($) => $.run_section.action_retry)}
            </Button>
            <Button type="button" size="xs" variant="outline" disabled={busy} onClick={() => void onSend(step, "skip")}>
              {t(($) => $.run_section.action_skip)}
            </Button>
            <Button
              type="button"
              size="xs"
              variant="outline"
              disabled={busy || !canRewind}
              onClick={() => setDialog("rewind")}
            >
              {t(($) => $.run_section.action_rewind)}
            </Button>
            <Button
              type="button"
              size="xs"
              variant="destructive"
              disabled={busy}
              onClick={() => setDialog("abort")}
            >
              {t(($) => $.run_section.action_abort)}
            </Button>
          </div>
          {!canRetry && <p className="text-micro text-muted-foreground">{t(($) => $.run_section.attempt_limit)}</p>}
          {!canRewind && <p className="text-micro text-muted-foreground">{t(($) => $.run_section.rewind_limit)}</p>}
        </div>
      )}

      {dialog === "redo" && (
        <DecisionDialog
          title={t(($) => $.run_section.redo_dialog.title)}
          fieldLabel={t(($) => $.run_section.redo_dialog.feedback_label)}
          placeholder={t(($) => $.run_section.redo_dialog.feedback_placeholder)}
          confirmLabel={t(($) => $.run_section.redo_dialog.confirm)}
          cancelLabel={t(($) => $.run_section.redo_dialog.cancel)}
          pending={busy}
          onClose={() => setDialog(null)}
          onConfirm={(text) => onSend(step, "redo", { feedback: text })}
        />
      )}
      {dialog === "rewind" && (
        <DecisionDialog
          title={t(($) => $.run_section.rewind_dialog.title)}
          fieldLabel={t(($) => $.run_section.rewind_dialog.feedback_label)}
          placeholder={t(($) => $.run_section.rewind_dialog.feedback_placeholder)}
          confirmLabel={t(($) => $.run_section.rewind_dialog.confirm)}
          cancelLabel={t(($) => $.run_section.rewind_dialog.cancel)}
          targetLabel={t(($) => $.run_section.rewind_dialog.target_label)}
          targets={rewindTargets(steps, step.node_key).map((target) => ({
            value: target.key,
            label: target.isSelf
              ? t(($) => $.run_section.rewind_dialog.target_self, { title: target.title })
              : target.title,
          }))}
          defaultTarget={step.node_key}
          pending={busy}
          onClose={() => setDialog(null)}
          onConfirm={(text, to) => onSend(step, "rewind", { to, feedback: text })}
        />
      )}
      {dialog === "abort" && (
        <DecisionDialog
          title={t(($) => $.run_section.abort_dialog.title)}
          fieldLabel={t(($) => $.run_section.abort_dialog.reason_label)}
          placeholder={t(($) => $.run_section.abort_dialog.reason_placeholder)}
          confirmLabel={t(($) => $.run_section.abort_dialog.confirm)}
          cancelLabel={t(($) => $.run_section.abort_dialog.cancel)}
          destructive
          pending={busy}
          onClose={() => setDialog(null)}
          onConfirm={(text) => onSend(step, "abort", { reason: text })}
        />
      )}
    </li>
  );
}

function DecisionDialog({
  title,
  fieldLabel,
  placeholder,
  confirmLabel,
  cancelLabel,
  targetLabel,
  targets,
  defaultTarget,
  destructive = false,
  pending,
  onClose,
  onConfirm,
}: {
  title: string;
  fieldLabel: string;
  placeholder: string;
  confirmLabel: string;
  cancelLabel: string;
  targetLabel?: string;
  targets?: { value: string; label: string }[];
  defaultTarget?: string;
  destructive?: boolean;
  pending: boolean;
  onClose: () => void;
  /** Resolves true when the decision was accepted; the dialog then closes. */
  onConfirm: (text: string, target: string) => Promise<boolean>;
}) {
  const [text, setText] = useState("");
  const [target, setTarget] = useState(defaultTarget ?? targets?.[0]?.value ?? "");
  const canConfirm = text.trim() !== "" && !pending;

  const confirm = async () => {
    if (!canConfirm) return;
    if (await onConfirm(text.trim(), target)) onClose();
  };

  return (
    <Dialog open onOpenChange={(v) => { if (!v) onClose(); }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        <div className="space-y-3">
          {targets && targetLabel && (
            <div className="space-y-1.5">
              <Label htmlFor="ext-workflow-rewind-target" className="text-caption text-muted-foreground">
                {targetLabel}
              </Label>
              <select
                id="ext-workflow-rewind-target"
                value={target}
                onChange={(e) => setTarget(e.target.value)}
                className="h-8 w-full rounded-lg border border-input bg-transparent px-2.5 text-body outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
              >
                {targets.map((option) => (
                  <option key={option.value} value={option.value}>
                    {option.label}
                  </option>
                ))}
              </select>
            </div>
          )}
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-decision-text" className="text-caption text-muted-foreground">
              {fieldLabel}
            </Label>
            <Textarea
              id="ext-workflow-decision-text"
              autoFocus
              rows={4}
              value={text}
              placeholder={placeholder}
              onChange={(e) => setText(e.target.value)}
            />
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" size="sm" onClick={onClose}>
            {cancelLabel}
          </Button>
          <Button
            type="button"
            size="sm"
            variant={destructive ? "destructive" : "default"}
            disabled={!canConfirm}
            aria-busy={pending}
            onClick={() => void confirm()}
          >
            {confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------- timeline

function Timeline({ events }: { events: ExtWorkflowRun["events"] }) {
  const { t } = useT("ext-workflows");
  const timeAgo = useTimeAgo();
  const { getActorName } = useActorName();

  if (events.length === 0) {
    return <p className="mt-1 text-micro text-muted-foreground">{t(($) => $.run_section.timeline_empty)}</p>;
  }
  return (
    <ol className="mt-1 max-h-56 space-y-1.5 overflow-y-auto border-l pl-3">
      {events.map((event) => {
        const kind = eventKey(event.kind);
        const detail = eventDetail(event.payload);
        const actor =
          event.actor_type === "agent" || event.actor_type === "member"
            ? event.actor_id
              ? getActorName(event.actor_type, event.actor_id)
              : null
            : t(($) => $.run_section.actor_engine);
        return (
          <li key={event.id} className="text-micro">
            <div className="flex items-baseline gap-1.5">
              <span className="font-medium">{t(($) => $.events[kind])}</span>
              {actor && <span className="truncate text-muted-foreground">{actor}</span>}
              <span className="ml-auto shrink-0 text-muted-foreground">{timeAgo(event.created_at)}</span>
            </div>
            {detail && <p className="whitespace-pre-wrap text-muted-foreground">{detail}</p>}
          </li>
        );
      })}
    </ol>
  );
}

// ---------------------------------------------------------------- child-issue line

/** One line on a child issue: "Workflow step k of n · MUL-123", linking to the parent. */
export function ExtWorkflowStepLine({ issueId }: { issueId: string }) {
  const { t } = useT("ext-workflows");
  const wsId = useWorkspaceId();
  const p = useWorkspacePaths();
  const { data } = useQuery({
    ...extWorkflowIssueRunsOptions(wsId, issueId),
    enabled: !!wsId && !!issueId,
  });
  const stepOf = data?.step_of ?? null;
  const { data: parent } = useQuery({
    ...issueDetailOptions(wsId, stepOf?.parent_issue_id ?? ""),
    enabled: !!stepOf,
  });
  if (!stepOf) return null;

  const params = { index: stepOf.index, total: stepOf.total };
  return (
    <AppLink
      href={p.issueDetail(stepOf.parent_issue_id)}
      className="flex items-center gap-1.5 rounded-md px-2 py-1 text-caption text-muted-foreground hover:bg-accent/70 hover:text-foreground"
    >
      {parent?.identifier
        ? t(($) => $.run_section.step_line_with_parent, { ...params, parent: parent.identifier })
        : t(($) => $.run_section.step_line, params)}
    </AppLink>
  );
}
```

Mount in `packages/views/issues/components/issue-detail.tsx`:

```tsx
import { ExtWorkflowRunSection, ExtWorkflowStepLine } from "./ext-workflow-run-section"; // ext-workflow
```
(after `import { WakeupsSection } from "./wakeups-section";`, line 112). Immediately before the `{/* Parent issue — standalone section ... */}` comment (line ~2877):

```tsx
      {/* ext-workflow: child issues of a workflow run show which step they are. */}
      <ExtWorkflowStepLine issueId={id} />

```
and immediately before the `{/* Execution log — active runs ... */}` comment (line ~2938):

```tsx
      {/* ext-workflow: the run panel of a workflow-assigned issue; renders nothing otherwise. */}
      <ExtWorkflowRunSection issueId={id} />

```

`packages/views/issues/components/issue-detail.test.tsx`, `mockApiObj` (line 308), next to the `listExtWorkflows` stub added in D6:

```ts
  getIssueExtWorkflowRuns: vi.fn().mockResolvedValue({ runs: [], step_of: null }), // ext-workflow: run section query
```

- [ ] **Step 4: Run, expect PASS**

```bash
pnpm --filter @multica/views exec vitest run ext-workflows issues/components/ext-workflow-run-section.test.tsx issues/components/issue-detail.test.tsx
pnpm --filter @multica/views test
pnpm typecheck
pnpm lint
```

- [ ] **Step 5: Commit**

```bash
git add packages/views/ext-workflows packages/views/issues/components
git commit -m "feat(ext-workflow): add issue run section, step line and human decision controls

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task D8: Playwright e2e: create a workflow, edit nodes, assign an issue, see the run panel

**Files:**
- Create: `e2e/ext-workflow.spec.ts`

**Interfaces:**
- Consumes: `createTestApi()` from `e2e/helpers.ts` (`api.getWorkspaces()`, `api.getEmail()`, `api.getToken()`, `api.createIssue()`, `api.cleanup()`); direct `pg` access like `e2e/issue-wakeups.spec.ts` (agents, runtime and ext tables have no API fixture). Needs the full stack from Parts A to C and D1 to D7, with `MULTICA_WORKFLOW_ENGINE` not `false`, and `workflows` in the reserved slugs.
- No real agents run: step tasks are enqueued, but the runtime is a fixture row that no daemon claims. The test asserts on the run panel, the child issues and the DB rows only.

- [ ] **Step 1: Write the failing test**

Create `e2e/ext-workflow.spec.ts`:

```ts
import "./env";

import { expect, test } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";

// Engine semantics (transitions, rewinds, decisions) are covered by the Go
// tests. This browser test covers the path a member takes: build a workflow in
// the editor, assign an issue to it, and see the run panel and the child issues.
test("a member builds a workflow, assigns an issue, and sees the run and its child issues", async ({ page }) => {
  test.setTimeout(180_000);
  const api = await createTestApi();
  const db = new pg.Client(process.env.DATABASE_URL);
  await db.connect();
  let runtimeId: string | undefined;
  const agentIds: string[] = [];
  let workspaceId: string | undefined;
  let parentId: string | undefined;
  try {
    const workspace = (await api.getWorkspaces())[0]!;
    workspaceId = workspace.id;
    const user = await db.query<{ id: string }>(`SELECT id FROM "user" WHERE email = $1`, [api.getEmail()]);
    const userId = user.rows[0]!.id;
    const runtime = await db.query<{ id: string }>(
      `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, last_seen_at)
       VALUES ($1, 'Workflow verification', 'cloud', 'e2e_ext_workflow', 'online', 'E2E fixture', '{}'::jsonb, $2, now()) RETURNING id`,
      [workspace.id, userId],
    );
    runtimeId = runtime.rows[0]!.id;
    for (const name of ["Flow Planner", "Flow Builder"]) {
      const agent = await db.query<{ id: string }>(
        `INSERT INTO agent (workspace_id, name, description, instructions, runtime_mode, runtime_config, runtime_id, visibility, permission_mode, max_concurrent_tasks, owner_id)
         VALUES ($1, $2, '', '', 'cloud', '{}'::jsonb, $3, 'private', 'private', 1, $4) RETURNING id`,
        [workspace.id, name, runtimeId, userId],
      );
      agentIds.push(agent.rows[0]!.id);
    }

    await page.addInitScript((token) => {
      localStorage.setItem("multica_token", token!);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, api.getToken());
    await page.setViewportSize({ width: 1440, height: 1000 });

    // 1. Sidebar entry and empty list.
    await page.goto(`/${workspace.slug}/issues`, { waitUntil: "domcontentloaded" });
    await page.getByRole("link", { name: "Workflows" }).click();
    await expect(page).toHaveURL(new RegExp(`/${workspace.slug}/workflows$`));
    await expect(page.getByText("No workflows yet.")).toBeVisible({ timeout: 30_000 });

    // 2. Create dialog: name + supervisor, then land on the detail page.
    await page.getByRole("button", { name: "New workflow" }).first().click();
    const createDialog = page.getByRole("dialog");
    await createDialog.getByLabel("Name").fill("E2E Flow");
    await createDialog.getByRole("button", { name: "Supervisor agent" }).click();
    await page.getByRole("button", { name: /Flow Planner/ }).click();
    await createDialog.getByRole("button", { name: "Create" }).click();
    await expect(page).toHaveURL(new RegExp(`/${workspace.slug}/workflows/[0-9a-f-]{36}$`), { timeout: 30_000 });
    await expect(page.getByRole("heading", { name: "E2E Flow" })).toBeVisible();

    // 3. Node editor: two nodes, the second depends on the first.
    await page.getByRole("button", { name: "Add node" }).click();
    await page.getByRole("button", { name: "Add node" }).click();
    const node1 = page.getByRole("listitem", { name: "Node 1" });
    const node2 = page.getByRole("listitem", { name: "Node 2" });
    await node1.getByLabel("Title").fill("Plan the work");
    await node2.getByLabel("Title").fill("Build it");
    await expect(node2.getByLabel("Key")).toHaveValue("build_it");
    await node2.getByRole("button", { name: "Agent" }).click();
    await page.getByRole("button", { name: /Flow Builder/ }).click();
    await node2.getByRole("button", { name: "Depends on" }).click();
    await page.getByRole("button", { name: "Plan the work" }).click();
    await page.keyboard.press("Escape");
    await expect(page.locator("[data-dag-edge]")).toHaveCount(1);

    // 4. Save, then reload: the definition persisted.
    await expect(page.getByText("Unsaved changes")).toBeVisible();
    await page.getByRole("button", { name: "Save" }).click();
    await expect(page.getByText("Unsaved changes")).toBeHidden({ timeout: 30_000 });
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByRole("listitem", { name: "Node 2" }).getByLabel("Title")).toHaveValue("Build it", {
      timeout: 30_000,
    });
    await expect(page.locator("[data-dag-node]")).toHaveCount(2);

    // 5. A local validation error is shown on the row and blocks the save.
    await page.getByRole("listitem", { name: "Node 2" }).getByLabel("Title").fill("");
    await page.getByRole("button", { name: "Save" }).click();
    await expect(page.getByRole("listitem", { name: "Node 2" }).getByText("Title is required")).toBeVisible();
    await page.getByRole("button", { name: "Discard changes" }).click();
    await expect(page.getByRole("listitem", { name: "Node 2" }).getByLabel("Title")).toHaveValue("Build it");

    // 6. Assign an issue to the workflow from the issue's assignee picker.
    const parent = await api.createIssue("E2E workflow parent", { status: "todo" });
    parentId = parent.id;
    await page.goto(`/${workspace.slug}/issues/${parent.id}`, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Unassigned" }).first().click({ timeout: 45_000 });
    await page.getByRole("button", { name: /E2E Flow/ }).click();
    await page.getByRole("button", { name: "Confirm assignment" }).click();

    // 7. The run panel lists both steps, each linking to its child issue.
    await expect(page.getByText("Workflow run")).toBeVisible({ timeout: 45_000 });
    await expect(page.getByText("Plan the work")).toBeVisible();
    await expect(page.getByText("Build it")).toBeVisible();
    await expect(page.getByRole("link", { name: "Open step issue" })).toHaveCount(2);

    const children = await db.query<{ id: string; title: string }>(
      `SELECT id, title FROM issue WHERE parent_issue_id = $1 ORDER BY created_at`,
      [parent.id],
    );
    expect(children.rows.map((r) => r.title)).toEqual([
      "E2E workflow parent · Plan the work",
      "E2E workflow parent · Build it",
    ]);
    const run = await db.query<{ status: string }>(
      `SELECT status FROM ext_workflow_run WHERE issue_id = $1`,
      [parent.id],
    );
    expect(run.rows).toEqual([{ status: "running" }]);

    // 8. A child issue says which step it is and links back to the parent.
    await page.getByRole("link", { name: "Open step issue" }).first().click();
    await expect(page.getByRole("link", { name: /Workflow step 1 of 2/ })).toBeVisible({ timeout: 45_000 });

    // 9. The workflow's Runs tab lists the run.
    await page.goto(`/${workspace.slug}/workflows`, { waitUntil: "domcontentloaded" });
    await page.getByText("E2E Flow").click();
    await page.getByRole("button", { name: "Runs" }).click();
    await expect(page.getByText("E2E workflow parent")).toBeVisible({ timeout: 30_000 });
  } finally {
    if (workspaceId) {
      // No foreign keys: delete ext rows explicitly, children and tasks before their parent.
      await db.query(`DELETE FROM ext_workflow_run_event WHERE workspace_id = $1`, [workspaceId]);
      await db.query(`DELETE FROM ext_workflow_run_step WHERE workspace_id = $1`, [workspaceId]);
      await db.query(`DELETE FROM ext_workflow_run WHERE workspace_id = $1`, [workspaceId]);
      await db.query(`DELETE FROM ext_workflow_node WHERE workspace_id = $1`, [workspaceId]);
      await db.query(`DELETE FROM ext_workflow WHERE workspace_id = $1`, [workspaceId]);
    }
    if (parentId) {
      await db.query(
        `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE parent_issue_id = $1 OR id = $1)`,
        [parentId],
      );
      await db.query(`DELETE FROM issue WHERE parent_issue_id = $1`, [parentId]);
    }
    await api.cleanup();
    for (const id of agentIds) await db.query(`DELETE FROM agent WHERE id = $1`, [id]);
    if (runtimeId) await db.query(`DELETE FROM agent_runtime WHERE id = $1`, [runtimeId]);
    await db.end();
  }
});
```

- [ ] **Step 2: Run, expect FAIL (before D1 to D7 are merged) or, on the finished branch, run to establish the baseline**

Start the checkout's environment and run the single spec:

```bash
make up
make env-exec ARGS="-- pnpm exec playwright test e2e/ext-workflow.spec.ts"
```
Expected before the views work exists: FAIL at step 1 (`link "Workflows"` not found). On the finished branch it must PASS; if a locator is ambiguous, tighten it by scoping to the row or dialog (for example `node2.getByRole(...)`), not by weakening the assertion.

- [ ] **Step 3: Implement**

No product code in this task. The test file above is the deliverable. Adjustments allowed while stabilising: scope locators, add `{ timeout }`, and wait on `page.getByText("Workflow run")` before reading the DB. If assigning through the picker proves flaky in CI, replace step 6 with `await api.updateIssue(parent.id, { assignee_type: "workflow", assignee_id: <workflow id read from the page URL> })` and keep every other step; the picker group itself is covered by `assignee-picker.workflows.test.tsx`.

- [ ] **Step 4: Run, expect PASS**

```bash
make env-exec ARGS="-- pnpm exec playwright test e2e/ext-workflow.spec.ts"
pnpm typecheck && pnpm lint && pnpm test
```
(`make check` runs the same frontend checks plus Go tests and the whole Playwright suite.)

- [ ] **Step 5: Commit**

```bash
git add e2e/ext-workflow.spec.ts
git commit -m "test(ext-workflow): add Playwright e2e for the workflow editor and run panel

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

# Part Z: Final verification and delivery

### Task Z1: Whole-branch verification and push

**Files:** none (verification only; fix commits as needed)

- [ ] **Step 1: Regenerate and confirm a clean tree.**
  - Run `make sqlc && pnpm generate:reserved-slugs && git status --short`.
  - Expected: no diff.
- [ ] **Step 2: Backend.** Run `make env-exec ARGS="-- bash -c 'cd server && go run ./cmd/migrate up && go test ./... -count=1'"`.
  - Expected: PASS, except tests that also fail on a clean `v0.6.1` checkout.
  - Record any such failure, and confirm it with `git stash` or a `v0.6.1` worktree before excluding it.
- [ ] **Step 3: Migration round trip.** Run `make env-exec ARGS="-- bash -c 'cd server && go run ./cmd/migrate down && go run ./cmd/migrate up'"`, scoped to the ext migrations if the runner supports a target.
  - Expected: no errors.
- [ ] **Step 4: Frontend.** Run `pnpm typecheck && pnpm lint && pnpm test`, then `pnpm --filter @multica/mobile typecheck`.
  - Expected: PASS.
- [ ] **Step 5: E2E.** With `make up` running, run `pnpm exec playwright test e2e/ext-workflow` (the D8 spec), plus one existing squads e2e as a regression check.
- [ ] **Step 6: Combined check.** Run `make check`.
- [ ] **Step 7: Whole-branch code review.** Use superpowers:requesting-code-review against `v0.6.1..HEAD`, and fix the findings.
- [ ] **Step 8: Push.** Run `git push -u origin feat/ext-workflow` (origin = `https://github.com/Comet0322/multica`). Do not open a PR unless asked.

---

# Appendix: original shared contract (superseded by part notes where they differ)

## Ext Workflow: shared implementation contract

Every plan part must use exactly these names. If a part needs a name that is not listed here, it defines it inside its own tasks, and only for internal use.

Spec: `docs/superpowers/specs/2026-10-07-ext-workflow-design.md`. Repo root: `/mnt/a0df7f69-337a-4cb4-a10a-f48243e3ddfc/Code/multica`, on branch `feat/ext-workflow`, based on v0.6.1.

### Rules that every part follows

- Read `AGENTS.md` and follow it.
- Do not change the `multica` CLI (`server/cmd/multica`).
- Isolate from upstream:
  - New files use the `ext_` / `ext-` prefix.
  - Edits to existing upstream files are minimal hooks. Each hook is marked with `// ext-workflow:` and a reason.
- Migrations go in `server/migrations/ext_NNNN_<name>.{up,down}.sql`, numbered from `ext_0001`.
- No foreign keys.
- Each index is created `CONCURRENTLY IF NOT EXISTS` in its own file, and is registered in `concurrentIndexCleanups` in `server/cmd/migrate/main.go`. The down file drops it with `DROP INDEX CONCURRENTLY IF EXISTS`.
- The engine must not depend on `handler`, and `service` must not import `extworkflow`. The dependency direction is `handler → extworkflow → service, db`. Service-side hooks are interfaces defined in `service`.

### Migrations (Part A)

| file | content |
|---|---|
| `ext_0001_workflow_tables` | creates `ext_workflow`, `ext_workflow_node`, `ext_workflow_run`, `ext_workflow_run_step`, `ext_workflow_run_event` (columns per spec §3) |
| `ext_0002_task_ext_columns` | adds `ext_workflow_run_id uuid null`, `ext_workflow_step_id uuid null`, `ext_workflow_role text null`, `ext_workflow_kind text null` to `agent_task_queue` |
| `ext_0003_issue_assignee_workflow` | idempotent DROP/ADD of `issue_assignee_type_check` with `('member','agent','squad','workflow')`; the down file restores the v0.6.1 list |
| `ext_0004` … `ext_0012` | one index per file, as listed in spec §3 |

### SQL query files

- Part A: `server/pkg/db/queries/ext_workflow.sql` (workflow and node CRUD).
- Part B: `server/pkg/db/queries/ext_workflow_run.sql` (runs, steps, events, engine queries).

Regenerate with `make sqlc`. Generated row types are `db.ExtWorkflow`, `db.ExtWorkflowNode`, `db.ExtWorkflowRun`, `db.ExtWorkflowRunStep`, `db.ExtWorkflowRunEvent`.

Part A queries:

- `CreateExtWorkflow`
- `GetExtWorkflowInWorkspace(id, workspace_id)`
- `ListExtWorkflows(workspace_id)`: non-archived workflows, plus `node_count`, `active_run_count`, `last_run_at`
- `UpdateExtWorkflow`
- `ArchiveExtWorkflow`
- `ListExtWorkflowNodes(workflow_id)`: ordered by position
- `DeleteExtWorkflowNodes(workflow_id)`
- `CreateExtWorkflowNode`
- `CountActiveExtWorkflowRuns(workflow_id)`

Part B owns all run, step and event queries, and the `CreateRetryTask` edit.

### Go package `server/internal/extworkflow`

#### Pure layer (Part B)

##### `definition.go`

```go
type Node struct {
    Key            string   `json:"key"`
    Title          string   `json:"title"`
    AgentID        string   `json:"agent_id"` // uuid string
    Prompt         string   `json:"prompt"`
    RequiresReview bool     `json:"requires_review"`
    MaxAttempts    int      `json:"max_attempts"`
    DependsOn      []string `json:"depends_on"`
}

type Definition struct { // also the JSON snapshot stored in ext_workflow_run.definition
    SupervisorAgentID string `json:"supervisor_agent_id"`
    MaxRewinds        int    `json:"max_rewinds"`
    Nodes             []Node `json:"nodes"` // slice order = position
}

type ValidationError struct {
    NodeKey string `json:"node_key,omitempty"`
    Field   string `json:"field"`
    Message string `json:"message"`
}

const MaxNodes = 50
const MaxPromptBytes = 20000

func ValidateStructure(d Definition) []ValidationError // keys, dup, deps, self-dep, cycles, limits, max_attempts 1..10, max_rewinds 0..10, >=1 node
func (d Definition) NodeByKey(key string) (Node, bool)
func Ancestors(d Definition, key string) map[string]bool   // transitive upstream, excludes key
func Descendants(d Definition, key string) map[string]bool // transitive downstream, excludes key
func DepthOf(d Definition) map[string]int                  // longest-path depth, for briefing outline
```

Part A's CRUD handler uses `ValidateStructure`. Part A must therefore create `definition.go` (with `definition_test.go`) in its validation task. Part B consumes it and does not recreate it.

##### `transitions.go` (Part B)

```go
type StepStatus string // "pending","running","awaiting_supervisor","awaiting_human","done","skipped","failed","cancelled"
type PendingReason string // "review","failure","rewind_request"
type DecisionAction string // "approve","redo","retry","skip","rewind","escalate","abort","request-rewind"
type Decision struct {
    Action   DecisionAction
    Step     string // node key (parent-issue comments)
    To       string // node key
    Reason   string
    Feedback string
}
```

The signature and semantics of the pure `Next` are owned by Part B. The run-level effects are: rewind, abort, and run completion.

##### `block.go` (Part B)

```go
const BlockLang = "ext-workflow"
func ContainsBlock(markdown string) bool                       // used by handler isNoteComment hook
func ParseBlock(markdown string) (d Decision, found bool, err error)
```

#### Engine (Part B, `engine.go` and friends)

```go
type Engine struct { /* deps */ }

func NewEngine(deps Deps) *Engine

type Deps struct {
    Pool      *pgxpool.Pool // or the repo's tx starter type
    Queries   *db.Queries
    Issues    *service.IssueService
    Tasks     *service.TaskService
    Access    AgentAccess
    Publisher Publisher
    Enabled   bool
}

type AgentAccess interface {
    CanInvokeAgent(ctx context.Context, workspaceID pgtype.UUID, actorType string, actorID pgtype.UUID, agentID pgtype.UUID) (bool, error)
}

type Publisher interface {
    Publish(eventType string, workspaceID string, actorType string, actorID string, payload map[string]any)
}

// assignment & lifecycle
func (e *Engine) ValidateAssignment(ctx context.Context, workspaceID, workflowID pgtype.UUID, actorType string, actorID pgtype.UUID) error // returns *AssignError{Code, Message}
func (e *Engine) StartRun(ctx context.Context, issueID pgtype.UUID, actorType string, actorID pgtype.UUID) error
func (e *Engine) OnChildEvents(ctx context.Context, tx pgx.Tx, parentID pgtype.UUID, events []service.ExtChildEvent) error
func (e *Engine) OnTaskTerminal(ctx context.Context, taskID pgtype.UUID) error
func (e *Engine) OnParentChanged(ctx context.Context, issueID pgtype.UUID) error // cancelled/deleted/reassigned
func (e *Engine) Reconcile(ctx context.Context) error
func (e *Engine) Enabled() bool

// decisions (Part C)
func (e *Engine) Decide(ctx context.Context, in DecideInput) error // in: RunID, StepID, Decision, ActorType, ActorID, OnBehalfOf, ExpectedStatus
func (e *Engine) CancelRun(ctx context.Context, runID pgtype.UUID, actorType string, actorID pgtype.UUID) error
func (e *Engine) OnComment(ctx context.Context, commentID pgtype.UUID) error     // agent comment protocol
func (e *Engine) OnMemberParentComment(ctx context.Context, issueID, commentID, memberID pgtype.UUID) error // conversation wake

// briefing (Part C)
func (e *Engine) BuildBriefing(ctx context.Context, task db.AgentTaskQueue) (text string, ok bool, err error)
```

Error sentinels (`errors.go`, Part B):

- `ErrEngineDisabled`
- `ErrWorkflowNotFound`
- `ErrWorkflowArchived`
- `ErrNoNodes`
- `ErrSupervisorUnavailable`
- `ErrAgentNotInvokable`
- `ErrStatusMismatch` (→ 409)
- `ErrIllegalDecision` (→ 422)
- `ErrForbidden` (→ 403)

#### Service-side hook (Part B; `server/internal/service/ext_workflow_hooks.go`)

```go
type ExtChildEvent struct { ChildID pgtype.UUID; Kind string }

type ExtWorkflowHooks interface {
    ValidateAssignment(ctx context.Context, workspaceID, workflowID pgtype.UUID, actorType string, actorID pgtype.UUID) error
    StartRun(ctx context.Context, issueID pgtype.UUID, actorType string, actorID pgtype.UUID) error
    OnChildEvents(ctx context.Context, tx pgx.Tx, parentID pgtype.UUID, events []ExtChildEvent) error
}
```

`IssueService` gets the field `ExtWorkflow ExtWorkflowHooks`; it may be nil, and nil means disabled.

#### TaskService (Part B; `server/internal/service/ext_workflow_task.go`)

```go
type ExtWorkflowTaskParams struct {
    IssueID, AgentID, RunID pgtype.UUID
    StepID      pgtype.UUID // invalid for summary/conversation
    Role        string      // "step" | "supervisor"
    Kind        string      // "step","review","failure","rewind_request","summary","conversation"
    HandoffNote string
}

func (s *TaskService) EnqueueExtWorkflowTask(ctx context.Context, tx pgx.Tx, p ExtWorkflowTaskParams) (db.AgentTaskQueue, error)
```

### HTTP API

Part A owns workflow CRUD; Part C owns run endpoints. Handlers:

- Part A: `server/internal/handler/ext_workflow.go`
- Part C: `server/internal/handler/ext_workflow_run.go`
- Part B: `server/internal/handler/ext_workflow_bridge.go` (implements `AgentAccess` and `Publisher`)

The handler field is `Handler.ExtWorkflow *extworkflow.Engine`. It may be nil, and nil means disabled.

JSON shapes (snake_case):

```
Workflow   {id, workspace_id, name, description, supervisor_agent_id, max_rewinds, creator_id, avatar_url|null,
            archived_at|null, created_at, updated_at, node_count, active_run_count, last_run_at|null, nodes?: Node[]}
NodeJSON   {id, key, title, agent_id, prompt, requires_review, max_attempts, depends_on: string[], position}
GET  /api/ext/workflows                 -> {workflows: Workflow[]}
POST /api/ext/workflows  {name, description, supervisor_agent_id} -> Workflow (201)
GET  /api/ext/workflows/{id}            -> Workflow (with nodes)
PUT  /api/ext/workflows/{id}  {name?, description?, supervisor_agent_id?, max_rewinds?, nodes?: [{key,title,agent_id,prompt,requires_review,max_attempts,depends_on}]}
                                        -> Workflow | 422 {error:"validation_failed", errors: ValidationError[]}
DELETE /api/ext/workflows/{id}          -> 204 (archive)
GET  /api/ext/workflows/{id}/runs?limit&offset -> {runs: RunSummary[], total}
RunSummary {id, workspace_id, workflow_id, workflow_name, issue_id, issue_identifier, issue_title, triggered_by_type, triggered_by_id,
            status, rewinds_used, max_rewinds, started_at, finished_at|null, created_at, updated_at}
GET  /api/ext/workflow-runs?issue_id=X  -> {runs: RunSummary[], step_of: null | {run_id, step_id, node_key, index, total, parent_issue_id}}
GET  /api/ext/workflow-runs/{id}        -> Run = RunSummary + {steps: Step[], events: RunEvent[]}
Step       {id, node_key, title, agent_id, issue_id, status, attempts, max_attempts, requires_review, depends_on,
            pending_reason|null, last_feedback|null, escalation_reason|null, started_at|null, finished_at|null}
RunEvent   {id, step_id|null, kind, actor_type, actor_id|null, on_behalf_of|null, payload: object, created_at}
POST /api/ext/workflow-runs/{id}/cancel -> 204
POST /api/ext/workflow-runs/{run}/steps/{step}/decision {action, to?, reason?, feedback?, expected_status} -> Run | 409 | 422 | 403
```

WS events are added to `server/pkg/protocol/events.go` (by Part A for workflow events, by Part B for run events):

- `ext_workflow:created`, `ext_workflow:updated`, `ext_workflow:deleted` with payload `{workflow_id}`;
- `ext_workflow_run:updated` with payload `{run_id, issue_id, workflow_id}`.

### Frontend

#### `packages/core/ext-workflows/` (Part A)

| file | contents |
|---|---|
| `types.ts` | `ExtWorkflow`, `ExtWorkflowNode`, `ExtWorkflowNodeInput`, `ExtWorkflowValidationError`, `ExtWorkflowRunSummary`, `ExtWorkflowRun`, `ExtWorkflowStep`, `ExtWorkflowRunEvent`, `ExtWorkflowStepStatus`, `ExtWorkflowRunStatus`, `ExtWorkflowDecisionAction`, `ExtWorkflowIssueRuns` |
| `schemas.ts` | zod schemas named `ExtWorkflowSchema` etc. Unknown step/run statuses `.catch("unknown")`. |
| `queries.ts` | `extWorkflowKeys`, plus the option functions `extWorkflowListOptions(wsId)`, `extWorkflowDetailOptions(wsId,id)`, `extWorkflowRunsOptions(wsId,id)`, `extWorkflowIssueRunsOptions(wsId,issueId)`, `extWorkflowRunOptions(wsId,runId)` |
| `mutations.ts` | `useCreateExtWorkflow`, `useUpdateExtWorkflow`, `useArchiveExtWorkflow`, `useDecideExtWorkflowStep`, `useCancelExtWorkflowRun` |
| `index.ts` | re-exports |

`extWorkflowKeys` has this shape:

```ts
{ all:(wsId)=>["ext-workflows",wsId], list:(wsId)=>[...all,"list"], detail:(wsId,id)=>[...all,"detail",id],
  runs:(wsId,id)=>[...all,"runs",id], runsAll:(wsId)=>["ext-workflow-runs",wsId],
  issueRuns:(wsId,issueId)=>["ext-workflow-runs",wsId,"issue",issueId], run:(wsId,runId)=>["ext-workflow-runs",wsId,"run",runId] }
```

API client: `ApiClient.fetch` is private, so add methods at the end of the class in `packages/core/api/client.ts`, inside one block marked `// ext-workflow`. The methods are:

- `listExtWorkflows`, `getExtWorkflow`, `createExtWorkflow`, `updateExtWorkflow`, `archiveExtWorkflow`
- `listExtWorkflowRuns`, `getIssueExtWorkflowRuns`, `getExtWorkflowRun`
- `cancelExtWorkflowRun`, `decideExtWorkflowStep`

They use schemas from `core/ext-workflows/schemas.ts` together with `parseWithFallback`. `updateExtWorkflow` must surface 422 `errors[]` to the caller, as a typed error class `ExtWorkflowValidationFailed` with `.errors`.

Realtime: Part A adds the handlers in `use-realtime-sync.ts`. An `ext_workflow:` event invalidates `extWorkflowKeys.all(wsId)`. An `ext_workflow_run:` event invalidates `extWorkflowKeys.runsAll(wsId)`, `extWorkflowKeys.all(wsId)`, and the issue detail and children of `payload.issue_id`.

Paths: `paths.workflows()` and `paths.workflowDetail(id)`, with segment `workflows`. Modal key: `"create-ext-workflow"`.

#### Views (Part D)

| location | contents |
|---|---|
| `packages/views/ext-workflows/` | `index.ts`, `components/workflows-page.tsx`, `components/workflow-detail-page.tsx`, `components/node-editor.tsx`, `components/dag-preview.tsx`, `components/workflow-runs-tab.tsx` |
| `packages/views/modals/create-ext-workflow.tsx` | the create dialog |
| `packages/views/issues/components/ext-workflow-run-section.tsx` | the issue-sidebar run section, plus the child-issue line |
| `packages/views/locales/<lang>/ext-workflows.json` | i18n namespace `ext-workflows`, for en, zh-Hans, ko, ja, fr |

### Testing commands

Check the Makefile, CONTRIBUTING.md and the existing tests for exact usage.

- Go: `cd server && go test ./internal/extworkflow/... -count=1`. DB tests need the env from `make up` / `.env.worktree`.
- Frontend: `pnpm --filter @multica/core test`, `pnpm --filter @multica/views test`, `pnpm typecheck`, `pnpm lint`.
- `make sqlc` after SQL changes.
