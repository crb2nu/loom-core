# Mills — Next Round of Functionality + UX Fixes (2026-05-18)

Multi-slice plan for the next round of Mills (loom-mills-operator) work. Scoped
to operator-facing leverage: data trust on the Overview, HUD parity with the
CLI for the few day-2 actions that still force a terminal drop, and closing
the deferred polish from the most recent KPI/Sub-tab feature.

## Status (2026-06-03)

| Slice | Status | Evidence |
| --- | --- | --- |
| 0 — Kill-test (operator walkthrough) | **PASSED 2026-06-02** | Operator interview: 5/5 day-2 actions answered "used recently + would use a button" (gate was ≥3). Kill-switch routing decided = GitOps auto-PR. |
| 1 — HUD action parity for day-2 ops | **all 5 actions SHIPPED + deploy MRs merged** (live 1e pending image rollout) | 4/5 via !609 (force-escalate, council run/dryrun, audit-by-iid); 5th = pause/resume autonomy kill-switch via `POST /api/mills/policy/kill-switch` GitOps auto-PR, merged !611 (`98f82f62`) + env wiring `platform/gitops!216` (`1f0bdd56`, merged). Dedicated `loom-mills-gitops` token (id 16) **installed** + env **reconciled** (deploy rev 323, confirmed 2026-06-03). Iteration plan: `.loom/127-…`. **Sole remaining gate: fresh operator image build off `main` + Flux rollout** — running image `:20260603-043339` predates the !611 merge (14:10Z) so the endpoint isn't in the live binary yet — then the live 1e operator kill-test. |
| 2 — KPI honesty | **shipped 2026-05-19** | `dbccd413` (MR merged via `a748d138`) |
| 3 — Sub-tab counts coverage | **shipped 2026-05-19** | `f99a48ef` (MR merged via `e6110f95`) |
| 4 — Spawn/escalator stabilization debt | **shipped 2026-05-19** | `985f2f5d` (MR merged via `55a0bcf0`); STATES.md at `pkg/mills/pipeline/STATES.md` |
| 5 — Idle-state one-click | **shipped 2026-05-19** | `53419638` (MR merged via `b9c6ca66`) |

Slice 4 deviation from the original plan: STATES.md landed in
`pkg/mills/pipeline/`, not `pkg/mills/runner/` — the pipeline package
owns the spawn lifecycle state machine; `pkg/mills/runner/` is the
council runner. The doc covers both state machines + the cross-component
seam and includes the 4a regression-test audit inline.

All five slices have now shipped code, and both kill-switch deploy MRs
(loom-core!611 + gitops!216) are merged, the `loom-mills-gitops` secret
is installed, and the operator env is reconciled. The only remaining gate
is a **fresh operator image build off `main` + Flux rollout** (the running
image predates the !611 merge) followed by the **live 1e operator
kill-test** on the deployed HUD. Slice 4 finished ahead of Slice 1 because
Slices 2/3/4/5 are mutually independent and 4 did not require the operator
walkthrough.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The "drop to CLI / curl" friction on the Mills
HUD is the operator's biggest day-2 pain point right now. Specifically, the
top-5 actions documented in `mcp/skills/mills-ops/SKILL.md` ("pause autonomy",
"replay council", "force-escalate stuck run", "audit by MR iid", "trigger
council run from idle") happen often enough that putting them behind HUD
buttons will materially change how often the operator opens a terminal.

**Kill test**: 30-minute live walkthrough with the actual operator on the
deployed HUD at the current state of `main`. Open `loom hud`, visit
`/mills`, and for each of the 5 documented actions in `mills-ops/SKILL.md`
have the operator answer:

  1. "When did you last need to do this?" (last 7 / 14 / 30 days)
  2. "Did you do it from the HUD or did you drop to CLI / curl?"
  3. "If a button existed here, would you have used it?"

Pass criteria: at least 3 of the 5 actions get a "used in the last 14 days"
+ "would use a button" answer. Anything less means the assumption is wrong
and the plan should pivot to KPI honesty (Slice 2) or stabilization debt
(Slice 4) first.

**Failure mode if the assumption is wrong**: We build HUD parity buttons
for actions the operator hits twice a quarter, while the actual daily
pain (e.g., misleading KPIs, recurring spawn flake, missing audit
drilldown) goes unaddressed.

**Status**: not run

> Run before starting Slice 1. If Slice 2 (KPI honesty) wins instead,
> reorder: Slice 2 → Slice 4 → Slice 3 → Slice 1 → Slice 5.

## Context (one-screen)

- **Where Mills lives in this repo**:
  - Operator binary: `cmd/loom-mills-operator/` (1159 LOC `main.go`, 11 `handlers_*.go`)
  - Domain + reconciler: `pkg/mills/` (reconciler.go ~27kB, kpi_writer.go ~8.5kB, council/eval/squads/crossrepo/pipeline/runner/gates/audit/budget subdirs)
  - HUD bridge: `internal/hud/domain/mills/` (mills.go, proxy.go)
  - HUD frontend: `internal/hud/frontend/src/lib/components/Mills/` (11 components, 3869 LOC)
  - Ops skill: `mcp/skills/mills-ops/SKILL.md`
  - GitOps policy ConfigMap: `platform/gitops/k3s/mills/configmap-policy.yaml`

- **Recent landmark commits**:
  - [23e40ad2] `feat(mills,hud): populate 5 missing KPI keys + Mills sub-tab counts + shortened run IDs` (the last big push)
  - [e46dd700] `feat(hud): Mills pipeline run drilldown drawer`
  - [bc9855db] `feat(hud): mills overview health banner surfaces escalation rate`
  - 25+ `fix(mills): ...` commits in recent history covering spawn, escalator, gate, MCP transport, devbox, dedupe, redial.

- **Admitted proxies in current KPI implementation** (per body of 23e40ad2):
  - `cost_per_merged_change_usd` "aliases per-pipeline cost today; switches to backlog-grouped when change attribution lands"
  - `regression_rate = escalatedRuns / (mergedRuns + escalatedRuns)` "best-effort proxy until post-merge regression tracking exists"

- **Deferred polish** (also from the same commit):
  - Sub-tab counts intentionally skipped on `squads`, `audit`, `cross-repo`
    "until their stores expose first-class counts."

- **Open TODOs in code**:
  - `pkg/mills/pipeline/runner.go:931` — fan out per-repo plan stages (slice 4.2 followup)
  - `cmd/loom-mills-operator/handlers_crossrepo.go:165` — signal running integrator (slice 4.4 followup)

## Goals for this round

1. **Make the Overview honest**: KPIs that are proxies must say so; KPIs we
   can compute accurately should be computed accurately.
2. **Close the HUD↔CLI parity gap** for the 5 day-2 actions in
   `mills-ops/SKILL.md`, gated behind the admin token already in use.
3. **Close the deferred sub-tab counts** so Mills navigation reads
   uniformly.
4. **Reduce stabilization-fix cadence** by consolidating the most-touched
   spawn/escalator surface and turning a few of those fixes into
   regression tests.
5. **Onboarding**: the "Council has never run" idle state gets a one-click
   trigger.

## Non-goals (explicit)

- No new Mills capability surface (e.g., new gate types, new ensemble
  modes, new policy fields). The plan is fixes + parity, not feature
  expansion.
- No mobile companion changes for Mills surfaces.
- No CI/CD pipeline changes.
- No theme/visual overhaul beyond the small surface diff each slice
  introduces.

## Slices

### Slice 1 — HUD action parity for day-2 ops (kill-test gated)

**Scope**: Add minimal HUD UI for the 5 day-2 actions documented in
`mcp/skills/mills-ops/SKILL.md`, behind a single shared
`MillsAdminActions` primitive that handles admin-token entry, confirm
dialog for destructive ops, and error toast on failure.

Actions:
  1. **Pause / resume autonomy** (current path: edit ConfigMap →
     commit → Flux reconcile). HUD action calls a new
     `POST /api/mills/policy/kill-switch` that writes through the
     same policy manager as the file reload, with a "this writes
     to the live ConfigMap and may be reverted by Flux" warning.
     Or — preferred — opens a pre-filled GitOps PR via the
     existing GitLab MCP rather than fighting Flux. Decide in
     Slice 1a.
  2. **Force-escalate a pipeline run** (current path: curl with
     bearer token). HUD button on `PipelineRunDetail.svelte`
     calls `POST /api/mills/pipeline/runs/<id>/escalate` directly.
  3. **Replay council with current ensemble** (current path:
     `loom mills council run`). HUD button on `CouncilPanel.svelte`
     calls a new `POST /api/mills/council/run`.
  4. **Trigger council dryrun** (current path:
     `loom mills council dryrun`). Same panel, secondary button.
  5. **Audit by MR iid** (current path: curl). HUD search box on
     `AuditPanel.svelte` calls existing
     `/api/mills/pipeline/runs?mr_iid=...` and links the result
     to its originating council run via Loop B attribution.

**Files**:
- `internal/hud/frontend/src/lib/components/Mills/shared/MillsAdminActions.svelte` (new)
- `internal/hud/frontend/src/lib/components/Mills/{PipelineRunDetail,CouncilPanel,AuditPanel}.svelte` (edits)
- `cmd/loom-mills-operator/handlers_council.go` (new `POST /run`, `POST /dryrun` endpoints)
- `cmd/loom-mills-operator/handlers_status.go` or new `handlers_policy.go` (kill-switch endpoint, if not GitOps PR)
- Tests: handler tests in `cmd/loom-mills-operator/` + a Playwright/Svelte component test if the project has one.

**Done when**:
- All 5 actions reachable from the HUD without dropping to a terminal.
- Each writes an audit-log entry (the existing audit log structure in
  `pkg/mills/audit/` should already cover this; verify in the slice).
- Kill-test runbook recorded results before merge.

### Slice 2 — KPI honesty (data trust)

**Status**: shipped 2026-05-19 via `dbccd413` (`a748d138` merge).
Backlog-grouped `cost_per_merged_change_usd` + label-driven
`regression_rate` landed; KPI cards no longer carry a `(proxy)` chip
for these two.

**Scope**: Stop two KPIs from lying. Both are flagged in the
23e40ad2 commit body as proxies.

**2a — cost_per_merged_change_usd: backlog-grouped attribution.**

  - Today: aliases per-pipeline cost (`cost_per_merged_pipeline_usd`).
  - Target: aggregate cost across all pipeline_runs that closed the
    same `backlog_item_id`, divide by distinct merged backlog items.
  - Files: `pkg/mills/kpi_writer.go` (add `ListMergedRunsGroupedByBacklog`
    or compute in the existing window join), `pkg/mills/store/...` if a
    new query method is needed, `pkg/mills/kpi_writer_test.go` (pin the
    new grouping with a fixture where 2 pipeline runs close 1 backlog).

**2b — regression_rate: real post-merge regression tracking.**

  - Today: `escalatedRuns / (mergedRuns + escalatedRuns)`. This counts
    escalation, not regression.
  - Target: define "post-merge regression" as a follow-up MR within 7d
    of a merged change that touches the same files and is itself
    labeled `regression-fix` (or originates from the
    `loom-mills-canary` regression-detector path). Compute:
    `regressionsLast7d / mergedRunsLast7d`.
  - Files: `pkg/mills/kpi_writer.go`, possibly a new
    `pkg/mills/regression/detector.go`. New test fixture in
    `pkg/mills/kpi_writer_test.go`.

**2c — UI label proxies as proxies.**

  - For any KPI that is still a proxy after 2a/2b, render a small
    `(proxy)` chip on the card so the operator knows not to draw
    conclusions from short-term moves.
  - File: `internal/hud/frontend/src/lib/components/Mills/MillsKPIRow.svelte`.
  - Add a `proxy?: boolean` field in the card object and a `MetricCard`
    chip slot or `info?:` icon with tooltip.

**Done when**:
- New unit tests pin both KPIs against fixtures.
- The fixture cases include "no merged runs in window" → key still
  conditionally omitted (matches current behavior in 23e40ad2).
- HUD cards display the chip on remaining proxies (none expected if 2a+2b
  land).

### Slice 3 — Sub-tab counts coverage (close deferred polish)

**Status**: shipped 2026-05-19 via `f99a48ef` (`e6110f95` merge).
`squads`, `audit`, and `cross-repo` sub-tabs now render count pills
when nonzero.

**Scope**: Wire counts on the three sub-tabs intentionally left
countless in 23e40ad2: `squads`, `audit`, `cross-repo`.

  - Squads count: number of active squads in `millsStore.squads`
    (likely from `mills_squads.svelte.ts`).
  - Audit count: open audit findings (state != closed) from
    `mills_audit.svelte.ts`.
  - Cross-repo count: in-flight cross-repo integrations from
    `mills_crossrepo.svelte.ts`.

**Files**:
- `internal/hud/frontend/src/App.svelte` (wire `count?:` on the three
  remaining `subViews` entries).
- The three stores above (expose `activeCount` / `openCount` derived
  state if not already present).
- `internal/hud/frontend/src/lib/components/shared/ViewShell.svelte`
  (already supports `count?: number` per the 23e40ad2 commit body —
  no change needed).

**Done when**: All Mills sub-tabs render a count pill when nonzero.

### Slice 4 — Spawn/escalator stabilization debt

**Status**: shipped 2026-05-19 via `985f2f5d` (`55a0bcf0` merge).
Audit finding: all 6 named fix commits already shipped with regression
tests that pin the post-fix invariants. No backfill was needed.
State-machine reference landed at `pkg/mills/pipeline/STATES.md` (the
pipeline package owns the spawn lifecycle wiring; the `runner/` path
named in the plan is the council runner). The doc covers both state
machines, the cross-component seam where the "ResumeSpawnID
propagation" bugs cluster, 8 known sharp edges, and 3 follow-up slice
candidates (none blocking).

Regression-test audit table (canonical version inline in STATES.md):

| Commit | Symptom fixed | Regression test |
| --- | --- | --- |
| `a3070890` | Empty `stage_results.log_tail` on plan_slice error rows | `TestRunner_PersistsSpawnFailureContextWhenWorkerReturnsEmptyError`, `TestRunner_PersistsSpawnFailureContextOnPendingPath`, `TestBuildFailureLogTail_Precedence` (`pkg/mills/pipeline/runner_test.go`) |
| `a433ce06` | Cached WebSocket transport went stale after close 1006 / broken pipe | `TestCallTool_RetriesOnceAfterTransportClose1006`, `TestCallTool_RetriesOnceAfterBrokenPipeOnSend`, `TestCallTool_DoesNotRetryJSONRPCErrors`, `TestCallTool_StopsAfterOneRetry`, `TestIsTransportError` (`pkg/mills/clients/mcphub_test.go`) |
| `6cb0dcd1` | `ResumeSpawnID` dropped at the fallback-dispatcher boundary | `TestFallbackDispatcher_PropagatesResumeSpawnID` (`cmd/loom-mills-operator/dispatcher_test.go`) |
| `a08c8f7d` | Accepted spawn marked failed on poll interruption | `TestRunner_KeepsAcceptedSpawnPendingOnInterruptedPoll`, `TestMapTelemetryToResponse_PreservesTerminalStatusWithoutTelemetry` |
| `f2dec2cf` | Double-dispatch on a stage with a pending spawn | `TestRunner_StartSuppressesDuplicateActiveRun`, `TestHandlePipelineEscalate_MarksRunAndBacklog` |
| `f55f8cfe` | Spawn ID not recorded before first poll → mid-spawn escalation | `TestRun_RecordsAcceptedSpawnBeforePolling`, `TestResumePollsExistingSpawnWithoutPost`, `TestRunner_ResumesPendingStageSpawnAttempt` |

---

**Original scope** (preserved for context): Reduce the `fix(mills): ...`
cadence by turning recent fixes into regression tests and
consolidating the spawn lifecycle paths.

Concrete subset (from `git log --oneline -- pkg/mills/ cmd/loom-mills-operator/`):
  - `a3070890 fix(mills): persist spawn failure context to stage_results.log_tail`
  - `a433ce06 fix(mills): redial broken MCP hub transport on close 1006 / broken pipe`
  - `6cb0dcd1 fix(mills): unstick pipeline runs by propagating ResumeSpawnID`
  - `a08c8f7d fix(mills): keep accepted spawns pending on poll interruption`
  - `f2dec2cf fix(mills): guard pending spawn ownership`
  - `f55f8cfe fix(mills): resume accepted HUD spawns`

Each "fix" landed with at most a single targeted test. Two things this
slice does:

**4a — Audit which of those fixes have regression tests now.**

  - Spawn a survey agent or grep for `TestMills...` + `TestSpawn...` to
    list coverage. For each fix without a test, add the smallest
    failing test that catches the original symptom.

**4b — Identify the consolidation seam.**

  - Read `pkg/mills/runner/` + `cmd/loom-mills-operator/handlers_*` for
    the actual spawn lifecycle state machine. Document it in a 1-page
    ASCII diagram in `pkg/mills/runner/STATES.md` (new). If the diagram
    shows two parallel state machines (likely cause of the
    "ResumeSpawnID propagation" class of bugs), file the consolidation
    as a follow-up slice — do NOT do it in this round.

**Done when**:
- Each of the 6 commits above has a named test in `pkg/mills/` or
  `cmd/loom-mills-operator/` that fails on the parent commit and passes
  on HEAD.
- `STATES.md` exists with the diagram + a "known sharp edges" list.

### Slice 5 — Idle-state one-click

**Status**: shipped 2026-05-19 via `53419638` (`b9c6ca66` merge).
Operator banner action now calls `/api/mills/council/run` directly
instead of just navigating to the Council panel. Slice 5 piggybacked
on the same MR that wired the deferred council scheduler.

**Scope**: From the system-health banner's "idle" state
(`OverviewPanel.svelte:54`, "Council has never run"), the action button
is currently "Open council" → navigates to the Council panel. Change
the idle-state action to "Run council now" which calls the new
`POST /api/mills/council/run` endpoint from Slice 1. Falls back to the
nav if Slice 1 hasn't landed.

**Files**:
- `internal/hud/frontend/src/lib/components/Mills/OverviewPanel.svelte`
  (bannerActionLabel, runBannerAction).

**Done when**: A first-time operator can trigger their first council
run with one click from the Overview banner.

## Sequencing

```
  ┌─────────────────────────────────────────────────────────┐
  │  Slice 0 (kill-test): walkthrough with operator         │
  │  → result determines whether Slice 1 proceeds first     │
  └─────────────────────────────────────────────────────────┘
          │
          ├─ pass ──► Slice 1 ─► Slice 5 (depends on 1's endpoint)
          │              │
          │              └─► Slice 3 (independent polish)
          │
          └─ fail ──► Slice 2 (KPI honesty) ─► Slice 4 ─► reconsider 1
                          │
                          └─► Slice 3 (independent polish)
```

Slices 2, 3, 4 are mutually independent; Slice 5 depends on Slice 1's
new `/council/run` endpoint.

## Risk register

- **Risk: Kill-switch via direct ConfigMap write fights Flux.** Mitigated
  by routing the HUD button through an auto-PR to
  `platform/gitops/k3s/mills/configmap-policy.yaml` rather than calling
  a write-through endpoint. Decide in Slice 1a.
- **Risk: Regression detector in 2b mislabels.** Mitigated by
  conservative `regression-fix` label + 7d window; keep the proxy chip
  in 2c on if the detector hasn't been validated against ≥10 real
  past regressions.
- **Risk: Slice 4 documentation reveals a deeper architectural seam
  we don't want to touch in this round.** That's fine — the slice
  explicitly stops at the diagram + follow-up-slice filing.
- **Risk: Each Slice 1 button needs its own confirm UX and error
  handling.** Mitigated by the shared `MillsAdminActions` primitive.

## Verification per slice (kill criteria)

- **Slice 1**: A live operator pauses autonomy, replays council,
  force-escalates a run, queries by MR iid, and triggers a council run
  — all from the HUD without opening a terminal. Audit log shows 5
  entries. Run on the deployed cluster, not localhost.
- **Slice 2**: `go test ./pkg/mills/... -run KPI -count=1` passes
  with new fixtures.  Overview cards still render "—" when fixtures
  have zero merged runs in the window.
- **Slice 3**: Visual inspection of all 8 Mills sub-tabs shows count
  pills where data is present.
- **Slice 4**: `git bisect run` against each of the 6 fix commits with
  the new tests fails on the parent and passes on the commit.
- **Slice 5**: First-time HUD render with empty Mills state shows a
  "Run council now" button that triggers a council run.

## Effort estimate (rough)

| Slice | Estimate | Notes |
|-------|----------|-------|
| 0 (kill-test) | 30 min live | Operator availability is the constraint |
| 1 (HUD parity, 5 actions) | 3 days | Bulk is shared admin-actions primitive + GitOps PR routing for kill-switch |
| 2 (KPI honesty) | 2 days | Most of it is the regression detector + 2 new SQL queries |
| 3 (sub-tab counts) | 0.5 day | Pure polish, three derived counts |
| 4 (stabilization) | 1.5 days | Time-bounded; stop at diagram, don't refactor |
| 5 (idle one-click) | 0.25 day | Depends on Slice 1 |

## Open questions

1. **Kill-switch routing**: write-through HTTP endpoint that competes
   with Flux, or auto-PR via GitLab MCP? (Slice 1a decision)
2. **regression-fix label canonical source**: who emits the label?
   The integrator? The operator? A nightly classifier? Decide before
   Slice 2b.
3. **Audit log destination**: confirm the existing `pkg/mills/audit/`
   structure is the right home for HUD-initiated actions or whether a
   new `hud-initiated` audit-source tag is needed.

## Sources

- [S1] Recent Mills commits: `git log --oneline --grep -i 'mill' -50`
- [S2] KPI proxy admissions: `git show 23e40ad2` body
- [S3] Day-2 action list: `mcp/skills/mills-ops/SKILL.md`
- [S4] Open code TODOs: `pkg/mills/pipeline/runner.go:931`, `cmd/loom-mills-operator/handlers_crossrepo.go:165`
- [S5] System health banner states: `internal/hud/frontend/src/lib/components/Mills/OverviewPanel.svelte:47-96`
- [S6] KPI card definitions: `internal/hud/frontend/src/lib/components/Mills/MillsKPIRow.svelte:74-121`
- [S7] Mills frontend inventory: `find internal/hud/frontend/src -name '*.svelte' | xargs grep -ln -i 'mill'`
- [S8] Mills component sizes: `wc -l internal/hud/frontend/src/lib/components/Mills/*.svelte`
- [S9] GitOps policy path: `mcp/skills/mills-ops/SKILL.md` ("Apply via GitOps")
