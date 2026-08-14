# RALPH Iteration: Fleet Reliability S0

## Review

- Roadmap milestone: Loom Core fleet performance and reliability architecture
- Canonical plan: `plan-loom-core-fleet-reliability-arch-20260710`
- Canonical slice: `plan-loom-core-fleet-reliability-arch-20260710#1`
- Spec section: P0 — Hermetic branch reliability gauntlet
- Prior decisions to preserve:
  - Keep ordinary local MCP calls storage-free; this slice proves the current fast path rather than changing it.
  - Keep generation supervision and Mills transition semantics in later slices. The gate may repair a narrowly proven ownership defect, but it must not silently broaden into the supervision redesign.
  - Branch pipelines are the canonical merge signal because detached merge-request pipelines are disabled at repository workflow level.

## Align

- Slice name: P0 — Hermetic branch reliability gauntlet
- Scope in:
  - Replace the broad `pkg/...` unit-test exclusion with dependency-derived fi-accel exclusions.
  - Add a required branch reliability job covering a hermetic proxy/fake-hub reset, targeted race/crash suites, contracts, OpenAPI conformance, a 60-second mixed load, and benchmark comparison.
  - Fail when a required test is absent, skipped, or records zero executed scenarios.
  - Retain a machine-readable artifact with build/config/schema provenance and scenario counts.
  - Replace the stale hub mux cache with one owner per fresh physical WebSocket, filter notifications before initialization and call responses, and close active transports before daemon shutdown waits.
- Scope out:
  - Generation-aware supervision and the broader hub fault soak (remaining Slice 2 work).
  - Mills transactional state transitions, leases, fencing, or effect ledgers (Slices 3+).
  - Production deployment, daemon restart, or cluster mutation.
- Acceptance criteria:
  - Branch CI selects only packages that can build without fi-accel and continues to test unaffected `pkg/...` packages.
  - The fake-hub close/reset scenario is mandatory under `LOOM_RUN_INTEGRATION=1` and cannot degrade to a skip.
  - Exactly 28 named daemon, muxstdio, Mills workflow/store, spawn, contract, integration, load, and benchmark scenarios are present and pass without skips or accidental extras; race coverage is explicit per group.
  - Seven same-runner, AB/BA-interleaved benchmark pairs block median per-pair regressions above 10% for time and 15% for bytes/allocations.
  - The required job remains within the ten-minute branch-pipeline budget and always publishes a run-scoped manifest/final status with build/config/schema provenance, including on early failure.
- Dependencies/blockers: Go toolchain and a fetchable default-branch merge base; no live hub, ambient daemon, Qdrant, Kubernetes, model provider, or native fi-accel archive.
- Known boundary: Linux `-race` requires CGO while the daemon transitively imports fi-accel native files. The manifest therefore records daemon and spawn scenarios as `race_enabled: false`; muxstdio, Mills workflow/store, and custom-server remain race-gated. Publishing a Linux FFI artifact or a repository-wide pure-Go acceleration tag is follow-up work, not a silent exclusion.
- Riskiest assumption: a same-runner comparison against the merge base plus a deterministic fake-hub/load harness is stable enough to block regressions without creating a flaky merge gate.
- Kill test: run the reliability job repeatedly on an unchanged SHA, inject a deliberate >10% benchmark delay and a skipped required test, and require stable clean passes plus deterministic failures for both injected regressions.

## Land

- File areas: `.gitlab-ci.yml`, `Makefile`, `scripts/ci/`, `internal/integration/`, `internal/daemon/`, `internal/fleetgate/`, `internal/reliability/`, `pkg/transport/muxstdio/`, `pkg/mills/`, `cmd/custom-server/`
- Implementation steps:
  1. Centralize dependency-derived package selection and test it against known affected/unaffected packages.
  2. Add exact required-test orchestration, hermetic fault/load scenarios, and paired versioned benchmark comparison.
  3. Repair fresh WebSocket ownership exposed by pre-init notification/reset tests, wire one required branch CI job, and emit the standardized run-scoped evidence bundle.

## Prove

- Full gate: `LOOM_RELIABILITY_SKIP_FETCH=1 GOWORK=off make ci-reliability` passed in 2m53s with 28/28 required scenarios.
- Hub reset: two fresh physical connections, one injected 1012 reset, two tool attempts, and three notifications (including pre-initialize and pre-response) completed without a stale response or daemon restart.
- Mixed load: 60 seconds; 11,998 events published/received with zero EventBus drops, 5,999 mux calls, 11,998 pool cycles, and 2,999 durable Mills writes with exact row parity.
- Paired benchmarks: seven AB/BA rounds each; daemon event -0.55%, mux round trip -1.30%, Mills append +0.07%, custom-server write -1.14%; bytes and allocations stayed within the versioned thresholds.
- Failure artifact kill test: an invalid baseline exited 128 at `fetch-baseline` while preserving a failed final status, build SHA, 64-character config/schema digests, and suite manifest; no verifier binary leaked into artifacts.
- Broader verification: `GOWORK=off CGO_ENABLED=0 go test ./...`, `go test -short ./...`, `golangci-lint run`, focused `-race`, `go vet`, `make ci-contracts ci-openapi`, ShellCheck, format/diff checks, package-selector self-test, and GitLab CI lint all passed.
- Restored unit coverage exposed a Darwin ConfigMap watcher flake; the staging-symlink path now has a bounded reload and passed 300/300 focused repetitions plus full `pkg/mills` and race checks.
- CI checks: required reliability job, existing unit/integration jobs, and the full branch pipeline remain the merge-time proof.

## Handoff/Harvest

- Slice status: implemented locally; awaiting merge-request and branch-pipeline refs.
- What landed: precise package selection; strict scenario/benchmark verification; run-scoped success/failure evidence; fake-hub reset/notification coverage; active hub transport ownership and bounded shutdown; zero-drop mixed load; paired performance gate; portable Mills ConfigMap reload.
- Known boundary: Linux daemon/spawn race coverage remains blocked by the pinned fi-accel module's missing native header/archive under `GOWORK=off`; task `4b26230b2511f509` tracks a hermetic path instead of silently claiming race coverage.
- Residual follow-up: a tool-refresh debounce callback already executing at shutdown is not joined; generation-aware cancellation belongs in the supervisor slice rather than this gate.
- Next slice: `plan-loom-core-fleet-reliability-arch-20260710#2` — generation supervisor, coordinated restart, and the extended transport fault soak.
