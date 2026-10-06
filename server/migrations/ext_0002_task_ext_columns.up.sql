ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS ext_workflow_run_id UUID NULL,
    ADD COLUMN IF NOT EXISTS ext_workflow_step_id UUID NULL,
    ADD COLUMN IF NOT EXISTS ext_workflow_role TEXT NULL,
    ADD COLUMN IF NOT EXISTS ext_workflow_kind TEXT NULL;
