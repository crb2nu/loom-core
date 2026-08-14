# Reconciliation: Mills Workflow Turn Liveness (loom-core#300 / S1c gate)

**Date**: 2026-07-17
**Author**: claude-code (W1 finish-phase session)
**Type**: Reconciliation memo + evidence + handoff (NOT a new spec)
**Supersedes for status**: the 21-day-old memory line "S1c liveness FAILED,
mobile-hud restart orphans turns, #300 — gate stalled".
**Companion docs**: `.loom/134` (§S1c result), `.loom/135` (original runbook),
`.loom/187` (sprint plan of record), `docs/runbooks/mills-workflow-s1c-killtest.md`.

## TL;DR

The **#300 liveness fix is already merged and unit-verified green.** The stale
memory is wrong. What remains is **not** a re-fix of #300 — it is the larger
**S4 durable-execution pod-owned supervisor** slice (codex-led, per `.loom/187`
Sprint 2) plus the deployed 3-run canary. The current re-drive recovery path is
**expected to fail** the runbook's stricter process-continuity contract by
design, so running the live canary now would burn a window on a known failure.

## 1. What the memory claimed vs. current code (reconciled)

| Memory (≈2026-06-26) | Reality on `origin/main` @ `35f77190` (2026-07-17) |
|---|---|
| `execAgent` builds incomplete `WorkerRequest` (missing Project/WorkingDir/Branch/BaseBranch/Namespace/Substrate) → live spawns fail "SpawnRequest.Project required" | **FIXED.** `pkg/mills/workflow/runtime.go:342-356` sets `Project` (fallback `loom-core`), `Branch` (`spawnBranch`), `BaseBranch`, `Namespace: "loom-mills"`, `Substrate: "k8s"`, `IdempotencyKey`. |
| S1c: exactly-once PASSED, **LIVENESS FAILED** — mobile-hud restart orphans in-flight turns (#300); gate stalled | **#300 fix merged + closed** (MRs !1009 → !1010 → !1017, 2026-07-08/09, plus follow-on hardening through 2026-07-16). Issue #300 CLOSED. Gate still officially closed, but for a **different, stricter reason** (see §3). |

## 2. What "orphaned turn" meant mechanically, and how it is now handled

**Mechanism** (issue #300, confirmed against code): a spawn pod's process model
is `sleep infinity` (PID 1) + a **mobile-hud-driven exec** running the agent CLI
followed by a foreground `sleep N` completion hold
(`internal/hud/spawn.go:1671`, `wrapAgentCommandWithCompletionHold` at
`:1955-1971`). Force-deleting mobile-hud (CRASH B) severs the exec; on restart
`K8sConfigMapStore.LoadAll` rehydrates the record as `status: running` but —
before the fix — **no goroutine re-drove the turn** and the request timeout was
never enforced (it lived in the dead driver). The record stayed `running`
forever; the operator's `WorkerResumer.Resume` polled indefinitely; the workflow
step wedged `pending`.

**How it is handled now** (all three of #300's own fix-sketch items landed):

1. **Re-drive or fail on recovery** — `internal/hud/spawn.go`:
   `RecoverSpawnsContext` (`:543`) runs `recoverInterruptedSpawns` (`:724`),
   which classifies each rehydrated non-terminal spawn via
   `classifyInterruptedSpawn` (`:695`): **keyed** (has IdempotencyKey +
   TaskDescription + Project) → `interruptedRedrive` (re-run `runSpawn`; the
   AlreadyExists backstop adopts the live pod, the turn re-executes
   at-least-once in the same workspace); **unkeyed / lossy** →
   `interruptedFailFast` (`failSpawn` with an honest error) so resumers get a
   terminal answer instead of hanging.
2. **Controller-side timeout watchdog** — the reconciler's restart-durable
   deadline backstop `spawnDeadlineExceeded` (`internal/spawn/controller.go`
   ~`:1406-1456`) fails a running record past
   `StartedAt + max(TimeoutMinutes, spawnAbsoluteMaxAge) + grace`, independent of
   any driving goroutine (the `TimeoutMinutes==0` floor covers label-rebuilt
   records).
3. **Terminal GC** — `reapTerminalSpawn` (`internal/hud/spawn.go:2769`, wired via
   `SetTerminalHook` at `:454`) + `StartPruneLoop` (24h retention).

**Boot ordering invariant** (the !1017 root cause) is intact and further
hardened: `finishSpawnInit` (`internal/hud/embed.go:776`) runs
`RecoverSpawnsContext` (with capped-backoff retry + a *degraded* fallback that
keeps read routes serving on a transient store-unreachable boot — the
2026-07-14 observation) **strictly before** `startSpawnLoops` →
`StartReconcileLoop` (`:816-819`). Recovery-before-reconcile prevents the
reconcile loop's lossy discovered-untracked-pod record from clobbering the keyed
durable record.

**Operator side** (`pkg/mills/workflow/scheduler_min.go`): the
`WorkflowScheduler` first tick fires immediately on restart (`:124-126`) and
replays each running imperative run via `interp.Run`; a pending-with-spawn-id
step re-attaches via `Resume`. The resume poll is **bounded**
(`HUDSpawnClient.pollSpawn`, `pkg/mills/clients/spawn.go:285`, `PollDeadline`
default 30m) and returns `ErrSpawnPollTimeout` → the step stays recoverable →
next tick re-attaches. There is **no separate `MillsWorkflowMonitor`**; the
scheduler tick + bounded poll + reconciler deadline already provide bounded
re-poll/re-attach, so an added operator watchdog would be redundant.

**Exactly-once is preserved across recovery.** The turn *execution* is
at-least-once (a re-drive re-runs the CLI), but the workflow *step outcome* is
exactly-once: the deterministic `IdempotencyKey =` `wf-` +
`sha256(run.ID ⊕ step_key ⊕ call_hash)[:24]` yields one derived spawn id /
pod, and the journal `readThrough` short-circuits only `success` (ON CONFLICT),
so a completed step is never re-executed and an outcome is never double-delivered.

## 3. Why the S1c gate is STILL closed (the real remaining blocker)

Not liveness. The runbook (`docs/runbooks/mills-workflow-s1c-killtest.md`,
rebuilt 2026-07-14) raised S1c to a **process-continuity contract**:

- PASS-1 requires the **same** `(PID, /proc/<pid>/stat starttime)` for the
  `sleep 90` hold **and** its completion-wrapper parent to survive **both**
  crashes (runbook §Procedure/§7, lines ~659-704, 851-895).
- The re-drive path kills + recreates the hold (new PID/starttime) and, per the
  July-14 diagnostic, "replaces the completion wrapper and hold while leaving the
  original hold as a zombie" (runbook lines 22-26). The runbook states plainly
  (lines 754-757): *"The current HUD recovery path re-drives the CLI after
  controller restart, so it is expected to fail this stronger process contract.
  A passing run requires the pod-owned execution supervisor/reaper to preserve
  or safely complete the original process pair without controller-owned replay."*

So the gate needs a **pod-owned execution supervisor** — the turn launched
detached (reparented to PID 1, independent of the exec/controller lifecycle),
with mobile-hud attaching by tailing status rather than owning the exec, so the
original process pair survives a controller restart and is reaped in-pod on
timeout.

**Plan of record** (`.loom/187` Sprint 1 item 2 + Sprint 2 item 2): the minimal
orphan-adoption patch is done; **"the S4 durable execution slice subsumes the
full fix"** and **"closes #300-class orphans for good"**
(`codex/arch-fleet-s4-durable-execution`). This is a codex-led fleet-arch slice,
**not** part of W1, and cannot be safely built + deployed + live-validated in a
single fix session.

## 4. Verification performed this session (green)

`GOWORK=off CGO_ENABLED=0` in `.worktrees/feat-wf-liveness` @ `origin/main`:

- `go build ./pkg/mills/... ./internal/hud/... ./internal/spawn/... ./cmd/mills-workflow-killtest/...` → rc 0.
- `go test -race ./pkg/mills/workflow/ -run 'TestRuntime_CrashResume|TestRuntime_ResumeReattaches|TestRuntime_TerminalSpawnFailureFailsRun|TestRuntime_ManualFailIsTerminalFenceForLateCompletion|TestRuntime_CanaryCompletes'` → PASS. These pin: crash → fresh interpreter completes exactly once; cold map → `Resume` re-attach exactly once; terminal spawn failure → run goes `error` (no zombie loop) and respects the !1025 terminal-freeze fence.
- `go test ./internal/hud/ -run 'TestClassifyInterruptedSpawn|TestRecoverInterruptedSpawns|TestRecoverSpawns'` → PASS. These pin the re-drive/fail decision table, owner-scoped recovery, and transient-store retryability.

The task-requested "unit-level orphan-recovery tests" (exactly-once on re-adopt;
spawn-gone → fail retryably → run resumes via replay) **already exist and are
green** — no new tests were needed.

## 5. Exact remaining manual steps for the S1c gate (handoff)

1. **Build + ship the S4 pod-owned execution supervisor/reaper** — dispatch the
   codex-led `codex/arch-fleet-s4-durable-execution` slice (`.loom/183` /
   `.loom/187` Sprint 2). It must make the agent turn survive a mobile-hud
   restart with the original `(hold, wrapper)` process pair intact (or safely
   completed) and reap orphans in-pod on timeout.
2. **Deploy** the operator + mobile-hud images carrying S4 (CI `build:image:*`
   ~90m → Flux IUA image bump → `Recreate` roll).
3. **Run the deployed 3-run dual-crash canary** per
   `docs/runbooks/mills-workflow-s1c-killtest.md` (§Procedure; `go run
   ./cmd/mills-workflow-killtest --runs 3 …`) from a stable on-LAN kube vantage.
   Open only the `workflows.enabled=true` gate behind the closed global
   admission barrier; **revert the canary window after PASS or FAIL** (runbook
   §Rollback).
4. **On PASS**: flip `.loom/132 §3` riskiest-assumption Check 7 to
   `passed YYYY-MM-DD` with the evidence bundle; advance the fleet plan
   `in_review → merging/merged` and record `kill_test_status`; unblock S6-full.

Do **not** run step 3 before step 1 lands — the current path is expected to fail
the process-continuity contract, and a failed run cannot count toward the gate.

## 6. Riskiest assumption (inherited, unchanged)

Load-bearing assumption for the whole Layer-3 program is still `.loom/132 §3`:
Starlark-Go deterministic memoized-replay holds exactly-once **across a real
dual crash on the k8s substrate**. Status there: **Tier-1 in-process PASSED
2026-06-06; Check 7 (deployed exactly-once / S1c) NOT run.** This memo does not
change that status — it clarifies that the blocker to *running* Check 7 to a
PASS is the S4 supervisor (§3), not a liveness regression.
