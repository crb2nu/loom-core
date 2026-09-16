-- 039_billing_class.sql — who pays for each stage attempt's tokens.
--
-- stage_results.billing: api | subscription | local (see store.BillingClass).
-- pipeline_runs.subscription_cost_usd: the subscription-billed slice of
-- cost_usd, rolled up by the runner alongside cost_usd so the budget can
-- subtract it without joining stage_results on every admission decision.
--
-- The backfill mirrors store.BillingForBackend: spawn harnesses (Claude Code,
-- Codex) run under the cluster OAuth subscriptions, flexinfer is local
-- hardware, any other attributed backend is a metered API. Rows with no
-- backend (pre-013 history, gates, ci_watch, merge) stay NULL — they carry
-- no cost.

ALTER TABLE stage_results ADD COLUMN billing TEXT;
ALTER TABLE pipeline_runs ADD COLUMN subscription_cost_usd REAL NOT NULL DEFAULT 0;

UPDATE stage_results
SET billing = CASE
    WHEN lower(backend) = 'spawn' THEN 'subscription'
    WHEN lower(backend) IN ('flexinfer', 'local') THEN 'local'
    WHEN backend IS NOT NULL AND backend <> '' THEN 'api'
END
WHERE billing IS NULL;

UPDATE pipeline_runs
SET subscription_cost_usd = COALESCE((
    SELECT SUM(sr.cost_usd) FROM stage_results sr
    WHERE sr.pipeline_run_id = pipeline_runs.id AND sr.billing = 'subscription'
), 0);

CREATE INDEX IF NOT EXISTS idx_stage_billing
    ON stage_results(billing)
    WHERE billing IS NOT NULL;

-- ----- Reverse (manual) -----
-- DROP INDEX IF EXISTS idx_stage_billing;
-- Recreate stage_results / pipeline_runs without the two columns.
