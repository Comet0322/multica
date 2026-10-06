CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_ext_workflow_step ON agent_task_queue (ext_workflow_step_id) WHERE ext_workflow_step_id IS NOT NULL;
