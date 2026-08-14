# Mills Pipeline Escalation Diagnosis — PIPE-MILLS-CANARY-M1D-VERIFY-2-1778969004

**Date:** 2026-05-16
**Investigator:** loom-core agent
**Status:** Diagnosis complete — no code changes made

## TL;DR

The run did **not** escalate because `pr_self_review` failed. It escalated because
the very next stage, `post_review_gate`, ran the `spec_conformance` LLM-judged
gate, the FlexInfer judge returned a free-text question instead of a JSON score
envelope, and the parser surfaced that as a hard error. The runner's gate path
treats parser/judge errors as infrastructure failures (not "gate-failed → retry"),
so it routed directly to escalation.

The deeper trigger: the `implement` spawn returned `FilesChanged=[]`,
`LinesAdded=0`, `LinesRemoved=0`, no `DiffPatch`, and no `CommitMessages` —
which makes the rubric prompt vacuous, which the judge can't grade.

## Root cause (top-1)

Smoking-gun event row from `state.db` (id=13803):

```
2026-05-16T22:13:16.636017848Z pipeline pipeline.run.escalated
reason="gate post_review_gate: gate \"spec_conformance\": spec_conformance: judge: rubric judge: parse: no parseable score envelope in response; raw=\"Please provide the **unified diff** and the **list of files changed/slice scope**. \n\nI cannot perform the review without the implementation (the diff) to compare against the provided spec.\""
```

The pipeline finished `pr_self_review` at 22:13:15 then immediately entered
`post_review_gate` (an `auto_gate` with `Gates: [spec_conformance, pr_self_review]`).
`spec_conformance` is an LLM judge wired to gemma4-26b-a4b-gptq via FlexInfer.
The judge response was prose, not a JSON envelope. `parseRubricEnvelope` returned
`no parseable score envelope in response`
([pkg/mills/clients/flexinfer.go:258](../pkg/mills/clients/flexinfer.go#L258)).
`LLMGate.Evaluate` wrapped it as `"spec_conformance: judge: %w"`
([pkg/mills/gates/llm_judge.go:79](../pkg/mills/gates/llm_judge.go#L79)).
`Registry.EvaluateAll` returned `(out, false, fmt.Errorf("gate %q: %w", n, err))`
([pkg/mills/gates/gates.go:169](../pkg/mills/gates/gates.go#L169)).

## What killed the run — exact call site

[pkg/mills/pipeline/runner.go:276](../pkg/mills/pipeline/runner.go#L276):

```go
if stage.Type == "auto_gate" {
    pass, err := r.runGate(ctx, run, item, stage, prior, policy)
    if err != nil {
        return r.escalateWithItem(ctx, run, item,
            fmt.Sprintf("gate %s: %v", stage.ID, err))
    }
    ...
}
```

This is the "infrastructure error" branch. The "gate-failed-but-no-error"
branch one block below (lines 281-292) would have rewound to `pr_self_review`
and retried — that's the intended recovery path. Because the judge returned
`(false, err)` not `(false, nil)`, retry never happened.

## Why the judge had no diff to score

`mapTelemetryToResponse` in
[pkg/mills/clients/spawn.go:304-336](../pkg/mills/clients/spawn.go#L304) only
populates `FilesChanged`, `LinesAdded`, `LinesRemoved` from HUD telemetry. It
**never sets** `DiffPatch` or `CommitMessages`. So `runGate → gateInputFor`
builds a `StageInput` with empty diff fields. `composePrompt` in
flexinfer.go:226 then writes no `=== Diff ===` section into the rubric prompt.
Gemma4 sees the rubric without any code, says "give me the diff," and the
parser fails.

Implement stage's persisted artifacts confirm this for the canary run:

```json
{"agent_id":"spawn-codex-51da268f37e4","stage_id":"implement","status":"completed","turn_count":1}
```

No `files_changed` array, no diff bytes. (The deterministic `diff_size` and
`scope` gates passed only because both short-circuit to PASS when
`FilesChanged` is empty — see scope.go:43-44 and diff_size.go:34.)

Whether the codex spawn actually produced a commit in the worktree is a
separate question — the HUD telemetry side of the pipeline is blind to it.

## Why operator stdout was nearly silent

`r.event(...)` writes to the events table, not stdout. Only `r.logger().Warn`
calls appear in stdout. During the run only one stdout line was emitted (the
post-escalation handoff decode error) because no warn-level path fired. All
the per-stage progress is in `events` table; nothing routes those rows into
slog.

## Proposed fix slices (DO NOT IMPLEMENT YET)

Ranked by expected leverage.

### A. Treat LLM-judge parser errors as "skip", not "escalate" (smallest, safest)

In [pkg/mills/gates/llm_judge.go](../pkg/mills/gates/llm_judge.go) around line
77-80, distinguish *judge call failed* (network, auth) from *judge returned
unparseable content*. The latter should return `Outcome{Pass: true, JudgedBy:
"flexinfer:unparseable"}` with reasons, **not** an error. Mirror the existing
`Disabled` short-circuit.

- **Files:** `pkg/mills/gates/llm_judge.go` (+15 LOC), one new test in
  `llm_judge_test.go` (+25 LOC).
- **Risk:** Low. Hides judge weakness behind a pass. Could mask real spec
  violations. Mitigate with a Prometheus counter
  `mills_llm_gate_unparseable_total{gate=}`.
- **Reversibility:** Trivial.

### B. Populate `DiffPatch` and `CommitMessages` from HUD telemetry

Root-cause fix for the empty-rubric problem. The HUD telemetry already tracks
file changes by path; either (a) extend the telemetry payload to include
unified diff bytes, or (b) have the spawn worker `git diff` against the
base branch in the worktree after the spawn completes.

- **Files:** `pkg/mills/clients/spawn.go` (mapTelemetryToResponse), HUD's
  `internal/hud/api_mobile.go` spawn-state response shape; or a new helper
  in `pkg/mills/clients/` that shells out to `git -C $worktree diff`.
  Estimate +80 LOC across 2-3 files, plus test updates.
- **Risk:** Medium. Mobile API contract change if option (a). Worktree path
  must be reachable from the operator pod for option (b) — it's not today
  (operator runs in `loom-mills` ns, worktrees live on the HUD pod). So (b)
  needs an additional HUD-side endpoint.
- **Reversibility:** Trivial via env flag.

### C. Convert the empty-diff special case to a hard fail upstream

In `runGate` or `gateInputFor`, if every diff field on `prior["implement"]`
is empty, return a gate failure with reason `"implement stage produced no
diff"`. This stops gate-eval before it reaches the LLM judge and triggers the
retry-from-implement path naturally.

- **Files:** `pkg/mills/pipeline/runner.go` (+12 LOC), test (+30 LOC).
- **Risk:** Low. Changes behavior only for empty-diff runs, which are
  currently always-escalated.
- **Reversibility:** Trivial.

### D. Strengthen the rubric prompt or switch judge model

Long-term, gemma4 returning a meta-question instead of complying with rubric
output format is brittle. Either (i) make the rubric prompt include strict
"output JSON only" instructions and a few-shot example, or (ii) move LLM-
judged gates to a stronger model (qwen3-30b is already in the pool). This
sits on top of A/B/C.

- **Files:** rubric YAML/template + flexinfer.go composePrompt. +40 LOC.
- **Risk:** Higher latency / cost. Worth measuring.

### Recommended sequence

1. **A** (immediate stop-the-bleeding so the canary stops escalating)
2. **C** (correct retry behavior on empty diffs)
3. **B** (actual fix — judge gets real diffs)
4. **D** (defense in depth)

## Other findings noted while investigating

- **No event-listing API**: there's no `GET /api/mills/events`. The `events`
  table is rich but only readable via the on-pod sqlite file. Worth a
  follow-up: add `GET /api/mills/pipeline/runs/{id}/events`.
- **Operator stdout is sparse**: the `r.event(...)` calls don't double-write
  to slog. During an incident the operator log alone is unhelpful; you have
  to copy state.db out. Worth a follow-up: mirror `pipeline.run.escalated`
  (and `pipeline.stage.error`) into a `logger().Warn` so kubectl logs surfaces it.
- **Handoff JSON parse bug** (the only stdout warn at 22:13:20) is a separate
  issue, already commented in the pre-existing `.loom/118-…` diagnosis. It's
  *not* the cause of escalation — it fires *after* the run is already
  `escalated`.

## Evidence trail

- Operator pod: `loom-mills/loom-mills-operator-5b7fff8db5-76nh8`
- DB copy: `/tmp/mills-state.db` (+ `-wal`, `-shm`) at investigation time
- Issue auto-filed by escalator: https://gitlab.flexinfer.ai/services/loom-core/-/issues/131
- Key event ids: 13787-13803 (pipeline run), 13803 = escalation, 13804 =
  escalator-published.

## Confidence

High that the *escalation trigger* is the spec_conformance parse error
(direct event row, exact reason string, code path traceable line-by-line).

Medium that the *underlying cause* is empty-diff telemetry — the implement
stage's artifacts are consistent with that, but I did not inspect the
worktree on the HUD pod to confirm whether a commit was actually made. If
the codex spawn *did* commit but HUD telemetry failed to report it, that
shifts blame to the telemetry path; either way fix B is the durable answer.
