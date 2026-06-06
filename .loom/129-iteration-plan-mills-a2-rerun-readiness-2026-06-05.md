# Iteration Plan — Mills Slice A2 re-run readiness: the first autonomous merge is now code-ready (2026-06-05)

RALPH pre-flight slice on the **Phase A critical path** of
`.loom/126-plan-mills-full-vision-roadmap-2026-06-01.md`. This is the
verification-and-readiness gate that precedes the live A2 kill-test re-run.
It lands **no prod mutation** — its output is (a) a code-level confirmation
that every known agent-execution blocker is fixed, and (b) the exact live
procedure to run when a human greenlights flipping prod `stage_substrate`.

- **Date**: 2026-06-05
- **Lineage**:
  - `.loom/126` Phase A / Slice A2 — first end-to-end autonomous merge,
    **FAILED 2026-06-02** (empty diff; north-star still 0).
  - `.loom/local/handoffs/mills-harvester-vm-slice-a2-killtest-2026-06-01.md`
    — the 06-02 failure memo (codex `turn_count=0` on the VM).
  - `.loom/128-iteration-plan-mills-empty-diff-gate-2026-06-04.md` — the
    defensive `nonempty_diff` gate (merged `4bb853a7`).

---

## Why this slice exists

The A2 kill-test is a **live cluster operation** (flip prod policy → enqueue
a canary → watch for an unattended merge), not a code change. It opens a real
MR and turns on autonomous-merge in prod for the window. Before spending a
live canary — and before asking a human to flip prod policy — we owe a
code-level confirmation that the blockers which killed the 06-02 and 06-04
attempts are actually closed. This slice is that confirmation.

## Pre-flight trace (code-verified 2026-06-05)

The codex-on-harvester-vm execution chain, layer by layer, with the commit
that closed each layer and the live evidence that surfaced it:

| # | Layer | Failure evidence | Fix on `main` | Regression test |
|---|---|---|---|---|
| 1 | Workspace + agent CLI absent on the stock VM (Build is a no-op vs. one shared base image) | 06-04 canary exit **127 / 176ms**: `cd: /workspace/...: No such file or directory` + `codex: command not found` | `b4a0485d` — `Start` git-clones repo into WorkDir + guarded pinned CLI install over SSH | `harvester_vm_provision_test.go`, `spawn_cli_install_test.go` |
| 2 | codex stdin hang (codex 0.120.0+ reads stdin even with a prompt arg; SSH session `session.Stdin` is nil) | 06-04 `spawn-ced0192f6540`: `thread.started`/`turn.started` on stdout (CLI present, **authenticated, turn started**), then `Reading additional input from stdin...` on stderr, **exit 1** | `75a89996` — `buildAgentCommand` appends `< /dev/null` | `TestBuildAgentCommand` (`< /dev/null` assertion) |
| 3 | Telemetry persist blind spot | 06-02: `failed to persist spawn telemetry summary error="session_id: is required"` on both spawns → turn detail not captured | `5fc4dd75` — persist under the spawn's resolved session_id (`resolveSpawnSessionID` guard) | `spawn_telemetry_persist_test.go` (210 LOC) |
| — | Empty-diff false-positive backstop | 06-02 MR `!598` (0 commits, `head_sha=null`) reached `mr` stage and was mergeable | `4bb853a7` — `nonempty_diff` gate prepended to `post_implement_gate`; empty implement **escalates** instead of opening a junk MR | `nonempty_diff_test.go` + `TestDefault_HasAllCoreGates` |

**Routing confirmed in code (not just claimed):** harvester-vm is not
`streamExecCapable` (`internal/hud/spawn.go:827-831`), so its spawns take the
buffered `be.Exec` path (line 852) — which passes spawn `Env` and routes to
`HarvesterVMBackend.Exec` (`harvester_vm.go:503`) →
`execOverSSH` → `session.Run(cmd)` with nil stdin. The assembled remote
command is
`cd <wd> && set -a; . <envfile> 2>/dev/null; set +a; export K=V; trap '...' EXIT; codex exec ... --json '<task>' < /dev/null`,
so the `< /dev/null` redirect binds to the final `codex exec` simple command
through the VM login shell — exactly the fd it must.

Targeted tests green locally 2026-06-05:
`go test ./internal/devbox/backend/ ./pkg/mills/gates/ ./internal/hud/`
(spawn-command, provisioning, CLI-install, telemetry-persist, nonempty_diff).

## Riskiest assumption + kill-test

**Load-bearing assumption**: With the workspace+CLI provisioned
(`b4a0485d`), auth mounted (home-parity PASSED 2026-06-01), and the stdin
hang removed (`75a89996`), codex on the harvester-vm now runs its single
turn to completion and produces a **non-empty diff** for a trivial canary
task — so the `mr` stage opens an MR with a real `head_sha`, CI runs, and
`merge_when_pipeline_succeeds=true` lands it unattended.

**Kill test** (≤30 min; this IS the live A2 re-run — human-gated):
1. Confirm `/api/mills/capabilities` shows the harvester substrate row green
   and the operator constructs the harvester backend (no silent k8s
   fallback).
2. Flip prod `stage_substrate: {implement: harvester-vm, tests: harvester-vm}`
   via a narrowly-windowed GitOps PR (the 06-02 run used `gitops!208`,
   reverted by `gitops!209` ~13 min later). Keep the window to the single
   canary.
3. Enqueue one canary with a real, trivial diff
   (`loom mills pipelines canary --force`, or a `mills-canary-harvester-vm`
   backlog item appending a line to `testdata/mills-canary/heartbeat.md`).
4. Watch `implement → tests → mr → ci_watch → merge`. From `stage_results`
   + the GitLab MR, assert: implement `diff_patch` non-empty and
   `files_changed ≥ 1`; MR has a real `head_sha` + running `head_pipeline`;
   `merge_when_pipeline_succeeds=true`; terminal `merged`, `merged_via=auto`,
   `attempts_total` low.
5. Revert routing to `stage_substrate: {}` once the canary terminates.

**Pass criteria**: `autonomous_merges_24h` ticks 0 → ≥1 with a **non-empty**
diff via harvester-vm. (An empty-branch merge does NOT count — but the
`nonempty_diff` gate now makes that outcome an escalation, not a false
merge.)

**Failure mode if wrong**: if codex *still* exits with `turn_count=0` or a
zero diff, the gap is upstream of every fix above — most likely prompt/SpecDoc
delivery to codex over SSH, or codex declining to act on the canary task.
The `5fc4dd75` telemetry fix means this time the turn detail **will** be
captured, so the next debug is not blind. Reopen `.loom/119` against the VM
path with the captured turn output.

**Status**: **FAILED 2026-06-06** — but on a NEW blocker, downstream of all
four verified fixes (which worked). The live re-run ran (`gitops!230` flip →
canary `MILLS-CANARY-A2-20260606-011720` → `gitops!231` revert; window ≈6
min). codex had the CLI + workspace, did NOT hang on stdin, authenticated,
and **started a turn** — then the OpenAI API returned **HTTP 400: "The
'gpt-5.3-codex' model is not supported when using Codex with a ChatGPT
account."** This is **substrate-independent** (it failed at `plan_slice` on
k8s, before `implement` reached a VM): `buildAgentCommand` runs `codex exec`
with **no `--model`**, so codex uses its default (`gpt-5.3-codex`), which the
mounted ChatGPT-account OAuth doesn't grant. The `nonempty_diff` gate
escalated cleanly — **no junk MR** (MRIID:None, $0). Evidence + the small
next-fix options:
`.loom/local/handoffs/mills-harvester-vm-slice-a2-killtest-2026-06-05.md`.
**Next**: run the first canary on `gemini` (clean auth — recommended), or pin
a ChatGPT-supported `--model` on the codex exec.

> Per `.loom/126`, this kill-test gates the entire rest of the plan. Phases
> B, C, D do not start until it produces one real autonomous merge.

## Scope

**In**: code-level pre-flight verification (above); `.loom/126` A2 status
update recording readiness; this readiness memo + live procedure.

**Out**: the live flip/enqueue/watch itself (human-gated — outward-facing,
turns on autonomous-merge in prod); the curated base image (Phase B1); warm
pool (B2); substrate health fallback (B3).

## Decision points carried into the live run

- **Agent**: `.loom/126` OQ2 recommends an agent with known-good cluster
  auth. The 06-04 evidence shows **codex authenticated and started a turn**
  on the VM (`thread.started`/`turn.started`), which materially de-risks
  codex vs. the 06-02 unknown — codex is now a defensible first-canary
  choice, with gemini (end-to-end-clean per Slice 2d.5) as the fallback.
- **Window discipline**: per-item substrate routing is still unimplemented
  (`.loom/126` "What is NOT done" #1), so the flip is global — keep the
  window tight and revert immediately after the single canary, as 06-02 did.

## Done / handoff

- `.loom/126` A2 status updated to **READY FOR LIVE RE-RUN**.
- This memo committed; live procedure ready for a supervised moment.
- On a green live run: record the merge in a
  `.loom/local/handoffs/` memo, tick the north-star, and unblock A3
  (sustain 7 green) + the Phase B parallel track.
