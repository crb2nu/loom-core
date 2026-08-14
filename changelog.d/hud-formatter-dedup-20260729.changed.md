- HUD frontend: finished the formatter dedup fenced off by the coherence sweep.
  `AgentCard` and `AgentTopology` no longer carry private `AGENT_COLORS` +
  `agentColor()` copies — both import the shared `agentColor()` from
  `utils/format.ts`. The Overview panel's local `agoText` delegates to
  `relativeTime()`, so a `lastRefreshed` older than a day reads `3d ago`
  instead of capping at `72h ago`. The dead `--status-*` custom-property tier
  in `theme.css` is gone now that its last consumer moved to `statusColor()`;
  status→color resolves through `STATUS_VARIANTS` alone. `freshnessLabel()`
  keeps its compound `2h 5m ago` form for the andon wallboard, now documented
  as deliberate rather than accidental duplication.
