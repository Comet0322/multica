CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_node_workflow
    ON ext_workflow_node (workflow_id);
