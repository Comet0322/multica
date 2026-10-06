CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_run_step_issue
    ON ext_workflow_run_step (issue_id);
