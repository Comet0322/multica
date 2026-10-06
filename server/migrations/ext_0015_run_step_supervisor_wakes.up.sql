-- How many supervisor tasks the engine started for a step's current pending
-- decision. The engine re-wakes a silent supervisor once, then escalates.
ALTER TABLE ext_workflow_run_step
    ADD COLUMN IF NOT EXISTS supervisor_wakes INT NOT NULL DEFAULT 0;
