# Implementation Plan: Agent Orchestration & Context Management — Next Wave

**Date:** 2026-04-17
**Research:** `.loom/86-research-agent-orchestration-context-next-2026-04-17.md`
**Spec:** `.loom/87-product-spec-agent-orchestration-context-next-2026-04-17.md`
**Worktree:** `.claude/worktrees/ecstatic-volhard-02ebb9` (branch `claude/ecstatic-volhard-02ebb9`, base `main` @ `d9bf5044`)

---

## 1. Index prep (Slice 0) — one-day pass before cutting Slice A

Codebase index readiness (required by planning skill):

- `codebase_memory__codebase_stats` → confirm index is current for `services/loom-core`.
- If stale: `codebase_memory__codebase_index_start --repo services/loom-core`, then `codebase_memory__codebase_index_poll` to terminal.
- `codebase_memory__codebase_text_search` and `…_get_definition` usable against `pkg/{weaver,agentcontext}/*`, `internal/{spawn,hud}/...`.

Update `.loom/00-mcp-inventory.md` with:
- Detected runtime mode (loom-mode vs direct MCP).
- Server counts + tool pagination strategy.
- Codebase index status + repo head commit.
- Delegation plan (which sub-agents pick up which slices — see §6).

## 2. Slice A — Context baseline upgrades (target: 3 working days)

### A1. `SearchWithReranking` production wiring (F1)

- `pkg/agentcontext/reranker_flexinfer.go` *(new)* — thin client wrapping `pkg/weaver/responses_client.go`; exposes `Rerank(ctx, query, entities) ([]float64, error)`. Reuses the weaver HTTP client from `.loom/75 §P0 HTTP-001`.
- `pkg/agentcontext/reranker_bge.go` *(new)* — CPU cross-encoder via existing BGE endpoint; preserves current behavior.
- `pkg/agentcontext/svc_context_search.go` — insert reranker factory + call `hs.SearchWithReranking` when `WEAVER_RERANKER != "off"`.
- `pkg/agentcontext/config.go` — parse `WEAVER_RERANKER{,_MODEL,_TIMEOUT}`; defaults per spec §8.
- `metrics.go` — add `loom_agentcontext_rerank_*` counters.
- Tests: `reranker_flexinfer_test.go` (stubbed HTTP), `reranker_bge_test.go`, integration test in `seamless_integration_test.go` asserting reorder behavior.

### A2. Context delta primitive (F3)

- `pkg/agentcontext/svc_context_search.go` — new `RecallSince(ctx, sessionID, cursor, limit)` returning `(entries, nextCursor, error)`.
- Cursor helper: `pkg/agentcontext/cursor.go` *(new)* — encode/decode `session_id|updated_at_ns` with HMAC-free base64 (opaque, not authenticated).
- `cmd/mcp-agent-context/tools_context.go` — register `agent_context_recall_since` MCP tool.
- REST: `internal/hud/domain/shuttle/…` — add `GET /api/agentcontext/sessions/{id}/delta?since=…` (shuttle already proxies agent-context).
- Tests: property test in `svc_context_search_test.go` asserting the partition invariant.

### A3. Cross-session bridging (F4)

- `pkg/agentcontext/knowledge_graph_query.go` — add `BridgeWalk(ctx, seedIDs, depth, namespace)` that walks only intra-namespace edges of type `{derived_from, references, followup_of}`.
- `pkg/agentcontext/svc_context_search.go` — extend `RecallEnhanced` with `bridge bool` and `bridgeBudget int`; merge bridged results after reranking, tag with `bridged_from`.
- Namespace deny test: new fixture `testdata/namespace_bridge_deny.json` + test in `knowledge_graph_pure_test.go`.

**A exit:** `go test ./pkg/agentcontext/... -run 'Rerank|Since|Bridge' -race` green; CI hooks pass; no public tool shape change except the new `agent_context_recall_since` tool.

## 3. Slice B — LLM-backed auto-compaction (target: 2 working days)

### B1. Compaction execution LLM path (F2)

- `pkg/agentcontext/compaction_strategy.go` — add `Mode` field to `CompactionConfig` (spec §5.F2); default keeps extractive.
- `pkg/agentcontext/compaction_execution.go` — when `Mode == "llm"`, call a new `llmSummarize(ctx, entries)` that delegates to `hudcoord.Compress` via a bridge interface. If the error is transient (5xx / deadline), fall back and emit `loom_agentcontext_compaction_fallback_total`.
- `pkg/agentcontext/compaction_pinned.go` *(new)* — small store for raw blobs pinned for `PinRawFor`. Lives in the same Qdrant registry as a dedicated collection to avoid scope creep.
- `pkg/agentcontext/compaction_audit.go` *(new)* — write `compaction_event` entries (before/after tokens, strategy, duration).
- Tests: new `compaction_execution_llm_test.go` with a stubbed coordinator; assert raw blob readable during pin window, purged after.

**B exit:** integration test asserts LLM path + fallback + audit entries. `make test` green.

## 4. Slice C — Coordination intelligence (target: 4 working days; depends on A, independent of B)

### C1. Auto-handoff triggers (F5)

- `pkg/agentcontext/service_handoffs.go` — new `maybeAutoHandoff(ctx, sessionID, telemetry, cfg)`. Emits handoff drafts, never auto-accepts.
- `pkg/agentcontext/handoff_triggers.go` *(new)* — trigger state: `lastBreachAt[sessionID][reason]`, debounce timer, two-consecutive-breach gate.
- Hook-in points:
  - Spawn budget watcher (`internal/hud/spawn.go:879` `runBudgetWatcher`) — call the trigger on each telemetry update.
  - Long-idle check (`StalledDuration`) — piggyback on existing `presence` tick.
- HUD `internal/hud/domain/handoff/handler_handoff.go` — expose auto-draft handoffs distinctly from human-created drafts via `source: "auto"`.
- Tests: `handoff_triggers_test.go` drives synthetic telemetry; asserts debounce and two-breach gate.

### C2. Fleet task queue + capability router (F6)

- `pkg/agentcontext/schema.go` — add `CapabilityNeeded []string` and `Scope string` to `TaskEntry`.
- `pkg/agentcontext/dispatch.go` *(new)* — pure scorer `func ChooseAgent(task, agents) (AgentID, reason)` — deterministic, table-testable.
- `mcp/context/agent-capabilities.yaml` *(new)* — seed with `claude-code: [go, ts, python, k8s, docs]`, `codex: [python, ts, tests]`, `gemini: [docs, summaries]` (values editable later).
- `cmd/mcp-agent-context/tools_tasks.go` — register `agent_task_dispatch` MCP tool; update `agent_task_add` schema for new fields.
- HUD: `internal/hud/frontend/src/lib/components/FleetPanel*.svelte` — add Queue column + Capability chip. Pull from existing `/api/fleet/*` API, extended server-side with capability passthrough in `internal/hud/domain/fleet/`.
- Tests: `dispatch_test.go` with table cases covering presence off / capability mismatch / tie-break by load.

**C exit:** auto-handoff draft observable in HUD within 5s of budget breach in manual test; fleet queue column renders pending tasks with capability chip in HUD e2e.

## 5. Slice D — Autonomy & economics (target: 5 working days; depends on C)

### D1. MentatLab autonomous refactor flow (F7)

- `cmd/mcp-mentatlab/templates/autonomous-refactor.yaml` *(new)* — DAG per spec §5.F7. Validated at flow-load via a new schema check that every write edge flows through a `review_gate` node.
- `internal/spawn/controller.go` — extend `AgentType` with `"mentatlab-node"`; adapter in `internal/spawn/mentatlab_adapter.go` *(new)* that resolves a DAG node to a spawn driver invocation.
- `internal/hud/domain/spawn/…` — surface MentatLab-driven spawns with a `dag_node_id` tag so the HUD spawn detail page can link back to the flow.
- Persist `mentatlab_event` agent-context entries with `flow_id` for tracer UX.
- Dry-run integration test in `internal/spawn/mentatlab_adapter_test.go`: stub MentatLab, run a flow, assert gate stops before commit.

### D2. Token economics dashboard (F8)

- `internal/hud/domain/fleet/economics.go` *(new)* — derive the six ratios from OTel spans (cost byte tracking) + weaver telemetry counters. Expose `GET /api/fleet/economics?window=7d`.
- `internal/hud/frontend/src/lib/components/fleet/EconomicsPanel.svelte` *(new)* — stacked-bar of frontier vs local tokens per day; drill-in link to existing session trace.
- Snapshot test for the JSON shape; Svelte component test rendering the fixture.

**D exit:** a dry-run of the autonomous flow stops at the review gate and writes events; the economics panel renders populated data from the fixture dataset.

## 6. Slice E — UX + weaver polish (target: 2 working days; runs parallel to D)

### E1. Live file-claim conflict overlay (F9)

- `pkg/agentcontext/file_claims.go` — add `ConflictBus chan ClaimConflictEvent` fed from `acquire` on collision.
- `internal/hud/domain/fleet/handler_claims_stream.go` *(new)* — SSE bridge for the bus.
- HUD `lib/components/fleet/ClaimConflictChip.svelte` *(new)* — subscribes to SSE, animates on new events.
- Integration test with an SSE client asserting 500ms latency against a colliding acquire.

### E2. Weaver auto-compose (F10)

- `pkg/weaver/auto_compose.go` *(new)* — classifier → domain pick → plan → execute, max `WEAVER_AUTO_COMPOSE_MAX_DOMAINS`. Refuse `write:true` domains.
- `pkg/weaver/router.go` — route `no_match` intents through auto-compose when flag on; emit `loom_weaver_auto_compose_*` metrics.
- Unit tests cover: unmatched query → auto-compose picks whitelist domains; `write:true` domains never selected even if adjacent.

## 7. Cross-cutting work

- **Config + flags** — all flags from spec §8 wired via `pkg/agentcontext/config.go` and `pkg/weaver/config.go`. Defaults = current behavior.
- **Metrics** — add Prometheus counters per feature; update `metrics.go` tests.
- **Docs** — update `.agents/skills/agent-context.md`, `.agents/skills/mcp-config.md`, `docs/hud/*` where panels change.
- **Registry sync** — after new MCP tools land (`agent_context_recall_since`, `agent_task_dispatch`), run:
  - `loom sync claude --regen`
  - `loom sync codex --regen`
  - `loom sync gemini --regen`
- **Worktree hygiene** — each slice lands in a dedicated linked worktree under `services/loom-core/.worktrees/{slice-a,slice-b,slice-c,slice-d,slice-e}` per the workspace policy.

## 8. Parallelism plan

```
Week 1: Slice A (A1 → A2 → A3 serially)
         Slice B starts mid-week once A1 lands (B reuses reranker's FlexInfer client)
Week 2: Slice B finishes
         Slice C starts (C1 and C2 can run in parallel; C1 needs shipped Slice A, C2 is independent)
Week 3: Slice D (D1 is the big one; D2 runs parallel to D1)
         Slice E in parallel (E1 independent; E2 reuses weaver hardening baseline)
Week 4: Integration, docs, flag flip to production defaults on `main`
```

Delegation per [64-planning §§ Claude Delegation Addendum] — spawn one sub-agent per slice in its own worktree (see §6 in the skill doc), reconcile in the last week.

## 9. Risk register (condensed from research §5)

| # | Risk | Mitigation |
|---|---|---|
| 1 | FlexInfer instability for reranker calls | `WEAVER_RERANKER=bge` stays default; flag flip reversible |
| 2 | LLM compaction hallucinates | Pin raw blobs for `PinRawFor`; limit input tokens; reuse coordinator path already proved in session summaries |
| 3 | Auto-handoff spam | 2-breach gate + 10m debounce + require human approval |
| 4 | Autonomous spawn commits unintended changes | Every write edge goes through `review_gate`; enforced at schema validation |
| 5 | Cross-namespace leakage via bridge walk | Strict namespace prefix match; dedicated deny-list test |
| 6 | Economics metrics drift from reality | Derive from existing OTel spans only; never double-count |
| 7 | Auto-compose picks domains that mutate state | Whitelist domains with `write:false`; refusal is required-passing test |

## 10. Test gates per slice

- **A:** `go test -race ./pkg/agentcontext/... -run 'Rerank|Since|Bridge'` + `make lint`
- **B:** `go test -race ./pkg/agentcontext/... -run 'Compaction'` + LLM stub integration test
- **C:** `go test -race ./pkg/agentcontext/... ./internal/hud/domain/fleet/... ./internal/hud/domain/handoff/...` + HUD e2e for fleet queue + auto-handoff
- **D:** `go test ./internal/spawn/... ./internal/hud/domain/fleet/...` + dry-run MentatLab flow integration
- **E:** `go test ./pkg/weaver/... ./internal/hud/domain/fleet/...` + SSE latency test

Full run at end of wave: `devbox_quality_gate(project="loom-core", agent_id="claude-code")`.

## 11. Commit / MR strategy

- One MR per feature (F1–F10). Each MR keeps diff < 800 lines where possible; split larger ones into prep + main.
- MR titles use conventional commits:
  - `feat(agentcontext): rerank hybrid recall via flexinfer (F1)`
  - `feat(agentcontext): recall delta primitive (F3)`
  - `feat(agentcontext): cross-session bridge walker (F4)`
  - `feat(agentcontext): llm-backed auto-compaction (F2)`
  - `feat(agentcontext): auto-handoff triggers (F5)`
  - `feat(agentcontext): fleet queue + capability router (F6)`
  - `feat(spawn): mentatlab autonomous refactor flow (F7)`
  - `feat(hud): token economics dashboard (F8)`
  - `feat(hud): live file-claim conflict overlay (F9)`
  - `feat(weaver): auto-compose for unmatched compound queries (F10)`
- Pre-ship self-review (`.agents/skills/pr-self-review.md`) required on each.

## 12. Open follow-ups (parked)

- **F7 v2:** parallel branches in MentatLab (Claude + Codex attempts on the same ticket).
- **F5 v2:** auto-accept handoffs for same-agent-type continuation.
- **F4 v2:** relax namespace prefix for opt-in cross-project bridging.
- **F8 v2:** cost alerting when a session crosses a projected daily budget.

## 13. Sources

See `.loom/86-research-agent-orchestration-context-next-2026-04-17.md §8`. Added references for this plan:

- `.loom/75-product-spec-weaver-hardening-2026-04-04.md §P0` (HTTP client reuse pattern)
- `.loom/82-plan-headless-agent-fullstack-2026-04-07.md §1` (shipped telemetry — foundation for F5/F8)
- `.loom/85-plan-hud-robustness-2026-04-12.md` (pool + transport patterns — not to regress)
- `pkg/agentcontext/hybrid_search.go:335` (rerank hook site)
- `pkg/agentcontext/compaction_execution.go:1` (compaction integration site)
- `internal/hud/spawn.go:879` (budget watcher — handoff trigger hook)
- `internal/hud/domain/coordinator/handlers.go:37` (coordinator compress for F2)
- `commit 5f995f27` (OTel cost byte tracking for F8)
