-- ext-workflow: queries of the comment protocol and the claim-time briefing.

-- name: HasExtWorkflowConversationTaskForComment :one
-- A person's comment wakes the supervisor at most once: the comment
-- re-evaluation paths (cancelled batches, end-of-run replays) reach the
-- conversation hook again for the same comment.
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue
    WHERE trigger_comment_id = @comment_id AND ext_workflow_kind = 'conversation'
) AS woken;
