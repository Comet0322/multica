CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uidx_ext_workflow_run_active_issue
    ON ext_workflow_run (issue_id)
    WHERE status IN ('running', 'waiting_human');
