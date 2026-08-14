-- 010_pipeline_escalation_metadata.sql — persists the classification metadata
-- emitted with Mills escalations so downstream history views, handoffs, and
-- budget/audit readers do not need to re-parse free-form reason strings.
--
-- The existing escalation_class column keeps the runner's historical
-- ErrorClass spelling. These columns add the policy-facing failure class,
-- retryability, and recognized external dependency incident identifiers.

ALTER TABLE pipeline_runs ADD COLUMN escalation_failure_class TEXT;
ALTER TABLE pipeline_runs ADD COLUMN escalation_external_dependency_id TEXT;
ALTER TABLE pipeline_runs ADD COLUMN escalation_external_dependency TEXT;
ALTER TABLE pipeline_runs ADD COLUMN escalation_retryable INTEGER;

CREATE INDEX IF NOT EXISTS idx_pipeline_runs_escalation_failure_class
  ON pipeline_runs(escalation_failure_class)
  WHERE escalation_failure_class IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_pipeline_runs_external_dependency
  ON pipeline_runs(escalation_external_dependency)
  WHERE escalation_external_dependency IS NOT NULL;

-- ----- Reverse (manual) -----
-- SQLite has no portable DROP COLUMN in older versions used by some local
-- tooling; reverse by recreating pipeline_runs without these columns if needed.
-- DROP INDEX IF EXISTS idx_pipeline_runs_escalation_failure_class;
-- DROP INDEX IF EXISTS idx_pipeline_runs_external_dependency;
