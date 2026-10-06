CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ext_workflow_node_agent
    ON ext_workflow_node (agent_id);
