CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uidx_ext_workflow_run_step_node
    ON ext_workflow_run_step (run_id, node_key);
