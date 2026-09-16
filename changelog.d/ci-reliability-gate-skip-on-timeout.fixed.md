- **Reliability gate survives one contended benchmark sample**
  (`scripts/ci/fleet_reliability_benchmarks.sh`, `fleet_reliability_gate.sh`):
  a benchmark binary run that exceeded the 30-second cap aborted the whole
  benchmark phase, so under runner contention every MR reported `observed
  0/4 scenarios` while the benchmarks were healthy. The paired-round driver
  now skips that package's pair on both sides (pairing never drifts), runs up
  to four make-up rounds for packages short of eleven pairs, and the cap is
  90 seconds. Deadline and genuine failures still stop the phase; a package
  that stays slow still fails the sample-count check. Bash tests cover the
  skip, make-up, deadline, and failure paths.
- **Reliability gate budget and reserve** (`.gitlab-ci.yml`, `fleet_reliability_gate.sh`):
  the job is 25 minutes with a 1380-second shell budget (was 20 and 1080), and
  the benchmark deadline check reserves twice the slowest observed sample
  (floor 20 seconds, ceiling the 90-second cap) instead of a flat 90 seconds,
  so a run of 2-second samples is no longer cut off with 90 seconds unused.
  Evidence: a saturated-runner run completed 84 of 88 pairs and failed two
  benchmarks at 10 of 11.
