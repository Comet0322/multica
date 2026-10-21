-- name: UpsertManagedAgentRuntime :one
-- Pre-creates (or re-labels) the runtime row for a platform-operated Sandbox.
-- The arbiter is the same partial unique index UpsertAgentRuntime uses, so when
-- the daemon later registers with the same (workspace, daemon_id, provider) it
-- lands on this row; UpsertAgentRuntime never touches managed, custom_name or
-- visibility, so they survive re-registration.
INSERT INTO agent_runtime (
    workspace_id, daemon_id, name, runtime_mode, provider, status,
    device_info, metadata, owner_id, visibility, custom_name, managed, last_seen_at
) VALUES (
    @workspace_id, @daemon_id, @name, 'local', @provider, 'offline',
    '', '{}'::jsonb, @owner_id, 'public', @custom_name, true, now()
)
ON CONFLICT (workspace_id, daemon_id, provider) WHERE profile_id IS NULL
DO UPDATE SET
    custom_name = EXCLUDED.custom_name,
    visibility = 'public',
    managed = true,
    updated_at = now()
WHERE agent_runtime.managed
RETURNING *;

-- name: GetManagedAgentRuntimeByDaemon :one
SELECT * FROM agent_runtime
WHERE workspace_id = @workspace_id AND daemon_id = @daemon_id AND managed
LIMIT 1;

-- name: ListManagedRuntimeState :many
-- One row per managed runtime for the runtime controller: liveness plus how
-- much work is waiting on it.
SELECT
    rt.id,
    rt.workspace_id,
    rt.daemon_id,
    rt.provider,
    rt.status,
    rt.last_seen_at,
    rt.custom_name,
    (SELECT count(*) FROM agent_task_queue q
       WHERE q.runtime_id = rt.id AND q.status = 'queued')::bigint AS queued_tasks,
    (SELECT count(*) FROM agent_task_queue q
       WHERE q.runtime_id = rt.id AND q.status IN ('dispatched', 'running'))::bigint AS active_tasks
FROM agent_runtime rt
WHERE rt.managed
ORDER BY rt.created_at, rt.id;
