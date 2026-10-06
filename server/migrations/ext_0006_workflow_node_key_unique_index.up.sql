CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uidx_ext_workflow_node_key
    ON ext_workflow_node (workflow_id, key);
