- **Mills store / reconciler**: event-window reads stop timing out. Measured on
  the 2026-09-02 production store (450MB, 285k events, 214k KPI snapshots):
  `ListSinceByKinds` was still pinned to the time index after migration 029
  added the kind index, so the KPI writer's 30-day superseded-run scan walked
  the whole window (~0.5s idle, past the 10s budget on the loaded node — the
  "kpi snapshot failed: event scan: context deadline exceeded" logged ~10×/h);
  it now rides `idx_events_kind_occurred`. The promotion report's actor-prefix
  read uses a half-open actor range instead of `substr()`, so it SEARCHes the
  actor index. The reconciler's per-tick bookkeeping rows (deferred, skipped,
  ghost-spark-skipped — 92% of a week's 75k rows, mostly the same item
  restating the same reason every minute) are now written once per 30-minute
  cooldown per subject and change, and a daily bounded retention sweep prunes
  those kinds after 14 days and KPI snapshots after 90 days
  (`mills_event_noise_suppressed_total`, `mills_retention_pruned_total`).
  Audit kinds are never pruned. The council mutator's skip audit rows are
  written under their own 5s budget so an exhausted stage context no longer
  loses the record of why a proposal was dropped.
