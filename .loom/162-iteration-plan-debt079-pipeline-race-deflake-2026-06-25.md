# Iteration Plan — DEBT-079: deflake `pkg/mills/pipeline` under `-race`

- **Date**: 2026-06-25
- **Issue**: [#176 DEBT-079](https://gitlab.flexinfer.ai/services/loom-core/-/issues/176) (P2, debt-cycle-7, wave-2)
- **Branch**: `debt/DEBT-079-pipeline-race-deflake`
- **RALPH slice**: Mills pipeline reliability / tech-debt

## Scope (in)

- Make `go test -race -short -count=10 ./pkg/mills/pipeline` pass 10/10.
- Root-cause + fix the `Started:2` double-start.
- Give the runner a deterministic stop/wait so tests don't leak goroutines past `t.Cleanup`.
- Remove the `pkg/mills/pipeline` quarantine from `.gitlab-ci.yml` `test:race`.

## Scope (out)

- No change to pipeline stage semantics, gate logic, or dispatch behavior.
- No broader reconciler redesign beyond the same-tick re-pickup fix.

## Root cause (ruled in/out)

1. **`Started:2` — NOT double agent-spend (ruled out as a spend bug).** Within one
   `Reconciler.Tick`, the queued-item loop `tryStart`s run R (`res.Started++`), then
   `pickupInFlightRuns` runs in the *same tick*, finds R still non-terminal, and calls
   `Starter.Start(R)` again. `Runner.Start`'s `r.active.LoadOrStore` guard prevents an
   actual second drive (emits the "already active" warn), but `pickupInFlightRuns` still
   counts the call → `res.Started==2`. Flaky because it depends on whether R's goroutine
   has reached a terminal/pending state before pickup runs. Fix: skip runs already started
   earlier in the same tick.

2. **DATA RACE — leaked goroutine vs shared test state.** `RunnerStarter.driveFanOut`
   (and `Runner.Start`) run detached goroutines. `Integrator.Run` does `*run = parentRun`
   (`integrator.go:188`) on the shared `*store.PipelineRun` while the test's poll loop
   reads `run` / `merger.calls` with no happens-before edge. `t.Cleanup` then closes the
   store while drive loops still run ("database is closed" noise). Fix: deterministic
   `Runner.Wait()` (+ `RunnerStarter.Wait()`); tests wait before reading shared state /
   closing the store, and poll on a captured `runID` instead of the shared struct.

## Acceptance criteria (from issue)

- [ ] Root-cause `Started:2` (ruled out as double-spend; same-tick re-pickup count artifact)
- [ ] Runner exposes deterministic stop/wait; tests use it before store close
- [ ] `go test -race -short -count=10 ./pkg/mills/pipeline` passes 10/10 locally
- [ ] Remove the quarantine line from `.gitlab-ci.yml` `test:race`

## Test plan

- `go test -race -short -count=10 ./pkg/mills/pipeline` → 10/10.
- `go test ./pkg/mills/...` → green (no behavior regressions).
- `golangci-lint` clean; CI `test:race` now includes the package.

## Risks

- `Runner.Wait()` must `wg.Add(1)` before launching each goroutine to avoid a
  Wait/Add race; fan-out goroutines register on the same Runner waitgroup.
- Reconciler same-tick skip set must not suppress legitimate later-tick re-drives
  (only same-tick duplicates).
