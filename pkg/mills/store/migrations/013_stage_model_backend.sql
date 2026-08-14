-- 013_stage_model_backend.sql — records the model + backend that produced each
-- stage attempt so the telemetry roll-up can attribute cost and reliability per
-- model tier (local flexinfer qwen, litellm→OpenRouter kimi-k3/k2.7-code, spawn
-- agents, anthropic). Both columns are nullable ON PURPOSE: historical rows and
-- any worker that does not surface its identity aggregate under an "unknown"
-- bucket, so the per-model roll-up stays complete rather than dropping cost.

ALTER TABLE stage_results ADD COLUMN model TEXT;
ALTER TABLE stage_results ADD COLUMN backend TEXT;

-- Partial index over the attributed rows only. The telemetry aggregation windows
-- on pipeline_runs.started_at and reads every windowed stage row once (durations
-- are computed in Go), so this index does not accelerate that scan — it keeps
-- ad-hoc per-model lookups indexed and stays small by excluding the NULL
-- (unattributed) rows, matching the partial-index convention in migration 010.
CREATE INDEX IF NOT EXISTS idx_stage_model_backend
    ON stage_results(model, backend)
    WHERE model IS NOT NULL;

-- ----- Reverse (manual) -----
-- SQLite lacks a portable DROP COLUMN on the older tooling some local
-- environments use; reverse by recreating stage_results without these columns.
-- DROP INDEX IF EXISTS idx_stage_model_backend;
