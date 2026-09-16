# Flake quarantine and honest reruns

How `loom-core` keeps a single timing-dependent test from blocking every
deploy, without letting reruns launder real races into green pipelines.

## Why this exists

On 2026-08-02 one test —
`TestStopSpawnLateStartCleanupFailureRetainsRetryablePod` — intermittently
failed and turned `main` red. Red `main` gates the deploy stage, so **every
image build was blocked for roughly ten hours**. That specific race was fixed
in `fix/spawn-stop-late-start-retry-handle`, but the class was not: any single
flaky test in the unit suite could do it again, and nothing distinguished
"flaked once, passed on rerun" from "hard red".

The obvious fix — `retry: 1` on the job — is the wrong one here. A job-level
retry re-runs the whole suite, reports nothing about which test misbehaved, and
would quietly hide races in a codebase whose product includes race-sensitive
spawn supervision. Reruns are only acceptable if every rerun is **recorded**.

## The split: unit keeps green, reliability hunts flakes

| Job | Reruns? | Purpose |
|-----|---------|---------|
| `test:unit` | **Yes** — `gotestsum --rerun-fails=2 --rerun-fails-max-failures=5` | Green-keeping *with honesty*. A flake is absorbed so it cannot block deploys, and is filed as a `flaky-test` issue. |
| `test:reliability` | **No — deliberately** | Flake *hunting*. Exact scenario verification, targeted race/crash tests, same-runner benchmarks, and a 60s mixed load, run to make races reproduce. |
| `test:race` | No | `-race` coverage on the default branch. |
| `test:integration` | No | Real binaries, `-race`, MCP smoke. |

**Never add reruns to `test:reliability`.** That job exists to produce the
signal `test:unit` is allowed to absorb. Its `retry:` block is scoped to runner
contention (`runner_system_failure`, `stuck_or_timeout_failure`,
`script_failure` from runner SIGTERM), not to test outcomes.

## What happens on a flake

1. `test:unit` runs the suite through `gotestsum`. A failing test is rerun up
   to twice, per test, in its own package.
2. If it passes on a rerun the job **stays green** and the test is written to
   `rerun-report.txt` as `pkg.TestName: 2 runs, 1 failures`.
3. The job's `after_script` runs `flakereport report`, which:
   - prints `FLAKY-TEST-DETECTED: <TestName> (1/2 attempts failed, then passed)`
     to the job log;
   - files a GitLab issue titled `flake: <TestName>` labelled `flaky-test`, or
     comments on the existing open one and bumps its recurrence counter.
4. `rerun-report.txt`, `junit-unit.xml`, and coverage ship as job artifacts.
   The JUnit report surfaces the flaked test in the merge-request test widget
   even though the job is green — that visibility is the point.

If a test fails **every** attempt, it is a hard failure: no flake issue is
filed, and the job is red. If more than five distinct tests fail, gotestsum
skips reruns entirely and the job is red — that many failures is a real break,
not a flake.

## Package timeouts are starvation, not flakes

A `test:unit` failure that reads

```
panic: test timed out after 10m0s
running tests:
	TestGhostSparkDisabledWithoutClient (0s)
```

is almost never the named test hanging. Read the goroutine dump under it. On
2026-09-15 four such jobs (pipelines 27071, 27086, 27121, 27123) each timed
out in the packages that open the most SQLite stores per test — `pkg/mills`,
`pkg/mills/store`, `pkg/mills/pipeline`, `cmd/loom-mills-operator` — and
every in-flight goroutine was parked in `modernc.org/libc.Xfsync` under
`_unixSync`, reached from `store.Open` (the `journal_mode=WAL` commit on a
fresh file) or `store.Close` (the WAL checkpoint). Nothing had hung: every
store-touching test was taking 1–50 s instead of milliseconds, because the
runner node's NVMe was 91–95 % busy with an average queue depth of 43–132
from 22–26 concurrent runner pods, and each fsync waited behind that queue.
The packages were 38–64 % through their test lists when the 10-minute budget
ran out. The green pipelines that overlapped the red ones had landed on the
other overflow host.

Two things keep that from redding the job:

- **Test stores skip fsync.** `pkg/mills/store` builds its DSN with
  `synchronous=OFF` inside `go test` binaries (`buildDSNFor`, gated on
  `testing.Testing()`), and `Open` switches a file to WAL only after those
  per-connection pragmas are in force — the driver applies DSN pragmas in
  sorted order, which had put `journal_mode` before `synchronous`, so the WAL
  switch on a brand-new file used to commit under SQLite's default
  `synchronous=FULL` and fsync regardless. Production binaries keep
  `synchronous=NORMAL`. Unit tests never need crash durability — the process,
  not the kernel, is what ends them.
- **A 30-minute per-package timeout.** `scripts/ci/run_unit_tests.sh` passes
  `-timeout ${UNIT_TEST_TIMEOUT:-30m}`. Go's 10m default assumes a quiet
  machine; the starved runs above extrapolate to 17–26 min per package. A
  timeout *panic* is worse than a slow package, because gotestsum treats it as
  a suspected panic and aborts every rerun — which is how the documented
  `TestStopSpawnLateStartCleanupFailureRetainsRetryablePod` flake turned into
  a hard red on those pipelines instead of being absorbed.

How to tell starvation from a real hang next time: the `running tests:` list
names a different test on every failing job; the `PASS … (Ns)` lines above the
panic show seconds where a green run shows milliseconds; the job runs 28–33
minutes instead of 6–12; and the blocked frame is a syscall (`Xfsync`,
`Xwrite`) rather than a channel receive or mutex. Confirm on the node the pod
ran on (`kube_pod_info{namespace="ci-jobs", pod="runner-…"}`) with
`rate(node_disk_io_time_seconds_total[5m])` and
`rate(node_disk_io_time_weighted_seconds_total[5m])`; CPU throttling of the
pod (`container_cpu_cfs_throttled_periods_total`) will be near zero during the
test phase, which is the tell that it is I/O, not CPU.

## Coverage: why `-test.gocoverdir`, not `-coverprofile`

This is the trap, and it is load-bearing.

gotestsum's reruns re-invoke `go test` with the *same flags*. With
`-coverprofile=coverage.out`, the rerun **overwrites** the profile with its
`-run`-filtered subset. Measured on a two-package probe: total coverage went
from 60.0% to **0.0%** on a single flake — which would have tripped the 35%
coverage gate and redded every pipeline that contained a flake, turning the
cure into a worse version of the disease.

So `test:unit` collects Go 1.20+ *binary* coverage into a directory instead:

```
gotestsum ... -- -cover -covermode=count -args -test.gocoverdir=<absolute dir>
```

Each test binary writes uniquely named counter files, so reruns **accumulate**
rather than clobber. `go tool covdata textfmt` then merges them back into the
ordinary text profile that `gocover-cobertura` and `go tool cover -func`
consume, preserving `mode: count` and the existing coverage gate unchanged.

Two details that will bite anyone editing this:

- **The covdir path must be absolute.** `go test` runs each package's test
  binary with that package's directory as the working directory.
- **`--packages` is required, not decorative.** gotestsum refuses to combine
  `--rerun-fails` with explicit `go test` args unless the package list arrives
  via `--packages`. Passing packages there (and *not* as positional args) also
  keeps reruns scoped to the failing package.

`scripts/ci/run_unit_tests_test.sh` pins all of this as a regression test. It
runs inside `test:unit` before the real suite and asserts, against a hermetic
fixture, that a flake exits 0, a deterministic failure exits non-zero, and
coverage stays at 60.0% through a rerun. **Do not bump `GOTESTSUM_VERSION`
without it passing.**

## The `flaky-test` label lifecycle

- **Opened** automatically by `flakereport` the first time a test flakes in
  CI. Title `flake: <TestName>`, label `flaky-test`.
- **Updated** on each recurrence: a comment with the pipeline/job link, and an
  incremented `hits=` counter in the invisible
  `<!-- loom-flake-dedup: ... -->` marker in the description. Only that marker
  line is rewritten, so investigation notes in the body survive.
- **Ranked weekly** by the `flake:digest` job into the rolling issue
  `flake digest: loom-core test:unit` (label `flake-digest` — deliberately not
  `flaky-test`, or the digest would list itself). Highest hit count first.
- **Closed only by a fix.** Close the issue from a merge request that fixes the
  flake and references it. Do **not** close it because the test has gone quiet:
  the recurrence counter is the signal, and a silent flake is the same test
  that cost ten hours of blocked deploys. If a test cannot be made
  deterministic, delete or rewrite it rather than adding it to an ignore list.

Dedup is keyed on the bare test name, so a test that moves between packages
keeps one issue. Lookup failures fail *open* — a fresh issue is filed rather
than dropping the signal — mirroring the mills escalator
(`pkg/mills/pipeline.EscalationDedupMarker`).

## Configuration

| Variable | Default | Purpose |
|----------|---------|---------|
| `GOTESTSUM_VERSION` | `v1.13.0` | Pinned gotestsum. Bump only with `run_unit_tests_test.sh` green. |
| `FLAKE_RERUN_ATTEMPTS` | `2` | `--rerun-fails` |
| `FLAKE_RERUN_MAX_FAILURES` | `5` | `--rerun-fails-max-failures`; above this, no reruns at all |
| `FLAKE_RERUN_REPORT` | `rerun-report.txt` | Rerun report path (also the artifact name) |
| `UNIT_TEST_TIMEOUT` | `30m` | Per-package `go test -timeout`. Sized for an I/O-starved runner, not a hang — see [Package timeouts are starvation, not flakes](#package-timeouts-are-starvation-not-flakes) |
| `FLAKE_DIGEST` | unset | Set to `true` on a weekly pipeline schedule to run `flake:digest` |
| `FLAKE_ISSUE_TOKEN` / `GITLAB_TOKEN` | — | Project access token with `api` scope. **Required for issue filing.** |

`CI_JOB_TOKEN` cannot create issues, which is why a project access token is
needed. Without one, `flakereport` still prints every `FLAKY-TEST-DETECTED`
line and warns loudly that it could not file — it never fails the job. Issue
bookkeeping must not be able to red a pipeline.

## Running it locally

```bash
bash scripts/ci/run_unit_tests_test.sh
```

```bash
bash scripts/ci/run_unit_tests.sh
```

`flakereport` reads its GitLab target from the CI environment, so a local
`run_unit_tests.sh` prints flake evidence without filing anything.
