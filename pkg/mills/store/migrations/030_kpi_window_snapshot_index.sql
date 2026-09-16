-- KPI window-snapshot hot-read index (from the bounded-hot-reads slice).
-- The slice's event-table composites were dropped at rebase: main's 027
-- (idx_events_occurred) and 029 (idx_events_actor_occurred,
-- idx_events_kind_occurred — landed under a measured append-tax waiver)
-- already cover those reads, and near-duplicate composites would tax the
-- hottest write path again for a marginal id-tiebreaker win.
CREATE INDEX IF NOT EXISTS idx_kpi_window_snapshot
    ON kpi_snapshots(window_seconds, snapshot_at ASC);
