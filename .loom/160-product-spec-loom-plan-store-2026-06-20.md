# Product Spec — Loom Plan Store (first-class, worktree-resilient, cross-agent)

- **Status**: Draft
- **Date**: 2026-06-20
- **Author**: Cody Blevins (via plan-loom-core)
- **Implementation plan**: [161-implementation-plan-loom-plan-store-2026-06-20.md](161-implementation-plan-loom-plan-store-2026-06-20.md)
- **Decisions locked** (operator, 2026-06-20):
  1. **Source of truth**: store-canonical, `.loom/*.md` is a rendered mirror.
  2. **Mills depth**: full unification (plans + `agent_task` + Mills backlog converge).
  3. **Priority loops** (all four): parallel slice shipping, plan→merge→deploy review, agent-context handoffs, Mills autonomy.

---

## Problem

Plans, specs, and context packs are stored as `.loom/*.md` files. Since the parallel-agent / worktree era, those files are the wrong substrate:

- A **fresh subagent in a worktree cannot find the plan**. Root `.loom/*.md` is git-tracked, so `git worktree add` checks out a *frozen copy* at that branch's HEAD (`pkg/agentcontext/svc_worktree.go:73-75`, `:200`). A plan written on `main` or in worktree A is invisible to a subagent in worktree B until committed **and** merged/rebased. Verified live: `.worktrees/feat-muxstdio-close-cause/.loom/` and `.claude/worktrees/silly-faraday-f01fb6/.loom/` are independent frozen copies at different commits.
- **Skills reference plans by literal relative path strings** in their prose (45 `.loom` mentions in `mcp/context/skills-registry.yaml`). A subagent resolves those paths against its *own* stale checkout — there is no shared index or API.
- **`parallel-slice-ship`'s slice decomposition lives only in the orchestrator's context window + spawn prompts.** A fresh `slice-implementer` (`.claude/agents/slice-implementer.md`) cannot look up its slice — it knows only what the prompt told it, and its "record decisions/blockers for the orchestrator" instruction has no durable anchor.
- **File-claims are advisory only.** `ClaimSvc.Acquire` (`pkg/agentcontext/svc_claims.go:50-130`) reports conflicts then acquires anyway. Parallel collision-safety relies entirely on the planner hand-assigning disjoint file sets; nothing enforces a slice's boundary.
- **No queryable plan→implement→review→merge→deploy lifecycle exists.** State is scattered: session status `active/ended/summarized` (`schema.go:117-119`), task status (`schema.go:43-46`), worktree status (`schema_presence.go:95-99`), an off-by-default workflow engine, and Mills' separate SQLite. Nothing answers "where is each plan/slice in its lifecycle?"
- **Mills is fully disjoint.** Its backlog is a separate SQLite store (`pkg/mills/store/types.go:55`) that reads `SpecDoc` *by file path only* (`cmd/loom-mills-operator/main.go:985,:994`), with **no link to `agent_task` or agent-context** (zero `agent_task` references in `pkg/mills/`). The interactive-session world and the factory world share nothing.
- **Handoffs are weak.** `agent_handoff_create` carries a curated list of `entry_ids` + text, dummy-vectored, targeted at an `agent_id` string (`pkg/agentcontext/service_handoffs.go:16`, `schema.go:385`). They cannot hand off *a plan* — only loose context pointers that degrade if entries are reaped, and the target `agent_id` often doesn't match the live conversation's minted id.

**Root cause**: there is no first-class, durable, queryable **Plan** entity. The agent-context server already runs a *global shared Qdrant* (`pkg/agentcontext/config.go:141`) that survives worktrees, repos, and agents — every other entity lives there (sessions, tasks, handoffs, worktrees, file-claims, memory, engrams), but plans don't. That is the missing connective tissue.

## Riskiest assumption + kill-test

**Load-bearing assumption**: A Qdrant-backed `Plan` entity in the agent-context MCP server, looked up by a stable `plan_id`, is reachable with **identical content** from (a) a fresh Claude subagent in a *different* worktree, (b) a Codex agent, and (c) a Mills-spawned pod agent — **without `agent_id` scoping hiding it**.

This is risky because today's recall path filters by `agent_id` by default and does **not** filter by project/namespace (`pkg/agentcontext/service_recall.go:316-327`); and because Mills spawns agents in pods/VMs whose MCP reachability to the same loom proxy + Qdrant is unproven for this purpose.

**Kill test** (≤30 min, run as Slice 1's completion criterion):
1. Agent A (main worktree) `agent_plan_create` → plan `P` + 2 slices.
2. Fresh Agent B in worktree B (different branch) `agent_plan_get(P)` → byte-identical plan + slices. *(Claude cross-worktree)*
3. A Codex session `agent_plan_get(P)` → identical. *(cross-vendor)*
4. A Mills-spawned pod agent (or the operator on its behalf via the loom proxy) `agent_plan_get(P)` → identical. *(Mills reachability)*
5. Confirm recall succeeds **without** passing Agent A's `agent_id` (proves cross-agent visibility by project/plan_id, not identity).

**Pair with negative search**: confirm there is genuinely no existing plan/slice entity in the store (done — searches across `pkg/agentcontext/` + `cmd/mcp-agent-context/` returned none; the only `Slice`/`Roadmap` types are in the disjoint Mills SQLite `pkg/mills/store/types.go:27,56`).

**Failure mode if wrong**: if Mills pods can't reach the proxy/Qdrant, or `agent_id` scoping hides the plan, we'd ship a store only the originating conversation can read — fixing nothing for the parallel + Mills cases, which are the whole point.

**Status**: data-model + non-agent-scoping legs **PASSED 2026-06-20**. Evidence: `pkg/agentcontext/svc_plans_killtest_test.go` (`TestPlan_KillTest_CrossProcessQdrant`) creates a plan via one `PlanSvc`/`QdrantClient` against the **real shared Qdrant** (`192.168.50.176:6333`) and retrieves it byte-identical from a *separate* `PlanSvc` with a fresh in-memory cache using only `plan_id` — no `agent_id`. Cross-worktree and cross-vendor (Codex) are the same "separate process → same Qdrant" shape, so both are covered. **Remaining live legs (require deploying the new binary to the shared daemon + restarting it — operator-gated):** (a) MCP callers see `agent_plan_*` through the loom proxy, (b) a Codex session round-trips it live, (c) a Mills-spawned **pod** reaches `192.168.50.176:6333` (the only genuinely unproven leg; the Mills *operator* already reaches agent-context for worktree/handoff).

## Goals

1. **Worktree-resilient plans**: any agent in any worktree/repo retrieves a live plan by `plan_id` — never from a frozen `.loom/` checkout.
2. **Store-canonical, file-mirrored**: the `Plan` entity is authoritative; `.loom/<NNN>-plan-<slug>-<date>.md` is an atomically-rendered, git-committable projection for human/MR review.
3. **First-class slices**: a fresh `slice-implementer` looks up its own slice by `slice_id`, records decisions/status back to that record, and its file set is enforced (not advisory).
4. **Queryable lifecycle**: every plan/slice tracks `plan→implement→review→merge→deploy` with MR/pipeline/deploy refs, reviewable in the HUD.
5. **Plan-aware handoffs**: hand off a `plan_id`+`slice_id` scope (cross-vendor) instead of loose entry pointers.
6. **One work unit, factory-wide**: Mills backlog + `agent_task` + plans converge so interactive sessions and the Mills factory share one backlog.
7. **Cross-platform**: works identically across Codex, Claude Code, and Mills via the loom MCP proxy and regenerated skills.

## Non-goals (this program)

- Replacing Qdrant or the embedding provider.
- A new UI beyond a HUD "Plans" lifecycle card.
- Changing the GitLab MR/CI mechanics themselves.
- Hard real-time locking semantics beyond per-slice file-claim enforcement.

## Design overview

### New entity: `Plan` (collection `agent_plans_v1`)

Authoritative record. Key fields (aligned with Mills `BacklogItem` for unification):
`id` (stable `plan-<slug>-<short>`), `slug`, `title`, `project` (canonical GitLab path), `namespace`, `phase` (`draft→planned→in_progress→in_review→merging→merged→deployed→done|abandoned`), `spec_doc` (rendered body, canonical), `spec_anchor`, `success_criteria{tests,metrics,manual_check}`, `budget`, `riskiest_assumption`, `kill_test`, `kill_test_status`, `dependencies[]plan_id`, `source_session_id`/`root_session_id`, `mr_refs[]`, `pipeline_refs[]`, `deploy_refs[]`, `mirror_path`, `mills_backlog_id`, `gitlab_issue_iid`, `created_by/at`, `updated_at`. Embedded vector over title+spec for semantic recall.

### New sub-entity: `Slice` (collection `agent_plan_slices_v1`)

`id` (`<plan_id>#<n>`), `plan_id`, `order`, `name`, `goal`, `files[]` (the disjoint set → claim-enforcement basis), `acceptance_criteria`, `test_strategy`, `interface_contracts`, `branch_name`, `depends_on[]slice_id`, `phase` (`pending→claimed→implementing→implemented→in_review→integrated→merged`), `assigned_agent_id`, `worktree_id` (FK → `WorktreeAssignment`), `session_id`, `commit_refs[]`, `mr_ref`, `decisions[]` (blockers/decisions the implementer records — anchored here, not lost).

### New tool family `agent_plan_*`

`agent_plan_create | update | get | list | search` (semantic); `agent_plan_slice_add | update | get | list | claim`; `agent_plan_lifecycle_advance` (validated phase transition + event emit); `agent_plan_render` (atomic `.md` projection); plan-aware extension to `agent_handoff_create`.

### Scoping requirement (non-negotiable)

Plan recall filters by `plan_id` / `project` / `namespace` — **never** by `agent_id`. This is the explicit fix for `service_recall.go:316-327` and the crux of the kill-test. Any agent may read a project's plans; writes are attributed but not gated by identity.

### Store-canonical `.md` mirror

On every plan/slice mutation, render to `.loom/<NNN>-plan-<slug>-<date>.md` via `writeFileAtomic` (tempfile+rename — required for watched files, per `pkg/skills/fileops.go`). The file is a *projection*: human-readable, diffable in MRs, committed by shipping skills. Agents read the **store** by id first; the file is the review snapshot. The mirror is re-rendered into the worktree at slice time so each MR carries an up-to-date snapshot.

### Worktree resilience (the core fix)

Because `Plan` lives in global Qdrant keyed by `plan_id`, a fresh subagent in any worktree calls `agent_plan_get(plan_id)` / `agent_plan_slice_get(slice_id)` and gets the live record. The spawn prompt carries only the small stable `plan_id`/`slice_id` strings — not the full plan, and not a path into a frozen checkout.

### Claim enforcement (parallel safety)

When a slice is claimed, its `files[]` become **hard** claims. `agent_file_claim_acquire` for a file already held by another *active* slice returns a hard reject (policy-configurable), upgrading the advisory path at `svc_claims.go:50-130`. Slice → worktree → claims become one enforced chain.

### Lifecycle review (HUD)

Phase transitions emit events on the existing ring-buffer/SSE channel (the same path that carries `chapter.marked`). A new HUD "Plans" card renders each plan/slice across `plan→implement→review→merge→deploy` with MR/pipeline/deploy refs, tied to the originating session/root-session.

### Plan-aware handoffs

`agent_handoff_create` gains optional `plan_id`/`slice_id`. Accepting a handoff = resuming a known plan + slice scope (cross-vendor, since it's just ids), not reconstructing from `entry_ids`.

### Full unification (end-state)

- **Mills `BacklogItem` becomes a projection over `Plan`**: a store adapter maps each backlog item ↔ plan; `backlogPromptContext` (`main.go:985`) reads plan+slice **content** from the store (not a bare file path). Council `backlog_mutator.go` and `gitlab_importer.go` write Plans.
- **`agent_task` gains `plan_id`/`slice_id`**: the slice is the work unit; tasks are granular TODOs under a slice. Interactive sessions and the factory query one backlog.

## Cross-cutting cleanup (found during research)

Fix alongside (Slice 0): `CLAUDE.md:29` falsely claims repo `.gitignore` carries `.loom/local|archive` rules (loom-core relies only on `~/.config/git/ignore`); `.loom/00-workspace-snapshot.md` + `.loom/50-worklog.md` are policy-"gitignored" but currently *tracked* (and copied stale into every worktree); 21 legacy files under `.loom/archive/roadmap-reconciliations/` are tracked despite the ignore rule; `skills-registry.yaml:700-705` omits `templates/40-decisions.md`.

## Success criteria

- **Kill-test passes** (cross-worktree + cross-vendor + Mills, non-agent-scoped recall).
- A `parallel-slice-ship` run spawns N `slice-implementer`s that each resolve their slice from the store by id, with enforced file boundaries, and record status/decisions back — orchestrator reconstructs full state from the store alone.
- HUD "Plans" card shows a real plan advancing through all lifecycle phases with live MR/pipeline refs.
- A Mills pipeline and an interactive session operate on the **same** plan/backlog record.
- `>80%` coverage on new store code; cross-worktree recall regression test.

## Evidence index

| Claim | Source |
|---|---|
| No plan entity in agent-context | search across `pkg/agentcontext/`, `cmd/mcp-agent-context/` |
| Global shared Qdrant backend | `pkg/agentcontext/config.go:141`; `qdrant_registry.go:10-27` |
| Recall is agent-scoped, not project-scoped | `pkg/agentcontext/service_recall.go:316-327`, `:50` |
| Worktree gets frozen `.loom/` copy | `pkg/agentcontext/svc_worktree.go:73-75,:200`; live `.worktrees/*/.loom/` |
| Slice state only in context window | `mcp/context/skills-registry.yaml` (parallel-slice-ship); `.claude/agents/slice-implementer.md` |
| File-claims advisory only | `pkg/agentcontext/svc_claims.go:50-130` |
| Handoffs = entry_ids + text, agent-targeted | `pkg/agentcontext/service_handoffs.go:16`; `schema.go:385` |
| Mills backlog disjoint, SpecDoc by path | `pkg/mills/store/types.go:55`; `cmd/loom-mills-operator/main.go:985,:994` |
| `.loom` gitignore drift | `~/.config/git/ignore`; `CLAUDE.md:29`; `git ls-files .loom/` |
