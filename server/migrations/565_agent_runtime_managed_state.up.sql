-- State the runtime controller reports for a managed runtime's Sandbox
-- (running | suspended | waking | unavailable), and when. The server shows a
-- managed runtime as available to users while the Sandbox is resumable, and
-- fails closed to unavailable when the report goes stale.
ALTER TABLE agent_runtime ADD COLUMN IF NOT EXISTS managed_state TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_runtime ADD COLUMN IF NOT EXISTS managed_state_at TIMESTAMPTZ;
