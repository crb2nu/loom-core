# Journal Engine — Adoption Plan

**Status by step:**

| Step | State |
| --- | --- |
| Step 1 — `mcp-agent-context` sessions | **Deferred, not implementable as written.** See [Step 1 revisited](#step-1-revisited--why-agentcontext-was-not-the-first-adopter). |
| Step 2 — Mills per-item memory | **Shipped behind `LOOM_MILLS_ITEM_JOURNAL`, default OFF.** `pkg/mills/store.ItemMemoryDAO` (migration 017) + `Runner.recordItemMemory` + the operator's prompt render hook. |
| Step 3 — instrumentation | **Shipped.** Every OpenAI-compatible chat client reports `usage.prompt_tokens_details.cached_tokens`. |

`pkg/journalengine` itself is unchanged: its render markers are a wire format
shared byte-for-byte with the Python `libs/journal-engine`, so adoption happens
entirely on the consumer side.

See [Reading the cache data](#reading-the-cache-data) for where the token
telemetry lands, and [Step 1 revisited](#step-1-revisited--why-agentcontext-was-not-the-first-adopter)
for why the first planned adoption site turned out not to fit.

## What it is

`pkg/journalengine` is durable, cheap, prefix-cache-friendly memory for agents
that live longer than their context window. Four primitives:

| Type | Job |
| --- | --- |
| `Journal` | Append-only lived memory plus a bounded distilled core. `Render()` is a strict prefix of the next `Render()`. |
| `TokenLedger` | Chars-per-token budget estimate that corrects itself against reported `prompt_tokens`. |
| `Consolidator` | The interface behind which the distillation LLM call lives (caller-side). |
| `CheckPrefixExtension` | The assertion that keeps the cache contract honest in *consumer* tests. |

It is a Go port of the core primitives of `libs/journal-engine` (Python), which
was extracted from `labs/psyche-simulation`'s memory engine — the same wire
format, byte for byte, so a Go agent and a Python agent can share a warm prefix
on the same lane.

## Why bother

The design was validated in production on 2026-07-25 against a vLLM lane with
automatic prefix caching enabled:

| metric | value |
| --- | --- |
| engine prefix-cache hit rate, whole run | **79.3%** |
| warm repeat, share of prompt served from cache | **99%** |
| warm repeat, end-to-end speedup | **7-14x** |

That is the transferable asset: not the memory model, the **cache contract**.
An agent can carry its entire history in every prompt for roughly the cost of
carrying the last turn, provided the history only ever grows at the tail.

Three rules, from the package doc:

1. **Byte-stable prefix.** System prompt + journal render, byte-identical between
   turns. No timestamps, no token counts, no retrieved context, no "turn 14 of
   30".
2. **Everything volatile below the now-block boundary.** Current task, peer
   output, retrieved memories, per-turn guidance.
3. **The cache resets only at consolidation.** Deliberate, infrequent, budgeted.

The failure mode is silent: one volatile byte above the boundary drops the hit
rate to roughly zero and every turn pays a full cold prefill. Nothing errors, it
just gets slow and expensive. Hence `CheckPrefixExtension` — the package can
prove its own `Render()` is append-only, but only a consumer's test can prove the
consumer's prompt assembly did not smuggle a clock reading above the line.

## Relationship to what already exists

### `pkg/agentloop`

Complementary, not competing, and a caller may use both.

`pkg/agentloop` is a ReAct tool loop. Its `Conversation` is append-only at the
*message* level for a single bounded session, and its `Budget`/`BudgetError` stop
the loop cleanly when the next turn would overflow `maxModelLen`. It has no
notion of outliving the window.

`pkg/journalengine` is what a long-lived agent needs when the history *will*
exceed the window: a durable serializable journal that survives a restart, a
distillation step that reclaims budget instead of stopping, a bounded core memory
plus an append-only episodic ledger, and a token estimate that calibrates itself
(compare `agentloop.EstimateTokens`, a fixed chars/4 heuristic).

A plausible end state: `agentloop` drives the within-session tool loop;
`journalengine` owns what the agent carries *between* sessions.

### `pkg/agentcontext`

`pkg/agentcontext` is the storage and retrieval layer behind
`cmd/mcp-agent-context`: `MemoryHierarchy` with `working` / `short_term` /
`long_term` tiers of `MemoryItem`, `ContextEntry` and `Session` records, hybrid
search, and Qdrant collections for durability
(`pkg/agentcontext/memory_hierarchy_persist.go`).

`journalengine` is not a replacement for any of that. `agentcontext` answers
"what do we know, and how do we find it"; `journalengine` answers "what does this
agent carry in its prompt this turn, and how do we not pay for it twice". The
overlap is precisely one thing — `pkg/agentcontext/compaction_llm.go` already
summarizes to reclaim space — and that is the integration point, not a conflict.

## Adoption path

### Step 1 — `mcp-agent-context` sessions get a journal

> **SUPERSEDED — do not implement as written.** This step was attempted and
> abandoned on four findings: the summarizer seam it depends on is dead code,
> there is no memory renderer to route, `Recall`'s ordering is mutated by
> reading it, and there is no per-session ordinal. Read
> [Step 1 revisited](#step-1-revisited--why-agentcontext-was-not-the-first-adopter)
> before touching any of this. The step is kept here because the reasoning below
> is still the right shape — it is the `agentcontext` data model that does not
> support it yet.

Today a session (`pkg/agentcontext/schema.go: Session`) accumulates
`ContextEntry` records and a caller recalls a *selection* of them per turn. That
is a retrieval model: correct, and it re-reads a freshly-assembled prompt every
time, so the prefix cache never hits.

The journal-backed alternative, for the subset of sessions that are one agent
working continuously:

1. Attach a `journalengine.Journal` per `Session`, keyed by session ID, persisted
   as a `Snapshot` (plain JSON — it fits the existing Qdrant payload path, or
   `pkg/mills/store`'s SQLite if a relational home is preferred).
2. Record each turn with `RecordTurn`. Use
   `journalengine.SortedUtterances(map)` when the caller's inbox is a map — Go
   map iteration is randomized and rendering a map directly would produce a
   different byte string every run, which destroys the invariant. (`RecordTurn`
   takes a slice specifically to make that mistake hard.)
3. ~~Reuse the existing compaction summarizer as a `Consolidator`.~~
   **False as written.** `pkg/agentcontext/memory_hierarchy.go` does carry a
   `summarizer func(content string, maxTokens int) (string, error)`, but the only
   thing that sets it — `MemoryHierarchy.SetSummarizer`
   (`pkg/agentcontext/memory_hierarchy_core.go:106`) — has **zero callers** in
   the repo, production or test, and `CompactionSummarizer` is never constructed
   in production either. Wrapping it in a `ConsolidatorFunc` would be a few lines
   that wrap nothing: the field is always nil and its one reader falls through to
   extractive compression. A real Consolidator here means building the LLM call,
   not adapting an existing one.
4. Keep `agentcontext`'s recall for what it is good at — cross-session and
   cross-agent lookup — and render its hits into the **ephemeral tail**, never
   into the journal.

Even repaired, Step 1 would be **storage-only** in its first increment: owner =
`agent_id` + namespace, compare-and-swap writes on the snapshot, and no
prefix-cache claim at all, because `mcp-agent-context` assembles no prompts —
there is no render path for a stable prefix to be the prefix *of*. The cache
argument only becomes available once something in that server actually builds a
completion request.

What this would buy: a session that survives a restart with its history intact,
and a distillation step that reclaims budget instead of hitting a wall.

What it would cost, once there *is* a prompt to cache: a per-session routing key
so requests with the same deep prefix land on the same replica (see Routing
below).

### Step 2 — Mills-spawned agents get durable memory across stages — **SHIPPED (default off)**

`cmd/loom-mills-operator/main.go` builds each stage's prompt in `stagePromptFor`
(main.go:2301) plus the per-stage wrappers `planSlicePromptFor` (:2393),
`researchPromptFor` (:2418), `implementPromptFor` (:2477) and
`prSelfReviewPromptFor` (:2556), wired into `pipeline.SpawnWorker.PromptFor` from
`buildDispatcher` (:2107). The comment at main.go:2198 anticipated this:
*"Production deployments register richer prompt builders here once spec doc
loaders ship."*

Two things landed. They are independent — the first is unconditional, the second
is gated.

**S0 — research notes reach the implement prompt (default ON, kill switch
`LOOM_MILLS_RESEARCH_NOTES_IN_IMPLEMENT=0`).** The research worker has always
written its output to `Artifacts["research_notes"]`
(`pkg/mills/pipeline/dispatcher.go`), and `jc.Prior` has always carried it — and
until now *nothing read either*. `researchNotesBlock` (main.go:2515) renders it
into the implement prompt under a `## Research findings` header, capped at 8 KiB.

**S1 — a per-backlog-item journal (default OFF, `LOOM_MILLS_ITEM_JOURNAL=1`).**

1. One `Journal` per backlog item, owner = item ID, `Snapshot` persisted in
   `backlog_item_memory` (migration `017_backlog_item_memory.sql`) via
   `store.ItemMemoryDAO` (`pkg/mills/store/dao_item_memory.go`). It is *not*
   called a journal DAO: migration 004 and `pkg/mills/workflow/journal_dao.go`
   already own that word for the exactly-once workflow effect ledger.
2. `Runner.recordItemMemory` (`pkg/mills/pipeline/item_memory.go`) records one
   turn per stage attempt, called from `runStage` immediately after the durable
   `PutStage` (runner.go:1433) so the journal can never claim work the audit
   trail lacks. The epoch is the journal's own entry count — never a clock
   reading. The composed outcome is the verdict, the diff **stat**, the commit
   messages, then the log tail, capped at 8 KiB and truncated from the tail so
   overflow costs log noise rather than the structured summary.
3. `stagePromptFor` renders `journal.Render()` as the leading stable block. The
   full assembly order is: *[research only: repo digest] → journal render →
   stage template + `backlogPromptContext` → disciplines → retry context last*.
   `researchPromptFor` was reordered for this — its invariant repo digest used
   to trail the volatile per-item text, which put a stable block behind a
   volatile one and wasted it.
4. Growth guard, two-stage. A snapshot over `ItemMemoryMaxSnapshotBytes`
   (256 KiB) is still refused with `ErrItemMemoryTooLarge` and the record
   skipped with a warning — that is the hard stop. Below it,
   `Runner.observeItemMemoryGrowth` measures the same bytes `Put` will measure
   and, past a **soft threshold of half the cap (128 KiB)**, logs
   `item memory: snapshot over soft threshold` and increments
   `mills_item_memory_soft_threshold_total`. This runs unconditionally: v1
   deliberately left consolidation unwired on the grounds that the cap was the
   cheaper guard *until it was observed to bite*, and until the soft threshold
   existed "biting" was invisible until it was already a refusal, at which
   point the item's memory had silently stopped growing.

   The response is pre-wired but dark. With `LOOM_MILLS_MEMORY_CONSOLIDATE` set,
   a record that crosses the soft threshold runs
   `journalengine.Consolidate(ctx, j, c, 0.5)` **before** `Put`, at most once
   per record call, distilling the oldest half of the entries into the identity
   passage plus append-only ledger lines. The `Consolidator`
   (`pkg/mills/pipeline/memory_consolidator.go`) dials the operator's
   instrumented OpenAI-compatible client under llmusage component
   `mills-memory`, on the model in `FLEXINFER_MEMORY_MODEL` (default: the
   resolved weaver/research model). Outcomes are counted by
   `mills_item_memory_consolidations_total{outcome=ok|noop|error}`. The call is
   bounded at 90s — it runs synchronously on the stage-completion path, and
   stalling a stage that already succeeded is the same harm as failing it, just
   slower. On any consolidator error — including an empty envelope —
   `Consolidate` leaves the journal completely untouched and the wrapper
   persists the grown, unconsolidated snapshot exactly as it would have without
   the feature; the hard cap remains the only thing that can refuse a write.
   Consolidation is the one legal prefix-cache reset, so it is deliberately the
   *rarest* event in the design:
   `pkg/mills/pipeline/memory_consolidator_test.go` asserts the reset happens at
   most once across a growth sequence and that renders on either side of it are
   each strict prefix extensions.
5. **Shipped behind a flag: the council lane is the second consumer** (default
   OFF, `LOOM_MILLS_COUNCIL_MEMORY=1`). `pkg/mills/council/brief.go`'s
   `Compile()` still truncates deliberately from the tail under `MaxBytes`, so
   the council remembered nothing of its own past deliberations between ticks.
   It now carries one durable `Journal` for the whole lane, owner = the fixed
   lane id `council`, `Snapshot` persisted in `council_memory` (migration
   `018_council_memory.sql`) via `store.CouncilMemoryDAO`
   (`pkg/mills/store/dao_council_memory.go`) — same 256 KiB cap and same
   refusal-not-truncation semantics as the item memory above, and named for the
   same reason it is not called a journal DAO.

   `Runner.recordCouncilMemory` (`pkg/mills/runner/memory.go`) records one turn
   per non-dryrun run, called from `Execute` after the artifacts are on disk,
   the verdict is persisted and the mutator has applied (or deliberately
   skipped) the backlog deltas — the council-side equivalent of recording after
   `PutStage`. Epoch is the journal's entry count and the displayed run ordinal
   is its turn count; neither is a clock reading, and the run id is deliberately
   absent because it is volatile. The composed outcome is the minted item
   ids + titles, plan-lane routing, dedup / gray-band refusals with their
   scores, the quality gate's verdict, then the mutator's disposition, capped at
   8 KiB and truncated from the tail so overflow costs the trailing sections
   rather than the mints. Dryruns record nothing.

   `buildCouncilEditorPromptParts` (`pkg/mills/clients/council.go`) renders it
   into the **stable** half, ordered constant preamble + guardrails → memory →
   repo-tree digest + pattern catalog → volatile brief, so it rides the
   Anthropic backend's `cache_control`'d system block and the OpenAI backend's
   `prompt_cache_key`'d static prefix. With the flag off the prompt is
   byte-identical to the pre-feature prompt.

   **What the kill-test measured.** `pkg/mills/clients/council_memory_prompt_test.go`
   renders the stable half across four simulated ticks with a growing memory and
   asserts `CheckPrefixExtension`; `pkg/mills/runner/memory_test.go` makes the
   producer-side assertion across three real runs. The memory block *and
   everything above it* is a strict prefix extension across ticks, and it stays
   inside the common prefix across a repo-tree change — which is the whole point
   of putting it above the digest. It is **not** true that the entire stable
   half is one prefix extension: the memory grows in the middle, so the small
   fixed-size repo digest and pattern catalog below the append point re-prefill
   every tick. That is the deliberate trade. Placed below the digest instead,
   the whole stable half would match within one commit and go fully cold on the
   next — and with Mills' council cadence, a commit between two ticks is the
   normal case, while the memory is the only block that grows without bound.

   Consolidation remains unwired on **this** lane; the cap is the council's v1
   guard. The consolidation seam in point 4 is deliberately item-memory-only —
   the council journal is one row for the whole lane rather than one per item,
   so it grows on a different clock and earns its own decision.

**Honest scope note.** Four of the five journal-carrying stages are
**unmeasurable today.** `plan_slice`, `implement` and `pr_self_review` go to
`claude`/`codex` CLIs through the HUD spawn API — `pkg/mills/clients/spawn.go`
contains no token accounting of any kind, no `llmusage` call, and no
cache-routing header seam, so none of the `mills_llm_*_prompt_tokens_total`
counters ever see that traffic. Only `research` runs through an instrumented
client (`clients.FlexInferClient`, component `mills-weaver`), which is the one
place the digest-above-journal reorder could show up as a `cached_share` move.

So Step 2 is justified as **durable cross-stage memory**, not on a measured cache
win: the implement stage knows what research found without being re-told, and a
retry does not start from amnesia. Both of those are worth having at any cache
hit rate, including zero. Keeping the byte-stable assembly order costs nothing
and is the difference between a cache that can be switched on once the spawn
path grows a seam and one that cannot. Adding that seam — per-spawn prompt-token
reporting plus a per-item routing key — is the prerequisite for judging this the
way Step 3 intended.

### Step 3 — Instrument, then judge — **DONE**

Neither step above is worth shipping on the strength of a doc. The measurement is
cheap and unambiguous *where a client reports it*: the proxy returns
`cached_tokens` per completion, so a before/after on one long-running session
settles it. Adopt where the hit rate actually materializes; leave the retrieval
model alone where it does not.

This is now in place. `pkg/llmusage` normalizes the two usage dialects and emits
one log line per completion, plus Prometheus counters in the two components that
already had a registry. Nothing reads the data back — no behavior changed.

**Caveat this step did not anticipate:** instrumentation covers the
OpenAI-compatible and Anthropic Messages clients, which is *not* the same set as
"the code paths a journal would help". Mills' spawn-backed stages, the ones Step
2 actually shipped against, are outside it entirely (see the scope note above).
Step 2 was therefore adopted on the durable-memory argument alone.

## Reading the cache data

### Log query

Every instrumented client emits one debug line per completion under the literal
message `llm usage` (the constant `llmusage.MessageUsage`). In Loki:

```logql
{namespace="loom-hub"} |= "llm usage" | json
  | line_format "{{.llm_component}} {{.llm_model}} p={{.prompt_tokens}} cached={{.cached_tokens}} share={{.cached_share}}"
```

Fields (all defined as constants in `pkg/llmusage/usage.go`, so grep there
rather than trusting this list):

| field | meaning |
| --- | --- |
| `llm_component` | which caller — `mills-judge`, `mills-weaver`, `mills-council-editor`, `mills-council-reviewer`, `mills-eval-judge`, `flexinfer`, `weaver`, `openai-responses`, `morph-fast-apply` |
| `llm_model` | the model the engine reports having **served**, which a fallback chain or gateway alias can make different from the one requested |
| `prompt_tokens` | whole prompt this turn, cached part included |
| `cached_tokens` | the part served from the engine's prefix cache. **Absent, not zero, when the engine does not report it** |
| `cached_share` | `cached_tokens / prompt_tokens`, 4dp. Absent when either input is unknown |
| `completion_tokens` | generated length |

One semantics caveat: when `mills-council-editor` / `mills-judge` traffic is
served by the **Anthropic Messages backend**, `cached_tokens` is Anthropic's
`cache_read_input_tokens` and `prompt_tokens` is its `input_tokens`, which
*excludes* the cached part — so `cached_share` exceeds 1 on a warm prefix
(that's the warm signature, not a parsing bug), and cache *writes*
(`cache_creation_input_tokens`) are not in these metrics at all — they land in
the editor's sidecar notes (see `anthropicLLMUsage` in
`pkg/mills/clients/anthropic.go`).

These lines are at **debug** level, so nothing is emitted until debug is on.
That is deliberate: one line per completion is too much for info on a busy lane.

| component | enable with |
| --- | --- |
| `loom-mills-operator` (judge, weaver, council) | `LOOM_MILLS_DEBUG=1` (`cmd/loom-mills-operator/config.go`) |
| MCP servers (`mcp-morph-fast-apply`, `mcp-weaver`) | `MCP_DEBUG=1` (`pkg/mcplog.NewDefault`) |
| HUD coordinator | inherits the HUD's `slog` level |

Mills pods already emit JSON to stderr (`newLogger`), which is the shape Loki
expects, so the query above works as soon as the env var is set — no log-format
change needed. The **metrics** path needs no debug flag; only the log lines do.

### Metrics

Only two components already exported Prometheus metrics, so only those two got
counters. No metrics stack was introduced anywhere it did not already exist.

**Mills** (`pkg/mills/metrics.go`, default registry, scraped from the operator's
`/metrics`):

```promql
# warm share by component, last hour
  sum by (component) (rate(mills_llm_cached_prompt_tokens_total[1h]))
/ sum by (component) (rate(mills_llm_prompt_tokens_total[1h]))
```

Also `mills_llm_completion_tokens_total`, carried because a cached-share trend
is uninterpretable if completion lengths moved at the same time.

**HUD coordinator** (`internal/hud/coordinator/metrics.go`, its own private
registry):

```promql
  sum(rate(loom_coordinator_llm_cached_prompt_tokens_total[1h]))
/ sum(rate(loom_coordinator_llm_prompt_tokens_total[1h]))
```

**Weaver** has `loom_weaver_tokens_total{domain,direction="cached_prompt"}`, a
subset of `direction="prompt"` — divide, don't sum. But note: both production
wirings call `weaver.NewMetrics(nil)` (`cmd/mcp-weaver/main.go`,
`internal/daemon/weaver_embed.go`), so weaver's vecs are **not registered with
any registry today**. For weaver, use the log line.

`pkg/agentloop` reports per-turn rather than cumulatively: `TurnMetrics.CachedTokens`
/ `PrefixHitRatio` / `PrefixCacheHitRate` come back in the `agent_loop_run` tool
result. It now prefers the body's `prompt_tokens_details` over the proxy's
`X-Flexinfer-Cached-Tokens` header, so a lane reached without the FlexInfer proxy
in front reports cached tokens where it previously reported none.

### What a healthy warm share looks like

Calibrate against the psyche-simulation numbers in the table above, but do not
expect them: they were measured on an agent deliberately built to the cache
contract, re-sending a byte-stable journal every turn.

| reading | interpretation |
| --- | --- |
| `cached_share` ≥ 0.9 on repeat turns | the contract is working. This is what psyche's warm repeats did (0.99), and it is only reachable when the prefix is byte-stable |
| 0.5–0.9 whole-run average | a real prefix exists and is partially reused. psyche's whole-run figure was 0.793 — mixed cold-start and warm turns |
| 0.1–0.5 | there is a shared prefix but something volatile sits early in the prompt, truncating the match. Look for a timestamp, a token count, a retry counter, or a map rendered without sorting |
| ~0 with `cached_tokens` **present** | genuinely cold. Either every prompt is unique (expected for one-shot calls like `morph-fast-apply` on different files) or a volatile byte sits at the very top |
| `cached_tokens` **absent** | **unmeasured, not cold.** Older vLLM builds omit `prompt_tokens_details` entirely. Do not report this as a 0% hit rate — go read the engine's own `vllm:prefix_cache_hit_rate`, or set `X-Flexinfer-Want-Prefix-Hit: 1` and read `X-Flexinfer-Prefix-Cache-Hit-Rate` off the response (the mechanism `pkg/agentloop` already uses) |

The absent-vs-zero distinction is enforced in code, not just documented:
`llmusage.Usage.CachedShare()` returns `-1` for unknown, the log fields are
omitted rather than zeroed, and every sink zero-guards so an unmeasured lane
cannot create an all-zero series that a dashboard would read as a real 0%.

**Interpret one component at a time.** A single aggregate across all of mills
would average a judge re-sending a rubric against a council editor re-sending a
diff — they have no reason to behave alike, which is exactly why the component
label exists.

## Step 1 revisited — why `agentcontext` was not the first adopter

Step 1 above proposes attaching a `Journal` to `mcp-agent-context` sessions and
reusing the hierarchy's summarizer as a `Consolidator`. That was attempted and
**abandoned deliberately**, before any code was written, on four findings. Step 1
as written above is not implementable; treat this section as its correction.

**1. The summarizer seam is dead code.** `MemoryHierarchy.summarizer`
(`pkg/agentcontext/memory_hierarchy.go:29`) is only ever set by
`SetSummarizer` (`memory_hierarchy_core.go:106`), which **has zero callers** in
the repo — production or test. The only live `SetSummarizer` calls are on the
unrelated `CompactionScheduler`. Its one reader,
`CompressItem` (`memory_hierarchy_promotion.go:102`), always sees nil and falls
through to extractive compression. Wrapping it in a `ConsolidatorFunc` would have
wrapped nothing. The parallel `CompactionSummarizer` / `Mode: "llm"` path is
likewise unreachable: `DefaultCompactionConfig()` hardcodes
`Mode: "extractive"` and no env var overrides it.

**2. There is nothing to route.** No function in `pkg/agentcontext` renders a
session's memory into a prompt string. The MCP surface returns `mcp.JSONResult`
throughout — zero `TextResult` uses in the package. The closest thing,
`ContextSvc.GenerateSummary` (`svc_context_summary.go:89`), builds a blob and
stores it as an entry without returning it. So "route the rendering through a
Journal" would have meant *inventing* the renderer, then inventing its consumer.
A byte-stable prefix with no caller is not a win; it is unverifiable surface with
a passing test attached.

**3. The data model actively contradicts the contract.** Not merely a poor fit —
the specific invariant `journalengine` requires is violated by design in four
places:

- `MemoryHierarchy.Recall` sorts by `ImportanceScore`, tie-breaking on
  `LastAccessedAt` (`memory_hierarchy_recall.go:46-51`).
- `GetItem` **mutates** `LastAccessedAt` and `AccessCount` on every read
  (`memory_hierarchy_core.go:263-264`). Reading memory reorders memory.
- `recallPriorityScore` multiplies by a `time.Since(entry.Timestamp)` recency
  boost (`service_recall.go:257-264`), so the ordering changes as a session ages
  even with no writes.
- Compaction rewrites `Content` → `Summary` in place
  (`memory_hierarchy_promotion.go:118-131`).

Recall is also selective — top-k, importance-ranked, budget-truncated — not
history in order. A render over it would produce different bytes on successive
calls **with identical inputs**, which is the precise failure `CheckPrefixExtension`
exists to catch. The assertion would fail, and it would be right to.

**4. No ordering primitive.** There is no turn, epoch, or sequence concept
anywhere in `pkg/agentcontext`. `MemoryHierarchy` is three
`map[string]*MemoryItem` tiers plus set-valued indexes; `bySession` gives
membership, not order. The only ordering key is wall-clock
`ContextEntry.Timestamp`, and the delta cursor pages on strict `>` of `UnixNano`
(`svc_context_delta.go:118`), so same-nanosecond entries can be dropped.
`journalengine` supplies exactly what is missing here (`Entry`, epochs,
`RecordTurn`, `SortedUtterances`) — which is the point: the gap is real, but
closing it is a data-model change to `agentcontext`, not a wrapper around it.

**What would make Step 1 viable**, in dependency order:

1. Give session memory an explicit monotonic ordinal per session, independent of
   wall-clock time.
2. Add a chronological, non-scoring read path — recall-in-order — separate from
   the importance-ranked one. The ranked path should stay exactly as it is; it is
   good at what it does.
3. Move access accounting (`LastAccessedAt`, `AccessCount`) off the items that
   feed rendering, or stop sorting on it.
4. Then, and only then, put a renderer over the chronological path and assert
   `CheckPrefixExtension` across appends.

**Mills (Step 2) was the better first adopter, and it shipped.** A backlog item
already has a durable ID, stages already run in a defined order, `pkg/mills/store`
already persists per-item state through SQLite migrations, and
`council/brief.go`'s `Compile()` already truncates deliberately from the tail
under `MaxBytes` — the same instinct the journal formalizes. None of the four
blockers above applies.

What did *not* survive contact: the plan to settle the question with the
`mills_llm_cached_prompt_tokens_total` ratio. That counter is fed by the
OpenAI-compatible and Anthropic clients, and the stages the journal serves run
through the HUD spawn API instead, which reports no tokens at all. Step 2 was
adopted on durable memory rather than a measured cache win.

## Rules a consumer must not break

**Nothing volatile above the boundary.** Retrieval hits, timestamps, budget
readings, retry counters, "attempt 2 of 3" — all of it goes in the ephemeral
tail. Assert it:

```go
func TestMyAgentPromptStaysCacheable(t *testing.T) {
    prefixes := []string{}
    for i := 0; i < 5; i++ {
        prefixes = append(prefixes, systemPrompt+"\n\n"+j.Render())
        runOneTurn(t, j)
    }
    for i := 1; i < len(prefixes); i++ {
        if err := journalengine.CheckPrefixExtension(prefixes[i-1], prefixes[i]); err != nil {
            t.Fatalf("prefix cache contract broken: %v", err)
        }
    }
}
```

**Render markers are a wire format.** `CoreHeader`, `LedgerHeader`,
`LivedHeader`, `EmptyJournal` and the `"You said: "` / `"X said: "` /
`"The world: "` line forms are exported constants. Changing one shifts the first
byte of every render and invalidates every warm prefix on the platform at once —
a fleet-wide cold prefill. They are also byte-identical to the Python package's
constants; changing one here forks the two.

**Routing.** On a multi-replica lane, give each journal owner a distinct routing
key (e.g. `X-Flexinfer-Cache-Key: session:<id>`) so requests with the same deep
prefix land on the same replica and one agent's short prefix never evicts
another's deep one. `journalengine` performs no I/O and does not set headers;
that is the caller's job. Mills does this on the research lane: every
`WeaverClient.Research` call sets `X-Flexinfer-Cache-Key: mills-item:<backlog-id>`
per request (`pkg/mills/clients/flexinfer.go`, reusing `agentloop.HeaderCacheKey`),
so each item's journal-led prefix keeps its own replica; a call with no backlog
id sends no header.

**A failed consolidation must not lose history.** `Consolidate` already
guarantees this — on any error, and on an empty result, the journal is left
completely untouched and the error is returned. Running one turn over budget is
recoverable; silently discarding an agent's past is not. Preserve that property
in any wrapper.

## References

- `pkg/journalengine/doc.go` — the design contract in full
- `pkg/journalengine/consolidator.go` — why identity and biography have different
  survival rules
- `pkg/mills/store/dao_item_memory.go` + `migrations/017_backlog_item_memory.sql`
  — the shipped Step 2 storage
- `pkg/mills/pipeline/item_memory.go` — the record hook, the env knob, and the
  outcome composition rules
- `pkg/mills/pipeline/item_memory_test.go` — the consumer-side
  `CheckPrefixExtension` assertion, which is the test every future adopter must
  copy
- `libs/journal-engine` (Python) — the fuller engine, including the vector
  archive layer and its neutral-key rule, with the retrieval kill-test that
  established it
- `labs/psyche-simulation` — where the production numbers were measured
- `pkg/agentloop/doc.go` — the sibling prefix-cache engine, and its own live
  validation note
