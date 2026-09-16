- **Mills operator boot no longer starves its own bookkeeping**
  (`cmd/loom-mills-operator`, `pkg/mills`, `pkg/mills/store`): every operator
  boot on 2026-09-07/08 logged 16× `append event failed … context deadline
  exceeded` for `reconciler.auto_requeue_failed`, one for `auto_requeue_sweep`,
  and `scheduler: initial tick failed` — every loop fired its first pass at
  once on a cold SQLite page cache and the report-rollup warm-up (~30s)
  drained the connection pool. The boot is now ordered (rollup warm-up →
  reconciler boot tick → escalation sweep; intake loops behind the warm-up,
  every wait budgeted), the boot tick carries only the control law with the
  four housekeeping sweeps staggered onto the ticks that follow, the event
  ledger appends through a dedicated single-connection write handle, and the
  reconciler's ledger rows are written under their own 5s budget detached from
  the sweep context. A starved auto-requeue pass now stops at the first
  dead-context read, reports the candidates it never judged as `unreached`
  (sweep result, `auto_requeue_sweep` row, `auto_requeue_unreached` on the
  `escalation_sweep` row) with outcome `timeout` instead of `ok`, and writes
  no phantom `auto_requeue_failed` rows; genuine per-item failures now carry
  `stage: lookup|transition`.
