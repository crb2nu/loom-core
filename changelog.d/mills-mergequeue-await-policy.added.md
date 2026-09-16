- **Merge queue pipeline wait is policy-driven** (`merge_queue.await_pipeline_minutes`,
  `pkg/mills/mergequeue`): the rebased-head pipeline await was a 45-minute
  compiled constant. Under a saturated CI (runner queues 60–90 minutes deep
  on 2026-09-10) every candidate evicted `ci_timeout`, and with
  `requeue_evictions` on each re-adoption rebased and launched another full
  pipeline, deepening the queue that caused the timeout. The bound now comes
  from policy (hot-reloaded; zero keeps the 45-minute default) so operators
  can set it above observed CI latency. Runbook: § Serial merge queue.
