CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_run_active_status
    ON ext_workflow_run (status)
    WHERE status IN ('running', 'waiting_human');
