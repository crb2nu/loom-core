- **Factory Model junctions reconciled; site guardrail works from worktrees**
  (`docs/FACTORY_MODEL.md`, `scripts/ci/check_flexinfer_site_integration.sh`):
  §4/§5 now record J4 Cloth Hall grading as shipped (S1–S4, 2026-08-13), J1's
  Spinning Room pattern mode as built with the stamp-guided spin still open,
  J5's deployment-aware dependency gate as the one built seam, and J5
  Finishing House as the next junction. The flexinfer-site integration
  guardrail resolves the sibling site checkout relative to the canonical
  loom-core checkout (git common dir), so it verifies the sync mapping from
  linked worktrees instead of silently skipping.
