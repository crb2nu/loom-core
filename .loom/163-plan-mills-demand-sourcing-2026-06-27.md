# Mills demand sourcing: multi-repo intake + roadmap/plan → backlog → cross-repo merge

- **Plan ID**: `plan-mills-demand-sourcing-multi-repo-intake-roadmap-plan-backlog-2d158d`
- **Phase**: draft
- **Project**: services/loom-core
- **Namespace**: mills/demand-sourcing
- **Created by**: claude-code
- **Created**: 2026-06-28T00:50:14Z
- **Updated**: 2026-06-28T00:50:14Z

> Rendered from the Loom plan store (canonical). Edit via `agent_plan_*` tools, not this file.

## Riskiest assumption + kill-test

**Assumption**: Mills can open + CI-watch + MERGE an MR in a repo OTHER than services/loom-core. The default pipeline GitLab worker (pkg/mills/clients/gitlab.go) is hard-pinned to ONE project — every call uses c.projectPath() from the construction-time cfg.Project (mr :199, merge :373, ci :473, issues :542). A separate target-repo-aware path exists in pkg/mills/crossrepo/ (its GitLabClient interface takes projectID per call: MergeMR(ctx, projectID, mrIID, ...)), but cross_repo.enabled:false and it is UNPROVEN whether that path is wired end-to-end into the reconciler->pipeline and can actually merge in a second repo.

**Kill test**: Enable the crossrepo path, hand-enqueue ONE safe-fixture backlog item targeting a SECOND repo (trivial docs change in e.g. services/flexdeck or a libs/* repo), and watch it open+merge an MR IN THAT REPO unattended within 30 min. Observable outcome: a merged MR in repo != loom-core authored by the operator, with operator logs showing the crossrepo integrator merging projectID=<second-repo>. This IS slice S0's completion criterion; S1-S4 are BLOCKED until it passes.


## Spec

# Mills demand sourcing — multi-repo intake + roadmap/plan → backlog bridge

**Date**: 2026-06-27 · **Author**: claude-code (RALPH loop, picking up from the canary-autopilot ship) · **Status**: draft — BLOCKED on S0 kill-test

## Problem

Mills' north-star (`autonomous_merges_24h`) read **4 over 30 days** (1d=1, 7d=4, 30d=4) because the loop has **almost no demand**. The only real-work intake is the GitLab importer, which polls **services/loom-core (project 47) only** for `mills-eligible` issues (currently 0). The daily canary-autopilot (shipped 2026-06-27: loom-core!819 + gitops!302, merged) keeps the loop *alive* — a heartbeat prod smoke-test — but merges a no-op fixture, so it adds **zero real value**.

To manage roadmaps, plans, and Mills automation across **services and libs**, Mills needs real demand from two sources (both selected by the user):
1. **Multi-repo importer** — poll services/* + libs/* for `mills-eligible` issues.
2. **Roadmap/Plan → backlog bridge** — auto-emit backlog items from Plan-Store slices so planning feeds the loop directly.

Both are **intake sources** following the existing `GitLabImporter` pattern (poll → dedupe by deterministic ID → `store.Backlog.Put` → reconciler picks up). The backlog ID scheme `gl-<project_id>-<iid>` is already collision-safe across repos, and `BacklogItem.PlanID` already links items to plans — so the *intake* halves are mechanically straightforward. The hard part is **execution**, captured below.

## Riskiest assumption + kill-test

**Load-bearing assumption**: Mills can open + CI-watch + **merge an MR in a repo other than services/loom-core**. The default GitLab worker (`pkg/mills/clients/gitlab.go`) is hard-pinned to ONE project via `c.projectPath()` (mr `:199`, merge `:373`, ci `:473`, issues `:542`). A target-repo-aware path exists in `pkg/mills/crossrepo/` (`GitLabClient.MergeMR(ctx, projectID, mrIID, …)` — per-call projectID) but `cross_repo.enabled:false` and its end-to-end wiring into reconciler→pipeline is unproven.

**Kill test**: Enable the crossrepo path, hand-enqueue ONE safe-fixture item targeting a SECOND repo, watch it open+merge an MR there unattended (≤30 min). Observable: a merged MR in repo≠loom-core authored by the operator, operator log shows crossrepo merge `projectID=<second-repo>`.

**Failure mode if wrong**: every multi-repo intake item (importer issue OR plan-slice emission) targeting a non-loom-core repo imports + authors a plan fine, then **dies at the MR stage** → escalations, not merges. Both intake halves would build toward work that can't land. Front-loading this in S0 (30 min) vs. discovering it after S1+S2 ship is the entire point.

**Status**: not run.

## Architecture (evidence, this worktree)

| Component | File:line | Today | Change |
|---|---|---|---|
| GitLab importer | pkg/mills/intake/gitlab_importer.go:71 | single-project (client-scoped) | iterate projects + tag target |
| GitLab worker | pkg/mills/clients/gitlab.go:137 `projectPath` | hard-pinned 1 project | route cross-repo via integrator |
| Crossrepo integrator | pkg/mills/crossrepo/integrator.go | per-call `projectID`, DISABLED | wire to pipeline + enable |
| IntakePolicy | pkg/mills/policy.go:96 | gitlab + canary_gc + canary_autopilot | + projects list + plan_slice_emitter |
| BacklogItem | pkg/mills/store/types.go:55 | has `PlanID` (:74) | + target-repo tag |
| Plan/PlanSlice | pkg/agentcontext/schema_plan.go:98 | phases pending→…→merged | emitter reads `pending` slices |
| Operator wiring | cmd/loom-mills-operator/main.go ~585–625 | errgroup of schedulers/importers | + emitter + multi-repo importer |

**Existing partial bridge**: GitLab importer + council mutator can author plans (`LOOM_MILLS_PLAN_AUTHORING`); `plan_backfill.go` stamps PlanIDs; `council/roadmap.go` extracts ROADMAP.md → `roadmap_intents` (council CONTEXT only). **Missing**: nothing scans plan slices / roadmap intents and EMITS runnable backlog items. That emitter is S2.

## Slice plan (S0 gates all)

- **S0 — Cross-repo execution proof (KILL-TEST)** — prove one cross-repo canary merges in a 2nd repo. BLOCKS S1–S4.
- **S1 — Multi-repo importer** — policy `projects` list; importer iterates + tags target repo.
- **S2 — Plan-slice emitter (the bridge)** — in-operator scheduler scans Plan Store `pending` slices → linked, target-tagged BacklogItems.
- **S3 — Roadmap-wave → plan slices** — ensure roadmap intents/council plans carry emitter-ready slices (extend existing authoring).
- **S4 — Safety + rollout** — per-repo protected-paths/auto_merge/budgets; flip defaults with soak; HUD.

See slices for files + acceptance criteria. The store is canonical; this doc is the rendered mirror.

## Slices

### 1. S0 — Cross-repo execution proof (KILL-TEST) — `pending`

- **Slice ID**: `plan-mills-demand-sourcing-multi-repo-intake-roadmap-plan-backlog-2d158d#1`
- **Goal**: Prove Mills can open + CI-watch + merge an MR in a repo OTHER than services/loom-core. Determine the wiring gap between pkg/mills/crossrepo/integrator.go (target-repo-aware) and the reconciler→pipeline path (single-project-pinned). Wire the minimum needed so a backlog item tagged with a target project routes to the crossrepo integrator, enable cross_repo in policy, and land one safe-fixture cross-repo canary.
- **Files**: pkg/mills/crossrepo/integrator.go, pkg/mills/crossrepo/registry.go, pkg/mills/crossrepo/planner.go, pkg/mills/pipeline/integrator.go, pkg/mills/pipeline/runner.go, cmd/loom-mills-operator/main.go, platform/gitops/k3s/mills/configmap-policy.yaml
- **Acceptance**: A merged MR exists in a repo != services/loom-core, authored by the loom-hive operator, produced from a hand-enqueued safe-fixture backlog item with a target-repo tag. Operator logs show the crossrepo integrator merging projectID=<second-repo>. Until this passes, S1–S4 stay BLOCKED (the riskiest-assumption gate). If the crossrepo path proves NOT wired/feasible in ≤1 day, escalate: the plan's intake halves are re-scoped to loom-core-only and cross-repo becomes its own program.

### 2. S1 — Multi-repo GitLab importer — `pending`

- **Slice ID**: `plan-mills-demand-sourcing-multi-repo-intake-roadmap-plan-backlog-2d158d#2`
- **Goal**: Extend the GitLab importer from single-project to multi-repo. Add a `projects` list (slugs or numeric ids; optionally globs services/* + libs/*) to the GitLabIntake policy. Iterate projects in importer Tick (or construct N importers in main.go). Tag each imported BacklogItem with its source TargetProject so S0's routing merges it in the right repo. Dedupe already collision-safe via gl-&lt;project_id&gt;-&lt;iid&gt;.
- **Files**: pkg/mills/policy.go, pkg/mills/intake/gitlab_importer.go, pkg/mills/clients/gitlab.go, cmd/loom-mills-operator/main.go, platform/gitops/k3s/mills/configmap-policy.yaml
- **Depends on**: plan-mills-demand-sourcing-multi-repo-intake-roadmap-plan-backlog-2d158d#1
- **Acceptance**: A `mills-eligible` issue opened in a second repo (e.g. services/flexdeck) is imported as a queued BacklogItem tagged with that repo, and (with cross_repo enabled from S0) runs the pipeline to a merged MR in that repo. Re-running the importer does not duplicate. loom-core-only behavior is unchanged when `projects` lists just loom-core.

### 3. S2 — Plan-slice emitter (roadmap/plan → backlog bridge) — `pending`

- **Slice ID**: `plan-mills-demand-sourcing-multi-repo-intake-roadmap-plan-backlog-2d158d#3`
- **Goal**: Build the missing bridge: a new in-operator PlanSliceEmitter intake scheduler (Option A) that polls the Plan Store for slices in phase `pending` belonging to plans whose project is mills-managed, and emits one linked, target-repo-tagged BacklogItem per ready slice. Deterministic backlog ID (e.g. plan-slice-&lt;slice_id&gt;) for dedupe. Policy-gated + env-flagged like the existing plan-authoring path. Closes the planning→execution loop so a Plan Store slice becomes a merged MR.
- **Files**: pkg/mills/intake/plan_slice_emitter.go, pkg/mills/intake/plan_slice_emitter_test.go, pkg/mills/policy.go, pkg/mills/clients/plan.go, cmd/loom-mills-operator/main.go, platform/gitops/k3s/mills/configmap-policy.yaml
- **Depends on**: plan-mills-demand-sourcing-multi-repo-intake-roadmap-plan-backlog-2d158d#1
- **Acceptance**: A Plan in the store with a `pending` slice (carrying files + acceptance_criteria + project) results in exactly one queued BacklogItem (PlanID + slice linkage set, TargetProject = plan.project), which runs the pipeline. The slice advances (claimed→…→merged) as the item progresses. Re-running the emitter does not duplicate. Disabled by default; no-op when off.

### 4. S3 — Roadmap-wave → plan slices — `pending`

- **Slice ID**: `plan-mills-demand-sourcing-multi-repo-intake-roadmap-plan-backlog-2d158d#4`
- **Goal**: Close the top of the funnel: ensure roadmap intents / council-authored plans carry emitter-ready slices so a roadmap wave becomes runnable work without hand-authoring. Extend the existing council plan-authoring + roadmap extraction (council/roadmap.go → roadmap_intents → council editor → BacklogMutator + PlanAuthor) so authored plans include concrete slices (files + acceptance_criteria) that S2's emitter can pick up. Prefer extending existing authoring over a new path.
- **Files**: pkg/mills/council/roadmap.go, pkg/mills/council/backlog_mutator.go, pkg/mills/clients/plan.go, pkg/mills/intake/plan_backfill.go
- **Depends on**: plan-mills-demand-sourcing-multi-repo-intake-roadmap-plan-backlog-2d158d#3
- **Acceptance**: An unchecked roadmap bullet (or a council run) produces a Plan with at least one well-formed `pending` slice (non-empty files + acceptance_criteria + project), which S2 then emits and the loop merges. A roadmap wave can be driven to a merged MR with no manual plan editing.

### 5. S4 — Safety rails + staged rollout — `pending`

- **Slice ID**: `plan-mills-demand-sourcing-multi-repo-intake-roadmap-plan-backlog-2d158d#5`
- **Goal**: Make multi-repo autonomy safe to leave on. Per-repo protected paths, per-repo auto_merge/human_review gating, per-repo budget caps, and escalation routing so a runaway in one repo can't merge into another's protected surface. Stage the rollout: enable cross_repo + multi-repo importer + plan-slice emitter behind their flags with a soak per repo; add HUD visibility (per-repo merge counts). Flip defaults only after a clean soak.
- **Files**: platform/gitops/k3s/mills/configmap-policy.yaml, pkg/mills/policy.go, pkg/mills/crossrepo/registry.go, internal/hud/api_mobile.go
- **Depends on**: plan-mills-demand-sourcing-multi-repo-intake-roadmap-plan-backlog-2d158d#2, plan-mills-demand-sourcing-multi-repo-intake-roadmap-plan-backlog-2d158d#3
- **Acceptance**: Policy supports per-repo overrides (protected_paths, auto_merge, human_review, budget). A protected-path change in any target repo routes to human review, not auto-merge. HUD shows per-repo merge counts. After a 1-week soak with the importer + emitter enabled across ≥2 repos and zero protected-path breaches, defaults are flipped on. north-star reflects real (non-canary) merges across repos.
