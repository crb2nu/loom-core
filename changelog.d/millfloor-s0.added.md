- **Mill-floor views scaffolding** (`internal/hud/frontend`): laid the foundation
  for the Warps ▸ Shuttles ▸ Sparks ▸ Bolts spine that replaces the generic
  Mills **Backlog** and **Pipelines** tabs. Adds the four sub-view routes (with
  `#mills/backlog`→Warps and `#mills/pipelines`→Shuttles redirects for old
  hashes and cross-links), lazy panel-registry entries, `[]`-safe mills store
  getters (`backlogByPriority`, `escalatedRuns`, `boltRuns`, `archiveRuns`,
  `lineageFor`, `millFloorSpine`), and the shared token-driven `LineageRibbon`
  component (spine + strand modes) backed by pure, unit-tested `lineage.ts`
  helpers. The retired Backlog/Pipelines panels stay functional until their
  view slices land.
