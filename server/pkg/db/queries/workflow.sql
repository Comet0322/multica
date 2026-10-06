-- server/pkg/db/queries/workflow.sql

-- name: ListWorkflowDefinitionCandidates :many
-- Todo issues carrying a flow:<name> label that have not been expanded yet
-- (no wf_state key), were previously marked invalid, or are stuck in
-- 'expanding' (a crashed expander). ClaimWorkflowDefinition decides atomically
-- whether an expanding claim is stale, so listing a live one is harmless.
SELECT i.* FROM issue i
WHERE i.status = 'todo'
  AND (
      NOT (i.metadata ? 'wf_state')
      OR i.metadata->>'wf_state' = 'expanding'
      -- An invalid definition is retried only after its description changed;
      -- the hash matches hashDescription in expand.go (sha256, first 8 bytes, hex).
      OR (i.metadata->>'wf_state' = 'invalid'
          AND COALESCE(i.metadata->>'wf_error_hash', '')
              <> left(encode(sha256(convert_to(COALESCE(i.description, ''), 'UTF8')), 'hex'), 16))
  )
  AND EXISTS (
      SELECT 1 FROM issue_to_label itl
      JOIN issue_label l ON l.id = itl.label_id
      WHERE itl.issue_id = i.id
        AND l.workspace_id = i.workspace_id
        AND l.resource_type = 'issue'
        AND LOWER(l.name) LIKE 'flow:_%'
  )
ORDER BY i.created_at ASC
LIMIT sqlc.arg('row_limit')::int;

-- name: ClaimWorkflowDefinition :one
-- Atomically claims a definition for expansion. Only one caller wins: the issue
-- must have no wf_state key, be marked invalid, or hold an expanding claim
-- older than stale_before. A missing, empty or invalid wf_claimed_at counts as
-- stale. pg_input_is_valid requires PostgreSQL 16+. The previous wf_*
-- definition keys are replaced, never merged with the new ones.
UPDATE issue SET
    metadata = (metadata - 'wf_state' - 'wf_error_hash' - 'wf_claimed_at' - 'wf_total') || sqlc.arg('value')::jsonb,
    revision = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND status = 'todo'
  AND (
      NOT (metadata ? 'wf_state')
      OR metadata->>'wf_state' = 'invalid'
      OR (metadata->>'wf_state' = 'expanding'
          AND (CASE WHEN COALESCE(metadata->>'wf_claimed_at', '') <> ''
                         AND pg_input_is_valid(metadata->>'wf_claimed_at', 'timestamptz')
                    THEN (metadata->>'wf_claimed_at')::timestamptz
                    ELSE '-infinity'::timestamptz END) < sqlc.arg('stale_before')::timestamptz)
  )
RETURNING *;

-- name: RefreshWorkflowClaim :one
-- Fencing: renews the expanding claim only while the caller still holds it,
-- i.e. wf_claimed_at still equals the token it last wrote. No rows means the
-- claim was lost to another expander.
UPDATE issue SET
    metadata = metadata || jsonb_build_object('wf_claimed_at', sqlc.arg('new_claimed_at')::text)
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND metadata->>'wf_state' = 'expanding'
  AND metadata->>'wf_claimed_at' = sqlc.arg('expected_claimed_at')::text
RETURNING *;

-- name: FinishWorkflowExpansion :one
-- Writes the final workflow metadata only while the caller still holds the
-- expanding claim (wf_claimed_at equals its token). No rows means the claim was lost.
UPDATE issue SET
    metadata = metadata || sqlc.arg('value')::jsonb,
    revision = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND metadata->>'wf_state' = 'expanding'
  AND metadata->>'wf_claimed_at' = sqlc.arg('expected_claimed_at')::text
RETURNING *;

-- name: MergeWorkflowMetadata :one
-- Merges flat wf_* keys into the issue's metadata in one statement. The value
-- must be a flat object of primitives (the platform metadata contract).
-- Rewriting identical values is a no-op and returns no rows, like
-- SetIssueMetadataKey.
UPDATE issue SET
    metadata = metadata || sqlc.arg('value')::jsonb,
    revision = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND NOT (metadata @> sqlc.arg('value')::jsonb)
RETURNING *;

-- name: ListRunningWorkflowDefinitions :many
SELECT * FROM issue
WHERE metadata @> '{"wf_state": "running"}'::jsonb
ORDER BY created_at ASC
LIMIT sqlc.arg('row_limit')::int;

-- name: ListWorkflowChildren :many
-- Direct children by parent_issue_id. Used only to find orphaned steps (a step
-- issue created but not yet stamped with metadata) and to verify parentage.
SELECT * FROM issue
WHERE workspace_id = sqlc.arg('workspace_id') AND parent_issue_id = sqlc.arg('parent_issue_id')
ORDER BY created_at ASC, number ASC;

-- name: ListWorkflowSteps :many
-- The authoritative way to find a run's steps: by the run id stamped in
-- metadata, not by parent_issue_id, so a step whose parent link is ever lost
-- (or cleared by another flow) is still tracked. GIN-indexed via metadata.
SELECT * FROM issue
WHERE workspace_id = sqlc.arg('workspace_id')
  AND metadata @> jsonb_build_object('wf_run', sqlc.arg('run')::text)
ORDER BY created_at ASC, number ASC;

-- name: LatestWorkflowTaskStatus :one
SELECT status FROM agent_task_queue
WHERE issue_id = sqlc.arg('issue_id') AND agent_id = sqlc.arg('agent_id')
  AND created_at >= sqlc.arg('since')::timestamptz
ORDER BY created_at DESC
LIMIT 1;

-- name: ClaimWorkflowStepTransition :one
-- Compare-and-set for a step: writes the new workflow metadata only while the
-- step is still in the phase, attempt count and dispatch generation the caller
-- observed. No rows
-- means another instance already moved the step.
UPDATE issue SET
    metadata = metadata || sqlc.arg('value')::jsonb,
    revision = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND metadata->>'wf_phase' = sqlc.arg('expected_phase')::text
  AND COALESCE((metadata->>'wf_attempts')::int, 0) = sqlc.arg('expected_attempts')::int
  AND COALESCE(metadata->>'wf_dispatched_at', '') = sqlc.arg('expected_dispatched_at')::text
RETURNING *;

-- name: SetWorkflowDefinitionState :one
-- Fenced definition state change: applies only while the definition is still in
-- expected_state. No rows means another writer already moved it.
UPDATE issue SET
    metadata = metadata || sqlc.arg('value')::jsonb,
    revision = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND metadata->>'wf_state' = sqlc.arg('expected_state')::text
RETURNING *;

-- name: WorkflowDBNow :one
-- The database clock, used to stamp dispatches so they compare against
-- agent_task_queue.created_at on the same clock.
SELECT now()::timestamptz AS now;

-- name: HasInFlightWorkflowTask :one
-- True while any non-terminal task (queued, dispatched, running,
-- waiting_local_directory, deferred) exists for the issue and agent,
-- regardless of when it was created.
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue
    WHERE issue_id = sqlc.arg('issue_id') AND agent_id = sqlc.arg('agent_id')
      AND status NOT IN ('completed', 'failed', 'cancelled')
) AS in_flight;

-- name: SetWorkflowDefinitionStatusIfState :one
-- UpdateIssueStatus (same repositioning and bookkeeping) applied only while the
-- definition is still in want_state, so a close that lost a race with a reopen
-- cannot overwrite the reopened status. No rows means the state moved on.
WITH wakeup_source AS MATERIALIZED (SELECT set_config('multica.source_task_id', '', true))
UPDATE issue AS i SET
    status = sqlc.arg('status')::text,
    duplicate_of_issue_id = CASE WHEN sqlc.arg('status')::text = 'cancelled' AND i.status = 'cancelled' THEN i.duplicate_of_issue_id ELSE NULL END,
    position = CASE WHEN i.status IS DISTINCT FROM sqlc.arg('status')::text THEN (
        SELECT COALESCE(MIN(target.position), 0) - 1
        FROM issue AS target
        WHERE target.workspace_id = i.workspace_id
          AND target.status = sqlc.arg('status')::text
    ) ELSE i.position END,
    revision = i.revision + CASE WHEN i.status IS DISTINCT FROM sqlc.arg('status')::text THEN 1 ELSE 0 END,
    last_activity_at = CASE WHEN i.status IS DISTINCT FROM sqlc.arg('status')::text
        THEN GREATEST(COALESCE(i.last_activity_at, i.updated_at), now())
        ELSE i.last_activity_at
    END,
    updated_at = now()
FROM wakeup_source
WHERE i.id = sqlc.arg('id') AND i.workspace_id = sqlc.arg('workspace_id')
  AND i.metadata->>'wf_state' = sqlc.arg('want_state')::text
RETURNING i.*;
