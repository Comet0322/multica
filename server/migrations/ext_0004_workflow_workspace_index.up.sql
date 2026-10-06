CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_workspace
    ON ext_workflow (workspace_id)
    WHERE archived_at IS NULL;
