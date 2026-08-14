- **mills: promotion evidence report for guarded actors** (`pkg/mills/guard/promotion_report.go`,
  `cmd/loom-mills-operator/handlers_promotion.go`): `GET /api/mills/promotion-report?actor=<prefix>&window=<duration>`
  (defaults `overseer.`, `168h`) aggregates the append-only events table into the
  artifact a human reads before flipping an agent's `dry_run` off — per actor and
  per action, dry-run vs executed counts folded back together across the recorder's
  `.dryrun`/committed kind split, plus unique-subject coverage, a ten-entry subject
  sample, and first/last timestamps. The report names the trap it exists to close:
  a soak that never acted satisfies "zero false positives" trivially, so a window
  holding no actions at all is reported as `zero_evidence: true` rather than an
  empty table a reviewer reads as a clean run. Repeat actions on one subject dedup
  into `unique_subjects`, so fifty retries of one item cannot read as broad
  coverage. `BuildPromotionReport` is pure over the narrowest events surface
  (`EventDAO.ListSince`) — no new DAO method and no new index on the hot append
  path the fleet-reliability gate protects — and a window that saturates the scan
  cap errors instead of returning a truncated count, since a review that
  under-counts executed actions reads as a safer soak than it was. The read is
  open like `GET /api/mills/overseers`, which already exposes the same audit rows.
