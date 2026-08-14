- **Mill-floor Sparks view** (`internal/hud/frontend`): the escalation view of
  the Warps ▸ Shuttles ▸ Sparks ▸ Bolts spine. Lists active + recent escalated
  runs (`millsStore.escalatedRuns`, unioning the live run poll with the terminal
  archive via `fetchArchiveRuns` — never `pipelineHistory`) with warp-bucket,
  escalation-class, and retryable chips, plus escalation KPIs. Each row's *why*
  (its failing gate names + reasons) fills in asynchronously from bounded
  per-run gate fetches without ever flipping the panel to loading, and every row
  links to its run drawer and carries a copy-able `!<MRIID>` chip. One-click
  requeue reflects the full `RequeueOutcome` distinctly: a 409 ghost-spark
  (already merged/done) and a 403 policy/token refusal render as their own inline
  states, never a generic failure. Empty floor shows a green "no sparks — every
  pick landed clean" state.
