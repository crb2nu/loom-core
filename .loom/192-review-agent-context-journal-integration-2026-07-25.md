# Review: agent-context MCP × journal engine × Mills — 2026-07-25

**Scope:** `cmd/mcp-agent-context` + `pkg/agentcontext`, integration with the newly
merged `pkg/journalengine` (!1217), and fitness for the current multi-agent
framework and Mills. Four parallel code sweeps plus live-store inspection.

**Verdict in one line:** the agent-context MCP is over-built as a *surface*
(93 tools) and under-used as a *memory system* — its plumbing (sessions,
presence, plans, worktrees) is genuinely wired, but the memory loop is empty in
production, Mills bypasses it for everything except plans, and
`docs/JOURNAL_ENGINE.md`'s Step 1 targets the wrong host. The right first
adopter for the journal engine is Mills, scoped as **durable inter-stage
memory**, not prefix-cache savings.

---

## A. Journal engine integration status

- `pkg/journalengine` has **zero importers** — confirmed by grep; only
  `docs/JOURNAL_ENGINE.md`, `AGENTS.md:499-525`, and the changelog fragment
  reference it. This matches its self-declared "staged, not wired" status, so
  it is not a defect — but two of the doc's three adoption steps don't survive
  contact with the code.

### Step 1 (sessions get a journal) is misdirected as written

1. **`mcp-agent-context` never assembles a prompt and never calls a completion
   API.** It is a Qdrant-backed store/retrieve server; the vendor CLI owns
   prompt assembly. No component in this path can honor the byte-stable-prefix
   contract, so the warm-prefix value proposition doesn't exist here.
2. **The summarizer the doc plans to wrap is dead wiring.**
   `MemoryHierarchy.summarizer` (`pkg/agentcontext/memory_hierarchy.go:29`) has
   the quoted signature, but `SetSummarizer`
   (`memory_hierarchy_core.go:106`) has **zero callers** — production or test.
   The second seam, `CompactionSummarizer` (`compaction_llm.go:25`), is never
   constructed outside tests and defaults to `"extractive"` mode
   (`compaction_scheduler.go:65`). Session summaries are pure string
   concatenation (`svc_context_summary.go:74-89`) — no LLM anywhere in the
   path. Wrapping "the existing summarizer" as a `Consolidator` is not a few
   lines; it's a new prompt + two-part output parser + a completion client the
   package doesn't have (~300-500 LOC).
3. **Session IDs are the wrong journal owner.** IDs are minted from
   `time.Now()` (`svc_sessions_start.go:113`); crash recovery and clean
   restarts *end the old session and mint a new ID*
   (`svc_sessions_start.go:66-84`), so a session-keyed journal starts empty
   after exactly the event it's supposed to survive. Owner should be
   `agent_id + namespace`.
4. **Multi-writer hazard.** The server is a stdio binary launched per vendor
   client against shared Qdrant. `svc_sessions.go:117-125` already documents
   the partial-merge trick needed for concurrent stat increments; an opaque
   `Snapshot` blob cannot be partially merged → last-write-wins silently
   discards history (the exact failure `JOURNAL_ENGINE.md:198-202` forbids).
   Any agentcontext-hosted journal needs CAS/versioned writes. Also: the
   session reaper (`svc_sessions_reaper.go:66-95`) would hard-delete a journal
   stored in the session payload.

### Step 2 (Mills) is right — but split the memory claim from the cache claim

- The memory half is cheap, additive, and lands where the biggest real gap is
  (see §C). ~1 day of work.
- The cache half **does not transfer** to `plan_slice` / `implement` /
  `pr_self_review`: those stages ship one opaque string to a fresh CLI pod via
  the HUD spawn API (`pkg/mills/clients/spawn.go:154`,
  `internal/hud/spawn.go:1815`) — no system-block seam, no
  `cache_control`/`prompt_cache_key`, no routing header, and
  `hudSpawnTelemetry` (`spawn.go:196-209`) carries **no token counts at all**,
  so Step 3's "proxy reports `cached_tokens`, before/after settles it" is not
  executable on the spawn path today. The one stage on the measured vLLM lane
  is `research` (WeaverClient → FlexInfer), but it runs once per pipeline and
  `FlexInferClient` doesn't parse `cached_tokens` (`flexinfer.go:311-330`).
- A journal prefix on spawn prompts is therefore **pure added input tokens**
  billed against `MaxCostUSD` with a quality upside — worth doing, but gate it
  behind an env knob and justify it as durable memory.

### Doc drift to fix in `docs/JOURNAL_ENGINE.md`

- Step 1's "wrapping it in `journalengine.ConsolidatorFunc` is a few lines" —
  false (nil summarizer, wrong shape, no client).
- Line anchors are stale: `stagePromptFor` is at `main.go:2165` (not ~2145);
  the "richer prompt builders" comment at `main.go:2064-2066` (not ~2044).
- Recommend explicitly reordering the adoption path: Step 2 first, Step 1
  demoted to "storage-only, CAS-guarded, owner = agent+namespace, no cache
  claim".

---

## B. The memory loop is broken in production (live-store evidence)

Queried the live server during this review:

- **15/15 most-recent sessions have `entry_count: 0, total_tokens: 0`** —
  including the Mills operator session and spawn-codex sessions in
  `loom-mills`. Sessions marked `summarized` also have 0 entries; one was
  started 12:28:20 and "summarized" at 12:28:21 (summarizing nothing).
- Namespace pathologies visible in the roster: `/main` (empty repo prefix),
  auto-minted `agents/<agent-id>` fallbacks, and truncated descriptions
  ("Claude Code ·").
- Tasks accumulate as `pending` forever (9/9 pending, some from finished
  sessions) — created by task-sync, never resolved.
- Patterns (4, all Mills Pattern Loom) and plans are the healthy subsystems.
  Engrams exist but are mostly `unverified`/`stale` tier-1 entries.

Root cause is structural, not a bug: **the lifecycle hooks only do ceremony**
(`session-start`, `keepalive`, `session-end` —
`pkg/generator/configs_hooks.go:113-129`), and almost nothing calls
`agent_context_add`. The only production writers of context entries are HUD
spawn telemetry (`internal/hud/spawn.go:2017,2935,3355`), the coordinator
summarizer, mrwatch pipeline events, and `work-handoff`. Interactive agents
(the primary "users") write nothing unless they volunteer, and Mills writes
nothing at all (§C). So `--auto-recall` on session start recalls from an
essentially empty store — the flagship loop (record → recall) runs on ceremony
in, ceremony out.

---

## C. Mills integration: deep on plans, absent on memory

What Mills actually calls (complete list, from `pkg/mills/clients/*.go` +
operator main): `agent_session_start/list/end` (operator lifecycle + hub health
probe), `agent_plan_{create,get,list,update,lifecycle_advance,slice_*}` (fully
wired, canonical plan store — Mills SQLite holds only a FK,
`migrations/005_backlog_plan_id.sql`), `agent_worktree_{allocate,list,release}`,
`agent_pattern_list` (read-only), `agent_handoff_create` (write-only).

Ranked gaps:

1. **Zero inter-stage semantic memory.** No prompt builder reads `jc.Prior`
   (`pkg/mills/pipeline/dispatcher.go:35`; consumers are only MR/CI plumbing
   at `dispatcher.go:1527,1537,1625` and gates at `runner.go:1926-1939`). Most
   starkly: **`research_notes` is computed, grounded, persisted
   (`dispatcher.go:883`) and never read by anything.** The implement agent has
   never seen what research found. The only cross-stage channel is the plan
   store fetched by the worker itself, plus the 4-field
   `implementRetryDiscipline` (`main.go:2340-2354`).
2. **No session continuity across stages.** `ClaimPipelineStart`'s only caller
   (`pkg/mills/reconciler.go:1553`) never sets `ParentSessionID`, though the
   field is threaded all the way to `LOOM_PARENT_SESSION_ID`
   (`dispatcher.go:143-144`). Each stage spawn gets a fresh agent id
   (`internal/hud/spawn.go:1025`) and fresh session → three unrelated sessions
   per pipeline run, unrenderable as a tree in the HUD.
3. **Operator records nothing.** Council debates, gate verdicts, escalation
   reasons, merge audits all land only in Mills SQLite. `agent_context_add` is
   never called → operator handoffs ship `entry_count: 0` (documented verbatim
   in `pkg/mills/clients/handoff.go:118-124`).
4. **Mills handoffs are write-only.** `AgentBridge.HandoffList`
   (`internal/hud/bridge/agent_ops.go:252-256`) enumerates inboxes by
   iterating presence, and the `mills-merges` / `human-on-call` targets never
   register presence → invisible in the HUD; nothing in Go reads them back.
5. **`agent_plan_slice_add` is prompted but off-profile.** The operator prompt
   instructs workers to call it (`main.go:2201`) but it's absent from
   `llm-core` (`cmd/loom/proxy_tool_filter.go:259-267`) — a silent no-op for
   profile-limited codex/antigravity workers. (Spawn pods on the WS proxy see
   all 93 tools unfiltered — `cmd/loom/proxy_wsbackend.go` applies no filter —
   so the bug bites only laptop/profile paths, inconsistently.)
6. Mills uses none of the coordination fabric: no presence, no file claims
   (it has its own SQLite scope-overlap serialization), no messages. Fine as a
   design choice, but the HUD "fleet" view consequently can't see Mills
   workers' claims/conflicts.

---

## D. Surface bloat and drift in the MCP itself

- **93 tools / 16 domains** registered (`cmd/mcp-agent-context/tools.go:17`).
  38 have zero Go callers; **26 are truly dead** (no Go caller AND not in the
  llm-core profile): both annotation tools, all 3 deprecated recall shims,
  `agent_context_summarize`, all 3 recipe aliases (thin forwarders to engrams,
  `svc_recipes.go:53-66`), `agent_task_dispatch`, `agent_workflow_start`,
  parts of graph/memory/worktree surface, etc.
- **5 tools carry `[Deprecated]` in their descriptions but are still
  registered**, consuming context budget on every client.
- **registry.yaml allowlist drift is bidirectional**: 2 stale entries
  (`agent_template_create/list`, removed by SIMP-7,
  `mcp/context/registry.yaml:716-717`) and **34 registered tools missing** —
  including the *entire* Plans/Patterns/Engrams/Recipes surface (27 tools).
  No test diffs the registry against the server's registered set.
- **Empty stub registrars claim CLI fallbacks that don't exist**:
  `tools_compaction.go:15` and `tools_templates.go:13` say "available via
  CLI: `loom agent compaction` / templates" — `cmd/loom` never imports
  `pkg/agentcontext` and `cmd_agent.go:49-72` has no such subcommands. Those
  capabilities were removed, not relocated.
- **Dead code ≥ ~2,100 LOC**: `codebase_sync.go` (616), `hybrid_search.go`
  (392, zero refs), `parallel_embed.go` (295), memory export/import (~840,
  constructed at `service.go:434` but unreachable — no tool, no CLI). Plus
  **28 orphaned `Service.Handle*` methods** (121 defined, 93 wired) and 12
  pure-forwarder `svc_*wrappers*.go` files (610 LOC of ceremony).
- **Package hygiene**: 190 Go files / 53k LOC flat in one package, zero
  subpackages; `Service` holds duplicated persisted/unpersisted pairs for
  graph, memory, and workflows (`service.go:45-110`).
- **Storage**: Qdrant-only, no store interface — hence the `*_pure_test.go`
  workaround pattern and 21.6k LOC of tests. Engrams + recipes + memory tiers
  all share `CollMemory` via category strings and `engram-*:` tag prefixes
  with no indexed fields for family/status (`qdrant_indexes.go:26`);
  the knowledge-graph bridge re-materializes graph nodes as `ContextEntry` —
  three representations of the same facts. Stale header claims "12 active
  collections" while 16 are wired (`qdrant_registry.go:9,52-67`).
- **Proxy curation is a manual footgun**: llm-core cap has been hand-bumped
  140→160→164→165 (`proxy_tool_filter.go:14-27`); adding a priority tool
  without bumping the cap silently displaces tail entries; spawn-pod WS path
  bypasses filtering entirely.
- `agent_worktree_release` is never called by the CLI cleanup path
  (`cmd_agent_worktree.go:84` is pure local git) — registry drift papered
  over by the server-side reconciler.

---

## E. Recommended plan (ranked slices)

**S0 — pipe research into implement (~10 lines, no journalengine needed).**
Render `Prior["research"].Artifacts["research_notes"]` into
`implementPromptFor` (`main.go:2289`). Delivers the doc's headline benefit
("implement knows what research found") immediately. Kill-test: a run whose
implement transcript references a research-only fact.

**S1 — Mills per-item journal (journalengine's first real adopter).**
- Migration `017_backlog_item_journals.sql` + DAO on `store.Store` — one row
  per `backlog_items.id`, `snapshot_json TEXT` (Snapshot is already
  JSON-round-trippable). *Naming caution:* migration 004 / `workflow/journal_dao.go`
  already own the word "journal" for the exactly-once effect ledger — call
  this `item_memory` or `backlog_journal` and say why.
- Record hook in `Runner.runStage` immediately after the durable `PutStage`
  (`runner.go:1425-1427`): `RecordTurn(epoch=stage index, situation=stage,
  own=log tail + commit messages + diff stat)`, reusing the 32 KiB/8 KiB caps
  from `spawn.go:84-85`.
- Render `journal.Render()` as the leading stable block in `stagePromptFor`;
  reorder `researchPromptFor` (its invariant repo digest currently *trails*
  the volatile item text, `main.go:2229-2239`).
- Port the `CheckPrefixExtension` consumer test — valuable regardless of
  caching because `SliceHydrator` mutates item fields mid-run
  (`runner.go:888`), so today's prompt prefix already drifts between stages.
- Gate behind an env knob (house style: `LOOM_MILLS_ITEM_JOURNAL=1`), justify
  as durable memory. Name the reader and the kill-test before the table lands
  — `squad_memory` is the cautionary precedent (writer shipped at
  `outcome_recorder.go:154`, reader `squads.Planner` has no production
  wiring).

**S2 — make Mills a first-class agent-context citizen.**
- Operator `agent_context_add` on escalation create and merge audit (two call
  sites) so handoffs stop shipping `entry_count: 0`.
- Set `ParentSessionID` at `reconciler.go:1553` from the operator session so
  stage spawns thread into one session tree the HUD already renders.
- Fix handoff discovery: union `PresenceList` with static inbox ids (or
  register presence for `mills-merges`/`human-on-call`) in
  `AgentBridge.HandoffList`.
- Add `agent_plan_slice_add` to the llm-core profile (and bump the cap — it's
  a silent-displacement list).

**S3 — surface overhaul of the MCP (SIMP-13).**
- Delete: 5 deprecated tools, 3 recipe aliases, 2 dead annotation tools, the
  dead recall shims; delete `hybrid_search.go`, `parallel_embed.go`,
  `codebase_sync.go`, memory export/import + their orphaned handlers; correct
  or delete the two stub registrars' false CLI claims. Target ≤ 60 tools.
- Add a registry-drift test: registered tool set ⊆/⊇ checks against
  `registry.yaml` `always_allow` and the llm-core profile (would have caught
  all 36 drift entries and the `agent_plan_slice_add` no-op).
- Apply `filterProxyTools` (or a spawn profile) to the WS backend path.
- Fix stale collection-count comment; index `engram family/status` if engrams
  are to be kept.

**S4 — telemetry before any cache claims.**
Parse `cached_tokens` in `FlexInferClient`; add token usage to
`hudSpawnTelemetry`. Only then re-evaluate journal-as-cache per
JOURNAL_ENGINE.md Step 3's own standard.

**S5 (deferred) — journal-in-agentcontext, storage-only.** Owner =
`agent_id+namespace`, CAS/versioned snapshot writes, reaper exemption, tool
that returns `Render()`. No prefix-cache justification.

Small independent fix worth taking anytime: `council/brief.go:390` — the
truncation `break` drops every section after the first oversized one; make it
skip-and-continue or budget per-section. (Do not move the brief into a journal:
its first bytes are a timestamp and it is deliberately the *volatile* half of
the shipped council cache split, `council.go:302-397`.)

---

*Sources: four parallel code sweeps (surface map, consumer map, session/compaction
fit, Mills fit) over worktree `core-agent-context-mcp-review-496a14` @ 43ea65f0,
plus live agent-context store queries via the loom hub, 2026-07-25.*

---

## Status addendum — shipped 2026-07-25 (same day)

Four Opus implementation agents executed the plan; every diff was reviewed before ship:

| Slice | MR | Content |
|---|---|---|
| S0+S1 | !1227 | research→implement pipe (default ON, `LOOM_MILLS_RESEARCH_NOTES_IN_IMPLEMENT=0` kills) + per-item journal on `pkg/journalengine` (`backlog_item_memory` migration 017, default OFF, `LOOM_MILLS_ITEM_JOURNAL=1`) + JOURNAL_ENGINE.md corrections |
| S2 | !1228 | `ContextRecorder` (escalation `decision` before handoff, merge `finding`), `Reconciler.OperatorSessionID` → `ParentSessionID`, handoff inbox static-union (`LOOM_HANDOFF_STATIC_INBOXES`) |
| S4 | !1226 | token usage through spawn telemetry wire + research `cached_prompt_tokens` (note: `pkg/llmusage` had landed on main independently; slice became consumer-side wiring) |
| S3 | !1229 | tools 93→81, −5,168 LOC dead code, registry reconciled + `registry_drift_test.go` (kill-tested), llm-core `agent_plan_slice_add` + cap 166, opt-in `LOOM_PROXY_WS_PROFILE` |

Corrections to this review discovered during implementation: `agent_workflow_start`, `agent_context_summarize`, `agent_plan_render/search`, `agent_task_dispatch` are NOT dead (HUD panel text, workflow YAML `tool_name` refs, skills). Eight HUD REST routes (graph path, reasoning chains, memory promote/demote, compaction status, templates) were already broken by earlier SIMP slices — bridge calls tools that no longer exist; handlers kept, re-registration queued as follow-up.

Still open after this cycle: platform/gitops registry mirror + `k8s/base/servers/*` configmap allowlists (stale independently), the 8 HUD route re-registrations, S5 (journal-in-agentcontext, storage-only), cached-token before/after measurement once !1226 data accumulates, pre-commit stash wrapper rename bug.
