-- server/pkg/db/queries/workflow.sql

-- name: ListWorkflowDefinitionCandidates :many
-- Todo issues carrying a flow:<name> label that have not been expanded yet
-- (no workflow metadata), were previously marked invalid, or are stuck in
-- 'expanding' (a crashed expander). ClaimWorkflowDefinition decides atomically
-- whether an expanding claim is stale, so listing a live one is harmless.
SELECT i.* FROM issue i
WHERE i.status = 'todo'
  AND (NOT (i.metadata ? 'workflow') OR i.metadata->'workflow'->>'state' IN ('invalid', 'expanding'))
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
-- must have no workflow metadata, be marked invalid, or hold an expanding claim
-- older than stale_before. A missing, empty or invalid claimed_at counts as
-- stale. pg_input_is_valid requires PostgreSQL 16+.
UPDATE issue SET
    metadata = jsonb_set(metadata, '{workflow}', sqlc.arg('value')::jsonb),
    revision = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND status = 'todo'
  AND (
      NOT (metadata ? 'workflow')
      OR metadata->'workflow'->>'state' = 'invalid'
      OR (metadata->'workflow'->>'state' = 'expanding'
          AND (CASE WHEN COALESCE(metadata->'workflow'->>'claimed_at', '') <> ''
                         AND pg_input_is_valid(metadata->'workflow'->>'claimed_at', 'timestamptz')
                    THEN (metadata->'workflow'->>'claimed_at')::timestamptz
                    ELSE '-infinity'::timestamptz END) < sqlc.arg('stale_before')::timestamptz)
  )
RETURNING *;

-- name: RefreshWorkflowClaim :one
-- Fencing: renews the expanding claim only while the caller still holds it,
-- i.e. claimed_at still equals the token it last wrote. No rows means the
-- claim was lost to another expander.
UPDATE issue SET
    metadata = jsonb_set(metadata, '{workflow,claimed_at}', to_jsonb(sqlc.arg('new_claimed_at')::text))
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND metadata->'workflow'->>'state' = 'expanding'
  AND metadata->'workflow'->>'claimed_at' = sqlc.arg('expected_claimed_at')::text
RETURNING *;

-- name: FinishWorkflowExpansion :one
-- Writes the final workflow metadata only while the caller still holds the
-- expanding claim (claimed_at equals its token). No rows means the claim was lost.
UPDATE issue SET
    metadata = jsonb_set(metadata, '{workflow}', sqlc.arg('value')::jsonb),
    revision = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND metadata->'workflow'->>'state' = 'expanding'
  AND metadata->'workflow'->>'claimed_at' = sqlc.arg('expected_claimed_at')::text
RETURNING *;

-- name: ListRunningWorkflowDefinitions :many
SELECT * FROM issue
WHERE metadata @> '{"workflow": {"state": "running"}}'::jsonb
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
  AND metadata @> jsonb_build_object('workflow', jsonb_build_object('run', sqlc.arg('run')::text))
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
    metadata = jsonb_set(metadata, '{workflow}', sqlc.arg('value')::jsonb),
    revision = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND metadata->'workflow'->>'phase' = sqlc.arg('expected_phase')::text
  AND COALESCE((metadata->'workflow'->>'attempts')::int, 0) = sqlc.arg('expected_attempts')::int
  AND COALESCE(metadata->'workflow'->>'dispatched_at', '') = sqlc.arg('expected_dispatched_at')::text
RETURNING *;

-- name: SetWorkflowDefinitionState :one
-- Fenced definition state change: applies only while the definition is still in
-- expected_state. No rows means another writer already moved it.
UPDATE issue SET
    metadata = jsonb_set(metadata, '{workflow}', sqlc.arg('value')::jsonb),
    revision = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND metadata->'workflow'->>'state' = sqlc.arg('expected_state')::text
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
  AND i.metadata->'workflow'->>'state' = sqlc.arg('want_state')::text
RETURNING i.*;
