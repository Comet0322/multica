-- Managed runtimes are provisioned and operated by the platform (a runtime
-- controller), not by a user's machine. Their implementation details are hidden
-- from ordinary members and the daemon API treats them differently.
ALTER TABLE agent_runtime ADD COLUMN IF NOT EXISTS managed BOOLEAN NOT NULL DEFAULT false;
