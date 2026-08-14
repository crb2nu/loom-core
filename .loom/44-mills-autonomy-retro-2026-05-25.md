# Mills Autonomy Round — Session Retrospective (2026-05-24 → 2026-05-25)

Closes out the work tracked in `.loom/43-plan-mills-autonomy-2026-05-24.md`.
Captures what shipped, what verified live, the structural finding that
blocks end-to-end auto-merge, and recommended next moves.

## TL;DR

- **10 MRs shipped** across `services/loom-core` (5) and `platform/gitops` (5).
- **8 slices of the autonomy round + 2 follow-up fixes** delivered.
- All shipped code verified clean on the unit-test and harness levels.
- **Live end-to-end auto-merge did NOT happen.** Three canary cycles
  produced three empty MRs (`!518`, `!520`, `!522`) because the spawn
  agent isn't actually modifying files when invoked through the HUD
  spawn API for the `implement` stage.
- The autonomy infrastructure is **sound**; the agent-execution path
  needs its own investigation in a follow-on session.

## What shipped

### loom-core MRs

| MR | Title | Status |
|---|---|---|
| `!517` | `feat(mills): autonomy round — 8 slices to lift merged-MRs/day no-touch` | merged 2026-05-24T22:11Z |
| `!519` | `fix(mills): push source branch before opening MR` | merged 2026-05-25T07:38Z |
| `!521` | `fix(mills): instruct implement spawn to push branch` | merged 2026-05-25T11:06Z |
| `!518`, `!520`, `!522` | Empty canary MRs from each verification cycle | closed |

### platform/gitops MRs

| MR | Title | Result |
|---|---|---|
| `!172` | `ops(mills): policy knobs for autonomy round (intake, notify, retry)` | merged (all disabled by default) |
| `!173` | `ops(mills): enable canary GC to clear 56 stuck escalated canaries` | merged; **swept 56 items in 1 tick** |
| `!174` | `ops(mills): escalation_auto_retry_cap=2 to enable Slice 3d` | merged; hot-reloaded |
| `!175` | `ops(mills): per-label auto_merge for mills-canary (step 4)` | merged |
| `!176` | `ops(mills): enable GitLab issue importer (step 6)` | merged; importer started after restart |

### Code paths now live in cluster

Operator image `20260525-112914` running on pod
`loom-mills-operator-5c477bf89b-l6sfj`. Active behaviors:

- **Slice 2c (transient retry classification)**: every stage error is
  now classified `transient / transient_quota / infra / code`.
  Transient errors get free retries up to `TransientRetryCap=5`.
  Verified in live log: ci_watch timeout correctly classified as
  `class=code` (not transient) and consumed attempt budget.
- **Slice 2e (buildah eviction race)**: `evictPod` waits for pod
  termination before recreate; 409 AlreadyExists gets one re-evict
  retry. Verified clean across three canary cycles — no buildah
  collisions logged.
- **Slice 2a (auto-merge wiring)**: `CreateMRRequest.AutoMerge` flows
  through to `merge_when_pipeline_succeeds`. Per-label override for
  `mills-canary` is set to `auto_merge: true`. **Could not verify live
  because the MRs are empty** — GitLab clears MWPS on a 0-commit branch.
- **Slice 3a (notify webhook)**: code-complete, **inert** —
  `policy.notify.webhook_url` left empty pending a real URL.
- **Slice 3b (tick-on-merge)**: `Scheduler.KickNow()` wired in the
  OnMerged chain. Hasn't fired in practice because no merges have
  happened.
- **Slice 3d (bounded escalation auto-retry)**:
  `EscalationAutoRetryCap: 2` set in policy. Hot-reloads via fsnotify.
  Verified inactive in practice because escalations were `class=code`,
  which correctly skips auto-retry.
- **Canary GC**: ran on first sweep — deleted 56 stuck canaries. Queue
  is now clean of stale escalated items.
- **GitLab issue importer**: polling project 47 every 5min for
  `mills-eligible` issues (currently zero).

## The structural finding that blocks end-to-end

**Three canary cycles, three empty MRs.** Each one:

1. Pipeline reached `mr` stage on attempt #1.
2. Mills called GitLab `CreateMR`. GitLab returned a real MR iid.
3. **But the branch had no commits on origin** — `head_sha: ""`,
   `merge_status: cannot_be_merged`, `head_pipeline: none`.
4. `ci_watch` polled for 30min × 3 attempts → escalated with
   `pipeline poll timed out after 30m0s`.

**Why**: the `implement` spawn (codex) ran 1 turn and returned
`status=completed` with **zero file modifications, zero commits, zero
push**. Verified directly from stage_results.artifacts:

```
spawn_id: spawn-codex-09424d22c913
turn_count: 1
outcome: success
diff_patch: (absent)
files_changed: (absent)
commit_messages: (absent)
```

The kill-test database confirmed this is **pre-existing**: all 27
historical successful `implement` stages also had `no_diff` /
`no_files`. The empty-MR pathology was masked before today because no
canary ever reached the `mr` stage — they all died at `tests` or
`plan_slice`. Slice 2c + 2e fixed those upstream stages and finally
exposed the empty-implement gap.

**Two parallel attempts to close the gap, both unsuccessful**:

1. **!519 (operator-side push)** — added `pipeline.BranchPusher`
   interface; `runMR` pushes the worktree before `CreateMR`. Inert in
   the single-repo flow because `jc.Run.WorktreePath` is never
   populated by the single-repo `SpawnWorker` path (only the cross-repo
   integrator allocates worktrees via Mills' `WorktreeAllocator`).
2. **!521 (prompt-side push)** — extended the implement prompt with an
   explicit `git push -u origin HEAD` instruction. Didn't help because
   the agent never reaches a state where it has anything to push — it
   isn't modifying files in the first place.

## Where the gap actually lives

**Root cause documented in `.loom/119-diagnosis-mills-spawn-no-diff-2026-05-25.md`**
(landed on main while this retro was being written; matches the live
findings 1:1):

> The Mills operator and the spawn pod do not share a filesystem in
> production. The operator allocates a worktree at
> `<host>/.worktrees/<branch>` and passes its absolute path in
> `pipeline.SpawnRequest.WorkingDir`. The HUD spawn API's wire format
> (`hudSpawnRequestBody`) silently drops that field. The HUD-side
> orchestrator creates the pod with `WorkDir` set from the canonical
> project path. The K8s devbox backend, in production's `git-clone`
> sync mode, hydrates the pod's emptyDir via a
> `git clone --depth 1 <url>` init container with **no `-b <branch>`
> flag**. The agent therefore runs in a pod-local, fresh, default-
> branch, shallow clone that has no connection to `req.Branch` or to
> the operator's worktree allocator path. When the spawn terminates,
> the operator's `attachGitContext` reads `git diff baseBranch...HEAD`
> from its own worktree dir — which was never touched by anyone — and
> records an empty diff.

Confirms: my prompt-push fix (`!521`) could never have worked. The
spawn pod isn't pushing because it isn't even on the right branch —
it's on the default branch of a fresh shallow clone. Even if the
agent successfully commits, those commits live in a pod-local
ephemeral checkout that's destroyed when the spawn ends.

## Post-merge verification log (2026-05-25 afternoon)

Continuing the loop after the retro shipped revealed *three* coupled
spawn-execution bugs, not the single one the diagnosis predicted. Each
became visible only after the previous one was fixed:

1. **Init container never checked out `req.Branch`** — MR `!525`
   (`ab6f8446`). Canary `PIPE-MILLS-CANARY-20260525-140741`
   verified: spawn pod ends on `feat/MILLS-CANARY-…` instead of
   `main`.
2. **Workspace cloned as root:0 0644, runtime is uid 1000** — MR
   `!527` (`5742ae07`). Live evidence: `echo foo >> testdata/mills-
   canary/heartbeat.md` ⇒ `Permission denied (uid 1000 agent)`.
   Fix added `umask 002` + `chown -R 1000:1000 <dest>` to the
   git-clone init script.
3. **Codex sandbox + approval policy** — MR `!531` (`46418c9f`)
   moved the flag to `--sandbox danger-full-access`, but the
   injected `~/.codex/config.toml` still set `approval =
   "auto-edit"` so codex paused for human approval on every shell
   command (`git add/commit/push`). MR `!535` switches to
   `--dangerously-bypass-approvals-and-sandbox` — codex's
   documented "externally-sandboxed environment" flag and the
   codex equivalent of claude's `--dangerously-skip-permissions`,
   which Mills has been using all along on the claude-code path.

Pattern: each fix is necessary but not sufficient; the next gap
only becomes legible after the previous one no longer blocks the
agent. The right way to discover the rest was running canaries and
reading the failure mode each time.

4. **Codex JSONL was being silently dropped by the parser** — MR
   `!543` (`1d18cc67`). Diagnostic-only patch: HUD codex parser now
   logs `item.started` (command + tool), `command_execution` (with
   exit code + stderr_tail), `file_change` (path + kind),
   `turn.failed`, and `error` events at INFO/WARN/ERROR. Without
   this, every spawn pod was reaped immediately on completion and
   there was no way to see what codex actually tried to do.
5. **OPENAI_API_KEY=PLACEHOLDER env var overrode auth.json** — MR
   `!545` (`c4aff279`). The diagnostic log from `!543` made the
   actual root cause visible:

     ```
     level=ERROR msg="codex error event"
       message="unexpected status 401 Unauthorized: Incorrect API
       key provided: PLACEHOLDER. You can find your API key at
       https://platform.openai.com/account/api-keys."
     ```

   Every codex LLM call had been failing at the auth gate for the
   entire history of the canary, but the spawn marker still went
   `status=completed` because codex's CLI exited cleanly after its
   5-retry loop. Root cause: codex CLI treats `OPENAI_API_KEY` env
   as an override that takes precedence over `~/.codex/auth.json`.
   The cluster's `cluster-agent-api-keys` secret stores
   `OPENAI_API_KEY` as the literal string "PLACEHOLDER" because
   operators use OAuth — but `agentSecretEnvVars` wired the env var
   anyway. Fix removes the OPENAI_API_KEY / CODEX_API_KEY env
   wiring for codex; falls back to the mounted auth.json.

Verified live in canary `PIPE-MILLS-CANARY-20260526-015233`: the
codex error mode changed from `401 PLACEHOLDER` to
`"Your access token could not be refreshed because your refresh
token was already used. Please log out and sign in again."` —
proof that codex is now reading auth.json instead of the env
override.

The remaining gap to a green end-to-end auto-merge is **operational,
not code**: the OAuth tokens in `cluster-agent-auth.codex-auth-json`
are stale. Refreshing requires `codex login` on a workstation with
the Mills service account → re-encrypt the resulting auth.json into
the SOPS-managed secret. Outside the scope of this code chain.

## What's worth doing next

Per `.loom/119-…` §1, the coupled fix is:
1. HUD spawn API stops dropping `WorkingDir` AND `Branch`.
2. The git-clone init container fetches the right branch (`-b
   <branch>`) and treats it as the working ref.
3. The spawn pod mounts the operator-allocated worktree directly
   (the original Mills v1 NFS design) — OR the init container
   checks out `<branch>` and the pod commits/pushes back to origin
   from inside.

That fix is tracked separately (memory: "Mills spawn no-diff root
cause" references MR `!523`).

Once `!523` (or its successor) lands, the autonomy round's plumbing
should finally exercise:

- The spawn agent actually modifies + commits + pushes.
- `mr` stage opens a real MR with `head_sha` set.
- Slice 2a's `auto_merge: true` on `mills-canary` flips
  `merge_when_pipeline_succeeds=true` on GitLab.
- CI pipeline runs against real commits.
- GitLab auto-merges on green.
- `OnMerged` chain fires → Slice 3b kicks scheduler within ~1s →
  next backlog item dispatches.
- `auto_merge_rate` in `mills_pipeline_kpi_snapshots` rises above
  zero for the first time ever.

## Files / artifacts worth keeping

- `.loom/43-plan-mills-autonomy-2026-05-24.md` — full plan + slice
  status table.
- `.loom/local/handoffs/mills-autonomy-killtest-2026-05-24.md` — 14d
  telemetry pull that reshaped the original plan.
- `.loom/local/handoffs/mills-autonomy-session-2026-05-24.md` — flip
  plan with recommended ordering.
- This file (`.loom/44-…`).

## Honest assessment

The autonomy round shipped real, well-tested infrastructure with no
regressions and clear behavior verification under the new operator
image. Every Slice 2/3 component does what it's supposed to do at the
harness level. **What it can't do is overcome a pre-existing gap in
the spawn agent's actual execution** — and that gap was invisible
before today because upstream failures masked it.

This was a kill-test of a different sort: by repairing the obvious
gaps, we made the next gap legible.
