- **`test:unit` no longer times out on an I/O-starved runner node**
  (`pkg/mills/store/store.go`, `scripts/ci/run_unit_tests.sh`): four
  2026-09-15 pipelines hit `panic: test timed out after 10m0s` in the
  SQLite-heaviest packages with every in-flight goroutine parked in `fsync(2)`
  while the node's NVMe sat at 95 % busy under 22–26 concurrent runner pods;
  the panic also made gotestsum abort its reruns, turning an unrelated
  documented flake into a hard red. Test binaries now open stores with
  `synchronous=OFF` (production keeps `NORMAL`), `Open` switches a file to WAL
  once after the per-connection pragmas are in force so a fresh file commits
  without a sync, and the unit runner passes a 30-minute per-package
  `-timeout` (`UNIT_TEST_TIMEOUT`) as the backstop. See
  `docs/FLAKE_QUARANTINE.md`.
