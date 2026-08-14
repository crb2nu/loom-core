# Implementation Plan — Loom Plan Store

- **Status**: ✅ **DONE** — S0–S8 merged to `main`; S7b (council/importer plan authoring + backlog backfill) **complete and live-verified**: S7b-α backfill + S7b-β importer born-link + S7b-γ council born-link all landed; the **live operator-gated canary PASSED 2026-06-26** ([gitops!291](https://gitlab.flexinfer.ai/platform/gitops/-/merge_requests/291)). Reconciled 2026-06-26.
- **Date**: 2026-06-20 (status reconciled 2026-06-26)
- **Spec**: [160-product-spec-loom-plan-store-2026-06-20.md](160-product-spec-loom-plan-store-2026-06-20.md)
- **Sequencing rule**: Slice 1 is the riskiest-assumption kill-test and **gates everything**. No unification work (Slice 7) commits until Slices 1–2 are proven and Slice 6 lands the lifecycle view.

## Shipped status (reconciled 2026-06-25)

The epic is functionally complete; the riskiest-assumption kill-test **PASSED live**. Per-slice landings:

| Slice | Status | Evidence |
|-------|--------|----------|
| S0 — `.loom` hygiene | ✅ merged | `6fab24c5` |
| S1 — Plan entity MVP + kill-test (GATE) | ✅ merged; gate PASSED | `81d50941` (data-model 2026-06-20); live legs proven by `69768b7e`/`feba82e5` |
| S2 — Full schema + tools + events | ✅ merged | `ee090396` |
| S3 — `.md` mirror + plan-skill rewire | ✅ merged | `9aa8bddd` |
| S4 — parallel-slice-ship rewire + claim enforce | ✅ merged | `461ee317` |
| S5 — Plan-aware handoffs | ✅ merged | `75329f2c` |
| S6 — Lifecycle + HUD | ✅ merged | `ab26f64a` (read API) + `1bb052a9` (HUD Plans panel) |
| S7 — Mills unification (links + read-through) | ✅ merged; **live-verified** | `4578ec3b`; **S7b complete** — α backfill + β importer born-link + γ council born-link all shipped 2026-06-25; **live canary PASSED 2026-06-26** ([gitops!291](https://gitlab.flexinfer.ai/platform/gitops/-/merge_requests/291)) |
| S8 — Cross-platform sync | ✅ propagated | registry plan-aware (S3/S4); `platform/gitops` mirror in sync; HOME skills for claude/codex/gemini/kilocode carry the plan-store-aware `plan-loom-core` + `parallel-slice-ship` (verified 2026-06-25) |

**Cross-vendor reachability hardening** (beyond the original slice list, required to make S1's live legs pass): proxy `llm-core`/`antigravity-core` profiles now expose `agent_plan_*` (`69768b7e`, [!785]); Mills spawn pods reach the store via `loom proxy --ws-backend` + bundled `loom` binary (`feba82e5`, [!786]); mobile-hud spawn orchestrator wires `SPAWN_LOOM_IMAGE` + plan-store (`b01d16ae`).

**Remaining: NONE — epic closed out.** The final open item, the live operator-gated canary, **PASSED 2026-06-26**. [gitops!291](https://gitlab.flexinfer.ai/platform/gitops/-/merge_requests/291) set `LOOM_MILLS_PLAN_AUTHORING=1` + `LOOM_MILLS_PLAN_BACKFILL=1` on the `loom-mills-operator` Deployment; Flux rolled the pod (`...568567bd7c`, boot 13:51Z). Verified live:
- **5/5 backfill links** succeeded over the hub (`plan backfill complete linked=5 scanned=5`); the operator authored each Plan via `agent_plan_create` with `?server=agent_context`.
- `GET /api/mills/backlog` now shows **5/5 items carrying `PlanID`** (0 empty).
- Cross-process resolution proven: the in-cluster operator authored, and a separate **claude-code** session resolved `agent_plan_get{plan-mills-mills-debt-ticklabel-20260625-005042}` → full Plan (phase, spec_doc, `created_by=loom-mills-operator`, `mills_backlog_id` round-trip, 1 slice).
- `council inline plan authoring enabled` + `gitlab importer inline plan authoring enabled` → all new backlog now born-links forward.

**Key finding (corrected a wrong assumption):** the `agent_plan_*` tools are absent from every `always_allow` list (canonical registry, gitops mirror, gateway configmap), but this is **harmless** — the operator pins `?server=agent_context`, and the fi-mcp-gateway client path (`pkg/gateway/hub.go`) relays `tools/call` verbatim to that host with **no per-tool filter**. `always_allow` is a downstream-client auto-approve concept, not a hub forwarding gate for `server=`-pinned clients. No registry/configmap change was needed — only the operator flag flip. (`LOOM_MILLS_PLAN_BACKFILL` is idempotent — skips already-linked — so it is safe to leave set; the one-time historical pass is done.)

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

**Status 2026-06-24 — DONE.** Repo `.gitignore` now carries the `.loom/local/`, `.loom/archive/`, `00-workspace-snapshot.md`, `50-worklog.md` rules (resolves the false `AGENTS.md:29` claim that they already existed). `git rm --cached` untracked the 2 auto-gen files + all 22 `.loom/archive/roadmap-reconciliations/*` files (kept on disk, history retained — policy marks `.loom/archive/*` local-only). `templates/40-decisions.md` added to the `plan-loom-core` skills-registry `assets:` block (the template file already existed on disk but was unlisted). Verified: `git check-ignore` reports all 4 paths ignored; `git ls-files .loom/archive` empty.

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

**Status — SHIPPED** (`ee090396`). Rich schema aligned with Mills `BacklogItem`; slices promoted to `agent_plan_slices_v1`; validated lifecycle transitions in `phase_history`; best-effort `agent_plan_search` (deterministic fallback vector so a failed embedder never blocks a write). Live cross-process kill-test re-run PASS with the slice collection.

---

## Slice 3 — `.md` mirror + plan-skill rewrite (parallel after S2)

**Goal**: store→file projection + migrate doc-writing skills to the store.

- `agent_plan_render`: atomic (`writeFileAtomic`) render to `.loom/<NNN>-plan-<slug>-<date>.md`; auto-render on mutation; re-render into the worktree at slice time.
- Rewrite `plan-loom-core` skill (`mcp/skills/plan-loom-core/`, registry block `skills-registry.yaml:610-727`) to **read/write through the store** (plan_id-addressed), emitting the `.md` as the review mirror.
- Migrate `research`, `brainstorm`, `decision-journal` skills to record into the plan/slice store.

**Files**: `pkg/agentcontext/svc_plan_render.go`, `mcp/skills/plan-loom-core/*`, `mcp/context/skills-registry.yaml`.

**Status — SHIPPED** (`9aa8bddd`). `agent_plan_render{plan_id, path}` projects the canonical store to a reviewable `.loom/*.md` mirror via `writeFileAtomic` (no partial reads for fs watchers), records `mirror_path`; pure `renderPlanMarkdown` is unit-tested. `plan-loom-core` skill rewritten store-first (create/render/edit/recall by `plan_id`).

---

## Slice 4 — parallel-slice-ship rewire + claim enforcement (parallel after S2)

**Goal**: true parallel shipping driven by the store.

- `parallel-slice-ship` orchestrator persists the slice decomposition to the store (Phase 1 → `agent_plan_create` + `agent_plan_slice_add`).
- Spawn `slice-implementer` with **only `plan_id`+`slice_id`** (not the full plan); fix the skill→agent mismatch (skill spawns `general-purpose`, not `slice-implementer`).
- `slice-implementer` contract: `agent_plan_slice_get` → implement → `agent_plan_slice_update` (status + decisions/blockers anchored to the slice).
- **Claim enforcement**: claiming a slice converts its `files[]` to hard claims; `agent_file_claim_acquire` hard-rejects cross-slice collisions (upgrade `svc_claims.go:50-130`, policy-gated).

**Files**: `mcp/context/skills-registry.yaml` (parallel-slice-ship), `.claude/agents/slice-implementer.md`, `pkg/agentcontext/svc_claims.go`, `svc_plan_slices.go`.

**Status — SHIPPED** (`461ee317`). `agent_file_claim_acquire` gains `enforce` (default false = unchanged advisory); `ClaimSvc.AcquireEnforced` claims a file set all-or-nothing; `agent_plan_slice_claim` hard-claims the slice's `files` so two slices sharing a file cannot both proceed (second refused with `conflicting_files`, no half-claim). `slice-implementer` agent + `parallel-slice-ship` skill rewritten store-first (spawn with only `plan_id`+`slice_id`).

---

## Slice 5 — Plan-aware handoffs (parallel after S2)

**Goal**: hand off a plan scope, cross-vendor.

- `agent_handoff_create` gains `plan_id`/`slice_id`; `agent_handoff_inbox`/`accept` surface plan context.
- Accepting resumes a known plan+slice (no `entry_ids` reconstruction). Real semantic vector (drop the dummy-vector at `service_handoffs.go:113`).

**Files**: `pkg/agentcontext/service_handoffs.go`, `schema.go` (Handoff), `tools` handoff registration.

**Status — SHIPPED** (`75329f2c`). `agent_handoff_create` accepts optional `plan_id`/`slice_id`; round-tripped on the payload and surfaced by `agent_handoff_accept` (with `resume_hint`) + `agent_handoff_inbox` so the receiver resumes a known plan scope by id (cross-vendor Claude ↔ Codex ↔ Mills). Plain handoffs unchanged (backward compatible).

---

## Slice 6 — Lifecycle + HUD (parallel after S2; gates S7)

**Goal**: the reviewable plan→merge→deploy view the operator asked for.

- Plan/slice phase transitions populate MR/pipeline/deploy refs (hook into existing `PipelineRef` + commit trailers `Agent-ID`/`Agent-Session`).
- HUD "Plans" card (`internal/hud/`): each plan/slice across all phases, tied to root-session; remember HUD `dist` is `go:embed`'d → `make hud-frontend` + commit.

**Files**: `internal/hud/domain/plans/*`, `internal/hud/frontend/src/lib/components/Plans/*`, `internal/hud/app_routes_*.go`, `pkg/agentcontext/svc_plans.go` (event emit).

**Status — SHIPPED** (`ab26f64a` read API + `1bb052a9` management panel). `GET /api/plans` + `GET /api/plans/{id}` (deliberately not agent-scoped) expose the lifecycle with MR/pipeline/deploy refs + `phase_history`; `POST /api/plans` + `POST /api/plans/{id}/advance` (illegal transition → 422, undeployed daemon → 503). HUD **Work → Plans** Svelte board groups plans by phase with create form, per-plan advance, and slice detail drawer; degrades cleanly to a "deploy pending" state on an older daemon. Verified live after a loomd rebuild+restart.

---

## Slice 7 — Mills full unification (largest; gated on S2 + S6)

**Goal**: one work unit factory-wide.

- Store adapter mapping Mills `BacklogItem` ↔ `Plan` (`pkg/mills/store/`); `backlogPromptContext` (`main.go:985`) reads plan+slice **content** from the store, not a file path.
- Council `backlog_mutator.go` + `gitlab_importer.go` write Plans.
- `agent_task` gains `plan_id`/`slice_id` (`schema.go:310`); slice = work unit, task = TODO under a slice.
- **Migration**: backfill existing `backlog_items` → Plans; dual-write/verify window before cutover. Keep Mills SQLite as the run/stage ledger; the *backlog/spec* is the converged plan.

**Files**: `pkg/mills/store/*`, `cmd/loom-mills-operator/main.go`, `pkg/mills/council/backlog_mutator.go`, `pkg/mills/intake/gitlab_importer.go`, `pkg/agentcontext/schema.go` (Task).
**Risk**: highest blast radius — dual-write + canary one backlog item end-to-end before flipping council/importer writers.

**Status — PARTIALLY SHIPPED** (`4578ec3b`). Links + read-through landed: `agent_task` gains `plan_id`/`slice_id` (Qdrant payload + keyword-indexed) so `plan → slice → task`; Mills `BacklogItem` gains a `plan_id` column (migration `005_backlog_plan_id.sql`); when set, `backlogPromptContext` instructs the spawned agent to resolve the **live** plan + slices via `agent_plan_get{plan_id}` instead of a stale `.loom` SpecDoc. **S7b-α SHIPPED 2026-06-25** (default-off backfill): `clients.PlanClient.AuthorPlan` (`agent_plan_create` via the MCP hub) + pure `backlogItemToPlanArgs` mapping (round-trips `mills_backlog_id`/`gitlab_issue_iid`/slices; deterministic `plan-mills-<id>`) + `intake.PlanBackfiller` (list → author → stamp `PlanID` → Put; skip-already-linked; best-effort) + operator boot pass gated by `LOOM_MILLS_PLAN_BACKFILL` (unset = zero behavior change, off the reconciler hot path). Unit-tested; live canary (set the flag, restart the operator, verify plans authored) is operator-gated. **S7b-β SHIPPED 2026-06-25** (born-linked import): `intake.GitLabImporter.Tick` authors a Plan for each newly imported item before its first `Put` (one write, stamped `plan_id`) when `LOOM_MILLS_PLAN_AUTHORING` is set + the hub is reachable; best-effort (author failure → item still imports unlinked, backfill links it later), off the reconciler hot path. **S7b-γ SHIPPED 2026-06-25** (council born-link): the same inline authoring at `council.persistOne` — `BacklogMutator` gains optional `PlanAuthor`/`Project`/`Logger` (local `PlanAuthor` interface keeps `council` decoupled from `intake`); `persistOne` authors a Plan before its `Put` (`maybeAuthorPlan`), best-effort. Operator wires `councilRunner.Mutator` post-hub (the runner is built before the hub) under the same `LOOM_MILLS_PLAN_AUTHORING` + hub-reachable gate. **Both Mills create sites now born-link new backlog; S7b complete** (live operator-gated canary — set the flag, restart the operator, run a council pass + import, verify `plan_id` stamped — pending).

---

## Slice 8 — Cross-platform sync

**Goal**: identical behavior across Codex / Claude / Gemini / Kilocode.

- Regenerate `skills-registry.yaml` so all plan-aware skills propagate; `loom sync <platform> --regen`; keep `platform/gitops` mirror in sync.
- Verify Codex `[agents]` parallel path + Mills harness all resolve plans by id.

**Files**: `mcp/context/skills-registry.yaml`, generated configs, `platform/gitops/mcp/context/*`.

**Status — SHIPPED / propagated** (verified 2026-06-25). The source `skills-registry.yaml` is plan-aware (rewritten in S3/S4); the `platform/gitops/mcp/context/skills-registry.yaml` mirror is byte-identical (`diff -q` clean). HOME skills for **claude, codex, gemini, kilocode** all carry the plan-store-aware `plan-loom-core` + `parallel-slice-ship`, so a fresh session on any vendor resolves plans by id. Cross-vendor resolution proven live (codex `agent_plan_get` byte-identical after `69768b7e`/[!785]; Mills-pod WS transport via `feba82e5`/[!786]). Note: repo-local generated skill files (`.claude/commands/`, `.kilocode/rules/`) are gitignored, home-only artifacts and are *not* refreshed by `loom sync skills --repo-only`; the consuming path is HOME, which is current.

---

## Per-slice quality gate

Each slice: `devbox_quality_gate(project="loom-core")` (fmt→lint→test), `>80%` coverage on new code, regression test for any fix, conventional commit, MR with auto-merge, CI to green. Ship per the workspace auto-ship policy.

## Open questions to resolve during S1/S2

1. **Slices: embedded vs own collection.** MVP inlines slices in the Plan; S2 promotes to `agent_plan_slices_v1` for independent claim/status updates. Confirm at S2 boundary.
2. **`plan_id` ↔ Mills `BacklogItem.ID` format reconciliation** (`gl-<project>-<iid>` vs `plan-<slug>-<short>`) — decide the canonical id scheme before S7 migration.
3. **Numbered-doc collisions** in `.loom/` (existing dup `31-`,`32-`,`102-`): the renderer should allocate the next free number atomically.
