# Implementation Plan: Engram Tech Tree

> Date: 2026-05-08
> Status: Draft
> Companion docs: [113-research](.loom/113-research-engram-tech-tree-2026-05-08.md), [114-product-spec](.loom/114-product-spec-engram-tech-tree-2026-05-08.md)

---

## 1. Strategy

Land as **three additive slices** on top of the existing `agent_recipe_*` infrastructure. Every slice is independently shippable and leaves the system better than it was; no big-bang merge. Old recipe call sites keep working at every step.

| Slice | Theme | Risk | Roughly | Ships when |
|------:|-------|------|---------|-----------|
| **S1** | Schema extension + back-compat shim | Low | 1-2 days | Recipes carry tier/family/prereqs; old calls unchanged |
| **S2** | Graph-aware recall + `agent_engram_*` tools | Medium | 2-3 days | New tools live; old tools forward to new |
| **S3** | Proof verification job + status surfacing | Medium | 2-3 days | Scheduled CI run flips `proof_status`; HUD shows counts |

Defer to a later cycle (call it Phase 2): visualization in HUD, structured compose API, cross-agent unlock events, federation.

---

## 2. Slice 1 — Schema Extension (S1)

### 2.1 Touch list

| File | Change |
|------|--------|
| [pkg/agentcontext/svc_recipes.go](pkg/agentcontext/svc_recipes.go) | Extend `Recipe` struct with `Tier int`, `Family string`, `Prerequisites []string`, `ProofStatus string`, `UnlockedIn []string`, `LastVerified time.Time`. Default tier = 1. |
| pkg/agentcontext/svc_engrams.go (new) | New service entry points `HandleEngramAdd/Recall/List/Graph`. Internally delegate to recipe service for storage so there is one source of truth. |
| [cmd/mcp-agent-context/tools_recipes.go](cmd/mcp-agent-context/tools_recipes.go) | Keep recipe tools as thin shims over engram service. |
| cmd/mcp-agent-context/tools_engrams.go (new) | Register `agent_engram_add/recall/list/graph` MCP tools. |
| pkg/agentcontext/svc_engrams_test.go (new) | Unit tests: cycle detection, default tier, prerequisite slug validation, recipe→engram round-trip. |
| [mcp/skills/agent-recipes/](mcp/skills/agent-recipes/) | Rename to `mcp/skills/agent-engrams/` with redirect file at the old path. Update `mcp/context/skills-registry.yaml`. |

### 2.2 Validation rules (added in S1)

- Slug: `^[a-z0-9][a-z0-9-]*$`, ≤ 64 chars.
- URI form: `engram://<family>/<slug>` (family is the path segment for grouping).
- `prerequisites` references must resolve to existing engram URIs at write time.
- Cycle check: DFS from new engram; reject if visiting self.
- Tier defaults to 1; tier 2/3 must include the proof contract markers documented in spec §3.2.

### 2.3 Migration

One-time script `pkg/agentcontext/migrate_recipes_to_engrams.go` that:
1. Scans long-term memory items where `category="recipe"`.
2. Adds `metadata.engram_tier=1`, `metadata.engram_family=<slug>`, `metadata.engram_prerequisites=[]`, `metadata.engram_proof_status=unverified`.
3. Updates `category` to `engram`. Adds `recipe` as an alias tag so old recall queries still work.

Run once at startup if the DB carries old recipe items, behind a `LOOM_ENGRAMS_MIGRATE=1` flag for the first release.

### 2.4 Done when
- `go test ./pkg/agentcontext/... -run Engram -v` is green.
- `make test` is green workspace-wide.
- Existing recipe MCP calls succeed against the new server (regression-tested via the existing `svc_recipes_test.go`).

---

## 3. Slice 2 — Graph-Aware Recall (S2)

### 3.1 Touch list

| File | Change |
|------|--------|
| pkg/agentcontext/svc_engrams.go | Implement `Recall` with `depth`, `include_locked`, `repo`, `tier_max`, `token_budget` semantics from spec §4.2. |
| pkg/agentcontext/engram_graph.go (new) | Graph traversal helpers: `transitivePrereqs(uri, depth)`, `dependents(uri, depth)`, `topoSortByTier(nodes)`. Reuse pure-Go BFS; no graph library. |
| cmd/mcp-agent-context/tools_engrams.go | Wire `agent_engram_recall` and `agent_engram_graph`. |
| pkg/agentcontext/svc_engrams_test.go | Add traversal tests, depth caps, locked-filter behavior, token-budget truncation. |

### 3.2 Recall ranking

Reuse the `agent_recall` enhanced ranking that already exists ([cmd/mcp-agent-context/](cmd/mcp-agent-context)) — pass-through query first, then post-process to include prerequisites and reorder lowest-tier-first. **Do not re-implement ranking.**

### 3.3 Token budget

When the resolved set exceeds `token_budget`:
1. Drop highest-tier prerequisites first (an agent can re-recall them on demand).
2. Then drop locked engrams (cannot be applied anyway).
3. Never drop the matched engrams themselves; truncate `solution` body if necessary, leaving `problem` and `proof` intact.

### 3.4 Done when
- Recall on a synthetic 10-engram DAG returns deterministically ordered results within budget.
- Cycle in malformed DB is detected at recall time and reported (do not crash).
- Skill `mcp/skills/agent-engrams/SKILL.md` documents the new recall flags.

---

## 4. Slice 3 — Proof Verification Job (S3)

### 4.1 Touch list

| File | Change |
|------|--------|
| cmd/mcp-agent-context/tools_engrams.go | Add `agent_engram_verify` (admin-only or agent-id allowlisted). |
| pkg/agentcontext/engram_verify.go (new) | For each engram, parse `proof`. If file-ref: `git log -1 --format=%H -- <file>` and check it still exists. If `command:`: run via devbox. If URL: HEAD request with timeout. Update `proof_status` and `last_verified`. |
| .gitlab-ci.yml | New scheduled job `engram-verify` that runs `loom engram verify --all --json` weekly and posts a summary issue if `failing` count > 0. |
| internal/hud/api_engrams.go (new) | REST endpoint `/api/engrams/summary` returning `{total, verified, stale, failing, by_tier}`. |
| HUD UI | Add a single line to the existing catalog view: "Engrams: 27 verified · 3 stale · 1 failing". No new screens. |

### 4.2 Devbox sandboxing

Proof commands run inside the project's devbox container ([devbox-sandbox.md rules](rules)) with the agent_id of the verification job, so they cannot escape into host shell. Timeout: 5 minutes per engram. Failures do not crash the job — they flip status and continue.

### 4.3 Done when
- Weekly job runs in CI, posts a summary, and updates `proof_status` for every engram in the workspace DB.
- HUD summary line renders with live counts.
- Stale/failing engrams are surfaced to recall callers via `proof_status` so an agent does not silently apply rotted advice.

---

## 5. Risk Register

| Risk | Likelihood | Mitigation |
|------|-----------:|-----------|
| Schema extension breaks downstream consumers of `Recipe` | Medium | Keep `Recipe` struct backwards compatible; add fields, never remove. Run `agent-context` integration tests against the prior client. |
| Cycle detection blocks legitimate co-references at write time | Low | Cycles are explicit DAG violations. If a real co-dependency exists, it indicates the engrams need to be merged or split. |
| Proof verification runs arbitrary commands → security risk | Medium | Constrain to devbox sandbox, fixed timeout, no network for file-ref proofs, allowlist of TLDs for URL proofs. |
| Token budget truncation removes content the agent needs | Medium | Default budget is 4000 (matches recipe recall). Surface a `truncated: true` flag so the agent can re-query with higher budget. |
| Engram authoring is too high-friction; nobody adds them | High | S1 ships migration that seeds 5 engrams from existing code. Add `agent_engram_suggest` (post-MVP) that proposes engrams from session activity. |

---

## 6. Open Decisions to Make Before Coding

1. Do we keep both `agent_recipe_*` and `agent_engram_*` forever, or deprecate recipe tools after one release? Recommend: keep recipe tools as thin shims for two minor releases, then mark deprecated in a follow-up cycle.
2. Slug naming convention — `engram://family/slug` or just bare `family/slug`? URI form is more discoverable; chose URI in spec §3.1 unless someone objects.
3. Where does the verify job live — `gitlab-ci.yml` only, or also as a `loomd` periodic task? Recommend: CI for now, since it gives us PR comments on stale engrams. Move to `loomd` if the CI cadence proves too coarse.
4. Should `unlocked_in` be derived (computed from successful proof runs) or asserted (manually written)? Recommend: derived — never trust an agent's claim that an engram is unlocked.

---

## 7. Sequencing & Dependencies

- S1 has no upstream dependency. Start anytime.
- S2 depends on S1.
- S3 depends on S1 (for the schema field) but only loosely on S2 (for surfacing). Could run partially in parallel.

Suggested order if a single agent ships it:

1. S1 schema + tests + recipe back-compat → ship → measure existing recipe call sites unaffected for 24 h.
2. S2 graph recall + tests → ship → seed 5 engrams via migration.
3. S3 verification job + HUD line → ship → first weekly run produces the baseline.

Total elapsed time, single agent: ~1 week of focused work.

---

## 8. Backout Plan

If S1 destabilizes the agent-context service:
- Feature flag: `LOOM_ENGRAMS=0` skips engram tool registration. Recipe tools alone keep serving traffic.
- Migration is reversible: a `LOOM_ENGRAMS_REVERT=1` script flips `category=engram` items back to `category=recipe` and strips the new metadata keys.

If S3 produces noisy false-positives:
- The job is scheduled, not blocking. Disable the schedule, fix the verifier, re-run. No production impact.

---

## 9. What This Plan Does Not Cover

- The HUD visual tech-tree explorer (Mermaid render of `agent_engram_graph` output).
- A `loom engram` CLI subcommand for inspecting locally.
- Cross-workspace federation (universal scope is per-workspace until proven otherwise).
- Engram-aware skill orchestration (a skill declaring required engrams and refusing to run if locked).
- Auto-suggestion of engrams from session activity (post-MVP).

These are tracked as Phase-2 candidates and should be revisited only after the MVP has 90 days of usage data.
