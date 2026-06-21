# Implementation Plan — Loom Plan Store

- **Status**: Draft
- **Date**: 2026-06-20
- **Spec**: [160-product-spec-loom-plan-store-2026-06-20.md](160-product-spec-loom-plan-store-2026-06-20.md)
- **Sequencing rule**: Slice 1 is the riskiest-assumption kill-test and **gates everything**. No unification work (Slice 7) commits until Slices 1–2 are proven and Slice 6 lands the lifecycle view.

## Dependency graph

```
S0 (cleanup) ──────────────── independent, parallel-safe
S1 (kill-test) ── gates ──▶ S2 (full schema + tools)
                              ├─▶ S3 (.md mirror + plan-skill rewire)
                              ├─▶ S4 (parallel-slice-ship rewire + claim enforce)
                              ├─▶ S5 (plan-aware handoffs)
                              └─▶ S6 (lifecycle + HUD)  ──┐
S2 ─────────────────────────────────────▶ S7 (Mills full unification) ◀── needs S6
S3,S4,S5,S6 ────────────────────────────▶ S8 (cross-platform sync)
```

S3 / S4 / S5 / S6 are mutually independent once S2 lands → good `parallel-slice-ship` candidates (and a dogfood of the very feature). Each owns a disjoint file set (listed per slice).

---

## Slice 0 — `.loom` hygiene cleanup (independent)

**Goal**: fix the storage inconsistencies found in research so the mirror in S3 lands on clean ground.

- Reconcile `CLAUDE.md:29` with reality (or add the repo `.gitignore` rules it claims).
- `git rm --cached .loom/00-workspace-snapshot.md .loom/50-worklog.md` (policy-gitignored but tracked → ship stale into worktrees).
- Decide + apply policy on the 21 tracked `.loom/archive/roadmap-reconciliations/` files.
- Add `templates/40-decisions.md` to `skills-registry.yaml:700-705` assets.

**Files**: `CLAUDE.md`, `.gitignore`, `mcp/context/skills-registry.yaml`, tracked `.loom/*` removals.
**Verify**: `git check-ignore` + `git ls-files .loom/` match the documented policy.

---

## Slice 1 — Plan entity MVP + kill-test (GATE)

**Goal**: build the *minimum* to run the riskiest-assumption kill-test, nothing more.

- `Plan` struct (MVP fields: id, slug, title, project, namespace, phase, spec_doc, slices-as-inline-list, created_by/at) in `pkg/agentcontext/schema_plan.go`.
- `CollPlans = "agent_plans_v1"` constant + registry client in `pkg/agentcontext/qdrant_registry.go`.
- `pkg/agentcontext/svc_plans.go`: `Create`, `Get` — **recall filters by `plan_id`/`project`, never `agent_id`** (the explicit fix vs `service_recall.go:316-327`).
- Tools `agent_plan_create`, `agent_plan_get` registered in `cmd/mcp-agent-context/tools.go:18`.
- **Verify Mills-pod reachability**: confirm spawned pods have the loom MCP proxy + Qdrant route (check the pod MCP config the spawn path injects; this is the unproven leg).

**Kill-test** (the completion criterion — run live, all 5 steps from the spec): create from main; `get` from (2) a fresh Claude worktree subagent, (3) a Codex session, (4) a Mills pod agent; (5) prove non-agent-scoped recall.
**Files**: `pkg/agentcontext/schema_plan.go`, `svc_plans.go`, `qdrant_registry.go`, `cmd/mcp-agent-context/tools.go`, `tools_plan.go`.
**Exit**: kill-test PASS recorded in this doc with evidence. **If FAIL → stop; redesign reachability before any further slice.**

**Status 2026-06-20 — code complete, core legs PASS.** Implemented: `Plan`/`PlanSlice` (`pkg/agentcontext/schema_plan.go`), `PlanSvc` create/get/list — Qdrant-first, non-agent-scoped (`pkg/agentcontext/svc_plans.go`), collection `agent_plans_v1` + keyword indexes + config (`qdrant_registry.go`, `qdrant_indexes.go`, `config.go`), wired into `service.go`, tools `agent_plan_create/get/list` (`cmd/mcp-agent-context/tools_plan.go` + `tools.go`). Tests: unit (`svc_plans_test.go`) + live cross-process real-Qdrant kill-test (`svc_plans_killtest_test.go`, gated by `RUN_PLAN_STORE_IT=1`) — **PASS**. Full `pkg/agentcontext` suite green; `go vet` + `gofmt` clean. **Deferred to an operator-gated deploy step** (restarts the shared daemon → affects the live fleet): the live Codex + Mills-pod legs.

---

## Slice 2 — Full schema + tool family + events

**Goal**: the store-canonical core.

- Full `Plan` + `Slice` schema (spec §"Design overview"), `agent_plan_slices_v1` collection.
- Tool family: `agent_plan_create/update/get/list/search`, `agent_plan_slice_add/update/get/list/claim`, `agent_plan_lifecycle_advance` (validated transitions + event emit on the ring-buffer/SSE channel).
- Semantic recall (embed title+spec); reuse `ResilientEmbedder` so embed outages degrade gracefully (per Morph-outage fix).
- `>80%` coverage incl. a **cross-worktree recall regression test**.

**Files**: `pkg/agentcontext/schema_plan.go`, `svc_plans.go`, `svc_plan_slices.go`, `tools_plan.go`, `*_test.go`.

---

## Slice 3 — `.md` mirror + plan-skill rewrite (parallel after S2)

**Goal**: store→file projection + migrate doc-writing skills to the store.

- `agent_plan_render`: atomic (`writeFileAtomic`) render to `.loom/<NNN>-plan-<slug>-<date>.md`; auto-render on mutation; re-render into the worktree at slice time.
- Rewrite `plan-loom-core` skill (`mcp/skills/plan-loom-core/`, registry block `skills-registry.yaml:610-727`) to **read/write through the store** (plan_id-addressed), emitting the `.md` as the review mirror.
- Migrate `research`, `brainstorm`, `decision-journal` skills to record into the plan/slice store.

**Files**: `pkg/agentcontext/svc_plan_render.go`, `mcp/skills/plan-loom-core/*`, `mcp/context/skills-registry.yaml`.

---

## Slice 4 — parallel-slice-ship rewire + claim enforcement (parallel after S2)

**Goal**: true parallel shipping driven by the store.

- `parallel-slice-ship` orchestrator persists the slice decomposition to the store (Phase 1 → `agent_plan_create` + `agent_plan_slice_add`).
- Spawn `slice-implementer` with **only `plan_id`+`slice_id`** (not the full plan); fix the skill→agent mismatch (skill spawns `general-purpose`, not `slice-implementer`).
- `slice-implementer` contract: `agent_plan_slice_get` → implement → `agent_plan_slice_update` (status + decisions/blockers anchored to the slice).
- **Claim enforcement**: claiming a slice converts its `files[]` to hard claims; `agent_file_claim_acquire` hard-rejects cross-slice collisions (upgrade `svc_claims.go:50-130`, policy-gated).

**Files**: `mcp/context/skills-registry.yaml` (parallel-slice-ship), `.claude/agents/slice-implementer.md`, `pkg/agentcontext/svc_claims.go`, `svc_plan_slices.go`.

---

## Slice 5 — Plan-aware handoffs (parallel after S2)

**Goal**: hand off a plan scope, cross-vendor.

- `agent_handoff_create` gains `plan_id`/`slice_id`; `agent_handoff_inbox`/`accept` surface plan context.
- Accepting resumes a known plan+slice (no `entry_ids` reconstruction). Real semantic vector (drop the dummy-vector at `service_handoffs.go:113`).

**Files**: `pkg/agentcontext/service_handoffs.go`, `schema.go` (Handoff), `tools` handoff registration.

---

## Slice 6 — Lifecycle + HUD (parallel after S2; gates S7)

**Goal**: the reviewable plan→merge→deploy view the operator asked for.

- Plan/slice phase transitions populate MR/pipeline/deploy refs (hook into existing `PipelineRef` + commit trailers `Agent-ID`/`Agent-Session`).
- HUD "Plans" card (`internal/hud/`): each plan/slice across all phases, tied to root-session; remember HUD `dist` is `go:embed`'d → `make hud-frontend` + commit.

**Files**: `internal/hud/domain/plans/*`, `internal/hud/frontend/src/lib/components/Plans/*`, `internal/hud/app_routes_*.go`, `pkg/agentcontext/svc_plans.go` (event emit).

---

## Slice 7 — Mills full unification (largest; gated on S2 + S6)

**Goal**: one work unit factory-wide.

- Store adapter mapping Mills `BacklogItem` ↔ `Plan` (`pkg/mills/store/`); `backlogPromptContext` (`main.go:985`) reads plan+slice **content** from the store, not a file path.
- Council `backlog_mutator.go` + `gitlab_importer.go` write Plans.
- `agent_task` gains `plan_id`/`slice_id` (`schema.go:310`); slice = work unit, task = TODO under a slice.
- **Migration**: backfill existing `backlog_items` → Plans; dual-write/verify window before cutover. Keep Mills SQLite as the run/stage ledger; the *backlog/spec* is the converged plan.

**Files**: `pkg/mills/store/*`, `cmd/loom-mills-operator/main.go`, `pkg/mills/council/backlog_mutator.go`, `pkg/mills/intake/gitlab_importer.go`, `pkg/agentcontext/schema.go` (Task).
**Risk**: highest blast radius — dual-write + canary one backlog item end-to-end before flipping council/importer writers.

---

## Slice 8 — Cross-platform sync

**Goal**: identical behavior across Codex / Claude / Gemini / Kilocode.

- Regenerate `skills-registry.yaml` so all plan-aware skills propagate; `loom sync <platform> --regen`; keep `platform/gitops` mirror in sync.
- Verify Codex `[agents]` parallel path + Mills harness all resolve plans by id.

**Files**: `mcp/context/skills-registry.yaml`, generated configs, `platform/gitops/mcp/context/*`.

---

## Per-slice quality gate

Each slice: `devbox_quality_gate(project="loom-core")` (fmt→lint→test), `>80%` coverage on new code, regression test for any fix, conventional commit, MR with auto-merge, CI to green. Ship per the workspace auto-ship policy.

## Open questions to resolve during S1/S2

1. **Slices: embedded vs own collection.** MVP inlines slices in the Plan; S2 promotes to `agent_plan_slices_v1` for independent claim/status updates. Confirm at S2 boundary.
2. **`plan_id` ↔ Mills `BacklogItem.ID` format reconciliation** (`gl-<project>-<iid>` vs `plan-<slug>-<short>`) — decide the canonical id scheme before S7 migration.
3. **Numbered-doc collisions** in `.loom/` (existing dup `31-`,`32-`,`102-`): the renderer should allocate the next free number atomically.
