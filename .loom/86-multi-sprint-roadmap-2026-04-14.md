# Multi-Sprint Roadmap: Loom Core — April–June 2026

> **Date:** 2026-04-14
> **Status:** Draft — awaiting review
> **Scope:** 4 sprints (2 weeks each), 8 weeks total
> **Theme:** From "works" to "works effortlessly" — hardening, observability, autonomy, simplification

---

## Current State Assessment

### Just Shipped (Last 2 Weeks)

| Feature | Commit | Impact |
|---------|--------|--------|
| Seamless session/recall/workflow/memory (M1-M4) | `cfb8f49e` | Crash recovery, parallel recall, workflow timeouts, memory persist-first |
| Presence auto-register on heartbeat | `a19f765b` | Agents never fail to register — auto-register on heartbeat if missed |
| HUD robustness: pool, transport, leak guard | Plan 85 | Pool exhaustion → wait-with-timeout, connection leak guard |
| Hub delegation skip for warmed servers | `8658562d` | Local servers preferred when already warm |
| Workspace-relative namespace + dynamic description | `e338402b` | Hooks infer namespace from git branch + workspace path |
| Go 1.25.9 stdlib security | `e04ba2e6` | Security patch |

### Partially Built (Needs Completion)

| Feature | Built | Missing |
|---------|-------|---------|
| **Headless agent spawning** | JSONL parsers (Claude/Codex/Gemini), spawn API, telemetry, SSE | Multi-turn SDK drivers, spawn↔session cross-navigation |
| **Orchestra (local agents)** | Router, 6 domains, compound tools, k3s binary | Embedded in loomd, specialized subagents, FlexInfer vLLM |
| **OTel traces** | Daemon init, server handler spans, `otel-status` | HUD trace explorer, proxy tool-call instrumentation |
| **Server catalog** | HUD catalog framing, registry.yaml | `loom catalog list/enable/disable/search` CLI |
| **Dark factory patterns** | Skills registry entries, workflow YAML stubs | Auto-quality-gate, TDD workflow, session retro, recipe library |
| **HUD UX polish** | Spawn↔session bridge, overview coherence | Attention rail navigation, presence loading states, polling consistency |

### Known Gaps (From Codebase Exploration)

| Gap | Severity | Source |
|-----|----------|--------|
| Codex idle sessions go offline (notify-only, no keepalive) | Medium | Heartbeat stamps show no activity for idle codex |
| Agent recall uses keyword matching, no semantic reranking | High | `service_recall.go` — Qdrant vector search without cross-encoder |
| Memory export/import untested | Medium | `memory_export_import.go` — merge strategy unimplemented |
| Compaction execution untested | Medium | `compaction_execution.go` — no test coverage |
| Knowledge graph reasoning chains limited | Low | `knowledge_graph_reasoning.go` — basic inference only |
| No rate limiting on HUD API endpoints | Medium | `api_*.go` — open endpoints |
| Stream monitor event ordering not guaranteed under load | Low | `monitor/stream.go` |
| HUD spawn↔session silo | High | `spawn.svelte.ts` and `fleet.svelte.ts` share no cross-ref |
| Session hierarchy invisible in Fleet panel | Medium | `parent_session_id` never surfaced |
| cmd_sync.go at 880 lines | Medium | DEBT-067 — not started |

---

## Sprint 1: Foundation Hardening (Weeks 1–2)

**Theme:** Make the core loops rock-solid. Every agent session should start reliably, recall should be fast and observable, and quality gates should be automated.

### S1.1: Codex Keepalive Gap

**Problem:** Codex `notify` hook fires only on turn completion. Idle codex sessions lose presence after HeartbeatTTL. This morning's debugging showed only 1 of 3 codex sessions visible despite all 3 running.

**Solution:** Two-layer fix:
1. **Daemon-side session awareness** — When `ensure-session` heartbeat arrives after TTL expiry, treat it as a session resume (not a new session). Update presence + session `last_heartbeat` atomically.
2. **Client-side keepalive wrapper** — Add `loom agent keepalive-wrap` command that wraps any CLI subprocess and sends periodic heartbeats. Codex config evolves from bare `notify` to a lightweight keepalive wrapper.

**Files:**
- `pkg/agentcontext/svc_sessions_start.go` — session resume logic for stale-but-restarted agents
- `pkg/agentcontext/svc_presence.go` — Heartbeat TTL grace period for known agents
- `cmd/loom/cmd_agent_presence.go` — `keepalive-wrap` subcommand
- `pkg/generator/configs_codex.go` — update codex config template

**Acceptance:**
- Idle codex session stays visible for 2x HeartbeatTTL after last turn
- `loom agent keepalive-wrap codex --session-id X` sends periodic heartbeats
- All 3 codex sessions visible continuously

**Effort:** 2-3 days

---

### S1.2: Recall Quality — Parallel Backends + Observability

**Problem:** `service_recall.go` runs context, memory, and graph backends sequentially. Errors are silently swallowed. Agents have no visibility into degraded results.

**Note:** This was planned as M2 of the seamless integration plan but not yet fully shipped. The parallel recall infrastructure (`errgroup.Group`) was added in `cfb8f49e` but quality signals (`recall_meta`) need to be surfaced through the MCP tool response.

**Solution:**
1. Verify parallel recall is active (errgroup-based) — confirm in tests
2. Add `recall_meta` to tool response: `{backends_queried, backends_failed, total_candidates, returned, token_budget_used, latency_ms}`
3. Surface backend timeouts as `_warnings` in tool response
4. Add recall latency histogram metric (`agent_recall_duration_seconds{backend=...}`)

**Files:**
- `pkg/agentcontext/service_recall.go` — quality signals in response
- `cmd/mcp-agent-context/tools_context.go` — warning surfacing
- `pkg/agentcontext/seamless_integration_test.go` — recall meta tests

**Acceptance:**
- `agent_recall` response includes `recall_meta` with backend status
- One backend timeout → others return + warning visible
- Recall latency metric exported

**Effort:** 2-3 days

---

### S1.3: Auto-Quality-Gate Skill (Dark Factory #1)

**Problem:** Human approval gates in workflows are handholding points. The `feature-dev` and `bugfix` workflows have manual test/lint verification steps.

**Solution:** Implement `auto-quality-gate` skill that replaces human approval steps with automated fmt→lint→test→diff-check verification. This was designed in plan 78 (dark factory patterns) but not built.

**Deliverables:**
- Skill definition in `skills-registry.yaml` with cross-platform targeting
- Workflow step type `auto_verify` that runs `devbox_quality_gate` and auto-approves on green
- Max 3 retry cycles on failure with structured error report
- Works in `feature-dev`, `bugfix`, and `parallel-slice-ship` workflows

**Files:**
- `mcp/context/skills-registry.yaml` — skill entry
- `.agents/workflows/feature-dev.yaml` — replace approval step with auto_verify
- `.agents/workflows/bugfix.yaml` — same
- `pkg/agentcontext/workflow_executor.go` — auto_verify step type handler

**Acceptance:**
- Workflow step `auto_verify` runs quality gate without human intervention
- Green → auto-approve and continue. Red → 3 retries → fail with structured report
- Existing manual approval steps still work (backward compatible)

**Effort:** 2-3 days

---

### S1.4: Test Coverage for Untested Subsystems

**Problem:** Several core subsystems have zero test coverage: compaction execution, memory export/import, codebase sync, hybrid search.

**Solution:** Targeted test additions for highest-risk untested code:

| File | Risk | Tests to Add |
|------|------|-------------|
| `compaction_execution.go` | High — runs async, modifies session state | Compaction succeeds, compaction with errors, concurrent access |
| `memory_export_import.go` | Medium — merge strategy unimplemented | Export round-trip, import with skip/overwrite/merge strategies |
| `hybrid_search.go` | Medium — combines semantic + lexical | Combined scoring, empty results, one backend fails |
| `codebase_sync.go` | Low — rarely exercised | Sync creates entries, sync with existing entries |

**Acceptance:**
- 4 new test files, each with 3-5 test functions
- `go test -race ./pkg/agentcontext/...` clean
- Coverage for these files goes from 0% to >60%

**Effort:** 2-3 days

---

### Sprint 1 Summary

| Slice | Effort | Parallelizable | Dependencies |
|-------|--------|----------------|--------------|
| S1.1 Codex keepalive | 2-3d | Yes | None |
| S1.2 Recall quality | 2-3d | Yes (with S1.1) | None |
| S1.3 Auto-quality-gate | 2-3d | Yes (with S1.1, S1.2) | None |
| S1.4 Test coverage | 2-3d | Yes (with all) | None |

**All 4 slices are independent — ship as parallel worktrees.**

---

## Sprint 2: Observability & Catalog (Weeks 3–4)

**Theme:** Make the system self-aware. Operators should see where time is spent, easily discover/activate servers, and get automated session insights.

### S2.1: OTel Trace Explorer in HUD

**Problem:** Tool call latency is invisible. When a compound operation takes 30 seconds, there's no way to see if it's model inference, network, or a slow MCP server.

**Solution:** (From productivity unlocks plan F3)
1. Instrument tool call latency at proxy layer (request→server→response spans)
2. Instrument server spawn/restart lifecycle spans
3. Build HUD Traces panel with recent spans, latency breakdown, error highlighting
4. Add per-tool latency percentiles to HUD Overview metrics rail
5. Export to configured OTel collector endpoint

**What already exists:**
- `pkg/mcpotel` tracing wrappers on all MCP server handlers
- Daemon OTel runtime init and `loom/otel-status` reporting
- Server restart tracing spans

**Files:**
- `internal/daemon/callpipeline_stages.go` — add span around tool dispatch
- `internal/daemon/callpipeline_routing.go` — span around pool acquire + RPC
- `internal/hud/domain/traces/` — new domain handler for trace data
- `internal/hud/frontend/src/lib/components/TracesPanel.svelte` — new panel
- `internal/hud/app_routes_observability.go` — wire trace endpoints

**Acceptance:**
- `loomd` emits spans for every proxied tool call with server, tool, duration, status
- HUD Traces panel shows recent tool calls with waterfall visualization
- Per-tool P50/P95 latency visible in HUD metrics
- `OTEL_EXPORTER_OTLP_ENDPOINT` enables export to external collector

**Effort:** 4-5 days

---

### S2.2: Server Catalog CLI

**Problem:** With 47 servers and 500+ tools, discovering what's available and enabling new servers requires manual registry.yaml editing and `loom sync`.

**Solution:** (From productivity unlocks plan F2)
1. `loom catalog list` — browsable server catalog with status, tool count, required env
2. `loom catalog enable <server>` — one-command activation (adds to registry, env hints)
3. `loom catalog disable <server>` — clean removal
4. `loom catalog search <query>` — fuzzy search across server names, descriptions, tools
5. HUD catalog panel upgrade — browse, enable/disable, per-server health

**What already exists:**
- `loom://servers` resource with full server inventory
- HUD Server Catalog panel (framing done)
- `pkg/generator/registry.go` for reading registry

**Files:**
- `cmd/loom/cmd_catalog.go` — new CLI command group
- `pkg/catalog/` — catalog logic (search, enable/disable)
- `internal/hud/api_catalog.go` — extend catalog API
- `internal/hud/frontend/src/lib/components/ServerCatalogPanel.svelte` — upgrade

**Acceptance:**
- `loom catalog list` outputs table of all servers with status + tool count
- `loom catalog enable prometheus` adds server and triggers sync
- `loom catalog search "kubernetes"` returns matching servers/tools
- HUD catalog shows enable/disable toggles

**Effort:** 3-4 days

---

### S2.3: Session Retrospective Automation (Dark Factory #2)

**Problem:** Session summaries exist but nobody reads them. Institutional learning doesn't compound across sessions.

**Solution:** (From dark factory plan, slice 2)
1. Automated retro hook fires on session end
2. Extracts: failures, novel solutions, workflow friction
3. Writes structured findings to `.loom/local/retro-<session-id>.md`
4. Appends to rolling queue for batch human review
5. On-demand `session-retro` skill for pattern extraction

**Files:**
- `pkg/generator/configs_hooks.go` — `postSessionEnd_retrospective` hook extra
- `mcp/context/skills-registry.yaml` — `session-retro` skill
- `mcp/skills/session-retro/scripts/session-retro.sh` — extraction script
- `pkg/generator/platform_profiles.yaml` — add to claude/gemini extras

**Acceptance:**
- Session end triggers retro extraction (async, non-blocking)
- `.loom/local/retro-queue.md` accumulates actionable findings
- `/session-retro` skill available on-demand

**Effort:** 2-3 days

---

### S2.4: HUD UX Coherence — Spawn↔Session Bridge

**Problem:** (From plan 84, P0 gap #1) Spawn panel and Fleet panel share no cross-reference. A spawned agent creates a fleet session, but the two are only linked by `agent_id`. Operators can't navigate between them.

**Solution:**
1. Add `spawn_id` to session metadata (populated at spawn time)
2. Add cross-navigation links in both panels (spawn row → session detail, session row → spawn detail)
3. Surface session hierarchy (`parent_session_id`, `root_session_id`) in Fleet panel
4. Add "Source: Headless Spawn" badge to sessions created by spawns

**Files:**
- `internal/hud/spawn.go` — set `spawn_id` on session creation
- `internal/hud/frontend/src/lib/stores/fleet.svelte.ts` — add spawn cross-ref
- `internal/hud/frontend/src/lib/stores/spawn.svelte.ts` — add session cross-ref
- `internal/hud/frontend/src/lib/components/FleetPanel.svelte` — hierarchy columns
- `internal/hud/frontend/src/lib/components/SpawnPanel.svelte` — session link

**Acceptance:**
- Click spawn row → navigate to session detail
- Click session "Spawned" badge → navigate to spawn detail
- Subagent sessions show indented under parent in Fleet panel

**Effort:** 3-4 days

---

### Sprint 2 Summary

| Slice | Effort | Parallelizable | Dependencies |
|-------|--------|----------------|--------------|
| S2.1 OTel trace explorer | 4-5d | With S2.3 | None |
| S2.2 Server catalog CLI | 3-4d | With all | None |
| S2.3 Session retro | 2-3d | With all | None |
| S2.4 Spawn↔Session bridge | 3-4d | With S2.1 | None |

**S2.2 + S2.3 are fully independent of S2.1 + S2.4. Two parallel tracks.**

---

## Sprint 3: Orchestration & Autonomy (Weeks 5–6)

**Theme:** Enable compound queries and multi-turn headless agents. The platform becomes an active participant, not just a passive proxy.

### S3.1: Embedded Orchestra in Daemon

**Problem:** Orchestra exists as a standalone binary (`cmd/mcp-orchestra`). To use it, frontier agents make separate tool calls. Embedding it in `loomd` enables sub-second compound queries without network hops.

**Solution:** (From productivity unlocks F1, phases 2-3)
1. Embed orchestra router as a synthetic MCP server in `loomd`
2. Wire 4 specialized subagents with curated tool subsets:
   - **cluster-ops**: k8s, helm, flux, prometheus, grafana
   - **codebase**: git, codebase-memory, github/gitlab
   - **ci-pipeline**: gitlab CI, github-actions, docker
   - **agent-fleet**: agent-context, presence, spawn, session
3. YAML-driven subagent definitions in registry
4. Parallel subagent dispatch with compressed result assembly

**Prerequisites:**
- FlexInfer vLLM backend for Qwen3.5-35B-A3B (validate in Phase 0)
- If vLLM not ready, fall back to Qwen3-8B on MLC-LLM

**Files:**
- `internal/daemon/daemon_orchestra.go` — embedded orchestra lifecycle
- `internal/daemon/orchestra_subagents.go` — subagent dispatch
- `mcp/context/registry.yaml` — subagent definitions section
- `pkg/orchestra/` — existing router, extend for embedded mode

**Acceptance:**
- `orchestra_query("What's failing in CI?")` returns structured response in <5s
- `orchestra_cluster_status` returns health via parallel subagent dispatch
- Subagent definitions are YAML-driven
- Token metrics track local vs frontier usage

**Effort:** 5-7 days

---

### S3.2: Agent Recall Reranking

**Problem:** Recall returns keyword-matched results from Qdrant without semantic reranking. This leads to noisy context injection — agents get 60% relevant results.

**Solution:** (From productivity unlocks F4)
1. Add cross-encoder reranking step to `agent_recall` using FlexInfer BGE model
2. Configurable rerank depth (top-K candidates → reranked top-N)
3. Score threshold filtering to suppress low-relevance results
4. Metrics: rerank latency, result count before/after filtering
5. Graceful fallback to unranked results when FlexInfer unavailable

**Files:**
- `pkg/agentcontext/service_recall.go` — reranking pipeline stage
- `pkg/agentcontext/reranker.go` — new: FlexInfer cross-encoder client
- `pkg/agentcontext/config.go` — reranking config (enable, depth, threshold)
- `pkg/agentcontext/seamless_integration_test.go` — reranking tests

**Acceptance:**
- `agent_recall(query="...", scope="vector")` returns reranked results
- Reranking adds <200ms to recall latency
- Relevance improves (manual eval on 10 test queries: 60% → >85%)
- FlexInfer unavailable → fallback to unranked

**Effort:** 3-4 days

---

### S3.3: Multi-Turn SDK Driver for Headless Agents

**Problem:** Headless spawns are single-shot. The SDK drivers in `tools/spawn-driver/` support multi-turn but the HUD only sends one message per spawn. Operators can't follow up or redirect agents mid-run.

**Solution:**
1. Add `POST /api/agent/spawn/{id}/message` endpoint for follow-up messages
2. Wire SDK driver's multi-turn capability (already built in `tools/spawn-driver/`)
3. Add message input to SpawnDetailPanel in HUD
4. Add interrupt + redirect capability (send message that changes task direction)
5. Mobile API parity: `POST /api/mobile/v1/agent/spawn/{id}/message`

**Files:**
- `internal/hud/domain/spawn/handler_message.go` — new message endpoint
- `internal/hud/spawn.go` — pipe follow-up message to SDK driver
- `internal/hud/frontend/src/lib/components/SpawnDetailPanel.svelte` — message input
- `internal/hud/domain/mobile/handler_spawn.go` — mobile endpoint

**Acceptance:**
- Operator sends follow-up message to running Claude/Codex spawn
- Agent receives message and continues work
- Message appears in spawn activity feed
- Works from both web HUD and iOS app

**Effort:** 3-4 days

---

### S3.4: TDD-First Workflow (Dark Factory #3)

**Problem:** `feature-dev` workflow puts tests at step 5/10 — after implementation. Agents write code first, then retrofit tests.

**Solution:** (From dark factory plan, slice 1)
1. New `tdd-dev` skill + workflow
2. Steps: `init → worktree → recall → write-tests → verify-red → implement → verify-green → refactor → precommit → commit → push → end → cleanup`
3. `verify-red` and `verify-green` are automated tool steps (not human approval)
4. Instructions encode: "Write minimum tests. Run. MUST fail. Implement. Run. MUST pass."

**Files:**
- `mcp/context/skills-registry.yaml` — `tdd-dev` skill entry
- `.agents/workflows/tdd-dev.yaml` — workflow definition
- `mcp/skills/tdd-dev/` — references, templates

**Acceptance:**
- `/tdd-dev` available on Claude Code, Codex, Gemini
- Workflow enforces red→green cycle
- Auto-verify steps use `devbox_quality_gate`

**Effort:** 2-3 days

---

### Sprint 3 Summary

| Slice | Effort | Parallelizable | Dependencies |
|-------|--------|----------------|--------------|
| S3.1 Embedded orchestra | 5-7d | Lead slice | FlexInfer vLLM validation |
| S3.2 Recall reranking | 3-4d | With S3.3, S3.4 | FlexInfer BGE (already deployed) |
| S3.3 Multi-turn SDK | 3-4d | With S3.2 | None |
| S3.4 TDD workflow | 2-3d | With all | S1.3 (auto-quality-gate) |

**S3.2 + S3.3 + S3.4 can run in parallel. S3.1 is the critical path.**

---

## Sprint 4: Config Simplification & Platform Polish (Weeks 7–8)

**Theme:** Reduce operational burden. Simplify config, clean tech debt, and make the platform self-improving.

### S4.1: cmd_sync Split (DEBT-067)

**Problem:** `cmd/loom/cmd_sync.go` is 880 lines — the largest CLI file. Every platform change touches it. Hard to review and reason about.

**Solution:** (From tech debt cycle 6, wave 3)
1. Split into: `cmd_sync_generate.go`, `cmd_sync_sync.go`, `cmd_sync_pull.go`, `cmd_sync_backup.go`
2. `cmd_sync.go` becomes a small composition root (<200 lines)
3. Each file owns one subcommand family
4. No behavior changes — pure structural refactor

**Files:**
- `cmd/loom/cmd_sync.go` → split into 4 files + residual
- Tests updated to match new file locations

**Acceptance:**
- `cmd_sync.go` reduced from 880 to <200 lines
- `loom sync status`, `loom sync <platform>`, `loom sync generate` all work identically
- All existing tests pass

**Effort:** 2-3 days

---

### S4.2: Data-Driven Platform Profiles

**Problem:** Adding a new platform requires Go code changes in `pkg/generator/`. Platform-specific logic is scattered across imperative code instead of declarative YAML.

**Solution:** (From productivity unlocks F5)
1. Extract platform profile definitions from Go code into YAML/JSON schema
2. Each platform profile declares: config format, hook events, tool schema, sync paths
3. Validate profiles with schema-driven checks
4. Add `loom sync diff` to preview changes before applying
5. New platform support = YAML addition only

**Files:**
- `pkg/generator/platform_profiles.yaml` — extend with full profile schema
- `pkg/generator/profile_loader.go` — new: load + validate profiles from YAML
- `pkg/generator/configs_*.go` — refactor to consume profile data
- `cmd/loom/cmd_sync.go` (or `cmd_sync_sync.go` post-S4.1) — `diff` subcommand

**Acceptance:**
- `loom sync diff codex` shows preview of what would change
- New platform profile is purely YAML (no Go code)
- Existing 9 platform syncs produce identical output

**Effort:** 4-5 days

---

### S4.3: Structured Recipe Library (Dark Factory #4)

**Problem:** Agent memories are unstructured text. When an agent solves a novel problem, the solution is prose. Future agents get fuzzy matches, not deterministic answers.

**Solution:** (From dark factory plan, slice 3)
1. New MCP tools: `agent_recipe_add`, `agent_recipe_recall`, `agent_recipe_list`
2. Recipe schema: `{problem, solution, proof, tags, created_by, verified}`
3. Proof field requires: file reference, test command, or URL
4. Recall uses exact tag match + semantic fallback
5. Integration with `agent_recall` — recipes returned as high-priority results

**Files:**
- `pkg/agentcontext/svc_recipes.go` — recipe CRUD
- `pkg/agentcontext/schema_recipes.go` — recipe types
- `cmd/mcp-agent-context/tools_recipes.go` — MCP tool wrappers
- `mcp/context/skills-registry.yaml` — `agent-recipes` skill

**Acceptance:**
- Agent records recipe with proof (test command or file ref)
- Future agent searching same problem class gets deterministic answer
- Recipes appear in `agent_recall` results with high priority

**Effort:** 3-4 days

---

### S4.4: Platform Enforcement Testing

**Problem:** Universal proxy policies exist in YAML but enforcement hasn't been tested across all 8 platforms. Policy violations on less-tested platforms (Kilocode, Antigravity, Zed) could go undetected.

**Solution:**
1. Contract test suite: for each platform × policy combination, verify enforcement
2. Test matrix: 9 platforms × 3 policy types (gitops, RBAC, resource limits)
3. Golden-file assertion: generated configs for each platform match expected output
4. CI gate: policy enforcement tests run on every registry.yaml change

**Files:**
- `internal/contracts/policy_enforcement_test.go` — new contract test suite
- `internal/contracts/golden/` — per-platform golden files
- `.gitlab-ci.yml` — add policy enforcement job

**Acceptance:**
- All 9 platforms × 3 policies have contract tests
- Golden files regenerated on `loom sync generate`
- CI fails if generated config drifts from golden

**Effort:** 2-3 days

---

### Sprint 4 Summary

| Slice | Effort | Parallelizable | Dependencies |
|-------|--------|----------------|--------------|
| S4.1 cmd_sync split | 2-3d | Yes | None |
| S4.2 Data-driven profiles | 4-5d | After S4.1 | S4.1 (split first, then refactor) |
| S4.3 Recipe library | 3-4d | With S4.1 | None |
| S4.4 Policy enforcement | 2-3d | With all | None |

**S4.1 → S4.2 is sequential. S4.3 + S4.4 are independent of both.**

---

## Cross-Sprint Metrics

| Metric | Current | After S1 | After S2 | After S3 | After S4 |
|--------|---------|----------|----------|----------|----------|
| Test coverage (agentcontext) | ~40.7% | ~50% | ~52% | ~55% | ~58% |
| Recall relevance (manual eval) | ~60% | ~65% | ~65% | >85% | >85% |
| Tool call latency visibility | None | None | P50/P95 in HUD | P50/P95 + waterfall | Same |
| Server activation time | ~5 min manual | Same | <30s CLI | Same | Same |
| Compound query latency | N/A (5-8 sequential) | Same | Same | <5s single call | Same |
| Platform config change effort | ~20 min Go code | Same | Same | Same | ~2 min YAML |
| Codex idle session visibility | Offline after TTL | Visible continuously | Same | Same | Same |
| Human approval gates | 3 per workflow | 1 (quality gate auto) | 0 (session retro auto) | 0 + TDD cycle | Same |

---

## Risk Register

| Risk | Sprint | Probability | Mitigation |
|------|--------|-------------|------------|
| FlexInfer vLLM for Qwen3.5 not ready | S3 | Medium | Fall back to Qwen3-8B on MLC-LLM |
| 24GB VRAM budget exceeded with concurrent models | S3 | Low | Serverless scale-to-zero between requests |
| cmd_sync split breaks existing platform behavior | S4 | Low | Golden-file regression tests |
| HUD frontend changes create merge conflicts | S2 | Medium | Separate branches for backend vs frontend slices |
| Cross-encoder reranking adds unacceptable latency | S3 | Low | Configurable timeout + fallback to unranked |

---

## Execution Strategy

### Parallel Tracks

```
Sprint 1 (Foundation):
  Track A: S1.1 (codex keepalive) + S1.2 (recall quality)
  Track B: S1.3 (auto-quality-gate) + S1.4 (test coverage)

Sprint 2 (Observability):
  Track A: S2.1 (OTel traces) + S2.4 (spawn↔session bridge)
  Track B: S2.2 (server catalog) + S2.3 (session retro)

Sprint 3 (Autonomy):
  Track A: S3.1 (embedded orchestra) — critical path, dedicated
  Track B: S3.2 (recall reranking) + S3.3 (multi-turn SDK) + S3.4 (TDD workflow)

Sprint 4 (Simplification):
  Track A: S4.1 (sync split) → S4.2 (data-driven profiles) — sequential
  Track B: S4.3 (recipe library) + S4.4 (policy enforcement) — independent
```

### Per-Sprint Ship Pattern

1. Implement changes in worktree branches
2. Run `devbox_quality_gate` (fmt → lint → test)
3. Run `go test -race ./pkg/agentcontext/...` for concurrency safety
4. Self-review via `pr-self-review` checklist
5. Commit, push, create MR, auto-merge
6. Deploy to k3s: `make deploy`
7. Verify in HUD + mobile app
8. Clean up worktrees + branches

---

## Sources

- Previous plan (seamless integration): `.claude/plans/dazzling-roaming-fern.md` — M1-M4 status
- Productivity unlocks: `.loom/73-planning-productivity-unlocks-2026-04-03.md` — F1-F5 ranking
- Dark factory patterns: `.loom/78-plan-dark-factory-patterns-2026-04-05.md` — slices 1-3
- Tech debt cycle 6: `.loom/tech-debt-plan-cycle6.md` — DEBT-067 status
- HUD UX polish: `.loom/84-plan-hud-ux-polish-2026-04-11.md` — P0 gaps
- HUD robustness: `.loom/85-plan-hud-robustness-2026-04-12.md` — complete
- Next-gen orchestration: `.loom/64-planning-next-gen-skills-agents-orchestration-2026-03-29.md`
- Headless agent full-stack: `.loom/82-plan-headless-agent-fullstack-2026-04-07.md`
- Codebase exploration: 3 exploration agents (commit history, pkg/agentcontext, internal/hud, internal/daemon)
- Git log: `a19f765b..b79e0774` (recent commits)
- Daemon log: `/tmp/loomd.log` (April 14, 2026)
- Presence verification: `curl -sk -H "Host: hud.flexinfer.ai" https://192.168.50.227/api/agents`
