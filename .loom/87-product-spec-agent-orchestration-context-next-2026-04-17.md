# Product Spec: Next Feature Wave — Agent Orchestration & Context Management

**Date:** 2026-04-17
**Research:** `.loom/86-research-agent-orchestration-context-next-2026-04-17.md`
**Implementation Plan:** `.loom/88-implementation-plan-agent-orchestration-context-next-2026-04-17.md`
**Scope:** Ten features across context quality, coordination, autonomy, and observability. Public surface changes only where called out.

---

## 1. Goal

Raise the platform from "we can drive one headless agent per session" to "we can operate a fleet of Claude + Codex agents sharing context, handing off automatically, and reporting token economics in one place" — without adding a new MCP server, without breaking any existing tool interface, and without removing a single currently-working feature.

## 2. Non-goals

- New MCP server (weaver, agent-context, coordinator domains all extend in place).
- Replacement of Qdrant or Neo4j backends.
- New frontier-agent CLI support beyond Claude + Codex.
- UI redesign — Svelte panels extend existing layouts.
- Multi-tenant RBAC (reuse current `rbac-policy.yaml`).
- Retraining or fine-tuning local models (reuse Qwen3.5-9B as deployed in FlexInfer).

## 3. Success criteria

A wave is "done" when all of these hold on `main`:

1. `agent_context_recall_enhanced` returns reranked results when `WEAVER_RERANKER=flexinfer` is set, else current behavior.
2. `agent_context_recall_since(cursor)` returns only entries newer than the cursor; 0 duplicates against a prior call.
3. Compaction at `WorkingMemoryThreshold` uses LLM synthesis via the coordinator path and pins raw blobs for `CompactionConfig.PinRawFor` (default 1h).
4. A spawn whose `SpawnTelemetry.TokenUsage.InputTokens` exceeds `AutoHandoff.InputTokenHigh` automatically creates a handoff entry and surfaces it in the HUD handoff panel.
5. The HUD **Fleet** view shows a "Queue" column with pending tasks and the weaver-classified **capability_needed** tag per task.
6. `mentatlab_run_flow("autonomous-refactor", …)` spawns a headless agent, persists decisions into agent-context, and closes with a pull request link (or diff preview) on completion.
7. HUD **Token Economics** panel renders six ratios from §5.F8 with a 7-day rolling window and per-session drill-in.
8. `ServersPanel` (or a sibling) overlays live file-claim conflicts within 500ms of a claim collision.
9. `weaver_query("…")` for an unrecognized compound intent auto-composes a subagent plan and executes, with an on-by-default safety cap (`WEAVER_AUTO_COMPOSE_MAX_DOMAINS=3`).
10. `make build && go test ./pkg/agentcontext/... ./pkg/weaver/... ./internal/hud/... ./internal/spawn/...` green, with new tests for each feature.

## 4. Decisions (resolving open questions from research §7)

| # | Question | Decision | Rationale |
|---|---|---|---|
| Q1 | Hard FlexInfer dep for F1/F2? | **Feature-flagged**: `WEAVER_RERANKER=flexinfer\|bge\|off`, `WEAVER_COMPACTION_MODE=llm\|extractive`. BGE cross-encoder stays as fallback. | Keeps CI + offline dev paths working; matches weaver hardening pattern from `.loom/75`. |
| Q2 | Task queue namespace for F6 | **Reuse `agent_task_*`**, add `scope="fleet"` and optional `capability_needed` + `requested_by` fields. | Avoids a second CRUD surface; existing MCP tools cover basic ops. |
| Q3 | F7 multi-agent branches | **Single agent per branch in v1**; MentatLab DAG supports branching but each node is one agent. Parallel attempts deferred. | Keeps cost + semantic conflicts bounded for the first autonomy pass. |
| Q4 | F8 endpoint surface | **Extend Fleet panel API**: add `/api/fleet/economics` under existing domain; no new top-level namespace. | Matches existing `/api/fleet/*` IA. |

## 5. Feature specifications

Each feature lists **surface changes, config, metrics, acceptance**. Features are grouped by slice.

### Slice A — Context baseline upgrades

#### F1. Recall reranking via FlexInfer (or BGE fallback)

- **Surface:** no public tool shape change. `agent_context_recall_enhanced` starts calling `SearchWithReranking` when reranker is enabled.
- **Config:** `WEAVER_RERANKER=flexinfer|bge|off` (default `bge`). `WEAVER_RERANKER_MODEL=qwen3.5-9b` (default); `WEAVER_RERANKER_TIMEOUT=2s`.
- **Behavior:** Hybrid search fetches top-`3k` candidates, passes to reranker, returns top-`k`. On timeout, returns the unreranked top-`k` with `rerank_status="timeout"` in metadata.
- **Metrics:** `loom_agentcontext_rerank_latency_seconds{backend}`, `loom_agentcontext_rerank_reorder_distance` (sum of index deltas).
- **Acceptance:** unit test asserts order changes when reranker mocked to invert scores; integration test against a stubbed FlexInfer returns deterministic order.

#### F3. Context delta primitive

- **Surface:** new MCP tool `agent_context_recall_since(session_id, cursor, limit)` plus REST `GET /api/agentcontext/sessions/{id}/delta?since={cursor}`.
- **Cursor:** opaque string; implementation = base64(`session_id|updated_at_ns`). Stored in session state per-agent so CLI hook can refresh it.
- **Behavior:** Returns entries with `updated_at > cursor` ordered ASC, plus a `next_cursor`. Empty-result calls return the same `next_cursor` they were given so the caller can cheaply re-poll.
- **Metrics:** `loom_agentcontext_delta_entries_returned`, `loom_agentcontext_delta_zero_rate`.
- **Acceptance:** property test: any two consecutive calls using the returned cursor partition the entry set with no overlap and no gap.

#### F4. Cross-session bridging via knowledge graph

- **Surface:** `agent_context_recall_enhanced` gains optional `bridge=true` (default false in v1, gate to true per-workflow in v2).
- **Behavior:** After hybrid+rerank, take top-`n` entities, walk knowledge graph edges of type `{derived_from, references, followup_of}` up to `depth=2` within the same namespace prefix, surface up to `bridge_budget=5` additional entries with a `bridged_from=<session_id>` tag.
- **Safety:** **namespace prefix match is required**. Unit test fixture has `projA/foo` and `projB/foo` — a recall in `projA/foo` never returns `projB/foo` entries.
- **Metrics:** `loom_agentcontext_bridge_hits_total{depth}`, `loom_agentcontext_bridge_cross_namespace_denied_total`.
- **Acceptance:** namespace deny-list test is required-passing.

### Slice B — Compaction intelligence

#### F2. LLM-backed auto-compaction

- **Surface:** no MCP tool change. `CompactionConfig` gains `Mode string` (`extractive|llm`), `PinRawFor time.Duration` (default 1h), `MaxSynthesisTokens int` (default 2048).
- **Behavior:** When `Mode=llm`, `compaction_execution.go` routes the candidate window through the coordinator's existing summarize path (`hudcoord.Compress`), preserving raw entries in a `compaction_pinned` collection until `PinRawFor` elapses.
- **Failure mode:** LLM error → fall back to existing extractive summarizer, emit `loom_agentcontext_compaction_fallback_total{reason}`.
- **Audit:** each compaction run writes a `compaction_event` agent-context entry with before/after token counts, strategy, model, and duration.
- **Acceptance:** integration test runs a compaction with a stubbed coordinator; asserts synthesized summary replaces sources AND raw blobs remain readable via `agent_memory_get` for `PinRawFor`.

### Slice C — Coordination intelligence

#### F5. Auto-handoff triggers

- **Surface:** new `handoff_trigger` config block in agent-context config. `service_handoffs.go` gains `maybeAutoHandoff(ctx, sessionID, telemetry)`.
- **Triggers:**
  - `InputTokenHigh` (default 160_000) — suggested handoff to a fresh session.
  - `TotalCostUSDHigh` (default $1.50/spawn).
  - `StalledDuration` (default 8m of no assistant output).
- **Behavior:** Two consecutive breaches before firing; debounce per-session 10m. Creates a handoff draft that the human approves (no auto-accept in v1), surfaced in HUD **Handoffs** panel.
- **Metrics:** `loom_handoff_trigger_fired_total{reason}`, `loom_handoff_trigger_suppressed_total{reason}`.
- **Acceptance:** unit test drives fake telemetry through the trigger loop and asserts debounce + two-breach gate hold.

#### F6. Fleet task queue + capability-aware router

- **Surface:** `agent_task_add` accepts optional `capability_needed: []string` (e.g. `["go", "k8s"]`) and `scope: "session"|"fleet"` (default `session`). New `agent_task_dispatch(task_id)` creates a spawn using the router's choice, honoring presence + claim data.
- **Router:** in `pkg/agentcontext/dispatch.go`, a pure-function scorer that takes (task, available_agents) → best_agent. Presence data from `svc_presence.go`; capabilities from a new YAML `mcp/context/agent-capabilities.yaml` (hand-edited initially).
- **HUD:** Fleet panel gains a "Queue" column and a "Capability" chip per task row.
- **Metrics:** `loom_fleet_queue_depth`, `loom_fleet_dispatch_latency_seconds`, `loom_fleet_dispatch_mismatch_total{reason}`.
- **Acceptance:** table-driven test for the scorer; HUD e2e asserts a `scope="fleet"` task appears in the Queue column within 2s of insertion.

### Slice D — Autonomy & economics

#### F7. Autonomous spawn via MentatLab DAG

- **Surface:** new MentatLab flow template `autonomous-refactor.yaml` bundled in `cmd/mcp-mentatlab/templates/`. Invoked via existing `mentatlab_run_flow` tool. `internal/spawn/controller.go` gains an `AgentType: "mentatlab-node"` that bridges a DAG node to a spawn driver call.
- **DAG shape:** `plan → spawn(claude|codex) → verify(tests/lint) → review_gate(human) → commit → push → open_pr`. Every write-step is gated; v1 allows only one agent per DAG (per Q3).
- **Persistence:** each DAG node writes a `mentatlab_event` agent-context entry with the flow_id so the HUD can render the trace.
- **Safety:** any DAG edge with a write operation must go through `review_gate` — enforced by schema validation at flow load time.
- **Acceptance:** integration test runs a dry-run flow that plans + spawns + verifies + stops at `review_gate`; no commits created.

#### F8. Token economics dashboard

- **Surface:** new HUD route `#fleet/economics`. New API `GET /api/fleet/economics?window=7d` returning the six ratios from `.loom/64 §5.1`:
  1. Token savings ratio
  2. Tool-call reduction ratio
  3. Cost ratio (frontier / total)
  4. Context waste
  5. Compression ratio
  6. Local utilization
- **Data source:** reuse OTel spans from `5f995f27` (cost byte tracking) plus weaver `telemetry.go` counters. No new recording — only new derivation.
- **UI:** stacked-bar of frontier vs local tokens per day + drill-in to session trace (already shipped).
- **Acceptance:** API returns stable values for a fixture dataset; snapshot test on the JSON shape.

### Slice E — UX + weaver polish

#### F9. Live file-claim conflict overlay

- **Surface:** new HUD panel chip "Claim Conflicts" on the Fleet view; existing `svc_claims.go` gains a `ConflictBus` channel and SSE endpoint `GET /api/agentcontext/claims/stream`.
- **Behavior:** when `agent_file_claim_acquire` collides with an existing claim, SSE emits `{file, holder_agent, requester_agent, ts}`. HUD shows overlay within 500ms.
- **Acceptance:** integration test opens an SSE stream, acquires a colliding claim, asserts event arrives <500ms.

#### F10. Weaver auto-compose

- **Surface:** `weaver_query` gains path: if intent classifier returns `no_match` for predefined compound tools, synthesize a plan (pick N domains, run in parallel, synthesize result) up to `WEAVER_AUTO_COMPOSE_MAX_DOMAINS` (default 3).
- **Safety:** refuse to auto-compose if any candidate domain has `write: true` in its SubAgent spec. Log every auto-compose decision with `query_id`.
- **Metrics:** `loom_weaver_auto_compose_total{outcome}`, `loom_weaver_auto_compose_domains_used`.
- **Acceptance:** unit test: unrecognized query → auto-compose runs on whitelist domains; query tagged `write:true` never auto-composes.

---

## 6. Dependencies

- **FlexInfer Qwen3.5-9B endpoint** (already deployed per `.loom/64 §3.3`) — required for F1/F2 when flags on; BGE fallback otherwise.
- **Coordinator summarize/compress path** (`internal/hud/domain/coordinator/handlers.go:37`) — required for F2.
- **Spawn controller + telemetry** (`internal/hud/bridge/spawn_telemetry.go:8`) — required for F5, F7, F8.
- **MentatLab** (`cmd/mcp-mentatlab/`) — required for F7.
- **OTel cost byte tracking** (commit `5f995f27`) — required for F8.

## 7. Out of scope / deferred

- Multi-agent parallel branches in MentatLab (Q3 deferred).
- Auto-accept handoffs (F5 stays manual-approve in v1).
- RBAC scoping of the fleet queue (current policy applies; new scoping in a later wave).
- Cross-workspace recall bridging (namespace prefix match is strict).
- Swapping the knowledge graph backend.

## 8. Rollout & flags

| Flag | Default | Owner |
|---|---|---|
| `WEAVER_RERANKER` | `bge` | F1 |
| `WEAVER_RERANKER_MODEL` | `qwen3.5-9b` | F1 |
| `WEAVER_RERANKER_TIMEOUT` | `2s` | F1 |
| `AGENTCONTEXT_RECALL_BRIDGE` | `false` | F4 |
| `AGENTCONTEXT_COMPACTION_MODE` | `extractive` | F2 |
| `AGENTCONTEXT_COMPACTION_PIN_RAW_FOR` | `1h` | F2 |
| `AGENTCONTEXT_HANDOFF_INPUT_TOKEN_HIGH` | `160000` | F5 |
| `AGENTCONTEXT_HANDOFF_COST_USD_HIGH` | `1.50` | F5 |
| `FLEET_QUEUE_ENABLED` | `true` | F6 |
| `WEAVER_AUTO_COMPOSE_MAX_DOMAINS` | `3` | F10 |

All flags default to the safe / current-behavior setting. Flipping one to "new" must be reversible without data migration.

## 9. Acceptance summary

Feature-level acceptance in §5; wave-level acceptance in §3. A feature is not shipped until its acceptance plus `make build && make test` are green on the slice branch.

## 10. Sources

See `.loom/86-research-agent-orchestration-context-next-2026-04-17.md §8`.
