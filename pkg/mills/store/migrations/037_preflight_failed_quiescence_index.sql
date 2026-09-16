-- Keep the pipeline quiescence partial index aligned with the terminal-state
-- predicate after invocation preflight failures became terminal. This must be
-- a forward migration: installations that already applied migration 012 do
-- not re-run edited migration bodies.
DROP INDEX IF EXISTS idx_pipeline_quiescence_active;

CREATE INDEX idx_pipeline_quiescence_active
    ON pipeline_runs(state)
    WHERE state NOT IN ('done', 'escalated', 'preflight_failed', 'paused');
