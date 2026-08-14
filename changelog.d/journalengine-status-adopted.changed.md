Refreshed the `pkg/journalengine` package doc's `# Status` section, which still
claimed the engine was staged and unwired. It now records the two shipped Mills
adoptions — per-backlog-item memory behind `LOOM_MILLS_ITEM_JOURNAL` and
council-lane cross-run memory behind `LOOM_MILLS_COUNCIL_MEMORY`, both
default-OFF — names the store/record/render site of each, and notes that
`cmd/mcp-agent-context` remains unadopted. Comment-only: no render markers or
behavior changed.
