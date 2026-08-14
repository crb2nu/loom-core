-- Persist whether a retryable pipeline failure consumed its bounded
-- auto-requeue budget before escalation.
ALTER TABLE pipeline_runs ADD COLUMN retry_exhausted INTEGER;
