# Mills runner does not resume after `errStagePending` (2026-05-17)

**Status:** Research only. Diagnosis + fix proposal for the wedge that held
`PIPE-MILLS-CANARY-VERIFY2-045207-1778993527` in `state=planning,
current_stage=plan_slice` for ~9 hours after the operator rolled.

## 1. Root cause

`pkg/mills/pipeline/runner.go:362-364` — when `runStage` returns
`errStagePending`, `Drive` logs `pipeline drive stopped; stage remains
pending` and **returns `nil`**. The goroutine started by `Runner.Start`
(`runner.go:263-274`) exits. Nothing else scans non-terminal `pipeline_runs`
to re-invoke `Drive`. The only re-driver,
`Reconciler.ResumeInFlightRuns` (`pkg/mills/reconciler.go:274-313`), fires
**exactly once at operator startup**. A transient `getSpawnState` error
(`pkg/mills/clients/spawn.go:275-300`) during Resume flips a healthy-but-still-Running
spawn into "pending row, no poller" state forever.

`Reconciler.Tick` only iterates `BacklogQueued` items
(`reconciler.go:175`); our backlog row is `running` so Tick never re-picks
it up. `pickupQueuedSubruns` only handles `parent_run_id != NULL`. There is
no mid-life re-driver.

## 2. What was supposed to happen

Startup (verified by `pipeline startup resume complete inspected=1 resumed=1`
at 05:57:09Z):

1. `main.go:340` → `Reconciler.ResumeInFlightRuns` →
   `Store.Pipeline.ListInFlight` (`dao_pipeline.go:201-222`) returns runs
   with `state NOT IN ('queued','done','escalated','paused')`.
2. For each, `Starter.Start` (`runner.go:263`) spawns `Drive` in a
   `context.Background()` goroutine.
3. `Drive` resolves `resumeIndex` to `plan_slice`, `pendingStage`
   (`runner.go:475-490`) returns the existing
   `stage_results` row with `outcome IS NULL && spawn_id != ''`.
4. `runStage` (`runner.go:495-555`) stashes
   `ResumeSpawnID=spawn-b7bc071ff949` on the stage context.
5. `fallbackDispatcher.Dispatch` (`main.go:842-855`) propagates
   `ResumeSpawnID` into `JobContext`; `SpawnWorker.Run`
   (`dispatcher.go:242-251`) calls `HUDSpawnClient.Resume`
   (`clients/spawn.go:195-203`) which polls until terminal status.

All five fired correctly. The expected continuation: poll converges on
`completed` and the loop advances to the next stage.

## 3. What actually happened

At 06:06:16Z (~9 min into Resume polling, well under the 30-min
`PollDeadline` default at `clients/spawn.go:70`), `pollSpawn` returned a
non-nil error — almost certainly a transient `getSpawnState` HTTP failure.

`runStage` then took the pending-write branch (`runner.go:539-554`):
`derr != nil` ✓, `out.SpawnID == "spawn-b7bc071ff949"` ✓ (Resume's error
path returns `SpawnResponse{SpawnID: spawnID}` at `spawn.go:218-221`),
`!hasTerminalSpawnStatus(out)` ✓ (Resume error path leaves `Artifacts` nil).
It refreshed the pending `stage_results` row, emitted
`pipeline.stage.pending`, and returned `errStagePending`. `Drive` logged and
returned `nil`. The goroutine exited.

Live evidence from operator state DB
(`kubectl cp loom-mills/loom-mills-operator-7f74cbd68f-vmt8c:/var/lib/loom-mills/state.db`):

```
pipeline_runs: state=planning current_stage=plan_slice ended_at=NULL
stage_results: stage=plan_slice attempt=1 outcome=NULL spawn_id=spawn-b7bc071ff949
```

Hypothesis (1) ("row never written") is **falsified** — the row is there,
nobody reads it. The spawn pod stayed `Running` until I manually deleted it
~9 hours later; only then did issue #118's `pod not found during
reconciliation` poison fire (downstream effect, not cause).

## 4. Proposed fix slice

**File:** `pkg/mills/reconciler.go` (~30 LOC) + new test
`pkg/mills/reconciler_test.go` (~30 LOC).

**Change:** extend `Reconciler.Tick` to call
`r.Store.Pipeline.ListInFlight(ctx)` after `pickupQueuedSubruns` and invoke
`r.Starter.Start(...)` for each non-terminal run. The existing
`r.active.LoadOrStore` guard in `Runner.Start` (`runner.go:259`) makes this
idempotent: a live goroutine returns `nil` with a warn log; a missing
goroutine gets re-spawned.

**Does:** re-drive any non-terminal `pipeline_runs` whose runner goroutine
exited (after `errStagePending` or a panic-recovered Drive). Idempotent —
`Drive` reads the pending row, gets `spawn_id`, reattaches via
`SpawnResumeClient.Resume`. No new spawn started.

**Does NOT:** re-create stage rows, bypass `r.active`, touch terminal runs
(`ListInFlight` already excludes them), bypass policy/autonomy guards
(the new scan stays inside Tick's existing gates so a paused operator stays
paused).

## 5. Risks

- **Re-running a completed stage.** Mitigated: `runStage` always calls
  `pendingStage`; a completed stage has `outcome=success|error` and
  `loadPriorOutputs` / `seedAttempts` (`runner.go:409-472`) already
  advances past it.
- **Concurrent Drives for the same run.** Blocked by
  `r.active.LoadOrStore` (`runner.go:259-262`).
- **Tick thrash during HUD outage.** A 60s re-drive while `getSpawnState`
  fails every call produces identical `drive stopped` log lines; pending row
  + spawn pod unmolested. Backoff/jitter is a follow-up, not a blocker.
- **Latency.** Worst-case 60s before stuck run resumes — fine vs. 30-min
  stage budgets.

## 6. Alternatives ranked

1. **Inline retry on `errStagePending` with backoff.** Rejected: a rollout
   mid-backoff returns to the same wedge. Tick-based re-drive survives
   rollouts by design.
2. **`pollSpawn` retries `getSpawnState` errors forever.** Rejected —
   hides real HUD outages and inverts the `errStagePending` contract
   that callers can recover later.
3. **Scan in `scheduler.Run`'s ticker instead of Tick.** Equivalent;
   Tick reuses the policy/autonomy guards and event helper.

## 7. Verification plan

1. **Unit test:** pipeline_runs row `state=planning` + pending stage_results
   row + fake Starter. Two consecutive Ticks both call `Start`.
2. **Idempotency:** starter that holds `r.active`; second Tick is no-op
   (warn log only) — no new dispatch.
3. **Deliberate-rollout integration (manual):** start canary, wait until
   `plan_slice` spawn accepted, `kubectl rollout restart deploy/loom-mills-operator`.
   Confirm new pod logs `startup resume complete`; within ~60s after any
   `drive stopped` line, another `Drive` cycle fires (event
   `reconciler.tick` shows it picked up the in-flight run). Run reaches
   `implement` without manual escalate.
4. **Negative:** point operator at a dead HUD URL; after 5 min confirm
   multiple `drive stopped` lines, single `stage_results` row, no duplicate
   rows, no new spawn pods.

## References

- Worktree: `.claude/worktrees/compassionate-benz-50a60a`
- State DB: `/tmp/mills-state.db` (2026-05-17T15:04Z)
- Logs: `kubectl logs -n loom-mills loom-mills-operator-7f74cbd68f-vmt8c`
- Prior: `.loom/118-diagnosis-mills-spawn-pod-not-found-2026-05-16.md`
- Code: `pkg/mills/pipeline/runner.go:135,259-275,290-381,475-555` ·
  `pkg/mills/reconciler.go:149-219,274-313` ·
  `pkg/mills/store/dao_pipeline.go:201-222` ·
  `pkg/mills/clients/spawn.go:151-236` ·
  `cmd/loom-mills-operator/main.go:332-346,842-855`
