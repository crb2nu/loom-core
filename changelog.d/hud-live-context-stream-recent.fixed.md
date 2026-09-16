- **HUD / agent-context**: the Operator Deck's live context stream (and
  `/api/stream`) showed months-old entries and never a new one. The HUD asked
  `agent_context_search` for `"since:<ts>"` as *query text*, which a vector or
  keyword search ranks by similarity to the word "since". `agent_context_search`
  gains `sort=recent` (+ optional RFC3339 `since`): a timestamp-ordered Qdrant
  scroll backed by a new datetime payload index on `context.timestamp`, with an
  in-process fallback (flagged `degraded`) for collections that predate the
  index. The HUD bridge uses it, and the stream store merges entries by parsed
  timestamp so mixed `-04:00`/`Z` offsets no longer misorder the list.
- **HUD Operator Deck**: the Sparks chip counts backlog items that are still
  escalated (`millsStore.openSparks`: one run per item, current state
  escalated/paused) instead of the all-time escalated-run history, which read
  "110" while 11 items were open. The Sparks view keeps the full history.
- **HUD Operator Deck**: the MRs chip counts merge requests that need
  attention (failed/flaky CI, conflict, auto-merge unarmed, skipped pipeline,
  stale branch, or a red head pipeline) instead of every non-`ok` state, so
  MRs that are simply in CI no longer read as "unhealthy".
- **HUD spawns**: a spawned agent's presence heartbeat now carries a one-line
  task label (first summary line of the task description, headings skipped,
  140 chars) instead of the whole prompt, so Deck/fleet agent rows no longer
  show "WHAT THIS PIPELINE HAS ALREADY DONE FOR THIS ITEM (…)" as the
  subtitle; with no usable line the row falls back to "on <branch>".
