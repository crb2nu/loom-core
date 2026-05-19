# Mills pipeline + spawn — state machines and known sharp edges

Reference for anyone debugging Mills pipeline runs or modifying spawn
plumbing. Captures what the state machines actually are today (not what
they should be), the regression tests pinning recent fixes, and the
seams where bugs tend to cluster.

Sources: `pkg/mills/store/types.go:127-142`,
`pkg/mills/pipeline/runner.go`, `pkg/mills/pipeline/dispatcher.go`,
`pkg/mills/clients/spawn.go`, `internal/spawn/types.go:55-66`,
`cmd/loom-mills-operator/handlers_pipeline.go`.

---

## Regression-test audit (Slice 4a)

Six representative `fix(mills):` commits from the recent stabilization
arc, with the named regression test that pins each. All 16 named tests
pass on HEAD:

```
ok   github.com/crb2nu/loom/pkg/mills/pipeline           0.385s
ok   github.com/crb2nu/loom/pkg/mills/clients            0.485s
ok   github.com/crb2nu/loom/cmd/loom-mills-operator      0.683s
```

| Commit | Symptom fixed | Regression test |
| --- | --- | --- |
| `a3070890` — persist spawn failure context to `stage_results.log_tail` | 33/33 plan-slice error rows had empty `log_tail` and were untriagable | `TestRunner_PersistsSpawnFailureContextWhenWorkerReturnsEmptyError` — `pkg/mills/pipeline/runner_test.go:788` <br> `TestRunner_PersistsSpawnFailureContextOnPendingPath` — `pkg/mills/pipeline/runner_test.go:843` <br> `TestBuildFailureLogTail_Precedence` — `pkg/mills/pipeline/runner_test.go:878` |
| `a433ce06` — redial broken MCP hub transport on close 1006 / broken pipe | Cached WebSocket transport went stale after `fi-mcp-gateway` tore the connection; every later `CallTool` hit "broken pipe" instantly | `TestCallTool_RetriesOnceAfterTransportClose1006` — `pkg/mills/clients/mcphub_test.go:453` <br> `TestCallTool_RetriesOnceAfterBrokenPipeOnSend` — `pkg/mills/clients/mcphub_test.go:502` <br> `TestCallTool_DoesNotRetryJSONRPCErrors` — `pkg/mills/clients/mcphub_test.go:541` <br> `TestCallTool_StopsAfterOneRetry` — `pkg/mills/clients/mcphub_test.go:570` <br> `TestIsTransportError` — `pkg/mills/clients/mcphub_test.go:599` |
| `6cb0dcd1` — propagate `ResumeSpawnID` through operator dispatcher | Pipeline runs accepted a spawn, lost the ID at the dispatcher boundary, and the next reconciler tick spawned a duplicate instead of resuming | `TestFallbackDispatcher_PropagatesResumeSpawnID` — `cmd/loom-mills-operator/dispatcher_test.go:28` |
| `a08c8f7d` — keep accepted spawns pending on poll interruption | Poll interruption (operator restart, context timeout) marked an accepted spawn as failed, even though the spawn pod was still running | `TestRunner_KeepsAcceptedSpawnPendingOnInterruptedPoll` — `pkg/mills/pipeline/runner_test.go:638` <br> `TestMapTelemetryToResponse_PreservesTerminalStatusWithoutTelemetry` — `pkg/mills/clients/spawn_test.go:397` |
| `f2dec2cf` — guard pending spawn ownership | `pipeline.Runner.Start` could double-dispatch a run whose stage already had a pending spawn — two operator workers fighting over the same row | `TestRunner_StartSuppressesDuplicateActiveRun` — `pkg/mills/pipeline/runner_test.go:689` <br> `TestHandlePipelineEscalate_MarksRunAndBacklog` — `cmd/loom-mills-operator/handlers_test.go:533` |
| `f55f8cfe` — resume accepted HUD spawns | After accepting a HUD spawn, the runner forgot to record the SpawnID until the first poll returned — a tick in the wrong order escalated mid-spawn | `TestRun_RecordsAcceptedSpawnBeforePolling` — `pkg/mills/clients/spawn_test.go:227` <br> `TestResumePollsExistingSpawnWithoutPost` — `pkg/mills/clients/spawn_test.go:340` <br> `TestRunner_ResumesPendingStageSpawnAttempt` — `pkg/mills/pipeline/runner_test.go:595` |

**Finding**: each fix landed with at least one named test that exercises
the original symptom — coverage for this cluster of bugs is already in
place. The fix cadence is real, but it's not because tests are missing.

---

## Pipeline state machine

`PipelineState` lives in `pkg/mills/store/types.go:127-142`. The Runner
in `pkg/mills/pipeline/runner.go` walks a run through stages; each stage
carries the `PipelineState` it advances `PipelineRun.State` to.

```
                  queued
                    │
                    ▼
   ┌── planning ──→ slicing ──→ implementing ──→ testing ──→ reviewing
   │                                                            │
   │   (gates run between stages; a failed gate retries the    │
   │    same stage up to the per-stage attempt budget)         │
   │                                                            ▼
   │                                                          mr
   │                                                            │
   │                                                            ▼
   │                                                          ci
   │                                                            │
   │                                                            ▼
   │                                                          merging
   │                                                            │
   │                                                            ▼
   │                                                          done   ← terminal
   │
   └── escalated ← any stage can escalate after exhausting attempts
                   (runner.go:776 escalateWithItem)

       paused    ← injected by external operator action; not produced
                   by the runner's normal walk
```

Notes:
- `paused` and `escalated` are terminal-for-the-reconciler in that the
  reconciler does not re-drive them on tick; the operator owns the next
  move.
- `done` is the only "success" terminal state.
- `markDone` (`runner.go:745`) flips both the run and the backlog
  (`PipelineDone` + `BacklogMerged`) in one transaction.

---

## Spawn lifecycle (internal/spawn)

`Status` lives in `internal/spawn/types.go:55-66`. The HUD owns this
state machine; the operator reads it through
`HUDSpawnClient.getSpawnState` (`pkg/mills/clients/spawn.go:316`) and
classifies terminals via `isTerminalSpawnStatus`
(`pkg/mills/clients/spawn.go:412`).

```
   creating ──→ building ──→ running ──┬──→ completed   ← terminal
                                       ├──→ failed      ← terminal
                                       └──→ stopped     ← terminal

   unknown     (rare; surfaced when the HUD can't classify the pod)
```

The operator-side `isTerminalSpawnStatus` only considers `completed |
failed | stopped` as terminal — `unknown` deliberately does NOT halt
polling so a transient HUD blip doesn't kill an otherwise healthy run.

---

## The cross-component seam (where the bugs live)

Two state machines run in parallel and synchronize via three rows in
the store:

```
   ┌─────────────────────────────┐         ┌──────────────────────────┐
   │ Pipeline state machine      │         │ Spawn state machine      │
   │ (operator: runner.go)       │         │ (HUD: internal/spawn)    │
   │                             │         │                          │
   │  queued → … → testing → …   │         │  creating → building →   │
   │                             │         │  running → {term}        │
   └────────────┬────────────────┘         └─────────────┬────────────┘
                │                                        │
                │  store rows synchronize the two:       │
                │                                        │
                ▼                                        ▼
        pipeline_runs.state              stage_results.spawn_id
        pipeline_runs.current_stage      stage_results.outcome
                                         stage_results.log_tail
```

The "ResumeSpawnID propagation" class of bugs (`6cb0dcd1`, `a08c8f7d`,
`f55f8cfe`) all live at this seam: the runner accepts a spawn ID from
the HUD, persists it on a `stage_results` row, and reads it back on the
next tick to resume polling. Anything that loses the ID between accept
and persist forces a re-spawn.

The pending-stage path (`runner.go:475 pendingStage` +
`runner.go:539-555 errStagePending`) is the deliberate "we accepted a
spawn but didn't see a terminal status yet" branch. It's the
load-bearing primitive: if it fires when it shouldn't, the run looks
stuck. If it fails to fire when it should, the run gets re-dispatched
and the spawn is double-counted.

---

## Known sharp edges

- **`runStage` can persist a row twice on the pending path**
  (`runner.go:526` then `runner.go:541`). Both rows carry the same
  `(PipelineRunID, Stage, Attempt, SpawnID)` and DAO logic dedupes on
  insert. If the dedupe logic in `dao_pipeline.go` ever changes,
  expect ghost rows.
- **`ResumeSpawnID` is carried on `context.Context`** via
  `withResumeSpawnID` / `resumeSpawnIDFromContext` (`runner.go:149-167`),
  not as a typed struct field. Cross-process boundaries (e.g., the
  fallback dispatcher in `cmd/loom-mills-operator/main.go`) have to
  unpack and re-pack manually — `6cb0dcd1` was exactly that miss.
- **`isTerminalSpawnStatus` is duplicated** between
  `pkg/mills/clients/spawn.go:412` and `internal/spawn` (the comment
  acknowledges this). If the canonical set ever expands beyond
  `completed | failed | stopped`, both sides need to move in lockstep.
- **Default `CallTimeout` on the MCP hub client is 10m** since
  `a433ce06`. `devbox_quality_gate` against a real workspace lives at
  ~2-3 minutes; anything shorter than 5m here will repeatedly hit the
  transport-error retry path and look like flake.
- **`hasTerminalSpawnStatus` reads from `out.Artifacts["status"]`**
  (`runner.go:646`). Workers that don't set this artifact will look
  non-terminal even when they finished — the pending path will fire.
- **`paused` has no producer in the runner.** Only operator endpoints
  produce it; if you see a run land in `paused` from a non-operator
  path, that's a bug.
- **Backlog ↔ pipeline state coupling is one-way at terminal.**
  `markDone` (`runner.go:745`) and `escalateWithItem` (`runner.go:776`)
  flip the backlog. Mid-pipeline state changes do NOT propagate to the
  backlog row — only terminals do. Don't try to derive backlog
  liveness from pipeline mid-state.

---

## Follow-up slice candidates (not in scope for Slice 4)

1. **Type the `ResumeSpawnID` carrier**. Replace the context-key
   carrier in `runner.go:149-167` with an explicit struct field on
   `JobContext` (or on `Stage`). The current carrier requires every
   dispatcher implementation to re-pack the value — `6cb0dcd1` was
   exactly that omission, and the same shape of bug will recur.
2. **Single source of truth for terminal spawn statuses**. Promote the
   `isTerminalSpawnStatus` table out of `pkg/mills/clients/spawn.go`
   and `internal/spawn` into one shared package; eliminate the
   duplicated string set.
3. **Stage-result write coalescing**. The pending and final
   `PutStage` calls in `runStage` could be a single upsert; today
   they're two writes that depend on DAO dedupe. Worth a look only if
   `pkg/mills/store/dao_pipeline.go` ever picks up a different
   insert strategy.

None of these are blocking for the next round of Mills work.
