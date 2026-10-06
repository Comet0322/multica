CREATE TABLE IF NOT EXISTS ext_workflow (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    supervisor_agent_id UUID NOT NULL,
    max_rewinds INT NOT NULL DEFAULT 3,
    creator_id UUID NOT NULL,
    avatar_url TEXT,
    archived_at TIMESTAMPTZ,
    archived_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ext_workflow_node (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    key TEXT NOT NULL,
    title TEXT NOT NULL,
    agent_id UUID NOT NULL,
    prompt TEXT NOT NULL DEFAULT '',
    requires_review BOOLEAN NOT NULL DEFAULT false,
    max_attempts INT NOT NULL DEFAULT 3,
    depends_on TEXT[] NOT NULL DEFAULT '{}',
    position INT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS ext_workflow_run (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    workflow_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    triggered_by_type TEXT NOT NULL,
    triggered_by_id UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'waiting_human', 'done', 'failed', 'cancelled')),
    definition JSONB NOT NULL,
    rewinds_used INT NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ext_workflow_run_step (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    node_key TEXT NOT NULL,
    agent_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'awaiting_supervisor', 'awaiting_human',
                          'done', 'skipped', 'failed', 'cancelled')),
    attempts INT NOT NULL DEFAULT 0,
    pending_reason TEXT,
    last_feedback TEXT,
    escalation_reason TEXT,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ext_workflow_run_event (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    step_id UUID,
    kind TEXT NOT NULL,
    actor_type TEXT NOT NULL,
    actor_id UUID,
    on_behalf_of UUID,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
