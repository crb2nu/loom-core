# RALPH Iteration: Fleet Reliability S1

## Review

- Roadmap milestone: Loom Core fleet performance and reliability architecture
- Canonical plan: `plan-loom-core-fleet-reliability-arch-20260710`
- Canonical slice: `plan-loom-core-fleet-reliability-arch-20260710#2`
- Spec section: P0 — Transport ownership and generation supervisor
- Prior decisions to preserve:
  - The local fast path remains storage-free.
  - Every physical resource has one owner, and every replaceable resource has a monotonic generation.
  - Fresh hub pool dials remain independent physical WebSockets; there is no server-name hub mux cache.
  - The S0 branch gate is the merge authority and must describe race coverage honestly.

## Align

- Slice name: P0 — Transport ownership and generation supervisor
- Scope in:
  - Add a dependency-light generation supervisor kernel with monotonic generations, single-winner creation and replacement, active-call leases, generation-fenced failure, idle retirement, and bounded shutdown.
  - Wire the production local process/mux path through that kernel so pooled transports carry their observed generation and every call holds a lease until its connection is returned.
  - Make local transport failure, health restart, idle reaping, reload, and shutdown stop only the generation they observed; a delayed old failure must not close a replacement process or mux.
  - Contain hub failures to the owning physical WebSocket instead of clearing unrelated idle or active hub transports.
  - Make tool-refresh shutdown cancel and join a callback that has already started.
  - Extend the mandatory reliability manifest with generation-kernel race tests, daemon wiring tests, and a fake-hub socket/fault soak.
- Scope out:
  - Mills transactional transitions, invocation identity/admission, durable execution, endpoint-health unification, or clustered workers.
  - A pool per generation; the existing local and hub pools remain long-lived.
  - Full-package Linux `-race` for `internal/daemon` and `internal/spawn`. The pinned fi-accel module selects native cgo files under Linux race builds but does not publish its header/archive. This slice races the exact dependency-light production supervisor kernel and keeps wiring tests non-race; a released `fiaccel_purego` mode is separate dependency work.
  - Production deployment, local daemon restart, or Kubernetes mutation.
- Acceptance criteria:
  - Concurrent cold acquisition creates one Ready local generation and initializes its process/transport once.
  - One generation failure has one winner; replacement advances monotonically; a delayed failure for the old generation returns false and cannot close, evict, or stop the current generation.
  - An active call lease prevents idle retirement in the default mux path; retirement succeeds after release without killing a long call at the timeout boundary.
  - Ten thousand in-memory fault cycles close every resource, leave zero generations/leases, and settle within +5 goroutines and +5 MiB post-GC.
  - Twenty-five blocked cold hub calls open exactly 25 physical sockets; one injected 1012 close causes at most one owning-call failure and one replacement; 1,000 subsequent calls preserve IDs/results without an extra dial; 100 token-addressed fault cycles each recover once; shutdown reaches active=0 and opened=closed.
  - A tool-refresh callback already running at shutdown observes cancellation and `stop()` does not return until it exits.
  - The required branch gate remains within ten minutes and records exactly 34 manifested scenarios with the generation kernel explicitly race-enabled.
- Dependencies/blockers:
  - S0 is merged and pipeline 18198 is green.
  - Hermetic tests use `GOWORK=off`; no ambient daemon, live hub, Kubernetes, Qdrant, or fi-accel native archive is required.
- Risk notes:
  - The dependency process manager is keyed only by server name. The supervisor must keep a generation in Draining until name-keyed teardown finishes so a new generation cannot be published while a stale stop still owns that name.
  - Pool connections do not carry metadata directly. Generation identity must travel on the transport wrapper and stale pooled entries must be rejected before Send.
  - The default mux path skips `callLock`, so the idle reaper must use active leases rather than the legacy lock heuristic.
  - Hub-wide `ClearServer` on one socket error would violate independent physical ownership and amplify a single reset.
- Riskiest assumption: a daemon-layer generation fence is sufficient to make the name-keyed process manager safe without replacing it or adding storage to the fast path.
- Kill test: publish generation 2, then release a delayed failure from generation 1 while generation 2 is idle and callable. Reject the design if the stale failure closes generation 2, causes a second replacement, leaks a resource, or makes any of the following 1,000 calls fail. Repeat 10,000 in-memory cycles under Linux `-race` and require bounded resource settling.

## Land

- Status: implemented on `codex/arch-fleet-s1-transport-generations`.
- Commits:
  - `634c5564` — generation-owned local process/mux and hub transports, publication fencing, bounded lifecycle teardown, cancellable credential helpers, and reliability evidence.
  - `8c8888f2` — dependency-derived build-hook filtering for test-only packages.
  - `197c9b28` — compile-once benchmark binaries that retain all paired samples while fitting the CI timeout.
- Landed architecture:
  1. Added a dependency-light generation supervisor with monotonic IDs, single-winner asynchronous creation, leases, exact-generation failure/retirement, idle retirement, and joined shutdown.
  2. Made local processes and mux transports generation-owned; separated logical pool views from physical ownership; fenced registry reload/publication; sharded process-manager ownership per server so a blocked dial or stop cannot serialize the fleet.
  3. Made hub socket failures owner-local, rejected stale pooled transports, and removed server-wide collateral clearing after an individual `1012` reset.
  4. Published registry, route, auth, tool-cache, profile, and manifest state through immutable snapshots/revisions; canceled and joined refresh, debounce, helper-process, and daemon shutdown work.
  5. Versioned the reliability gate at 34 scenarios with deterministic race, reset, soak, mixed-load, and paired benchmark evidence.

## Prove

- Authoritative local gate: `GOWORK=off make ci-reliability` passed in 141 seconds.
- Evidence: `.loom/local/evidence/fleet-reliability/20260711T195329Z-17204/`
  - Build `197c9b284bf93d05dfaac7c48a9e5c8d9976cfcd` versus merge base `9b02cfe473ba8c9d9610eb2b5134a395baffa5cd`.
  - Exactly 34 manifested scenarios passed; no required test was skipped.
  - Fake-hub soak ended at active `0`, opened/closed `126/126`, handled `101` resets and `100` token-addressed faults, then completed `1,000` sequential calls without an extra dial.
  - Mixed load ran for 60 seconds and completed 44,989 operations across event publish/receive, mux calls, pool cycles, and Mills writes.
  - Four benchmarks retained seven paired samples per side; time, bytes, and allocation thresholds all passed.
- Additional proof: full pure-Go test suite; exact generation and secret-helper race loops; fake-hub integration scenarios repeated 20 times; contracts/OpenAPI; full `go vet`; full `golangci-lint`; ShellCheck; hook regressions; pre-commit; and `git diff --check` all passed.
- Platform boundary: the pinned fi-accel module still prevents full-package CGO/race builds because its published module lacks the native header/archive. The exact dependency-light generation kernel is race-proven, and all daemon wiring is covered by pure-Go and black-box tests.
- Shipping proof:
  - MR `!1051` merged as `7e6f6d78` after branch pipeline `18260` passed every automatic job.
  - Post-merge pipeline `18261` passed all functional scenarios but exposed that the default-branch benchmark gate had selected `HEAD` as both baseline and candidate; identical binaries then crossed the 10 percent threshold under runner drift.
  - Follow-up `73c10668` selects the exact pre-push default-branch commit, rejects unavailable/divergent/self baselines, and executes each base/candidate package pair adjacently with alternating order. It retains seven samples per side, 500 ms duration, 56 executions, and the original thresholds.
  - Corrected authoritative run `20260711T213155Z-69492` passed all 34 scenarios in 146 seconds against merged `main`; all four paired time/allocation comparisons passed.
- Remaining CI proof: the focused benchmark-gate follow-up branch and resulting `main` pipeline must reach terminal green.

## Handoff/Harvest

- `ROADMAP.md` now marks S0/S1 complete and points at the Mills transition kernel.
- Canonical plan and Agent Context contain the implementation, failure-recovery, merge-request, pipeline, and evidence references.
- Active task: `b2f785ab37597a88`
- Next-slice candidate: `plan-loom-core-fleet-reliability-arch-20260710#3` — Mills transactional transition kernel.
