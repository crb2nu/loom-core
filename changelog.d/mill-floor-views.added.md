- **Mill-floor Mills views** (`internal/hud/frontend`): the HUD Mills view now
  runs the loom metaphor as a coherent four-view spine — **Warps** (plans
  strung on the beam, bucketed by priority P0–P3), **Shuttles** (active
  pipeline runs in flight across the weave stages), **Sparks** (escalated runs
  needing a human eye, with one-click requeue), and **Bolts** (merged runs as
  woven cloth, exportable as the week's tartan + shift report). All four share a
  token-driven `LineageRibbon` (spine nav + per-entity strand), flow every
  loading/error/empty state through `PanelShell`, and drill into the reused
  `BacklogDetail`/`PipelineRunDetail` drawers. This replaces and retires the two
  generic `Backlog` and `Pipelines` tables (`BacklogPanel.svelte`,
  `PipelinesPanel.svelte` removed); the `#mills/backlog`→Warps and
  `#mills/pipelines`→Shuttles hashes redirect so old bookmarks and cross-links
  keep resolving.
