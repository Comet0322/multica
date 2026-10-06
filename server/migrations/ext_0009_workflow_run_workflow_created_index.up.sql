CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_run_workflow_created
    ON ext_workflow_run (workflow_id, created_at DESC);
