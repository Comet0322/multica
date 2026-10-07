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

-- name: ExtWorkflowRunExistsForIssue :one
SELECT EXISTS (SELECT 1 FROM ext_workflow_run WHERE issue_id = @issue_id AND workspace_id = @workspace_id);

-- name: ResolveExtWorkflowEscalationInbox :many
-- Takes a run's open escalation items out of the inbox (archived and read)
-- once nobody needs to act on them: their step was decided or the run ended.
-- With step_id only that step's items go; without it, the whole run's.
UPDATE inbox_item SET archived = true, read = true
WHERE workspace_id = @workspace_id AND issue_id = @issue_id
  AND type = 'ext_workflow_escalation' AND archived = false
  AND details->>'run_id' = @run_id::text
  AND (sqlc.narg(step_id)::text IS NULL OR details->>'step_id' = sqlc.narg(step_id)::text)
RETURNING recipient_type, recipient_id;
