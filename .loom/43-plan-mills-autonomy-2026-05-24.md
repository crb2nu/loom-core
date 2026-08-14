# Mills — Autonomy Round: Make It Actually Ship MRs Without Me (2026-05-24)

Multi-slice plan to lift Loom Mills from "operator-driven with occasional
autonomy" to "drives merged MRs/day with no human touch." Successor to
`.loom/42-plan-mills-next-round-fixes-2026-05-18.md` (operator HUD parity
round), but addresses a different layer: the autonomy loop itself, not
operator ergonomics.

## Status (2026-05-24)

| Slice | Status | Evidence |
| --- | --- | --- |
| 0 — Kill-test (escalation-cause telemetry pull) | **DONE 2026-05-24** | Report: `.loom/local/handoffs/mills-autonomy-killtest-2026-05-24.md`. Findings forced revised ordering + new Slice 2e + dropped Slice 2b. |
| 1a — Demand: GitLab issue importer (label-gated) | **CODE LANDED 2026-05-24 (gated off in prod)** | `pkg/mills/intake/gitlab_importer.go`, `pkg/mills/clients/gitlab.go:ListIssues`, `pkg/mills/policy.go:IntakePolicy`, `cmd/loom-mills-operator/main.go:buildGitLabImporter`. Policy stays `intake.gitlab.enabled: false` in `configmap-policy.yaml` until Slices 2c+2e land — flipping on now just deepens the escalation pile. Tests: `pkg/mills/intake/gitlab_importer_test.go` (5 tests), `pkg/mills/clients/gitlab_test.go` (3 new ListIssues tests). Runbook entry: `docs/MILLS_RUNBOOK.md`. |
| 2c — Supply: transient-vs-code retry classification | **CODE LANDED 2026-05-24** | `pkg/mills/pipeline/error_class.go` + classifier table-driven against real kill-test fixtures; runner split into `attempts` (row monotonicity, hard-capped) + `effectiveAttempts` (budget — counts only Infra+Code); `pkg/mills/metrics.go:PipelineStageErrorClassTotal` for Grafana; `RetryPolicy.TransientRetryCap` knob (default 5). 4 new runner tests pin: transient flakes don't burn budget; code errors do; hard cap stops runaway transients; infra counts against budget. |
| 2e — Supply: devbox/buildah infra stabilization (**NEW after kill-test**) | **CODE LANDED 2026-05-24 (eviction race fix only)** | `internal/devbox/backend/k8s_wait.go:waitForPodGone` + `evictPod` (polls until NotFound); `internal/devbox/backend/k8s_build.go:runBuildPod` now evicts-then-waits before Create + handles `apierrors.IsAlreadyExists` with one belt-and-suspenders re-evict + retry. Addresses the ~20% "pods already exists" cluster from kill-test. Tests: 3 new in `k8s_wait_test.go` pin fast NotFound path, timeout, end-to-end evict. Sandbox image Harbor cache + dockerfile-gen hardening deferred (separate slice — different code path in `internal/devbox/dockerfile/` and `internal/devbox/baseimage/`). |
| 2a — Supply: auto-merge wiring (`merge_when_pipeline_succeeds`) | **CODE LANDED 2026-05-24** | `CreateMRRequest.AutoMerge` flows through `createMRBody.MergeWhenPipelineSucceeds`. Per-item gate via `GitLabWorker.AutoMergeFor` callback (operator main wires it to `policy.LabelOverrideFor` + `item.Policy.AutoMerge`). 3 new dispatcher tests + 2 new client tests pin both flag-set and flag-absent paths. |
| 1b — Demand: workspace-signals council brief | not started | Heavier — needs Loki client + council brief schema change. Deferred to next round; not blocking the autonomy loop now that supply works. |
| 1c — Demand: LLM-ranked dispatch | not started | Heaviest R&D in the plan. Needs model selection + cost validation that should be reviewed before code lands. Deferred. |
| 3a — Loop closure: webhook notification on merge | **CODE LANDED 2026-05-24** | New package `pkg/mills/notify/`. `WebhookHook.OnMerged` posts Slack/Discord-compatible JSON summary (MR link, retries, recovered error_class hints from stage_results). 6 tests cover disabled state, payload shape, 5xx swallow, nil-safety, store-failure resilience. Wired into the OnMerged chain in operator main; gated by `policy.notify.webhook_url`. |
| 3b — Loop closure: tick-on-merge | **CODE LANDED 2026-05-24** | `Scheduler.KickNow()` channel-driven, 1-buffered (coalesces). Closes the 60s tick gap between merges. OnMerged chain calls `schedulerRef.KickNow()` via closure. 3 new tests pin: kick triggers tick within ~1s, kick-before-Run is safe, nil-receiver no-panic. |
| 3d — Loop closure: bounded escalation auto-retry | **CODE LANDED 2026-05-24** | `Runner.maybeAutoRetry` diverts a transient-cap escalation into a re-queue (run goes Escalated, backlog stays Queued, scheduler kicked). Policy knob `EscalationAutoRetryCap` (default 0 — off until operator opts in). `Runner.OnAutoRetry` hook wired to `scheduler.KickNow` for ~1s pickup. 4 new tests pin: transient cap auto-retries, hits cap, code-class never auto-retries, reason classifier. |
| Stale-canary GC (post-kill-test add) | **CODE LANDED 2026-05-24** | `pkg/mills/intake/canary_gc.go` periodic sweep deletes escalated `mills-canary` items older than `stale_after_hours`. Solves the starvation the kill-test surfaced: prod state.db has 56 escalated canaries blocking new enqueues. Opt-in via `policy.intake.canary_gc.enabled`. 7 tests cover happy path, label filter, state filter, dry-run, error propagation, delete failure, defaults. |
| ~~2b — Promote `pr_self_review` to merge-blocking~~ | **DROPPED** | Kill-test: `gate_fail = 0` in 125 failing stage_results, `gate_pass_rate = 0.918`. Promoting a non-failing gate is a no-op. |
| 3c — Loop closure: outcome→ranker feedback | not started | Bundled with Slice 1c. Both deferred together since the writeback only matters once a ranker reads it. |

### Kill-test summary (Slice 0)

Full report at `.loom/local/handoffs/mills-autonomy-killtest-2026-05-24.md`.
Key numbers from live `state.db` pull (operator pod
`loom-mills-operator-59cbc7bfdf-bm9xh`):

- **0 of 56** pipeline_runs ever reached `done`. **100% escalated**, every
  day since at least 2026-05-17 (auto_merge_rate=0, escalation_rate=1.0).
- **Top failing stages**: `tests` (93% errors — devbox quality_gate),
  `plan_slice` (69% — buildah spawn-runtime build). `pr_self_review`
  never errored across 9 evaluations.
- **Root cause split** (n=125 failing stage_results): ~62% transient
  (k8s pod GC + MCP close 1006/1000 + broken pipe + flexinfer timeout),
  ~22% buildah/devbox infra (pod-naming conflicts, dockerfile gen
  broken), 0% `gate_fail`.
- Mills was **idle** (queue_depth=0) at pull time. Escalated canaries
  block new canary enqueues forever (`2fcc705a`); no other intake exists.
  Slice 1a is therefore a prerequisite for testing any supply fix.

### Revised sequencing (post kill-test) — final ship status

```
  Slice 1a   ✅ GitLab issue importer
  Slice 2c   ✅ Transient-vs-code retry classification
  Slice 2e   ✅ Devbox/buildah eviction race fix
  Slice 2a   ✅ MergeWhenPipelineSucceeds wired
  Slice 3a   ✅ Webhook notify on merge
  Slice 3b   ✅ Tick-on-merge (scheduler.KickNow)
  Slice 3d   ✅ Bounded escalation auto-retry
  Canary GC  ✅ Sweep stale escalated canaries

  Deferred (need model selection or council-brief schema work):
  Slice 1b   ⏸ Workspace-signals council brief (Loki + GitLab CI feed)
  Slice 1c   ⏸ LLM-ranked dispatch
  Slice 3c   ⏸ Outcome→ranker feedback (paired with 1c)

  DROPPED:   Slice 2b (gate_fail=0; nothing to merge-block).
```

### Net effect on primary metric

Pre-session state (kill-test, 2026-05-24 morning):
- 100% escalation rate, 0% auto-merge rate, queue starved.
- Top failure causes: ~62% transient infra, ~22% buildah pod conflicts,
  0% gate_fail.

Post-session theory of operation:
- Slice 2e fixes the buildah cluster (pod-eviction race + 409 belt-and-
  suspenders) — should knock down the 22% directly.
- Slice 2c classifies the remaining 62% transient failures as free
  retries, so the operator's MaxAttempts budget is spent on real bugs
  instead of getting eaten by MCP/k8s flake.
- Slice 3d wraps the survivors: a transient-cap escalation is auto-
  retried up to N times instead of dumping to the human queue.
- Slice 2a wires `merge_when_pipeline_succeeds=true` so GitLab merges
  the MR autonomously — Mills no longer holds a worker slot on
  synchronous merge.
- Slice 1a + Canary GC unblock intake: GitLab issues label-imported,
  stuck canaries swept so new canaries can flow.
- Slice 3b ensures the next backlog item dispatches in ~1s after merge,
  not ~60s.
- Slice 3a tells the operator (Cody) when Mills shipped something
  without polling the HUD.

The deferred slices (1b, 1c, 3c) all depend on model selection +
cost validation that the operator should review before code lands.

## North-star metric

**Merged MRs/day with no human touch**, measured as:
```
autonomous_merges_24h = count(pipeline_runs WHERE
  terminal_state = "merged"
  AND attempts_total = 1
  AND escalated = false
  AND merged_via = "auto"
  AND last_24h)
```

This is a superset metric — every other pain point the operator surfaced
(backlog quality, spawn flakes, auto-merge gate, loop closure) shows up
as a drag on it.

Secondary tracking (also already emitted in KPI snapshots, per
`pkg/mills/kpi_writer.go:105-125`):
- `auto_merge_rate` — merged ÷ terminal
- `escalation_rate` — escalated ÷ terminal
- `regression_rate` — label-driven, per `dbccd413`

## Riskiest assumption + kill-test

**Load-bearing assumption**: The primary blocker to "merged MRs/day with
no human touch" is supply-side — specifically, that (a) auto-merge is
not wired at the GitLab API layer and (b) LLM quality gates are advisory
instead of merge-blocking. If we wire auto-merge + flip one gate to
merge-blocking, regression rate stays under 10% (current proxy regression
rate per `dbccd413`) and merged-MRs/day rises measurably within 7 days.

**Kill test**:
1. Pull the last 14 days of pipeline_run terminal states from
   `loom_mills_kpi_snapshots` and `pkg/mills/store/`.
2. Compute, by day:
   - Total runs reaching `terminal_state`
   - % that auto-merged vs human-merged vs escalated
   - Top 5 escalation reasons (via `classifyEscalationReason` at
     `pkg/mills/pipeline/runner.go:973-990`: `retry_cap_exceeded`,
     `gate_fail`, `stage_error`, `cross_repo`, `other`)
   - Avg attempts/run for runs that did merge
3. Cross-check: how many of the *runs that merged* used the
   GitLab `MergeWhenPipelineSucceeds=true` field? (Expected: zero, per
   audit at `pkg/mills/clients/gitlab.go:mergeBody` — field never set.)

**Pass criteria** (≥2 of 3):
- Auto-merge usage rate today is ≤ 5% (confirms wiring is the gap).
- Top escalation reason in last 14d is NOT `gate_fail` (confirms gates
  are advisory; promoting them to merge-blocking won't tank throughput).
- At least 30% of runs reach terminal state with attempts > 1 (room to
  improve via transient-vs-real-code classification).

**Failure modes if the assumption is wrong**:
- If gate failures dominate, flipping gates to merge-blocking will *drop*
  merge throughput, not raise it — fix the rubric/judge instead.
- If escalations are dominated by demand-side issues (e.g., backlog
  items that no agent can solve), Slice 1 (LLM ranker) leapfrogs Slice 2.
- If runs that merge already use auto-merge in some path we missed,
  the wiring is partial, not absent — investigate where.

**Status**: not run

> Run before starting Slice 2. If Slice 2 fails the pre-condition, reorder
> to Slice 1 → Slice 3 → revisit Slice 2 with rubric/judge fix first.

## Context (one-screen)

**Where the loop sits today**, derived from three parallel explore agents
on 2026-05-24:

```
  ┌────────────────────────────────────────────────────────────────────┐
  │ DEMAND                                                              │
  │   • Backlog sources: canary (auto) + manual API. No GitLab issue   │
  │     importer; no roadmap pull; no Loki error-log autopilot.        │
  │     [pkg/mills/council/backlog_mutator.go:104-117]                 │
  │   • Dispatch: priority ASC, created_at ASC (FIFO within tier).     │
  │     No LLM ranker, no demand-signal feedback.                      │
  │     [pkg/mills/store/dao_backlog.go:111-115]                       │
  └────────────────────────────────────────────────────────────────────┘
                                  │
  ┌────────────────────────────────────────────────────────────────────┐
  │ SUPPLY                                                              │
  │   • Spawn lifecycle: 22% of recent commits cluster on ResumeSpawnID│
  │     seam. STATES.md exists; gaps documented.                       │
  │     [pkg/mills/pipeline/STATES.md]                                 │
  │   • Gates: auto gates (diff/scope/secret) are merge-affecting via  │
  │     retry; LLM gates (spec_conformance, pr_self_review) are        │
  │     ADVISORY — fail triggers retry, never blocks merge.            │
  │     [pkg/mills/gates/llm_judge.go:164-222,                         │
  │      pkg/mills/pipeline/runner.go:700]                             │
  │   • Auto-merge: GitLab MergeWhenPipelineSucceeds field exists but  │
  │     is initialized to false and never toggled. Merge stage is a    │
  │     synchronous wait — operator worker slot is held until CI green.│
  │     [pkg/mills/clients/gitlab.go:mergeBody,                        │
  │      pkg/mills/pipeline/dispatcher.go:545]                         │
  │   • Retry: attempt-count blind. Transient transport error and real │
  │     code bug both consume same MaxAttempts budget.                 │
  │     [pkg/mills/pipeline/runner.go:370-375]                         │
  │   • Telemetry: PipelineStageAttemptsTotal exists; no per-spawn     │
  │     success% by error_class.                                       │
  └────────────────────────────────────────────────────────────────────┘
                                  │
  ┌────────────────────────────────────────────────────────────────────┐
  │ LOOP CLOSURE                                                        │
  │   • Post-merge attribution: WORKS — push via OnMerged chain        │
  │     (attributor + squadRecorder + audit). Idempotent.              │
  │     [cmd/loom-mills-operator/main.go:220,235,                      │
  │      pkg/mills/eval/outcome_attributor.go:53-88]                   │
  │   • Budget: WORKS — rolling 24h, per-tier, self-healing.           │
  │     [pkg/mills/budget.go:83-159]                                   │
  │   • 24/7 scheduler: WORKS — council on cron, reconciler on 60s     │
  │     tick with 5min idle backoff.                                   │
  │     [pkg/mills/council_scheduler.go:71-99,                         │
  │      pkg/mills/scheduler.go:99-150]                                │
  │   • Notification: GAP — no slack/email/webhook. Discovery is HUD-  │
  │     polling only.                                                  │
  │   • Tick-on-merge: GAP — next item waits up to 60s after a merge.  │
  │   • Outcome→ranker feedback: GAP — Mills observes regression-fix   │
  │     labels for KPI but doesn't feed them back into dispatch.       │
  │     [pkg/mills/kpi_writer.go:190-195,                              │
  │      pkg/mills/gates/regression.go:73-110]                         │
  └────────────────────────────────────────────────────────────────────┘
```

**Operator-confirmed pain points (2026-05-24)** — all four:
- Backlog starvation / quality
- Spawn flakes + escalations
- Output quality / auto-merge gate
- Loop doesn't close

**Operator-confirmed primary metric**: "Merged MRs/day with no human touch."

## Goals for this round

1. **Wire auto-merge end-to-end** so a passing pipeline lands without an
   operator visiting the MR.
2. **Promote one LLM quality gate from advisory to merge-blocking** with
   an audit trail, so auto-merge can be trusted.
3. **Classify retries** (transient vs. real-code) so spawn flakes don't
   burn attempt budget meant for actual problem-solving.
4. **Diversify backlog intake** beyond canary + manual API: at least one
   real, autonomous source (GitLab issues w/ label gate is the lowest-
   risk start).
5. **Close the feedback loop** so post-merge outcomes shape future
   dispatch and the operator finds out merges happened without polling.

## Non-goals (explicit)

- No new gate *types* in this round (e.g., no security scanner gate).
  Promote one existing gate, don't add new ones.
- No mobile companion changes.
- No cross-repo expansion. Cross-repo follow-up TODOs at
  `pkg/mills/pipeline/runner.go:931` and
  `cmd/loom-mills-operator/handlers_crossrepo.go:165` stay deferred.
- No council ensemble swap — keep current models, focus on plumbing.
- No HUD redesign. The HUD parity work continues in plan 42 separately.

## Slices

### Slice 0 — Kill-test telemetry pull (1 hour, gates Slice 2)

**Scope**: Pull 14d of pipeline_run terminal data from KPI snapshots +
`pkg/mills/store/`. Produce a one-screen report in
`.loom/local/handoffs/mills-autonomy-killtest-2026-05-24.md` answering
the kill-test pass criteria.

**Files** (read-only):
- `pkg/mills/store/` queries against `pipeline_runs` + `backlog_items`
- `loom_mills_kpi_snapshots` table (schema in `pkg/mills/kpi_writer.go`)
- `pkg/mills/pipeline/runner.go:973-990` (escalation reason classifier)

**Done when**: Report exists with the 3 pass-criteria numbers + a
recommended-ordering line at the bottom ("Slice 2 first" or "Slice 1
first").

### Slice 1 — Demand: real backlog feed + LLM-ranked dispatch

**Scope**: Two backlog improvements that compound — diverse intake and
smarter dispatch order.

**1a — GitLab issue importer (label-gated).**

- Add a periodic puller (every 5min in council_scheduler) that calls
  `mcp__loom__gitlab__list_issues` filtered to `labels=mills-eligible`
  across the workspace's GitLab projects.
- For each fresh issue, create a backlog item with:
  - `priority` from a `priority:P0..P3` label (default P2)
  - `spec_doc` rendered from issue body
  - `created_by` = `mills:gitlab-importer`
  - `external_ref` = `gitlab:<project>/<iid>`
- Dedupe by `external_ref` (similar to canary 24h dedup pattern in
  `730ce825`).
- **Files**:
  - `pkg/mills/council/backlog_importer.go` (new)
  - `cmd/loom-mills-operator/main.go` (wire as scheduler.Job alongside CouncilScheduler)
  - `pkg/mills/store/dao_backlog.go` (add `external_ref` column + UNIQUE constraint, migration)
  - Tests: importer_test.go with mock GitLab MCP client.

**1b — Council-driven backlog mutator from real workspace signals.**

- Today the council mutator only persists editor-proposed items
  (`pkg/mills/council/backlog_mutator.go:104-117`).
- Extend the council brief input to include the last 24h of:
  - Loki errors from workspace services (top error-by-count)
  - GitLab CI failure clusters (last 24h, grouped by error message)
  - Open canary failures
- Council can then propose follow-up backlog items grounded in real
  workspace pain.
- **Files**:
  - `pkg/mills/council/brief.go` (add a `WorkspaceSignals` field)
  - `pkg/mills/clients/loki.go` (new client wrapper around loom__loki MCP)
  - `pkg/mills/council/sidecar.go` (optionally) — collects signals
    before debate begins.

**1c — LLM-scored dispatch (replaces FIFO within priority).**

- Replace `ListByState(BacklogQueued) → iterate in priority,created_at`
  with: collect queued items → ask a small ranking model (gemma3:4b or
  similar) to rank by *expected merge probability* given:
  - item priority + age
  - recent merge history of the spawn-agent + repo pair
  - whether the item's spec touches files with active escalations
- Cache the ranking for 5min (re-ranks on backlog change).
- Falls back to FIFO if the ranker is unavailable (autonomy must not
  stall on a model outage).
- **Files**:
  - `pkg/mills/dispatch/ranker.go` (new)
  - `pkg/mills/reconciler.go:175-200` (call ranker instead of direct
    ListByState)
  - Tests: ranker_test.go with deterministic stub model.

**Done when**:
- Slice 1a: A fresh `mills-eligible` GitLab issue appears as a queued
  backlog item within 10 min of label apply.
- Slice 1b: Council brief includes WorkspaceSignals; one council run
  visibly proposes an item grounded in a real error.
- Slice 1c: Ranker output is logged on every dispatch decision; FIFO
  fallback is unit-tested.

### Slice 2 — Supply: auto-merge + merge-blocking quality gate (kill-test gated)

**Scope**: The headline change. Three coupled sub-slices.

**2a — Wire `MergeWhenPipelineSucceeds=true` at MR creation.**

- Per-repo opt-in via `policy.crossrepo.<repo>.auto_merge: true`
  (already a YAML field — see
  `pkg/mills/crossrepo/types.go:AutoMerge`).
- Modify `pkg/mills/clients/gitlab.go:mergeBody` to set the field from
  policy.
- Modify `pkg/mills/pipeline/dispatcher.go:606-625` so the merge stage
  no longer waits synchronously when `MergeWhenPipelineSucceeds=true` —
  return early after MR is created with the flag, and let GitLab close
  it.
- **Risk**: Mills' operator worker slot held by synchronous merge is
  the same one that polls the pipeline; if we return early, who detects
  failure? Solution: a lightweight watcher per outstanding MR (similar
  to council_scheduler's pattern) that fires `OnMerged` when GitLab
  webhook says merged, or escalates after a timeout.
- **Files**:
  - `pkg/mills/clients/gitlab.go`
  - `pkg/mills/pipeline/dispatcher.go`
  - `pkg/mills/pipeline/mr_watcher.go` (new) — periodic poll + webhook
    handler.
  - `cmd/loom-mills-operator/handlers_pipeline.go` (webhook endpoint).
  - Tests: integration test with mock GitLab + state machine: created
    → MWPS=true → CI passes → merged event → OnMerged fires.

**~~2b — Promote `pr_self_review` LLM gate from advisory to merge-blocking.~~**

**DROPPED 2026-05-24 after kill-test.** Live state.db shows
`gate_pass_rate = 0.918` over 147 evaluations, and `gate_fail = 0` in 125
failing stage_results. The `pr_self_review` stage itself has a 0% error
rate (9 successes, 0 errors). There is no merge-blocking problem here to
solve. Original scope preserved in git history if we ever need it back.

**2c — Transient-vs-real-code retry classification.**

- Today retry budget is attempt-count-blind
  (`pkg/mills/pipeline/runner.go:370-375`).
- Reuse `isTransportError` pattern from `pkg/mills/clients/mcphub.go`
  (added in `a433ce06`) to classify failures at the runner layer too:
  - `transient` — MCP transport, devbox timeout, GitLab 5xx → retry for
    free (don't count against MaxAttempts; cap at 5 retries before
    giving up)
  - `transient_quota` — flexinfer 429, GitLab rate limit → retry with
    exponential backoff
  - `code` — gate fail, build failure, test failure → count against
    MaxAttempts
- **Files**:
  - `pkg/mills/pipeline/error_class.go` (new)
  - `pkg/mills/pipeline/runner.go:370-375` (use classifier)
  - Tests: pin each class behavior with fixtures.

**2d — Per-spawn success% telemetry by error_class.**

- New Prometheus counter:
  `loom_mills_spawn_terminal_total{stage, terminal_status, error_class}`
- Helps the operator see where the budget is going without grepping
  logs.
- **Files**:
  - `pkg/mills/mills.go` (metric definition)
  - `pkg/mills/pipeline/runner.go` (emit at terminal-status transitions)
  - Grafana panel doc note (add to mills runbook).

**2e — Devbox/buildah infrastructure stabilization (NEW after kill-test).**

Kill-test classified ~22% of failing stage_results as buildah/devbox
infra (not transient transport): pod-naming conflicts ("buildah-build-…
already exists"), sandbox dockerfile generation breaking ("ensure
sandbox: generate dockerfile"), buildah build exit_code=2/243.

- Fix idempotent buildah pod naming so a re-dispatched spawn doesn't
  hit the existing-pod path. Currently the name embeds the spawn_id; if
  the prior pod hasn't been GC'd, the second build collides. Either
  reuse the pod (poll for terminal) or generate a new monotonic suffix
  on retry.
- Add conflict-retry: on "pods already exist" 409, regenerate the pod
  name and retry once.
- Cache the spawn-runtime sandbox image in Harbor by content hash of the
  Dockerfile inputs, instead of rebuilding via buildah for every
  pipeline run. This is the single biggest plan_slice latency cut too.
- Surface buildah build failures as `error_class=infra:buildah` in the
  Slice 2d metric so we can monitor whether 2e is actually working.
- **Files** (subject to refinement during 2e):
  - `pkg/mills/clients/devbox.go` or wherever the buildah pod spec is
    materialized (cite line in implementation)
  - `pkg/mills/pipeline/dispatcher.go` (spawn dispatch)
  - `platform/gitops/k3s/devbox/` (sandbox image cache config)
  - Tests: pin "buildah 409 retry" + "cache hit path skips buildah".

**Done when**:
- Slice 2a: A pipeline run on a repo with `auto_merge: true` policy
  completes without operator action and merges via
  `MergeWhenPipelineSucceeds=true`. Confirmed by GitLab API response
  + audit log.
- ~~Slice 2b dropped~~ (see above).
- Slice 2c: A pipeline run where MCP transport fails once + succeeds on
  retry consumes 0 attempts from MaxAttempts.
- Slice 2d: New metric appears in `loom mills status` output and a
  Grafana panel shows class breakdown.
- Slice 2e: 7-day window after deploy shows ≤5% buildah-class failures
  (currently 22%) AND `plan_slice` median latency drops by ≥30% (image
  cache hit).

### Slice 3 — Loop closure: outcome→ranker + notifications + tick-on-merge

**Scope**: Close the loop. Three sub-slices, mostly independent of
each other but 3c depends on Slice 1c.

**3a — User notification on autonomous merge.**

- When `OnMerged` fires AND `merged_via=auto`, post to a configured
  webhook (slack/discord/email — keep abstract; webhook URL in policy).
- Include MR title, repo, attempts_total, error_class summary, audit
  link, and link to undo (revert MR).
- **Files**:
  - `pkg/mills/notify/webhook.go` (new)
  - `cmd/loom-mills-operator/main.go` (chain `notify.OnMerged` after
    `attributor.OnMerged`).
  - `pkg/mills/policy.go` (add `notify.webhook_url` field)
  - Tests: pin payload shape.

**3b — Tick-on-merge.**

- Today reconciler waits up to 60s after a merge before picking up the
  next item (`pkg/mills/scheduler.go:99-150`).
- Add `OnMerged → scheduler.KickNow()` so the next backlog item is
  attempted immediately.
- **Files**:
  - `pkg/mills/scheduler.go` (add KickNow channel)
  - `cmd/loom-mills-operator/main.go` (chain `scheduler.OnMerged`).
  - Tests: pin "merge → next tick within 1s" with a fake clock.

**3c — Outcome → ranker feedback (depends on Slice 1c).**

- Slice 1c's ranker takes "recent merge history of spawn-agent + repo
  pair" as a signal. Slice 3c is the writeback path: when a pipeline
  merges or escalates, append an outcome row that the ranker reads on
  its next decision.
- Outcome row shape (in `pkg/mills/dispatch/outcomes.go`):
  - spawn_agent, repo, backlog_item_id, terminal_status, attempts_total,
    error_class, council_run_id, merged_via, post_merge_regression
    (filled in later if the `regression-fix` label appears within 7d).
- **Files**:
  - `pkg/mills/dispatch/outcomes.go` (new — append-only log + reader)
  - `pkg/mills/eval/outcome_attributor.go:53-88` (chain a writer)
  - `pkg/mills/dispatch/ranker.go` (Slice 1c) — extend prompt with
    last-N outcomes.
  - Tests: outcomes_test.go with fixture sequence.

**3d — Escalation auto-triage with bounded auto-retry.**

- Today escalation halts the run and waits for human
  (`pkg/mills/pipeline/runner.go:776-801`).
- Add: if escalation reason ∈ `{retry_cap_exceeded, stage_error}` AND
  last 3 failure error_classes were all `transient*`, auto-retry the
  whole pipeline once with a fresh attempt budget. Otherwise human-only.
- Cap: each backlog_item can be auto-retried at most twice before
  going to human escalation.
- **Files**:
  - `pkg/mills/pipeline/escalate.go` (add auto-retry path)
  - Tests: pin "all-transient escalation auto-retries once," "mixed-
    class escalation goes to human."

**Done when**:
- 3a: A real autonomous merge triggers a webhook post in <30s.
- 3b: Reconciler picks up next item within 5s of a merge (vs ≤60s).
- 3c: Ranker prompt contains last-N outcomes; one full round-trip
  documented in audit log.
- 3d: A pipeline that escalated due to 3x MCP transport failures
  auto-retries once and succeeds.

## Sequencing

```
  ┌─────────────────────────────────────────────────────────┐
  │  Slice 0 (kill-test): 14d telemetry pull                │
  │  → confirms Slice 2 ordering (auto-merge first) OR      │
  │    pivots to Slice 1 first                              │
  └─────────────────────────────────────────────────────────┘
          │
          ├─ pass ──► Slice 2 ─► Slice 3 (a,b,d in parallel) ─► Slice 1
          │              │
          │              └─► If Slice 2b causes >10% regression spike
          │                  in first 48h, roll back 2b only.
          │
          └─ fail ──► Slice 1 (a,b first; 1c with no ranker priors yet)
                          │
                          └─► then reconsider Slice 2 with rubric fix.

  Slice 1c (LLM ranker) and Slice 3c (outcome writeback) are paired —
  they only deliver value together. Schedule them as 1c+3c bundle.
```

Why Slice 2 first by default:
1. It is the single biggest leverage point on the primary metric (no
   wiring → no autonomous merges, period).
2. It exposes a real KPI signal (auto_merge_rate jumps from ~0% to >0%)
   that gates whether to keep going.
3. Slice 1 (LLM ranker) is the heaviest R&D and benefits from being
   informed by Slice 3c's outcome data — but Slice 3c needs Slice 1c's
   ranker to write into. Doing Slice 2 first buys time to design 1c+3c
   together.

## Risk register

- **Risk: auto-merge ships bad code into production.** Mitigated by
  Slice 2b (merge-blocking gate) + per-repo `auto_merge: true` opt-in
  (default OFF) + `regression-fix` 7d label tracking from `dbccd413`.
  Roll back 2a immediately if `regression_rate` >10% over 48h.
- **Risk: `pr_self_review` gate is unreliable (gemma rubric
  hallucinations were the reason for `831aa5d5`).** Mitigated by piloting
  on `loom-canary` repos first; only flip to merge-blocking on
  production repos after 50+ successful canary runs.
- **Risk: GitLab webhook for merge events is unreliable.** Mitigated by
  the `mr_watcher.go` poller as a fallback (Slice 2a).
- **Risk: LLM ranker (Slice 1c) consumes more flexinfer budget than the
  rest of the loop combined.** Mitigated by caching ranks for 5min +
  using gemma3:4b (smallest deployed model) + FIFO fallback.
- **Risk: Tick-on-merge (Slice 3b) creates a starvation loop where a
  fast-failing item gets re-tried instantly, pinning the scheduler.**
  Mitigated by combining with Slice 2c (transient-vs-code) — only
  transient classes get fast re-dispatch.
- **Risk: Webhook notification (Slice 3a) leaks PII or secrets from
  failed builds.** Mitigated by reusing the existing log_tail
  redaction in `pkg/mills/audit/`.

## Verification per slice (kill criteria)

- **Slice 0**: `.loom/local/handoffs/mills-autonomy-killtest-2026-05-24.md`
  exists with the 3 numbers + ordering recommendation.
- **Slice 1**: A live GitLab issue with `mills-eligible` label produces
  a backlog item within 10 min. A council run shows WorkspaceSignals
  in the brief. Ranker decisions visible in audit log on 10 consecutive
  dispatches.
- **Slice 2**: One real, non-canary pipeline run merges via
  `MergeWhenPipelineSucceeds=true` on a repo with `auto_merge: true`.
  A deliberately-bad change is blocked at `pending_self_review_human`.
  3 transient transport failures consume 0 attempts.
- **Slice 3**: Webhook fires on an autonomous merge within 30s.
  Reconciler picks up next item within 5s after merge.
  An all-transient escalation auto-retries once and succeeds.

## Effort estimate (rough)

| Slice | Estimate | Notes |
|-------|----------|-------|
| 0 (kill-test telemetry) | 1 hour | Pure read, no code change |
| 1 (demand) | 4-5 days | 1c (LLM ranker) is half the slice |
| 2 (supply) | 3-4 days | 2a is the bulk (webhook + watcher); 2b is the controversial piece |
| 3 (loop closure) | 2-3 days | Each sub-slice is small; 3c bundled with 1c |
| **Total** | **~10-12 days** | Sequential by default; 3a/3b can run in parallel with 2c |

## Open questions

1. **GitLab webhook secret + endpoint location**: where does the operator
   accept inbound webhooks? Today it's a polling-only design. Slice 2a
   needs a new HTTP handler — gated by which token? (Decide before 2a.)
2. **`pr_self_review` rubric correctness**: how do we know the rubric is
   trustworthy enough to merge-block? Need a baseline measurement: run
   it advisory on the last 50 merged MRs and check false-positive rate
   before flipping. (Decide before 2b.)
3. **Notification destination**: which webhook? Slack via incoming
   webhook URL, or the workspace `agent_context` handoff inbox? Or both?
4. **Per-repo `auto_merge: true` rollout**: which repo first? Suggest
   `loom-core` itself on a sandbox label, then promote.
5. **Ranker model**: gemma3:4b vs qwen3:8b vs flexinfer default? Cost +
   latency comparison needed.

## Sources

- [S1] Demand-side exploration (2026-05-24):
  - Backlog sources: `pkg/mills/council/backlog_mutator.go:104-117`,
    `cmd/loom-mills-operator/handlers_backlog.go:44-48`
  - Dispatch order: `pkg/mills/store/dao_backlog.go:111-115`
  - Tick: `pkg/mills/reconciler.go:175-200`
- [S2] Supply-side exploration (2026-05-24):
  - Spawn cluster: `pkg/mills/pipeline/STATES.md:85-142`,
    commits `f55f8cfe`, `6cb0dcd1`, `a08c8f7d`, `f2dec2cf`, `a3070890`,
    `a433ce06` (22% of recent commits)
  - Gates advisory: `pkg/mills/gates/llm_judge.go:164-222`,
    `pkg/mills/pipeline/runner.go:700`
  - Auto-merge not wired: `pkg/mills/clients/gitlab.go:mergeBody`,
    `pkg/mills/pipeline/dispatcher.go:545`
  - Retry blind: `pkg/mills/pipeline/runner.go:370-375`
- [S3] Loop-closure exploration (2026-05-24):
  - OnMerged chain: `cmd/loom-mills-operator/main.go:220,235`,
    `pkg/mills/eval/outcome_attributor.go:53-88`
  - Budget: `pkg/mills/budget.go:83-159`
  - Schedulers: `pkg/mills/council_scheduler.go:71-99`,
    `pkg/mills/scheduler.go:99-150`
  - Regression label: `pkg/mills/kpi_writer.go:26,190-195`,
    `pkg/mills/gates/regression.go:73-110`
- [S4] Prior plan: `.loom/42-plan-mills-next-round-fixes-2026-05-18.md`
- [S5] Operator pain points + primary metric: 2026-05-24 planning chat.
- [S6] Recent commit log: `git log --since=2026-05-01 --oneline --
  pkg/mills/ cmd/loom-mills-operator/`
- [S7] mills-ops skill: `mcp/skills/mills-ops/SKILL.md`
