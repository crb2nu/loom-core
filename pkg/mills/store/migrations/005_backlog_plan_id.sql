-- 005_backlog_plan_id: link a backlog item to a first-class Plan in the
-- agent-context store (plan store convergence). Nullable + appended last so the
-- positional column order of existing queries is preserved.
ALTER TABLE backlog_items ADD COLUMN plan_id TEXT;
