-- 008_backlog_target_project: per-item target repo for cross-repo Mills
-- execution. When empty the item targets the operator's home repo (backward
-- compatible). Nullable + appended last so the positional column order of
-- existing queries is preserved (same pattern as 005_backlog_plan_id).
ALTER TABLE backlog_items ADD COLUMN target_project TEXT;
