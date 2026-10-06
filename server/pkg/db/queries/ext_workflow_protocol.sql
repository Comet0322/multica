-- ext-workflow: queries of the comment protocol and the claim-time briefing.

-- name: HasExtWorkflowConversationTaskForComment :one
-- A person's comment wakes the supervisor at most once: the comment
-- re-evaluation paths (cancelled batches, end-of-run replays) reach the
-- conversation hook again for the same comment.
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue
    WHERE trigger_comment_id = @comment_id AND ext_workflow_kind = 'conversation'
) AS woken;

-- name: GetLatestAgentCommentOnIssue :one
-- The newest comment an agent wrote on an issue: a step's output, as the
-- briefing hands it to downstream steps and to the supervisor.
SELECT * FROM comment
WHERE issue_id = @issue_id AND author_type = 'agent' AND author_id = @agent_id AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 1;
