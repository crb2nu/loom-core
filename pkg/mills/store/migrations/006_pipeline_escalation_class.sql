-- 006_pipeline_escalation_class.sql — records the terminal fault class of an
-- escalated pipeline run so budget accounting can tell a run that did real
-- work apart from a no-op capacity escalation.
--
-- Motivation (spawn-pool wedge, 2026-07-02): a saturated spawn pool
-- ("400 max concurrent spawns reached (6)") makes a run escalate at its very
-- first spawn — class=transient_quota, cost $0, zero stage progress, ~9ms.
-- The pipeline budget enforcer (pkg/mills/budget.go) counts every
-- pipeline_runs row toward MaxRunsPerDay regardless of outcome, so a burst of
-- these no-op escalations exhausted the whole day's run budget and blocked
-- ready items until the rows aged out of the rolling 24h window — a full-day
-- throughput DoS on a transient, self-correcting capacity limit.
--
-- The class is the ErrorClass string (pkg/mills/pipeline/error_class.go) parsed
-- from the escalation reason's "[class=…]" marker; NULL when the escalation
-- carried no class marker (gate-fail / cross-repo / drive-failure paths) or the
-- run is not escalated. PipelineDAO.CountBudgetedSince excludes only
-- escalated + zero-cost + class='transient_quota' rows, so real code/infra
-- failures still count against the cap (runaway-loop protection intact) while
-- capacity no-ops (which cost ~$0) do not. This column is the same signal the
-- Mills failure-classification / fault-taxonomy backlog work builds on.
--
-- Append-only: nothing in 001–005 changes. The column is nullable so
-- pipeline_runs created before this migration leave it empty (→ counted).

ALTER TABLE pipeline_runs ADD COLUMN escalation_class TEXT;

-- Partial index over the exempt rows only, so the budget count's exclusion
-- predicate stays cheap without weighing on the common (non-escalated) path.
CREATE INDEX IF NOT EXISTS idx_pipeline_runs_escalation_class
  ON pipeline_runs(escalation_class)
  WHERE escalation_class IS NOT NULL;

-- ----- Reverse (manual) -----
-- SQLite has no DROP COLUMN before 3.35; reverse only by recreating the table
-- without the column. Only needed if a soak exposes a problem.
-- ALTER TABLE pipeline_runs DROP COLUMN escalation_class;
-- DROP INDEX IF EXISTS idx_pipeline_runs_escalation_class;
