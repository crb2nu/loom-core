# Plan: Mills telemetry-driven optimization + Factory telemetry panel

Date: 2026-07-16 · Author: claude-code (Fable 5) · Branch: `claude/mills-optimization-telemetry-9d9292`

## Riskiest assumption + kill-test

**Load-bearing assumption**: The dominant escalation drivers in the last 7 days of
live Mills runs are *harness defects*, not code-quality problems — specifically
(1) rubric-judge JSON envelopes truncated at the 256-token default
(`pkg/mills/clients/flexinfer.go:282`), (2) `ci_watch` 30m poll deadline firing
while the GitLab pipeline is still running, (3) GitLab `mr` 409 / `merge` 422
handled as terminal, and (4) flexinfer pinned-model 503 storms with no fallback
(`flexinfer.go:49` — pinned model gets NO automatic fallbacks) — and each is
fixable inside `pkg/mills` with unit tests against captured production bytes.

**Kill test**: Unit tests in slice S1 must parse the 7 REAL captured judge
responses (saved from live `gate_outcomes.reasons` on 2026-07-16, committed at
`pkg/mills/clients/testdata/judge_unparseable_live_2026-07-16.json`) into valid
score envelopes or a single bounded retry — runnable in <5 min via
`GOWORK=off go test ./pkg/mills/clients/ -run TestRubric`. If the real bytes
can't be recovered by parser hardening (e.g. they carry no score signal at all),
the S1 approach is wrong and the fix must move to the model/prompt layer instead.

**Failure mode if wrong**: We ship parser/timeout hardening that doesn't move the
61% escalation rate because escalations are actually caused by genuinely bad
diffs; the telemetry panel (S5/S6) still lands value, but S1–S4 were misdirected.

**Status**: not run (S1 executes the kill test as its first deliverable).

## Evidence (live cluster, 2026-07-16, window = last 7d, 36 terminal runs)

Source: `GET /api/mills/pipeline/runs?state=terminal` + per-run
`/api/mills/pipeline/runs/{id}` on `loom-mills-operator` (k3s, port-forward).

- **Escalation rate 61%** (22 escalated / 14 done). Classes: code 7, config 5,
  transient 3, infra 2, unclassified 5.
- **Retry burn: $11.15 of $33.41 total (33%) and ~489 min** spent on stage
  attempts > 1.
- **Cost concentration**: implement $17.59 + pr_self_review $12.12 = 89% of
  spend. pr_self_review p50 724s ≈ implement p50 731s — the self-review stage
  costs nearly as much as implementation.
- **Gate failures**: pr_self_review 8/32 fail, spec_conformance 4/32, scope 6/34.
  ALL 12 judge-gate failures inspected are "judge response could not be parsed
  into a score envelope" — raw bytes show either a `Thinking Process:` preamble
  eating the 256-token cap before JSON, or valid JSON (`{"score": 0.8, ...`)
  truncated mid-array. These are false-fails.
- **Stage error taxonomy** (LogTail classification):
  - research: 24 errors, all `flexinfer chat: status 503 … model parked behind
    higher-priority primary` — one run retried 8× against a parked model.
  - implement/plan_slice/pr_self_review: spawn infra (`hud spawn: POST max
    retries exceeded`, `image build failed: buildah`, `pod creation failed`,
    `agent turn driver lost across restart`).
  - ci_watch: 7 errors — `pipeline poll timed out after 30m0s` while the GitLab
    pipeline was still running. Largest escalation sink (5 runs died here).
  - mr: 409 `Another open merge request already exists` (not adopted).
  - merge: 422 `Branch cannot be merged` (never rebased).
  - cleanup: branch DELETE 400 `reference update failed` marks stage error.
  - tests: `devbox quality gate failed (0/0 checks marked failed; gate reported
    not passed)` — empty check set reported as failure.
- **Escalation funnel (last stage before escalate)**: ci_watch:error 5,
  research:error 3, merge:error 3, implement:success 3, pr_self_review:success 3
  (success-then-escalate = gate false-fails).

## Code-map facts (structure for the slices)

- Stage/gate telemetry IS recorded: `stage_results` (stage, attempt, started/
  ended, outcome, cost_usd, spawn_id, log_tail) and `gate_outcomes` (gate_name,
  outcome, reasons_json, judged_by) — `pkg/mills/store/migrations/001_initial.sql:80-108`,
  structs `types.go:332-365`.
- Rubric judge: default `MaxTokens: 256` (`pkg/mills/clients/flexinfer.go:282`),
  parse at `flexinfer.go:463-482` (`parseRubricEnvelope`) — handles complete JSON
  anywhere in prose, NOT truncated JSON. Unparseable soft-fail path exists:
  `JudgedBy:"flexinfer:unparseable"` (`pkg/mills/gates/llm_judge.go:218-226`).
- Pinned judge/weaver model (`FLEXINFER_JUDGE_MODEL=qwen35-35b-clean-gptq-workhorse`
  in the deployment) disables fallbacks (`flexinfer.go:45-51`).
- KPI writer computes ~26 metrics per 1d/7d/30d window (`pkg/mills/kpi_writer.go:196-305`)
  but HUD fetches only `window=1d` (`internal/hud/frontend/src/lib/stores/mills.svelte.ts:641`).
- No stage roll-up endpoint exists; the run drawer shows per-stage duration/cost
  for ONE run only (`PipelineRunDetail.svelte:317-318`). Escalation class fields
  exist on `PipelineRun` but are dropped by the frontend type
  (`mills.svelte.ts:73-99`).
- Prometheus already exports `mills_pipeline_stage_duration_seconds`,
  `mills_pipeline_stage_attempts_total`, `mills_gate_evaluations_total`
  (`pkg/mills/metrics.go`) — cluster-side aggregation exists; HUD-side does not.

## Slices (6, parallel, Opus implementers, isolated worktrees)

File-ownership is strict to keep integration conflict-free. **No slice touches
CHANGELOG.md or docs/** — consolidated at integration.

### S1 — judge-envelope-robustness
Own: `pkg/mills/clients/flexinfer.go` (judge/parse sections only), new testdata.
- Raise RubricJudge default MaxTokens 256→1024; env override `FLEXINFER_JUDGE_MAX_TOKENS`.
- Truncation-tolerant parse: brace-balance repair of the final JSON candidate;
  fallback regex extraction of `"score": <float>` (accept [0,1] only) with
  best-effort reasons; keep existing complete-JSON path first.
- If the chat response signals length-stop (finish_reason=="length" where
  available), retry ONCE with 2× tokens and a "Respond with ONLY the JSON
  object" suffix.
- Kill test: all 7 live fixtures parse or recover (see above).

### S2 — model-unavailable-degrade
Own: `pkg/mills/clients/flexinfer.go` (chat/fallback sections), research-stage
call sites in `pkg/mills/pipeline/`.
- On 503/service_unavailable (incl. "parked"), iterate fallback chain even for
  pinned models; env `FLEXINFER_JUDGE_MODEL_FALLBACKS` / `FLEXINFER_WEAVER_MODEL_FALLBACKS`
  (comma-separated) if not already wired end-to-end.
- Per-model short-cooldown circuit breaker so a parked model isn't retried 8×.
- Research stage is advisory: if every model is unavailable, soft-skip (outcome
  success + explicit "research skipped: model unavailable" note) instead of
  stage error; ensure failure classification maps model-unavailable → transient.

### S3 — ci-watch-resilience
Own: ci_watch stage in `pkg/mills/pipeline/` + gitlab poll helper.
- Poll deadline while pipeline still pending/running ⇒ NOT an error: continue
  watching up to a hard cap (env/policy `MILLS_CI_WATCH_MAX_MINUTES`, default 90;
  current base 30m stays the per-poll-session deadline).
- At hard cap with pipeline still running ⇒ classify as external dependency
  (reuse the external-incident classification from `pkg/mills/pipeline/escalate.go`
  / commit 044af064), retryable=true — not a generic error.

### S4 — stage-and-gate-hardening
Own: mr/merge/cleanup stage handlers + `pkg/mills/gates/scope*.go` + tests-stage
devbox gate handling.
- mr 409 "already exists" → look up open MR by source branch, adopt its IID,
  stage succeeds.
- merge 422 "Branch cannot be merged" → call GitLab rebase API, bounded wait,
  retry merge once; only then fall to existing config-terminal classification.
- cleanup branch-delete 400/404 → log-and-succeed (cleanup must not fail a run).
- scope gate with no slices → outcome `skip` + reason (advisory), not fail.
- tests stage: devbox quality gate reporting not-passed with 0 executed checks →
  infrastructure error (retryable), not a test failure.

### S5 — telemetry-aggregation-endpoint
Own: `pkg/mills/store/dao_telemetry.go` (new), `cmd/loom-mills-operator/handlers_telemetry.go`
(new), route registration in `server.go`, HUD proxy route in
`internal/hud/domain/mills/mills.go` (one GET line), `pkg/mills/kpi_writer.go`
(two new metrics: `retry_cost_usd`, `gate_unparseable_rate`).
- `GET /api/mills/telemetry/stages?window=1d|7d|30d` → JSON contract (S6 builds
  against this exact shape):

```json
{
  "window_seconds": 604800,
  "generated_at": "2026-07-16T00:00:00Z",
  "runs": {"total": 36, "done": 14, "escalated": 22, "retry_burn_cost_usd": 11.15, "retry_burn_seconds": 29340},
  "stages": [{"stage": "implement", "attempts": 57, "errors": 23, "error_rate": 0.40,
              "p50_seconds": 731, "p90_seconds": 1385, "max_seconds": 2632,
              "total_seconds": 40548, "cost_usd": 17.59,
              "retry_attempts": 12, "retry_cost_usd": 8.40}],
  "gates": [{"gate": "pr_self_review", "evaluations": 32, "passes": 24,
             "fails": 8, "skips": 0, "unparseable": 6}],
  "escalation_funnel": [{"last_stage": "ci_watch", "outcome": "error", "count": 5}],
  "failure_classes": [{"stage": "research", "class": "model_unavailable", "count": 24}]
}
```
- `unparseable` = gate_outcomes with `judged_by == "flexinfer:unparseable"`.
- `failure_classes` from needle-classification of `stage_results.log_tail`
  (reuse/extend the classifier in `pkg/mills/pipeline/escalate.go`).
- Aggregation in SQL where possible; percentiles computed in Go over the
  window's stage rows (bounded: window max 30d).

### S6 — hud-telemetry-panel
Own: `internal/hud/frontend/src/lib/components/mills/TelemetryPanel.svelte` (new),
`panelRegistry.ts` (one entry), `stores/mills.svelte.ts` (fetch + state additions),
`PipelineRunDetail.svelte` (escalation chips), vitest fixtures/tests.
- New "Mill Telemetry" panel (factory family): window selector (1d/7d/30d);
  stage-duration waterfall (p50/p90 bars); stage error-rate bars; gate health
  stacked pass/fail/unparseable; escalation funnel; retry-burn stat tiles;
  failure-class Pareto list. Data: the S5 contract above + `kpis?window=`.
- Build against the committed fixture of the S5 contract (do NOT block on S5).
- PipelineRunDetail: chips for `EscalationClass`, `FailureClass`,
  `EscalationRetryable`, `ExternalDependency` (fields already on the wire).
- Conventions: `lang="ts"`, `createPoller` (visibility-pause, no initial tick),
  panelRegistry entry, `:global()` rules for child-DOM CSS, no `$effect`
  read+write of the same `$state` without `untrack`.

## Integration & ship

1. Merge order: S1 → S2 (shared file: flexinfer.go, disjoint functions) → S3 →
   S4 → S5 → S6.
2. Consolidated CHANGELOG entry + docs update (`docs/` mills operator page:
   new endpoint + panel) — satisfies the docs-guardrail gate.
3. Quality gate: `GOWORK=off CGO_ENABLED=0 go build ./... && go test ./pkg/mills/... ./cmd/loom-mills-operator/... ./internal/hud/...`
   + frontend `pnpm build && pnpm test`.
4. Single MR from this branch → auto-merge per Auto-Ship policy.
5. Post-merge (follow-up, not this MR): watch next 24h KPI snapshot for
   escalation-rate delta; the panel itself makes this visible.
