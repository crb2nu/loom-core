# Plan — Mills cross-repo execution keystone (multi-repo demand → merge)

**Status**: S1–S5 DONE 2026-07-05 — cross-repo keystone **PROVEN LIVE** (flexdeck!244
merged **autonomously by the operator** in a non-home repo via the group token).
**S6 demand code SHIPPED 2026-07-06** (this MR): the emitter sources a
`cross_repo.demand_projects` allowlist + stamps `TargetProject`, default-OFF/empty.
Remaining: **S6 activation** — gitops `demand_projects` + operator roll + live verify
(per-repo CI onboarding, gated on explicit go-ahead per blast radius).
**Owner**: current execution loop (S6 activation)
**Predecessor**: [services/loom-core!941](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/941) (spawn capacity + git-clone multi-repo readiness, MERGED `8ef4c8e4`)

## Goal

Let the **default** Mills pipeline execute a single backlog item end-to-end
against **any services-group repo** (not just the operator's home repo,
loom-core): implement → tests → MR → ci_watch → merge, all targeting the item's
repo. This is the keystone the "expand to more repos" directive actually needs;
spawn capacity (!941) and demand-sourcing emitters are inert without it.

## Current state (evidence, 2026-07-05)

| Layer | State | Evidence |
|---|---|---|
| Per-item target repo | **Absent** — `BacklogItem` has no target-project field | `pkg/mills/store/types.go:56` (struct has PlanID but no TargetProject) |
| Backlog persistence | **Columnar** → new field needs a migration | `pkg/mills/store/dao_backlog.go:19` (`backlogColumns`) |
| Spawn stages | Hardwired to `loom-core` | `pkg/mills/pipeline/dispatcher.go:279-281` (`SpawnWorker.Run`: `project=""` → `"loom-core"`) |
| Devbox (tests) stage | Uses route-level `project` (single value, operator-configured) | `dispatcher.go:855` (`DevboxWorker{Project: project}`), `DefaultRoutes` param |
| GitLab stages (mr/ci_watch/merge) | **Single-project-pinned client** | `pkg/mills/clients/gitlab.go` — `projectPath()` used at ~20 call sites |
| Cross-repo machinery | Per-`projectID` integrator EXISTS but **not wired** into the default pipeline | `pkg/mills/crossrepo/integrator.go:53-55` (per-projectID iface); `main.go` builds `pipeline.Integrator`, not `crossrepo.Integrator` |
| Merge credential | **Ready** — group token, Maintainer on `services` group; proven | `loom-mills-gitlab-group/api-token`; memory: merged `loom-flightdeck!33` |
| Spawn/devbox clone of target | **Ready for services repos** | !941 fallback + `SPAWN_GIT_BASE_URL=.../services` + devbox clones fresh |

## Riskiest assumption + kill-test

**Load-bearing assumption**: The default single-item Mills pipeline can run an
item end-to-end against a non-home **services** repo — implement (spawn clones
repo X) → tests (devbox clones repo X) → mr/ci_watch/merge (group token, project
X) — by adding only **per-item project threading** (BacklogItem field + worker
resolution + per-project GitLab client). No new substrate, no new credential
(the services group token already merges any services repo), no group-aware
git-clone base (services base already serves any services repo).

**Kill test**: on an operator built with slices S1–S4, inject
`BacklogItem{TargetProject: "services/loom-flightdeck", <small doc-only slice>}`
via `POST /api/mills/backlog` and run the pipeline. **Observable pass**: an MR is
opened AND merged **in loom-flightdeck** (not loom-core), the diff lands in
loom-flightdeck, and `mills_autonomous_merges_real` increments. ≤30 min once the
operator image carries S1–S4. (loom-flightdeck is the proven 2nd services repo.)

**Failure mode if wrong**: a layer is more loom-core-coupled than the seams
suggest — the branch contract (`BranchContractFor`), the git-capture `RepoRoot`
(operator-local single clone in `dispatcher.go:252-259`), ci_watch's
branch-pipeline resolution (`clients/gitlab.go` PollPipeline), or the spawn
clone auth — so a cross-repo item escalates. Wasted work ≈ the wired slice(s)
past the break; the kill-test localizes it in one run instead of one-per-roll.

**Status**: PASSED 2026-07-05 — cross-repo keystone proven end-to-end against a
**non-home** services repo (flexdeck, after the loom-flightdeck→flexdeck pivot).
Evidence: [flexdeck!244](https://gitlab.flexinfer.ai/services/flexdeck/-/merge_requests/244)
(`merge_commit 07c429ca`, `merged_by group_3_bot` = the `loom-mills-gitlab-group`
token) merged **autonomously by the operator**. Full slice chain ran: plan_slice →
research → implement (spawn cloned flexdeck + pushed a branch there via the group
token) → pr_self_review → tests (devbox cloned + `gofmt`'d flexdeck) → mr (opened via
`GitLabWorker.ForProject` + group token) → ci_watch (flexdeck's own branch pipeline) →
merge. Blockers surfaced + fixed **in-loop**: !948 (devbox `resolveProject` cross-repo
fallback), flexdeck!242 (branch-pipeline CI onboarding), and a transient git-clone
**OOM** on the flexdeck-sized full clone → fixed by !957 (git-clone init container
memory 256Mi→1Gi, env-configurable, MERGED `abb20046`). ~$0.50 across all
kill-test runs. Marker artifact reverted (flexdeck!245); flexdeck restored.
**S6 (demand half) follows** — the emitter now sources a foreign-repo allowlist
and stamps `TargetProject` (this MR).

### Positive/negative search (per workspace rule)
- Positive: group token already merged a 2nd services repo (loom-flightdeck!33);
  crossrepo per-projectID client iface exists and is unit-tested.
- Negative / disconfirming: the default pipeline's GitLab client is
  single-project-pinned (`projectPath()` everywhere) and the crossrepo integrator
  is oriented to **atomic multi-repo** runs, not a single foreign-repo item — S3
  must prove a per-item project reuses the merge path without the atomic-run
  machinery.

## Slices (vertical; S1–S4 pure loom-core code, S5 live)

| # | Slice | Surface | Status |
|---|---|---|---|
| **S1** | `BacklogItem.TargetProject` — migration 008 + DAO scan/insert + REST intake auto-accepts (`handleBacklogCreate` decodes into the struct); empty = home repo (back-compat) | `store/{types,dao_backlog,migrations/008}` | ✅ SHIPPED |
| **S2** | Per-item project in spawn+devbox workers — `SpawnWorker.Run`/`DevboxWorker.Run` resolve `jc.Item.TargetProject` via `effectiveProject`; cross-repo skips the operator-local `RepoRoot` capture | `pkg/mills/pipeline/dispatcher.go` | ✅ SHIPPED |
| **S3** | Per-target-project GitLab worker — `GitLabClient.ForProject` (shallow project-scoped copy) + `GitLabWorker.ForProject` hook on mr/ci_watch/merge/cleanup; reuses group token | `pkg/mills/clients/gitlab.go`, `dispatcher.go`, `main.go` | ✅ SHIPPED |
| **S4** | Safety gate — `cross_repo.enabled` (**default OFF**); reconciler **skips fail-closed** an item with `TargetProject != HomeProject` when off (never runs against home); `HomeProject` empty = inert | `pkg/mills/reconciler.go`, `main.go` | ✅ SHIPPED |
| **S5** | **KILL-TEST (live)** — operator on S1–S4 (`145726`/`20260705-210904`), `cross_repo.enabled: true` + group token (`loom-mills-gitlab-group`, gitops!326). Target pivoted loom-flightdeck→**flexdeck** (loom-flightdeck is Elixir, no `make fmt`; flexdeck is Go → devbox `gofmt` passes). Proven end-to-end: **flexdeck!244 merged autonomously** by `group_3_bot` (merge_commit `07c429ca`). | cluster | ✅ PASSED 2026-07-05 |
| **S6** | Demand — plan-slice emitter sources a `cross_repo.demand_projects` allowlist + stamps `TargetProject`; two-key activation (allowlist consulted only when `cross_repo.enabled`). Default-OFF/empty = home-only. | `pkg/mills/policy.go`, `pkg/mills/intake/plan_slice_emitter.go`, `cmd/loom-mills-operator/main.go` | ✅ **code SHIPPED** 2026-07-06 (default-OFF); ⏳ activation (gitops `demand_projects` + roll + verify) |

**Sequencing note**: S1–S4 ship together (or S1+S2, then S3, then S4) as a
**default-OFF** capability so nothing is inert-yet-live and no half-pipeline ever
runs (S4's fail-closed guard is what makes a partial rollout safe). The flag flips
ON only in S5 alongside the kill-test.

## Supporting (independent quality) slice — NOT the keystone

- **Workspace-PVC population/resize** (originally queued as "S2"): stage more
  services repos on `loom-hub-workspace` (Longhorn **RWO** 40Gi, read-only mount)
  so build-heavy target repos fingerprint on the correct toolchain image instead
  of !941's generic fallback. Independent of the keystone (the tests stage clones
  fresh in its own devbox sandbox regardless), **lower priority**, and needs the
  cluster (RWO single-writer coordination + possible resize). Riskiest assumption:
  a co-located writer Job can write the RWO PVC while mobile-hud holds it
  read-only, with room at 40Gi. See `docs/MILLS_RUNBOOK.md` § "Spawn pool capacity
  & multi-repo".

## Dependencies / blockers

- S1–S4: pure loom-core code — locally unit-testable, shippable via `git push` +
  `glab` even during the MCP outage. **Unprovable end-to-end until S5.**
- S5 + PVC slice: require cluster + loom MCP (down 2026-07-05).
- No new credential needed for services repos (group token ready). Cross-*group*
  (libs/, platform/) is explicitly OUT (needs a separate token grant + a
  group-aware git-clone base — see the S3 follow-up task in agent-context).
