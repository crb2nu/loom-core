- **CI: flake quarantine with honest reruns in `test:unit`** (`.gitlab-ci.yml`,
  `scripts/ci/run_unit_tests.sh`, `scripts/ci/run_unit_tests_test.sh`,
  `scripts/flakereport/`, `docs/FLAKE_QUARANTINE.md`): a single
  timing-dependent test (`TestStopSpawnLateStartCleanupFailureRetainsRetryablePod`)
  redded `main` on 2026-08-02 and blocked every image build for ~10h, because
  red `main` gates the deploy stage. The race is fixed; the class was not.
  `test:unit` now runs through `gotestsum --rerun-fails=2
  --rerun-fails-max-failures=5`, so a test that fails once and passes on rerun
  keeps the pipeline green — but the rerun is never swallowed. Each rerun-pass
  prints `FLAKY-TEST-DETECTED: <TestName>` to the job log and files (or
  updates, with a recurrence counter) a deduplicated `flaky-test` GitLab issue
  via the new `scripts/flakereport` command, mirroring the mills escalator's
  dedup-marker pattern; the rerun report and a JUnit report ship as artifacts
  so an absorbed flake stays visible in the MR widget. A test that fails every
  attempt is still a hard red, and more than five distinct failures suppresses
  reruns entirely. A new weekly `flake:digest` job (pipeline schedule with
  `FLAKE_DIGEST=true`) ranks open `flaky-test` issues by hit count into a
  rolling digest issue for backlog triage. `test:reliability` is deliberately
  left rerun-free — unit is green-keeping with honesty, reliability is
  flake-hunting. **Coverage gotcha, now pinned by a regression test**:
  gotestsum's reruns re-invoke `go test` with the same flags, so the old
  `-coverprofile=coverage.out` was overwritten by the rerun's `-run`-filtered
  subset, collapsing total coverage 60.0% → 0.0% on a probe and tripping the
  35% gate on any pipeline containing a flake. Coverage is now collected as Go
  binary coverage into a `-test.gocoverdir` (which accumulates across reruns)
  and merged back with `go tool covdata textfmt`, preserving `mode: count` and
  the existing gate. `scripts/ci/run_unit_tests_test.sh` asserts the whole
  contract (flake → exit 0, deterministic failure → non-zero, coverage
  preserved) against a hermetic fixture and runs before every real unit-test
  run, so a `GOTESTSUM_VERSION` bump cannot silently regress it.
