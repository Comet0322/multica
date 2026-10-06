CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_run_event_run_created
    ON ext_workflow_run_event (run_id, created_at);
