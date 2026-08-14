# Iteration Plan — Mills `nonempty_diff` gate: stop empty-implement runs reaching the MR stage (2026-06-04)

RALPH slice executing a defensive fix on the **Phase A critical path** of
`.loom/126-plan-mills-full-vision-roadmap-2026-06-01.md`. Closes the
A2-class false-positive at the deterministic-gate layer.

- **Date**: 2026-06-04
- **Lineage**:
  - `.loom/126` Phase A / Slice A2 — first end-to-end autonomous merge,
    **FAILED 2026-06-02** (empty diff; north-star still 0).
  - `.loom/local/handoffs/mills-harvester-vm-slice-a2-killtest-2026-06-01.md`
    — root cause: codex executed `turn_count=0` on the VM, so the branch
    was empty, yet the `implement` stage was marked **success** and the
    run advanced to `mr`, opening empty MR `loom-core!598`
    (`head_sha=null`, 0 commits). The same empty-MR false-positive
    killed `!518`/`!520`/`!522` in the `.loom/44` autonomy round.

## Problem (code-level, confirmed 2026-06-04)

`pkg/mills/pipeline/runner.go` `post_implement_gate` runs
`{diff_size, scope, path_policy, secret_scan, commit_format}`. **None of
these fail on an empty diff**: `scope` and `path_policy` early-return
*pass* when `len(FilesChanged)==0`; `diff_size`/`secret_scan` pass on a
zero/absent diff; the LLM rubric explicitly scores "empty diff" 1.0
(`gates/rubric_boilerplate.go:49`). So an `implement` stage that produced
**no change at all** sails through every gate to the `mr` stage and opens
an empty MR — wasting an MR + CI cycle and (pre-`merge_when_pipeline_
succeeds` discipline) risking a no-op merge that falsely ticks the
north-star.

## Scope

**In**:
- New deterministic gate `nonempty_diff` (`pkg/mills/gates/nonempty_diff.go`):
  fails iff the implement stage produced no observable change
  (`len(FilesChanged)==0 && len(DiffPatch)==0`).
- Register it in `gates.Default()` and prepend it to
  `post_implement_gate.Gates` so it short-circuits before the size/scope
  checks.
- Fix `NoOpDispatcher` to emit a non-empty placeholder `DiffPatch` for
  `implement` (it currently returns an empty diff — the in-code
  embodiment of the false-positive; its own doc claims it "satisfies the
  deterministic gates").
- Tests: gate unit table + update `TestDefault_HasAllCoreGates`.

**Out**:
- The *upstream* root cause (codex `turn_count=0` on the VM — almost
  certainly the agent CLI being absent on the stock VM image; the k8s
  pod image installs it via `agentCLIInstallLines`, the VM cloud-init
  does not). That is Phase B Slice B1 (curated base image) / a
  cloud-init self-heal, requires a live VM to verify, and is tracked
  separately. This slice makes the failure **loud and correct**
  regardless of *why* the agent produced nothing.

## Acceptance criteria

1. An `implement` `StageOutput` with empty `FilesChanged` **and** empty
   `DiffPatch` fails `post_implement_gate` → run retries `implement`
   (per `RetryFrom: "implement"`) and, on repeated empty output,
   escalates instead of reaching `mr`.
2. A non-empty implement (≥1 file **or** a non-empty diff) passes the
   gate unchanged.
3. `gates.Default()` now exposes `nonempty_diff`; the gate is wired into
   `post_implement_gate` in the production operator path
   (`cmd/loom-mills-operator/main.go` already uses `gates.Default()`).
4. `go test ./pkg/mills/...` green, including the existing
   `TestPipeline_E2E_QueuedItemMergesWithEvalRow` (NoOp dispatcher now
   produces a non-empty placeholder diff so the smoke path still merges).

## Risk notes

- **Blast radius**: behavior changes only for runs whose implement output
  is *entirely empty* — historically a 100%-false-positive population
  (`.loom/44`: all 56 runs had `no_diff`). Strictly an improvement:
  empty work now escalates to a human instead of producing a junk MR.
- **scope-gate interaction**: keying the gate on "no change at all" (vs.
  requiring `FilesChanged≥1`) lets `NoOpDispatcher` keep `FilesChanged`
  empty — avoiding a spurious `scope` failure on no-slice smoke items —
  while still being satisfied by a non-empty `DiffPatch`.
- Unregistered-gate-name safety: the runner treats unknown gate names as
  *skip*, and the test helper `newPassingGates` lists a fixed set, so
  custom-registry tests are unaffected.

## Test plan

- `pkg/mills/gates/nonempty_diff_test.go`: table — both empty → fail;
  files-only → pass; diff-only → pass; both → pass.
- Update `TestDefault_HasAllCoreGates` want-set.
- `go test ./pkg/mills/gates/... ./pkg/mills/pipeline/...`.

## Done / handoff

- MR merged to `main`; `.loom/126` A2 notes updated to record the
  false-positive guard is now in place (north-star still gated on the
  live agent-execution fix).
