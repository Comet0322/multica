-- server/pkg/db/queries/workflow.sql

-- name: ListWorkflowDefinitionCandidates :many
-- Todo issues carrying a flow:<name> label that have not been expanded yet
-- (no workflow metadata) or were previously marked invalid.
SELECT i.* FROM issue i
WHERE i.status = 'todo'
  AND (NOT (i.metadata ? 'workflow') OR i.metadata->'workflow'->>'state' = 'invalid')
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
-- older than stale_before.
UPDATE issue SET
    metadata = jsonb_set(metadata, '{workflow}', sqlc.arg('value')::jsonb),
    revision = revision + 1,
    last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now()),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
  AND (
      NOT (metadata ? 'workflow')
      OR metadata->'workflow'->>'state' = 'invalid'
      OR (metadata->'workflow'->>'state' = 'expanding'
          AND (metadata->'workflow'->>'claimed_at')::timestamptz < sqlc.arg('stale_before')::timestamptz)
  )
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
ORDER BY created_at DESC
LIMIT 1;
