ALTER TABLE agent_task_queue
    DROP COLUMN IF EXISTS ext_workflow_kind,
    DROP COLUMN IF EXISTS ext_workflow_role,
    DROP COLUMN IF EXISTS ext_workflow_step_id,
    DROP COLUMN IF EXISTS ext_workflow_run_id;
