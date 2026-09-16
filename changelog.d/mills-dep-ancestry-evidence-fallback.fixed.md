- Mills reconciler: the deployment-aware dependency gate now recovers a merged
  dependency's landed commit when its run left no `merged_sha` artifact (a
  hand-finished MR reaped by the ghost-spark sweep, an external merge-queue
  candidate) — from the serial merge-queue ledger, the ghost-spark closure
  event's MR, or a single cached GitLab lookup — instead of erroring into a
  fail-open admission on every tick. The remaining fail-open WARN
  ("dependency deployment ancestry unavailable") is rate-limited to one line
  per dependency per 30 minutes (it was ~3k lines/hour, 97% of the operator
  log, on 2026-09-13). New counters `mills_dependency_ancestry_unavailable_total{reason}`
  and `mills_dependency_merge_sha_resolved_total{source}` keep the unresolved
  rate visible on the metrics listener.
