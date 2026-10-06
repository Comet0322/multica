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
