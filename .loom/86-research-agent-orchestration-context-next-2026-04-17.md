# Research: Next Feature Wave — Agent Orchestration & Context Management

**Date:** 2026-04-17
**Status:** Draft v1 — inputs for `87-product-spec` and `88-implementation-plan`
**Scope:** What to build next in the agent orchestration + context layers, now that headless spawn, weaver subagents, hybrid recall, and session telemetry have landed.

---

## 1. Current state (sourced)

### 1.1 Orchestration primitives already in place

| Capability | Location | Notes |
|---|---|---|
| Local-model router (ex-"Orchestra", renamed Weaver) | `pkg/weaver/router.go:1`, `pkg/weaver/domain.go:1` | `SubAgent{Name, Tools, SystemPrompt, Model, TokenBudget, MaxTokens}` + `DomainRegistry`; compound tools in `pkg/weaver/compound.go:1` |
| Weaver hardening shipped (retries, timeouts, query IDs, env rename) | `.loom/75-product-spec-weaver-hardening-2026-04-04.md`, `.loom/76-implementation-plan-weaver-hardening-2026-04-04.md` | `ORCHESTRA_* → WEAVER_*`, HTTP client reuse, synthesis timeout |
| Headless agent spawn (Claude + Codex SDK drivers) | `internal/spawn/controller.go`, `tools/spawn-driver/src/{claude,codex}-driver.ts`, `internal/hud/spawn*.go` | Multi-turn via control-file loop; REST endpoints for `message/interrupt` shipped (`a8108bf1`). |
| Canonical telemetry | `internal/hud/bridge/spawn_telemetry.go:8` | `SpawnTelemetry{TurnCount, TotalCostUSD, TokenUsage, ModelUsage, ToolCalls, FileChanges, Errors, StopReason}` capped at 500 tools / 200 files |
| Coordinator domain (LLM summarize/compress/plan) | `internal/hud/domain/coordinator/coordinator.go:23` | `POST /api/coordinator/summarize/{session_id}`, `/compress`, `/plan` |
| MentatLab DAG workflows | `cmd/mcp-mentatlab/` | Exists but not wired as an autonomous-mode path from weaver or the spawn controller |
| Fleet orchestration UX | `.loom/73-planning-productivity-unlocks-2026-04-03.md` §1 | Merge queue, recommendations, dispatch history, attention lanes (`1ff61661`, `e49e1ae0`) |

### 1.2 Context primitives already in place

| Capability | Location | Notes |
|---|---|---|
| Memory hierarchy (working / short / long-term + archival) | `pkg/agentcontext/memory_hierarchy*.go` | Dedup, promotion, persistence already split into single-responsibility files |
| Automatic compaction scheduler | `pkg/agentcontext/compaction_scheduler.go:14`, `compaction_execution.go` | Thresholds per tier; `summarization_depth` and `target_capacity` configurable |
| Hybrid search with rerank hook | `pkg/agentcontext/hybrid_search.go:335` | `SearchWithReranking(ctx, query, k, reranker)` — **reranker arg is never supplied in production paths** (`grep SearchWithReranking → only tests + this file`) |
| Knowledge graph (Neo4j) | `pkg/agentcontext/knowledge_graph*.go`, `service_graph.go` | Entities, relations, reasoning, persistence — mature |
| Handoffs (explicit) | `pkg/agentcontext/service_handoffs.go`, `internal/hud/domain/handoff/` | Manual create/accept/reject; **no auto-handoff on context pressure or budget exhaustion** |
| File claims | `pkg/agentcontext/file_claims.go` | Acquire/release/query — surfaced in fleet UX, no live conflict visualizer |
| Recipes | `pkg/agentcontext/svc_recipes.go` | Structured library with required proof; used by `agent-recipes` skill |
| Presence + heartbeats | `pkg/agentcontext/presence.go`, `svc_presence.go` | Driven by CLI lifecycle hooks |
| Memory export/import | `pkg/agentcontext/memory_export*.go` | Full round-trip; no cross-session *bridging* logic |

### 1.3 Known gaps (from `.loom/73` plus direct tree scan on `09a53b89`)

- **Rerank wiring** — `SearchWithReranking` has no production caller; recall still runs plain hybrid score. `.loom/64 §WS-2A` called this out; not shipped.
- **LLM synthesis inside compaction** — scheduler triggers, strategy decides, but the `execute` path uses extractive/heuristic summarization. No FlexInfer/weaver call on the compaction loop.
- **Autonomous mode** — `.loom/64 §4.5 Phase 5` (spawned autonomous via MentatLab DAG) is not wired. `mentatlab|MentatLab` appears only in HUD `TasksPanel.svelte` and the compiled asset bundle, not in `internal/spawn` or `pkg/weaver`.
- **Auto-handoff triggers** — no code matches `handoff.*trigger|autoHandoff|pressure` in `pkg/agentcontext`. All handoffs today are manual.
- **Fleet planner** — there is a dispatch panel and recommendations in HUD, but no Go-side planner that picks which agent should take which task from a queue. The human is still the planner.
- **Cross-session context bridging** — no graph-walk path that stitches related sessions together at recall time. Knowledge graph exists but isn't used to expand recall across session boundaries.
- **Token-economics dashboard** — `.loom/64 §5` telemetry schema defined; the HUD panel for savings ratio / compression ratio / local-vs-frontier utilization is not built.
- **Context-diff / "what's new"** — no primitive that returns *delta* memories since a given cursor, so every `agent_context_recall_enhanced` re-injects previously-seen items.
- **Live file-claim conflict overlay** — claims are recorded, but the HUD surface stops at attention lanes; no per-file "N agents want this file right now" view.

---

## 2. Market scan (2026 agentic-coding landscape)

Sources gathered during prior research cycles: `.loom/13-research-agentic-workflows-openclaw.md`, `.loom/77-research-agentic-engineering-patterns-2026-04-05.md`, `.loom/79-research-headless-agent-telemetry-sdk-2026-04-06.md`.

### 2.1 Convergent patterns

1. **Hierarchical orchestration is standard.** Claude Code SubAgents, Codex `[agents]`, Kilocode / OpenCode mode switching, Cursor "Manager" mode — all ship a router-agent + specialist-agent topology. Loom's weaver is aligned; the laggard areas are *autonomous spawn* and *cross-tool* coordination.
2. **Context compression is the product.** Anthropic's prompt caching, OpenAI Responses API auto-compaction, and Claude Code's `compact` slash command all make compression a first-class feature surfaced to the user. Loom has the building blocks (coordinator summarize/compress) but does not yet expose them as a push-button UX surface.
3. **Background / "headless" agents with delegation are the frontier.** Codex cloud tasks, Claude Code's `@claude` GitHub bot, Cursor background agents, Anthropic's Skills + Managed Agents. The pattern is: queue a task, an agent wakes up, runs to completion, writes a summary + diff/PR, hands back. Loom has spawn + follow-up but no durable *queue* and no auto-triggered scheduling.
4. **Telemetry as the flywheel.** All the frontier tools now ship per-session token / cost / tool-call logs to a dashboard; Anthropic's cost-explorer, OpenAI's Responses telemetry, Cursor's usage page. Loom has canonical `SpawnTelemetry` — needs the dashboard.

### 2.2 Divergent / "things Loom can uniquely win"

- **Multi-CLI, multi-model mix on one fleet.** Nothing upstream natively coordinates Claude + Codex + Gemini on the same ticket. Loom's spawn layer already speaks both Anthropic and OpenAI SDKs; the missing pieces are (a) a shared task queue and (b) a capability-aware router.
- **Local GPU tier.** FlexInfer + Qwen3.5 via vLLM give Loom a cheap tier for recall reranking and compaction synthesis that frontier vendors don't have. `.loom/64 §3` has the model lineup.
- **GitOps-native everything.** Loom already ships everything through `platform/gitops/`. Autonomous agents that commit-review-merge with Flux reconciliation is a pattern we can ship in a month that upstream vendors can't match for this workspace.

### 2.3 What to skip (low-leverage in 2026)

- **Vector-only recall rewrites.** The hybrid + reranker path already dominates pure vector in every eval we've read. Invest in the reranker, not a new vector backend.
- **Building yet another MCP server from scratch for orchestration.** Weaver is already the shape we need. Extend, don't replace.
- **Custom DAG engine for autonomous tasks.** MentatLab already exists in-tree. Wire it, don't rebuild.

---

## 3. Problem framing for the next wave

**User-visible pain today** (observed in recent sessions + `.loom/73 §1`):

1. Asking a frontier agent a compound question still costs 3-8k frontier tokens. Weaver helps for the routed queries but is not the default path for all compound lookups.
2. Recall quality at session start injects stale / duplicate memories. No "what's new since I last looked" primitive.
3. Handoffs between Claude and Codex are copy-paste of markdown summaries. The receiving agent has no structured state.
4. File claims are advisory; two agents can still fight over the same file because nothing surfaces the conflict until after the fact.
5. Long-running work (refactors spanning many files) has no durable queue the platform can pick up tomorrow. Each session re-plans from scratch.
6. Token-economics — we can't answer "how much are we saving with the local tier?" because the dashboard isn't built.

**Design principle for this wave:** ship the *smallest* integration that turns each of the above pains into a first-class platform surface. No new MCP servers; extend weaver, agent-context, spawn, and the HUD.

---

## 4. Candidate feature set (discussed in §5 of the product spec)

| # | Feature | Pain it addresses | Primary home | Depends on |
|---|---|---|---|---|
| F1 | **Recall reranking** wired to FlexInfer | Stale/noisy recall | `pkg/agentcontext/hybrid_search.go` + new `pkg/agentcontext/reranker_flexinfer.go` | weaver ResponsesClient reuse |
| F2 | **LLM-backed auto-compaction** | Working-memory hits threshold → heuristic squash today | `pkg/agentcontext/compaction_execution.go` | coordinator summarize, weaver client |
| F3 | **Context delta primitive** (`agent_context_recall_since`) | Every recall re-injects known items | `pkg/agentcontext/svc_context_search.go` | memory cursor/watermark |
| F4 | **Cross-session context bridging** via knowledge graph walk | Related sessions look isolated to the receiving agent | `pkg/agentcontext/svc_context_search.go`, `knowledge_graph_query.go` | graph is ready; add walker |
| F5 | **Auto-handoff triggers** (budget / context-pressure / stalled) | Handoff is manual; friction means we don't use it | `pkg/agentcontext/service_handoffs.go`, spawn budget watcher | SpawnTelemetry, handoff schema |
| F6 | **Fleet task queue + capability router** | Human is still the planner | new `pkg/agentcontext/dispatch.go` + HUD panel | agent_task CRUD, presence, SpawnController |
| F7 | **Autonomous spawn via MentatLab DAG** | No durable queue for long-running refactors | `internal/spawn/controller.go` + `cmd/mcp-mentatlab/` bridge | F6 (queue) |
| F8 | **Token-economics dashboard** | Can't prove local-tier ROI | HUD `/api/coordinator/*` + new `/api/weaver/economics` | weaver telemetry, spawn telemetry |
| F9 | **Live file-claim conflict overlay** | Claims are advisory | HUD `ServersPanel`-style panel, `svc_claims.go` already emits events | WebSocket or SSE from agent-context |
| F10 | **Weaver "auto-compose" for novel queries** | Today only predefined compound tools cover compounds | `pkg/weaver/compound.go`, new `auto_compose.go` | domain registry validation (shipped in weaver hardening) |

---

## 5. Risk & dependency map

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| FlexInfer Qwen3.5 vLLM backend still unstable on ROCm for reranker calls | Medium | Med | Fall back to BGE cross-encoder on CPU (already deployed); feature-flag per-call reranker |
| Auto-compaction LLM synthesis introduces hallucinated summaries | Medium | High | Route through coordinator's existing `compress` path (proved safe in session summaries); cap input tokens; keep raw blobs pinned for N hours |
| Auto-handoff triggers spam presence churn | Low | Med | Require two consecutive threshold breaches before firing; debounce per `session_id` |
| Fleet queue / autonomous spawn lets agents run unattended | High (by design) | High | Keep MentatLab approval gates required for any write-op in autonomous mode; honor RBAC policy |
| Token-economics metrics drift from reality | Low | Low | Reuse existing OTel spans (`5f995f27` cost byte tracking); derive ratios in HUD, do not re-count |
| Cross-session bridging leaks info across namespaces | Medium | High | Bridge only within the same `namespace` prefix; unit test with a deny-list fixture |

---

## 6. Sequencing recommendation (into §4 of implementation plan)

1. **Slice A — Context baseline upgrades:** F1 reranker + F3 delta + F4 bridging. All three are agent-context pkg changes with no UI surface; unblocks every recall consumer and is the lowest-risk wave.
2. **Slice B — Compaction intelligence:** F2 LLM compaction. Pairs naturally with A because the reranker model is the same Qwen3.5-9B endpoint.
3. **Slice C — Coordination intelligence:** F5 auto-handoff + F6 fleet queue. These depend on SpawnTelemetry (shipped) and presence (shipped). Ship the data plane first, then the HUD panels.
4. **Slice D — Autonomy & economics:** F7 MentatLab autonomous spawn + F8 token-economics dashboard. F7 is the biggest scope; F8 is light but needs all prior slices to have something interesting to plot.
5. **Slice E — UX + weaver auto-compose:** F9 live claim overlay + F10 auto-compose. Polish pass, runs in parallel once Slice A is green.

Expected calendar: ~4 weeks serial, ~2.5 weeks if A+B and C+E are parallelized (see implementation plan §6).

---

## 7. Open questions for spec pass

1. Do we accept a hard dependency on FlexInfer availability for F1/F2, or keep a local-embedding fallback path gated behind a flag?
2. For F6, do tasks belong in `agent_task_*` (existing, per-session) or a new top-level `fleet_queue_*` namespace that persists across sessions?
3. F7 — is the autonomous path restricted to a single agent type per task, or can MentatLab branch to Claude *and* Codex for parallel attempts?
4. F8 — do we add a new `/api/weaver/economics` endpoint or extend the HUD Fleet panel with the new metrics?

(All four are resolved in `.loom/87-product-spec-agent-orchestration-context-next-2026-04-17.md`.)

---

## 8. Sources

- `.loom/64-planning-next-gen-skills-agents-orchestration-2026-03-29.md`
- `.loom/73-planning-productivity-unlocks-2026-04-03.md`
- `.loom/74-research-weaver-hardening-2026-04-04.md`
- `.loom/75-product-spec-weaver-hardening-2026-04-04.md`
- `.loom/77-research-agentic-engineering-patterns-2026-04-05.md`
- `.loom/79-research-headless-agent-telemetry-sdk-2026-04-06.md`
- `.loom/80-product-spec-headless-agent-telemetry-sdk-2026-04-06.md`
- `.loom/82-plan-headless-agent-fullstack-2026-04-07.md`
- `.loom/83-plan-headless-agent-ux-parity-2026-04-07.md`
- `pkg/weaver/{router,domain,compound,responses_client}.go`
- `pkg/agentcontext/{compaction_scheduler,compaction_execution,hybrid_search,knowledge_graph*,memory_hierarchy*,service_handoffs,file_claims}.go`
- `internal/spawn/{controller,reconciler,store,types}.go`
- `internal/hud/domain/{coordinator,handoff,fleet,spawn}/`
- `internal/hud/bridge/spawn_telemetry.go:8`
- `git log --oneline -30` (branch `claude/ecstatic-volhard-02ebb9`, worktree head `d9bf5044`)
