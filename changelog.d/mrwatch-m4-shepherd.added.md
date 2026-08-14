- HUD mrwatch shepherd (M4): a bounded-autonomy reconciler that runs after each
  MR-registry poll and takes at most one small, reversible action per unhealthy
  MR — retry a flaky-classified pipeline once, create a fresh branch pipeline for
  a skipped/missing head pipeline (>10m old), or arm auto-merge on a settled
  unarmed MR (>30m old, `auto_merge=true`). It never rebases, force-pushes, merges
  directly, or touches `conflict`/`ci_failed_deterministic` MRs. Guarded by a
  per-MR daily action budget (`LOOM_MRWATCH_SHEPHERD_BUDGET`, default 2; in-memory,
  resets on restart) and a global kill switch (`LOOM_MRWATCH_SHEPHERD`, DEFAULT
  OFF — must be set truthy to enable). A 405 on arm is treated as retry-next-poll,
  not a failure, and does not consume budget. Actions are audit-logged and served
  at `GET /api/mrwatch/actions`.
