# 193 — Mills journal-engine wiring: completion program (2026-07-26)

Successor to `.loom/192-review-agent-context-journal-integration-2026-07-25.md`
and `docs/JOURNAL_ENGINE.md`. This closes the remaining journal-engine adoption
work in Mills. Four slices, dispatched to parallel agents; A/B/C are loom-core
MRs, D is a platform/gitops MR.

## Current state (verified 2026-07-26)

Shipped:
- **S0** research→implement notes pipe, default ON (`!1227`).
- **S1** per-backlog-item memory: `pkg/mills/store/dao_item_memory.go`
  (migration 017), `Runner.recordItemMemory`
  (`pkg/mills/pipeline/item_memory.go`), operator render hook
  `itemJournalBlock` (`cmd/loom-mills-operator/main.go:2382`). Gated by
  `LOOM_MILLS_ITEM_JOURNAL`, default OFF.
- Spawn token telemetry decoded (`!1226`): `pkg/mills/clients/spawn.go`
  now reads `token_usage` (incl. cache_read) off the HUD spawn wire.
- `pkg/llmusage` instrumentation + `mills_llm_*` counters (Step 3, DONE).

Dark / open:
- `LOOM_MILLS_ITEM_JOURNAL` is **not set anywhere in platform/gitops** — S1 has
  never run in production. (Verified: grep of gitops yaml finds nothing.)
- Consolidation deliberately unwired in v1 (`dao_item_memory.go:31`): the
  256 KiB hard cap (`ItemMemoryMaxSnapshotBytes`) refuses writes with
  `ErrItemMemoryTooLarge`; when it bites, the item's memory silently stops
  growing. No soft-threshold warning exists, so "the cap is biting" is
  currently unobservable until it's a hard refusal.
- Council is the named second consumer (JOURNAL_ENGINE.md Step 2, point 5)
  and the **only lane where a cache win is measurable**: component
  `mills-council-editor` is llmusage-instrumented and both backends already
  implement prompt caching (Anthropic `cache_control` on the stable system
  block — `pkg/mills/clients/anthropic.go:139`; OpenAI static
  `prompt_cache_key` — `council_openai.go:23`). The stable⇄volatile split
  already exists: `buildCouncilEditorPromptParts`
  (`pkg/mills/clients/council.go:315`).
- No routing key on the research lane: `X-Flexinfer-Cache-Key` precedent
  exists only in `pkg/agentloop` (`metrics.go:12`, `client.go:33`);
  `pkg/mills/clients/flexinfer.go` sets only a global Authorization header.

## Riskiest assumption + kill-test (program-level)

**Assumption:** the council editor's stable prompt half is actually
byte-stable across ticks, so an append-only memory block inserted there
extends a warm prefix rather than sitting behind volatile bytes.

Known threat: `buildCouncilEditorPromptParts` puts `repoTree` (repo-tree
digest, churns per commit) and `patterns` in the stable half. Anything
placed *after* a churned byte is cold every tick.

**Kill-test (must ship inside slice A, not after):** a consumer test that
renders the editor stable half across ≥3 simulated council ticks with a
growing memory and asserts `journalengine.CheckPrefixExtension` over the
sequence — copying the pattern in `pkg/mills/pipeline/item_memory_test.go`.
If the assertion cannot be made to pass with the memory block placed
*before* `repoTree`/`patterns` (i.e. ordering: constant preamble+guardrails →
memory render → repo tree/patterns → volatile brief), slice A downgrades its
claim to durable-memory-only, same as S1, and says so in the MR.

## Slice A — Council durable memory (the measured consumer)

**Owner:** one agent. **Repo:** loom-core. **Flag:** `LOOM_MILLS_COUNCIL_MEMORY`, default OFF.

The council currently recompiles a fresh brief every tick and remembers
nothing of its own past deliberations; `brief.Compile()` truncates sections
from the tail (`council/brief.go:382`). Give the council lane one durable
`journalengine.Journal` so tick N+1 knows what tick N minted and why.

1. **Storage.** New migration `018_council_memory.sql` + a DAO in
   `pkg/mills/store` mirroring `dao_item_memory.go` exactly (same cap, same
   `Err*TooLarge` semantics, same "not called journal" naming rule — migration
   004 / `workflow/journal_dao.go` own that word). Owner key: a fixed lane id
   (`"council"`) — one row.
2. **Record hook.** After each council run persists its outcome (find the
   post-run persistence point in the council orchestration; it must sit after
   the durable write, same discipline as `recordItemMemory` being called after
   `PutStage`). One turn per run: epoch = entry count (never a clock),
   situation = "Council run <ordinal> completed.", outcome = neutral
   third-person composition of: proposals minted (id + title), dedup/gray-band
   verdicts, autonomy-gate outcome, dispositions. Cap 8 KiB per entry,
   truncate from the tail, reuse `truncateTailBytes` (export or copy).
   Best-effort by contract: a memory write never fails a run.
3. **Render.** Into the **stable half** of `buildCouncilEditorPromptParts`,
   positioned after the constant preamble/guardrails and **before**
   `repoTree`/`patterns` (stability-ordered: constant → append-only →
   churning → volatile). Prefaced by a constant label string (pattern:
   `itemJournalPreface`, main.go:2374). Reviewer prompts (`council.go:42`)
   are volatile single-strings — leave them alone in this slice.
4. **Tests.** The kill-test above; plus DAO round-trip, record-hook
   composition, flag-off = zero behavior change (byte-identical editor
   prompt), and cap refusal.
5. **Doc.** Update `docs/JOURNAL_ENGINE.md` Step 2 point 5 from "Not done" to
   shipped-behind-flag; changelog fragment `changelog.d/council-memory.added.md`.

Explicitly out of scope: consolidation (slice B), enabling the flag in
production, reviewer-prompt adoption.

## Slice B — Consolidation seam + cap observability

**Owner:** one agent. **Repo:** loom-core. **Flag:** `LOOM_MILLS_MEMORY_CONSOLIDATE`, default OFF.

The v1 decision ("cap is the cheaper guard") stands until the cap is
observed to bite — but D makes the cap *reachable* for the first time, and
today the only signal is a Warn at the hard 256 KiB refusal. This slice makes
"the cap is biting" observable and pre-wires the response, dark.

1. **Soft-threshold observability (unconditional).** At record time
   (`recordItemMemory`, and slice A's council hook if merged first — do not
   couple; land item-memory-side regardless), when the persisted snapshot
   exceeds a soft threshold (128 KiB = half cap), log one structured Warn
   (`item memory: snapshot over soft threshold`) and increment a counter in
   `pkg/mills/metrics.go` (e.g. `mills_item_memory_soft_threshold_total`).
   This is the "observed to bite" instrument the v1 comment asked for.
2. **Consolidator implementation (dark until flagged).** A
   `journalengine.Consolidator` in `pkg/mills` (new file, suggest
   `pkg/mills/pipeline/memory_consolidator.go` or a small
   `pkg/mills/memory/` package): prompt built from
   `ConsolidationRequest` (`RenderEntries` exists), served through the
   operator's existing instrumented FlexInfer/OpenAI-compatible client with a
   new llmusage component label `mills-memory`. Model: reuse the weaver/judge
   lane config pattern in `cmd/loom-mills-operator/config.go` (new env,
   sensible default = the research/weaver model).
3. **Trigger.** When the flag is ON and a record-time snapshot exceeds the
   soft threshold, run `journalengine.Consolidate(ctx, j, c, keepFraction)`
   (keepFraction ~0.5) *before* Put, then persist the consolidated journal.
   On consolidator error: persist the unconsolidated journal exactly as
   today — `Consolidate` already guarantees the journal is untouched on
   error/empty; preserve that in the wrapper. Never consolidate more than
   once per record call.
4. **Tests.** Soft-threshold warn+counter fire; flag-off = no LLM call ever
   (assert via fake consolidator); consolidation failure loses zero entries
   and still persists the grown journal (unless over hard cap — then today's
   refusal path); post-consolidation render still satisfies
   `CheckPrefixExtension` *from the reset point* (consolidation is the one
   legal cache-reset event — the test should assert the reset happened at
   most once, not that the prefix survived it).
5. **Doc + fragment.** `docs/JOURNAL_ENGINE.md` growth-guard paragraph;
   `changelog.d/memory-consolidation.added.md`.

## Slice C — Research-lane per-item routing key

**Owner:** one agent. **Repo:** loom-core. Small.

JOURNAL_ENGINE.md "Routing": on a multi-replica lane, same-prefix requests
must land on the same replica. The research stage is the one instrumented
lane that carries the item journal render (`researchPromptFor`,
main.go:2441). Today `FlexInferClient` sets no cache key, so two items'
prefixes compete for the same replica cache arbitrarily.

1. Add a per-request header seam to the research completion path in
   `pkg/mills/clients/flexinfer.go` (the client is constructed once; the
   header must be per-call, not `SetHeader`-global).
2. Research calls send `X-Flexinfer-Cache-Key: mills-item:<backlog-id>` —
   reuse the constant from `pkg/agentloop/metrics.go:12` rather than
   restating the literal.
3. While there: the OpenAI-compatible path used by council already has
   `prompt_cache_key` (static); do NOT change council keys in this slice
   (slice A owns that lane's semantics).
4. Tests: header present with the right value on research calls, absent
   where no item id exists; no behavior change otherwise.
5. Fragment `changelog.d/research-cache-routing.added.md`. Doc: one line in
   JOURNAL_ENGINE.md Routing section noting Mills research now sets it.

## Slice D — Production enablement + soak (platform/gitops)

**Owner:** one agent. **Repo:** platform/gitops. **No loom-core code.**

1. Set `LOOM_MILLS_ITEM_JOURNAL: "1"` in the loom-mills-operator
   deployment env (find the operator's Deployment/Kustomize overlay; follow
   the existing env-var pattern there; pin nothing else).
2. MR per gitops conventions, auto-merge, then `flux reconcile` and verify
   the operator pod restarts with the env set (`kubectl -n <ns> get pod` +
   env check via describe; no kubectl edit).
3. Soak checklist (execute what's executable same-session, record the rest
   in the MR description):
   - operator logs: absence of `item memory:` warns (load/persist failures);
   - after ≥1 pipeline run: `backlog_item_memory` row growth (via operator
     logs or HUD, no direct sqlite in pod — see memory note on operator
     recovery gotchas);
   - `mills_llm_*` cached-share on component `mills-weaver` (research lane)
     — a move here is the only client-side cache signal S1 can produce today;
   - spawn `token_usage.cache_read` on implement-stage spawns (!1226 wire)
     — directional only, spawn CLIs cache independently.
4. Do NOT set `LOOM_MILLS_COUNCIL_MEMORY` or `LOOM_MILLS_MEMORY_CONSOLIDATE`
   — those soak later, after A/B merge and this flag proves quiet.

## Coordination rules (all agents)

- Work from a fresh worktree off `origin/main` of the owning repo, under
  `<repo>/.worktrees/<branch>`; never sibling checkouts. Branch names:
  `feat/mills-council-memory`, `feat/mills-memory-consolidation`,
  `feat/mills-research-cache-routing`, gitops: `feat/mills-item-journal-enable`.
- loom-core MRs each need a `changelog.d/<slug>.<category>.md` fragment
  (never edit CHANGELOG.md); slugs must be unique across the three MRs.
- Docs-guardrail gate: the fragment satisfies it.
- Build/test from a worktree: `GOWORK=off` (see memory note); full gate =
  `go build ./... && go test ./pkg/mills/... ./pkg/journalengine/...` minimum,
  full `go test ./...` before push.
- Auto-ship policy applies: commit → push (`-u`) → MR → auto-merge → poll CI →
  cleanup worktree+branches. Re-arm auto-merge after any fix push.
- Migration number: slice A owns 018. Nobody else adds a migration.
- Render markers in `pkg/journalengine` are a shared wire format with the
  Python package — do not touch them.
