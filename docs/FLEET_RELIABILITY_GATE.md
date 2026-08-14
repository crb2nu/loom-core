# Fleet Reliability Gate

The fleet reliability gate is the merge-blocking proof for daemon transport,
Mills, spawn, contract, integration, load, and performance behavior.

Run it locally from a branch that is ahead of `main`:

```bash
GOWORK=off make ci-reliability
```

CI runs the same target with an inner 810-second deadline. Evidence is written
under `.loom/local/evidence/fleet-reliability/<run-id>/` and uploaded from the
`test:reliability` job even when the job fails.

## Evidence contract

The version 4 suite requires exactly 49 manifested tests and four manifested
benchmarks:

- 9 daemon generation, restart, routing, and shutdown tests;
- 30 race-enabled transport, generation, Mills, workflow, council, and
  custom-server tests;
- 3 spawn recovery tests;
- 4 contract and live OpenAPI tests;
- 2 black-box fake-hub reset and generation-soak tests;
- 1 sixty-second mixed-load test;
- 4 paired performance benchmarks.

The runner derives package lists, exact test expressions, and expected counts
from an immutable copy of the suite manifest captured at the start of the run.
There is no second hard-coded scenario list to drift from the manifest, and a
working-tree edit made after capture cannot change the active proof set.

`final-status.json` records the build and baseline SHAs, stage, exit status,
manifest/config/schema digests, and whether complete metadata was produced.
Test-group reports must contain every required scenario; a missing or skipped
scenario, count mismatch, or manifest mutation fails the gate.

## Baseline selection

The gate never permits a candidate to benchmark against itself.

Baseline precedence is:

1. `LOOM_RELIABILITY_BASE_REF`, when explicitly set;
2. on the default branch, the valid `CI_COMMIT_BEFORE_SHA` from the push;
3. on the default branch when the CI value is unset or all zeroes, `HEAD^1`;
4. on a feature branch, the merge base of `HEAD` and `origin/<default>`.

The selected commit must exist, differ from `HEAD`, and be an ancestor of the
candidate. Missing, divergent, root-commit, and self-resolving baselines fail
closed with an actionable error. This keeps default-branch retries pinned to
the original pre-push state even if `origin/main` advances later.

## Paired benchmark method

The gate compiles four test binaries for the baseline and four for the
candidate once. It then executes 11 rounds for each benchmark with:

- base and candidate samples adjacent for the same package;
- first/second order reversed on alternating rounds;
- `GOMAXPROCS=2`, `-benchmem`, and a one-second benchmark duration;
- exactly 88 binary executions and 11 samples per side.

The comparison uses the median of same-round candidate/baseline ratios. This
discounts isolated noisy rounds while preserving a matched comparison against
short-term shared-runner drift. A time regression blocks only when the median
paired effect exceeds the threshold and an exact one-sided sign test finds the
candidate consistently slower at `p <= 0.05` (9 of 11 when none tie; exact
ties are excluded). The report records the slower/comparable pair counts,
p-value, and significance decision so an inconclusive noisy result remains
auditable.

The merge thresholds remain:

- time: at most 10 percent regression;
- bytes per operation: at most 15 percent regression;
- allocations per operation: at most 15 percent regression.

Do not weaken the threshold or add blanket retries for a noisy result. Inspect
`benchmark-base.txt`, `benchmark-candidate.txt`, and `benchmark-report.json`.
If the SHAs are wrong, fix baseline selection. If matched samples are separated
in time or order-biased, fix orchestration while retaining the evidence count.

## Platform boundary

The pinned fi-accel module does not publish its native header and archive, so a
full CGO race build of every daemon-linked package is not hermetic. The gate
derives precise import-graph exclusions, races the dependency-light generation
kernel and other affected concurrency surfaces directly, and keeps daemon
wiring covered by pure-Go and black-box integration tests.
