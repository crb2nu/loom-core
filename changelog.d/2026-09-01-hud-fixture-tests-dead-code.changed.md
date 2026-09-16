- HUD frontend: the nine manually-run tsx smoke fixtures (agents grouping,
  fleet rows, namespace parsing, session reconcile, chapters, mills system
  health) are now vitest tests that run in CI, and the orphaned
  `ErrorCard.svelte` / `AgentCard.svelte` components are removed.
