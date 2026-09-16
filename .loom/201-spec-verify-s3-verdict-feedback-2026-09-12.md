# Spec: S3 — a real tests-stage verdict rewinds to implement with the findings

- **Date**: 2026-09-12
- **Plan**: `.loom/200-plan-executable-verification-2026-09-12.md` (slice S3); lineage `.loom/brainstorm-loom-core-mills-critical-improvements-2026-09-12.md`
- **Lane**: Mills (`bl-verify-s3-verdict-feedback-20260912`), filed after S1 (!1920) is in the running operator build
- **Priority**: P1

## Defect

`DevboxWorker.Run` (`pkg/mills/pipeline/dispatcher.go`, the `tests` stage) returns `fmt.Errorf("devbox quality gate failed (%s)", failedSummary)` for every failed quality gate. Its comment says "so the runner can retry implement", but the runner's non-gate stage error path (`pkg/mills/pipeline/runner.go`, the `Classify(err)` block after the stage attempt) retries the **tests** stage in place, up to `policy.Pipeline.Retry.MaxAttempts`, and nothing changes between attempts — same diff, same sandbox — so a real lint or test finding fails identically until the run escalates as `code`. `post_tests_gate` declares `RetryFrom: "implement"` but `Gates: []`, so the rewind that would hand the implement spawn a `StageRetryContext` never fires for a tests failure.

Kill-test 2026-09-12 over 209 errored tests attempts: 19 were real findings (13 lint, 6 test), and in 9 of 13 retry pairs the next attempt repeated the identical signature (e.g. `TestMigrate_v2_Idempotent` three times in one run; three golangci findings three times in `bl-finishing-s1`). Each such run burned its attempt budget and reached a human as "code", with the fix list already in the store.

## Change

1. **Real verdict → stage output, not error.** In `DevboxWorker.Run`, after the existing no-checks (`ErrDevboxGateNoChecks`), contradictory-verdict, and baseline-oracle (`ErrDevboxBaselineAlsoFails` → `baseline_verdict: environment`) branches, a failure whose failed checks carry a genuine verdict returns `out` with **nil error** and artifacts `passed: false`, `checks: resp.Checks`, `failed_summary: failedSummary` (baseline artifacts as today). Every substrate path keeps returning an error exactly as now: provisioning/`ensure sandbox`, checkout (`ErrDevboxCheckoutInfra`), no-checks, contradictory verdict, baseline-environment, and any failed check whose output classifies as infra/transient through `pipeline.Classify` (exit 137, "could not start the exec process", "parallel golangci-lint is running", …) — those must still travel the in-place retry path with their class.
2. **`tests_verdict` gate** (`pkg/mills/gates/tests_verdict.go`, registered in `gates.Default()` and listed in `DefaultStages` for `post_tests_gate`): fails when the tests output's `passed` artifact is `false`, with one reason per failed check — `"<check name>[exit=<code>]: <first 12 lines of output, 1.5KB cap>"` — and passes otherwise. `gateInputFor` (`runner.go`) gains `TestsVerdict *gates.TestsVerdict{Passed bool, FailedChecks []gates.FailedCheck}` decoded from `prior["tests"].Artifacts` (the artifacts survive a JSON round-trip through `stage_results`, so decode both the live `[]DevboxCheck` and the `[]any` shape), and `in.TestsPassed` reads that decoded value instead of "prior tests output exists" (legacy rows without a `passed` artifact keep `true`).
3. **Findings reach the retry spawn verbatim.** `StageRetryContext` gains `Findings []string` (the gate's reasons, untruncated up to 4KB total) alongside `FirstFailure`/`LastFailure`; the gate-failure rewind in the runner populates it; `implementRetryDiscipline` (`cmd/loom-mills-operator/main.go`) renders them as a "Findings to fix (from the previous attempt's tests stage)" bulleted block after the existing discipline text. `seedRetryContexts` rehydrates `Findings` from the persisted gate outcome so an operator restart mid-retry does not drop them.
4. **Class and budget unchanged.** The rewind uses the existing gate-failure path: `attempts["implement"]` bumps, cap `maxAttempts`, exhaustion escalates as `code` with the failing checks named. No new policy field.
5. **Docs**: `pkg/mills/pipeline/STATES.md` gains a "tests verdict" paragraph (real verdict → `post_tests_gate` → implement rewind; substrate → in-place retry); changelog fragment `changelog.d/verify-s3-verdict-feedback.fixed.md`.

## Acceptance (executable)

- `TestDevboxWorker_RealVerdictReturnsFailedOutput` (dispatcher_test.go): a `change_implicated` lint failure returns `err == nil`, `Artifacts["passed"] == false`, `Artifacts["checks"]` populated; the same fixture with `baseline_verdict: environment`, an `ensure sandbox` error, and an exit-137 check still return errors classed `infra`/`transient`.
- `TestTestsVerdictGate` (gates): passed → pass; two failed checks → fail with two reasons containing the check names and output tails; missing artifact → pass.
- `TestRunner_TestsVerdictRewindsToImplementWithFindings` (runner_test.go, fake dispatcher): implement → tests(passed=false) → the next dispatched stage is `implement` with `JobContext.RetryContext.GateStage == "post_tests_gate"` and `Findings` equal to the gate reasons; after `maxAttempts` the run escalates `code` and the reason names the failing check.
- `TestImplementRetryDisciplineRendersFindings` (operator): the prompt contains each finding verbatim.
- `gateInputFor` test: `TestsPassed` false when the artifact says so; true for a legacy output without the artifact.
- `go test ./pkg/mills/gates ./pkg/mills/pipeline ./cmd/loom-mills-operator` green; `golangci-lint run` on those packages 0 issues.

## Out of scope

Executable acceptance per item (S5), judges to advisory (S4), sandbox provisioning (S1/S2 shipped as !1920/!1921).
