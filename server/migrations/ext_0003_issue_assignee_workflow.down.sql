-- Rows assigned to a workflow cannot satisfy the v0.6.1 constraint; unassign them.
UPDATE issue SET assignee_type = NULL, assignee_id = NULL WHERE assignee_type = 'workflow';
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_assignee_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_assignee_type_check
    CHECK (assignee_type IN ('member', 'agent', 'squad'));
