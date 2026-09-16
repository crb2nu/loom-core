- Engram tech tree: the Pattern Loom graph had never rendered in production —
  three stacked defects fixed end to end. `agent_engram_graph` now treats
  `root` as optional and returns the full catalog as rich URI-keyed nodes
  (bounded, deterministic) instead of rejecting the HUD's empty-args call;
  the bridge's proof decode accepts the tool's string `proof` field (it
  rejected every proof-bearing catalog, 502ing `/api/engrams` too); and the
  engrams store gives the catalog its own error channel so a fast catalog
  502 is no longer clobbered by the slower summary fetch's success — the
  promise race behind the eternal "loading engram graph…" card.
