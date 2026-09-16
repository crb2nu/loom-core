# Plan: executable verification, one loop (C1)

- **Date**: 2026-09-12
- **Lineage**: chosen via `.loom/brainstorm-loom-core-mills-critical-improvements-2026-09-12.md` (recommendation C1 = F1 sandbox honesty + F4 judges advisory + F3 verdict feedback); kill-test PASSED 2026-09-12 (88.5% of 209 errored tests-stage attempts were substrate faults; 14 runs escalated as `code`/`config` on a substrate final attempt; 69% of retries after a real failure repeated the same signature).
- **Goal**: "escalated" means "the code is wrong". Measured by three numbers already exported: `mills_pipeline_stage_error_class_total{stage="tests"}` (down), the share of escalations classed `code` whose last tests attempt carried no lint/test verdict (to zero), and `retry_cost_usd` (down).
- **Lane discipline**: S1 and S2 are operator-lane (they change the machine that would verify them); S3–S5 route through the Mills backlog as `bl-verify-s{3,4,5}-*` with explicit `Slices` and `Dependencies`, filed only after S1 is deployed (operator build SHA contains S1's merge commit — the reconciler's `dependency_undeployed` hold enforces this for S3 if its `Dependencies` name S1's item id).

## Status (2026-09-12 23:50Z)

| Slice | State | Where |
|---|---|---|
| S1 lint parity under concurrent runs + no substrate verdict is `code` | **merged** 16:28Z, merge `3ae3180f`; in the operator build since `d4b82b41` | !1920 (operator image; the devbox half rides the custom-server image) |
| S2a devbox client rate limits, pod-gone wait, 6Gi sandbox | **merged** 16:33Z, merge `07940969`; **regression fixed** (see below) | !1921 (custom-server image + `k8s/base/servers/devbox/deployment.yaml`); gitops !679 |
| S2b sandbox drill (two concurrent gates must both mint a verdict) | not started | operator lane, after S2a rolls |
| S3 tests verdict → implement rewind with findings | **filed** 19:2xZ as `bl-verify-s3-verdict-feedback-20260912` (P1); the factory run reached the `merge` stage the same evening | MR !1931 (CI running at time of writing); spec `.loom/201-spec-verify-s3-verdict-feedback-2026-09-12.md` |
| S4 judges advisory | not started — **premise needs one more measurement** (see below) | Mills lane, after S3 |
| S5 executable acceptance per item | not started | Mills lane, after S4 |

Correction from S2a's investigation: the "wait to replace non-running pod" family was **not** volume release. The devbox backend polled the API every 200ms under client-go's default 5 QPS limiter shared by every concurrent sandbox, and 44 of 46 failures died inside the limiter ("client rate limiter Wait returned an error") before observing the pod once. The memory limit, not test parallelism, was the second live cause (exit 137 on `pkg/mills`); S2a raises the sandbox default to 6Gi.

S2a regression (20:4xZ): the 6Gi default was refused by the `devbox` namespace LimitRange (`k3s/devbox/resourcequota.yaml`, max 4Gi per container), so every Mills sandbox creation failed with `forbidden: maximum memory usage per Container is 4Gi, but limit is 6Gi` and the tests stage escalated `infra` on every run for about an hour. Fixed live (LimitRange max raised to 8Gi) and durably in gitops !679. Lesson for S2b and any later sandbox-resource change: check the namespace LimitRange/ResourceQuota before changing a pod default.

S4 evidence (23:2xZ): on `bl-core-mcp-start-degraded-when-unconfigured-20260912` the rubric judge (gpt-5.6-luna, corroborated by claude-sonnet-5) failed `pr_self_review` with five specific and correct reasons (a tool bypassing the NotConfigured wrapper, config captured at startup instead of per call, tests that never drove initialize → tools/list → tools/call, a swallowed validation error, a warning naming both variables when one was missing). The judge-calibration metric only scores merged work, so it cannot see a pre-merge catch like this. S4 stays queued behind a measurement: count judge-caught defects on escalated runs (gate reasons that a hand-finish confirms) for a fortnight before deciding whether verdicts become advisory. The item itself was hand-finished from the judge's list as loom-core !1932, which also adds the shared in-process driver `internal/mcptest` so every server package can prove the handshake contract.

## What the kill-test changed in the slicing

The brainstorm expected the sandbox itself to be the work. The time series says most of that work already landed (09-04 `fingerprint git-clone sandboxes from remote manifests`, `populate buildah registry cache`; 09-05 `key sandbox image tags by recipe`; 09-06 init-image and startup-timeout fixes): the `still building` / `buildah timeout` / lint v1-v2 families are gone after 09-05. The live defects are smaller and sharper:

| Live family (09-06 → 09-12) | Attempts | Mechanism | Slice |
|---|---|---|---|
| `lint:parity` exit 3 "parallel golangci-lint is running" | 21 | up to 4 concurrent runs execute the gate in the same per-repo sandbox pod; golangci-lint takes a file lock; the operator classes the failed check as `code` | **S1** |
| `wait to replace non-running pod: wait pod gone` | 10 | `waitForPodGone` polled Get every 200ms under client-go's default 5 QPS limiter shared by every concurrent sandbox; the limiter's queue outlived the 30s deadline (`internal/devbox/backend/k8s_wait.go`, `k8s_runtime.go:64-75`) | **S2a** (!1921) |
| exit 137 / "could not start the exec process (container memory limit 4Gi)" on `./pkg/mills ./pkg/mills/store` | ~12 | `pkg/mills` + `store` test binaries exceed the 4Gi sandbox limit | **S2a** (!1921, 6Gi) |
| checkout / network / TLS | 15 | already `infra`; keep | — |
| lint findings repeated across attempts | 9 of 13 retry pairs | the runner retries the `tests` stage **in place** (`post_tests_gate` has `Gates: []`, so its `RetryFrom: implement` never fires); nothing changes between attempts and the retry spawn never exists | **S3** |

## Slices

### S1 — lint parity survives concurrent runs, and no substrate verdict is ever `code` (operator lane, this session)

- **Change 1**: `gates.LintParityCommand` appends `--allow-parallel-runners` after `--config .golangci.yml` (golangci-lint ≥ v1.x and v2.8 support it; the flag disables the start-up file lock, which is the only thing colliding — the Go build cache underneath is concurrency-safe). Position matters: both `cmd/mcp-devbox/quality_gate.go:225` and `pkg/mills/pipeline/dispatcher.go:2106` detect the parity check by the substring `golangci-lint run --config .golangci.yml `, so the flag must follow the config path.
- **Change 2**: `gates.ClassifyLintParity` treats exit 3 with "parallel golangci-lint is running" as degraded (no verdict produced) with a dedicated warning, the same fail-open contract as exit 127 / exit 7 + "0 issues" / exit 137. This protects the skew window where the devbox image is older than the operator (or vice versa).
- **Change 3**: `pkg/mills/pipeline/error_class.go` gains `infra` needles for the sandbox families the kill-test found classed `code`: `wait pod gone`, `wait to replace non-running pod`, `could not start the exec process`, `failed to exec in container`, `error executing command in container`, `setns process`, `parallel golangci-lint is running`. Free retry, not a human.
- **Tests**: table rows in `lint_parity_test.go` (command shape; degraded classification with warning text; exit 3 with a *real* lint error stays a failure); `error_class_test.go` rows for each needle with the expected class and a counter-row that keeps a genuine test failure `code`.
- **Verification**: `go test ./pkg/mills/gates ./pkg/mills/pipeline`, `golangci-lint run` on both packages; after deploy, `kill_test` re-run over the next 48h must show zero `lint_fail_no_findings` attempts and zero `code`-classed substrate finals.
- **Deploy note**: Change 1 and 3 ship in the operator image; Change 2 also matters in the `custom-server` (devbox) image. Both roll from the same main merge; verify each ImagePolicy separately (memory: the devbox server runs the custom-server image, not loom-core).

### S2 — sandbox pod replacement and memory honesty (operator lane)

- Replace-wait: when the per-repo sandbox pod is non-running, delete-and-recreate under a budget that covers Longhorn RWO detach (the 09-06 startup-timeout fix made the *spawn* wait configurable; the *devbox sandbox* wait in `k8s_wait.go` is the remaining fixed budget) and surface the wait as `infra` with the pod phase in the message.
- Memory: run the tests check with `-p 1` (or a `GOMAXPROCS` cap) when the touched packages include `pkg/mills`, or raise the sandbox memory limit for the loom-core recipe; either way exit 137 classes `infra`, never `code`.
- Drill: a daily canary that runs two concurrent quality gates against the loom-core sandbox and asserts both mint a verdict.

### S3 — a real tests verdict rewinds to implement with the findings (Mills lane, `bl-verify-s3-verdict-feedback-20260912`)

- Full spec: `.loom/201-spec-verify-s3-verdict-feedback-2026-09-12.md`. Mechanism: `DevboxWorker.Run` returns a real failing verdict as stage output (`passed: false` + `checks`) instead of an error; a new deterministic `tests_verdict` gate on `post_tests_gate` fails with one reason per failed check; the existing gate-failure rewind re-dispatches `implement` with `StageRetryContext.Findings` rendered verbatim by `implementRetryDiscipline`. Substrate errors keep the in-place retry path and their class.
- Acceptance: on a synthetic run whose attempt 1 fails lint with three findings, the next dispatched stage is `implement` and its prompt contains the three file:line findings verbatim; after 14d the repeat-signature rate drops below 25%.

### S4 — judges to advisory (Mills lane, `bl-verify-s4-judges-advisory`)

- Policy flag: `post_review_gate` LLM verdicts become annotations (persisted, surfaced on the bolt card and shift ledger) that do not fail the gate; the deterministic gates keep their teeth. Calibration keeps recording so the decision can be reversed on evidence (`mills_judge_calibration_discrimination` above 0.3 for a fortnight would reopen it).

### S5 — executable acceptance per item (Mills lane, `bl-verify-s5-executable-acceptance`)

- `BacklogItem.Success` carries an `acceptance` command (or test name) that the tests stage runs after the touched-package tests; the `fabricated_slice` gate's sibling refuses enqueue of council-minted items without one; the council editor prompt is taught to author it. Acceptance: a run whose acceptance fails cannot reach `mr`; council items without acceptance are rejected at enqueue with a reason the demand log records.

## Riskiest assumption + kill-test

**Assumption**: `--allow-parallel-runners` removes the collision without corrupting shared cache state in the sandbox. **Kill-test** (S1, ≤10 min): in one loom-core sandbox pod run two `golangci-lint run --config .golangci.yml --allow-parallel-runners ./pkg/mills/gates` and `… ./pkg/mills/pipeline` concurrently; both must exit 0/1 with findings text, never exit 3. Run it against the deployed devbox after S1 rolls, and once more with three runners.

## Handoff

- Backlog IDs become the source of truth once S3–S5 are filed; this doc mirrors intent.
- Linked: brainstorm doc above; kill-test artifacts in the session scratchpad (`kill_test_result.json`, `classify2.py`).
