- `pkg/journalengine`: long-context agent memory with a prefix-cache prompt
  contract — append-only `Journal` whose `Render()` is a strict prefix of the
  next render, a self-calibrating `TokenLedger`, a `Consolidator` interface that
  keeps the distillation LLM call caller-side, and `CheckPrefixExtension` for
  asserting the contract in consumer tests. A Go port of the core primitives of
  `libs/journal-engine`, byte-compatible with its render markers. Staged only:
  nothing imports it yet, so there is no behavior change. See
  `docs/JOURNAL_ENGINE.md` for the `mcp-agent-context` and Mills adoption plan.
