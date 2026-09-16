-- 036_vaccine_ref.sql (renumbered from 030 at rescue; 030-035 landed meanwhile) — tracks the prevention obligation born by a terminal
-- escalation. NULL means the rescued item still needs a Pattern Loom vaccine.
ALTER TABLE pipeline_runs ADD COLUMN vaccine_ref TEXT;

CREATE INDEX IF NOT EXISTS idx_pipeline_runs_rescue_vaccine
  ON pipeline_runs(backlog_id, state, vaccine_ref)
  WHERE state = 'escalated';
