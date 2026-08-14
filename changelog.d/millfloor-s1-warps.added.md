- **Mill-floor Warps view** (`internal/hud/frontend`): the beam — every plan
  strung and waiting, bucketed into collapsible priority bands P0–P3 (+ an
  `other` catch-all) from the co-fetched backlog. Each band shows a live
  warp-thread motif whose count scales with the work strung on it, a shared
  `DataTable` of items (id, title, plan/pattern-book chip, cost estimate,
  state, age) with one-shot cost previews, and an autonomy-blocked banner when
  the council can't string the beam. Row click opens the shared `BacklogDetail`
  drawer keyed on the `#mills/warps/<id>` hash segment; "Spin a plan" reuses
  the Spinning Room dialog. Loading, error, and empty states all flow through
  `PanelShell` (never co-rendered), with the floor-nav `LineageRibbon` spine as
  the first row. Replaces the retired generic Backlog table's content; the
  `BacklogPanel.svelte` component itself is retained until S6 removes the last
  `panelRegistry` reference.
