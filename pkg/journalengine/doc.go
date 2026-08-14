// Package journalengine is durable, cheap, prefix-cache-friendly memory for
// agents that live longer than their context window.
//
// An agent that must remember a long history has two bad options and one good
// one. It can re-summarize its past every turn — cheap per turn, and it forgets
// everything concrete. It can carry the whole history in the prompt — remembers
// everything, and pays to re-read it every turn. Or it can carry the whole
// history in the prompt *in append-only order*, so the serving engine's prefix
// cache turns "re-read 100k tokens" into "prefill the last 800".
//
// This package is the third option. It is a Go port of the core primitives of
// libs/journal-engine (Python), which was extracted from the psyche-simulation
// memory engine after that engine ran the design in production. Measured on a
// vLLM lane with automatic prefix caching, 2026-07-25:
//
//	engine prefix-cache hit rate, whole run     79.3%
//	warm repeat, share of prompt from cache     99%
//	warm repeat, end-to-end speedup             7-14x
//
// # The contract
//
// Three rules. Break any one and the cache stops paying.
//
//  1. Byte-stable prefix. The system prompt and the journal render are
//     byte-identical between turns. Nothing volatile — no timestamps, no token
//     counts, no retrieved context, no "turn 14 of 30" — may appear above the
//     now-block boundary.
//  2. Everything volatile lives below the boundary. The current task, what
//     other agents just said, retrieved memories, per-turn guidance: all of it
//     renders into the ephemeral tail, which is the only part the platform
//     prefills cold.
//  3. The cache resets only at consolidation. Distilling old epochs rewrites
//     the top of the journal, which is a deliberate, infrequent, budgeted event
//     — not an accident of prompt assembly.
//
// The failure mode is silent and expensive: one volatile byte above the
// boundary drops the hit rate to roughly zero and every turn pays a full cold
// prefill of the entire history. It does not error, it just gets slow. That is
// why CheckPrefixExtension exists and why callers should assert on it in their
// own tests — this package can prove Journal.Render is append-only, but only
// the caller's test can prove its prompt assembly did not smuggle a clock
// reading above the line.
//
// # Consolidation preserves identity, not biography
//
// Summarize a summary enough times and only *register* survives. In the run
// that motivated this design, one vivid opening scene persisted through twelve
// consolidations while every concrete event between it and the present
// dissolved into voice. So a Consolidation carries two things with different
// survival rules: an Identity passage that is re-synthesized every time (this
// part should drift), and Ledger lines that are append-only, so events
// accumulate instead of paraphrasing each other away. See Consolidator.
//
// # Relationship to pkg/agentloop
//
// These are complements, not alternatives, and a caller may well use both.
//
// agentloop is a ReAct tool loop: it keeps an append-only Conversation of
// messages for a single bounded session, and stops cleanly when the budget
// would overflow (its Budget/BudgetError). It has no notion of outliving the
// window.
//
// journalengine is what a long-lived agent needs when the history *will*
// exceed the window: a durable, serializable journal that survives a restart,
// a distillation step that reclaims budget instead of stopping, a bounded core
// memory plus an append-only episodic ledger, and a TokenLedger that calibrates
// itself against reported prompt_tokens rather than assuming chars/4 forever
// (compare agentloop.EstimateTokens, which is a fixed heuristic).
//
// # Status
//
// Wired into Mills in two lanes, both default-OFF behind their own flag, so
// with the flags unset every prompt is byte-identical to its pre-feature form:
//
//   - Per-backlog-item memory, gated by LOOM_MILLS_ITEM_JOURNAL: stored by
//     pkg/mills/store (dao_item_memory.go, migration 017), recorded by
//     pkg/mills/pipeline/item_memory.go, rendered into stage prompts by
//     itemJournalBlock in cmd/loom-mills-operator/main.go.
//   - Council-lane cross-run memory, gated by LOOM_MILLS_COUNCIL_MEMORY:
//     stored by pkg/mills/store (dao_council_memory.go, migration 018),
//     recorded by pkg/mills/runner/memory.go, gated and prefaced by
//     pkg/mills/council/memory.go, rendered into the editor prompt by
//     buildCouncilEditorPromptParts in pkg/mills/clients/council.go.
//
// cmd/mcp-agent-context does not import this package yet. See
// docs/JOURNAL_ENGINE.md for the details of both adoptions and for what
// agent-context adoption would look like.
package journalengine
