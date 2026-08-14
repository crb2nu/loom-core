# Operational Fault Runbooks

Operator-facing procedures for recurring workspace faults in Loom Mills and
agent-spawned implementation runs. These runbooks focus on failures where the
code, plan, branch, or sandbox workspace is inconsistent with what the pipeline
expects.

Use this document with `docs/MILLS_RUNBOOK.md` and
`docs/mills-operational-guardrails.md`. The safe default for ambiguous workspace
faults is to pause new autonomous work, capture evidence, fix the underlying
class, and retry from the failed stage only after the branch and worktree are
known-good.

## Quick Triage

Capture these details before changing state:

```bash
loom mills pipelines list # active/non-terminal runs
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?state=terminal&limit=50" | \
  jq '.[] | select(.State == "escalated")'
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/capabilities" | jq
kubectl logs -n loom-mills deploy/loom-mills-operator --tail=300
git status --short --branch
git worktree list
```

For a specific pipeline run, collect:

```bash
curl -sf -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<run_id>" | jq
```

Record the backlog item ID, plan ID, slice name, worktree path, branch name, MR
IID, stage outcome, failing gate names, and the first failing reason. If the
stage produced a spawn ID, inspect the spawn output before rerunning; many
workspace faults are visible only in the agent stderr tail or telemetry summary.

## Safe State

Use this whenever the fault could affect more than one pipeline run:

1. Pause new autonomous work through GitOps:

   ```bash
   # In platform/gitops/k3s/mills/configmap-policy.yaml:
   #   enabled: false
   flux reconcile kustomization apps -n flux-system --with-source
   kubectl logs -n loom-mills deploy/loom-mills-operator --since=2m | grep "policy reloaded"
   ```

2. Let already-safe runs finish, but force-escalate any run that is operating on
   a suspect branch, stale plan, missing workspace, or unverifiable diff:

   ```bash
   curl -sf -X POST -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
     "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<run_id>/escalate"
   ```

3. Fix the fault family below.

4. Resume only after `autonomy_ready=true`, the failed stage has a clear human
   disposition, and the replacement run has a non-empty pushed diff or an
   intentional docs-only diff.

## Plan Resolution And Slice Drift

Use when an agent does no work because it cannot find the plan, reads a stale
`.loom` mirror, thinks a slice is already completed, or implements files that do
not match the current slice.

Common symptoms:

| Signal | Likely cause |
|---|---|
| Spawn says the plan or slice is missing, but HUD shows it queued | Agent used stale checked-out `.loom` files instead of the plan store |
| Retry produces an empty diff after the first attempt changed files | Plan-slice status from the discarded attempt was treated as current |
| Scope gate reports no slices for a plan-linked backlog item | Backlog projection omitted slice metadata |
| Agent edits files outside the slice, or no files at all | Slice files are stale, fictional, or not hydrated into the prompt |

Diagnosis:

```bash
git status --short --branch
git diff --name-only origin/main...HEAD
```

Then use the `mcp-agent-context` tool `agent_plan_get` with
`{"plan_id":"<plan_id>"}`. The shared store is canonical; do not rely on local
`.loom` mirrors.

Remediation:

1. Confirm the plan and slice in the shared store with `agent_plan_get`.
2. Compare the live slice files with the changed files on the branch.
3. If the plan store is correct but the backlog item or retry prompt is stale,
   escalate the run and requeue a fresh item from the canonical plan.
4. If the plan store itself is wrong, patch or abandon the plan in the store
   before rerunning. Do not hand-edit a `.loom` mirror as the source of truth.
5. On retry, require the agent to redo the implementation from the canonical
   plan and to treat prior discarded attempt state as stale.

Verification:

```bash
git diff --name-only origin/main...HEAD
```

Confirm the same file list against `agent_plan_get{"plan_id":"<plan_id>"}`.

The branch diff must match the current slice, and the stage retry reason must
name the first failing gate so the operator can distinguish root cause from the
follow-on empty diff.

## Dirty Worktree And Branch Hygiene

Use when a worker starts in a dirty worktree, the wrong branch is checked out,
linked worktrees accumulate, or unrelated user changes risk being staged.

Common symptoms:

| Signal | Likely cause |
|---|---|
| `git status --short --branch` shows unrelated modified files | Pre-existing user or generated changes were treated as active work |
| `git worktree add` fails or creates duplicate trees | Old linked worktrees were not inspected or cleaned |
| MR includes files unrelated to the backlog item | Agent staged everything instead of only the active slice |
| Cleanup cannot remove a worktree | Active process, uncommitted work, or missing worktree metadata |

Diagnosis:

```bash
git status --short --branch
git worktree list
git diff --name-only
git diff --cached --name-only
```

Remediation:

1. Treat pre-existing changes as baseline context, not as an automatic blocker.
2. Stage and commit only files intentionally changed for the active task.
3. If unrelated changes are in files needed for the task, inspect them and work
   with them. Escalate only when they make the requested change ambiguous.
4. Before allocating another multi-file worktree, inspect existing trees with
   `git worktree list`.
5. If a worktree must be removed, first salvage real source changes into a
   scoped WIP commit or handoff. Never run destructive cleanup against unknown
   dirty state.

Verification:

```bash
git diff --name-only origin/main...HEAD
git status --short
```

Only the intended slice files and required documentation files should appear in
the final diff.

## Empty Diff Or Zero-Commit MR

Use when `implement` completes without source changes, `nonempty_diff` fails, or
the MR stage would open an empty branch.

Common symptoms:

| Signal | Likely cause |
|---|---|
| `nonempty_diff` fails with zero changed files | Agent exited early, read stale plan state, or workspace was missing |
| MR has `head_sha=null` or no commits | Branch was pushed before any commit, or the MR stage ran after an empty implement |
| Retry also produces an empty diff | Retry prompt lacks first failure context, or prior slice state is stale |
| Agent transcript has no tool calls or no final patch summary | Agent CLI failed before doing work |

Diagnosis:

```bash
git log --oneline origin/main..HEAD
git diff --stat origin/main...HEAD
git diff --name-only origin/main...HEAD
```

Inspect the pipeline detail and spawn output for the first failure. The first
failing gate usually explains the real defect; later empty diffs are often
secondary.

Remediation:

1. Keep or force the run in an escalated state; do not allow a zero-commit MR to
   continue.
2. Fix the earliest concrete cause: missing workspace, stale plan, prompt gap,
   auth failure, or agent CLI failure.
3. Re-run `implement` only after the worker prompt includes retry context and
   states that the previous attempt was discarded.
4. Require a conventional commit and a pushed branch before `mr`.

Verification:

```bash
git log --oneline origin/main..HEAD
git diff --stat origin/main...HEAD
git push -u origin HEAD
```

The branch must contain at least one intentional commit and a non-empty diff
unless the backlog item is explicitly a no-op documentation update.

## Spawn Workspace Bootstrap Failures

Use when the agent sandbox starts but the repository, branch, or agent CLI is
missing. This includes K8s spawn pods and `harvester-vm` sandboxes.

Common symptoms:

| Signal | Likely cause |
|---|---|
| `cd: /workspace/services/loom-core: No such file or directory` | Workspace clone/provisioning did not run or failed |
| `codex: command not found` or equivalent | Agent CLI was not installed in the sandbox image or bootstrap step |
| Spawn exits in under one second with no diff | Bootstrap failed before the agent could operate |
| VM reaches running state but SSH never succeeds | Guest agent, cloud-init, network, key, or user provisioning failure |

Diagnosis:

```bash
kubectl logs -n loom-mills deploy/loom-mills-operator --tail=300 | grep -E 'spawn|workspace|codex|harvester'
curl -sf -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<run_id>" | jq '.stages[] | select(.spawn_id)'
```

For `harvester-vm`, also check the VM substrate status through the devbox or
cluster tooling used by the deployment.

Remediation:

1. Confirm the requested `WorkDir`, branch, base branch, Git remote, and token.
2. Confirm bootstrap cloned the repository and checked out the requested branch.
3. Confirm the agent CLI install command is present and idempotent for the
   substrate.
4. For `harvester-vm`, verify the `agent` user, `/home/agent`, SSH key,
   qemu-guest-agent, and mounted auth files before rerunning the Mills stage.
5. If the substrate cannot be trusted quickly, pause Mills or route the stage
   back to the known-good substrate through policy.

Verification:

```bash
git -C /workspace/services/loom-core status --short --branch
command -v codex
test -d /workspace/services/loom-core/.git
```

The replacement spawn should run long enough to execute repository commands and
should report a concrete patch, test result, or explicit operator-facing error.

## Agent CLI, Stdin, And Auth Failures

Use when the workspace exists but the agent process hangs, exits before work, or
cannot authenticate.

Common symptoms:

| Signal | Likely cause |
|---|---|
| Agent logs `Reading additional input from stdin...` and then hangs or exits | CLI inspected an open stdin pipe instead of running the provided prompt |
| Agent exits with auth or model-access errors | Missing mounted auth file, wrong home directory, expired token, or wrong user |
| Telemetry shows `turn_count=0` | Process failed before starting a useful turn |
| Session ID is empty or output summary is missing | Spawn session registration or telemetry persistence failed |

Diagnosis:

```bash
kubectl logs -n loom-mills deploy/loom-mills-operator --tail=300 | grep -E 'turn_count|stdin|auth|session|spawn'
```

If you can exec into the sandbox safely:

```bash
id
printf '%s\n' "$HOME"
ls -la "$HOME/.codex" "$HOME/.codex.auth" 2>/dev/null || true
command -v codex
```

Remediation:

1. Ensure the spawn command redirects stdin from `/dev/null` when the prompt is
   already passed as an argument.
2. Confirm the sandbox user and home directory match the mounted agent auth
   paths. For Loom-managed sandboxes, the expected user is `agent` with
   `/home/agent`.
3. Restore or rotate the affected auth secret through GitOps when possible.
4. Confirm spawn session registration errors are logged; missing telemetry is an
   incident artifact, not proof the agent did nothing.
5. Retry only after a minimal agent command can authenticate and complete.

Verification:

```bash
codex --version
test -r "$HOME/.codex/auth.json" || test -r "$HOME/.codex.auth/auth.json"
```

The next spawn should have a nonzero turn count, a populated output summary, and
either a patch or a clear failure reason.

## Scope, Path Policy, And Fictional Paths

Use when a stage changes real files but gates reject the diff as out of scope, or
when the council proposes paths that do not exist in the repository.

Common symptoms:

| Signal | Likely cause |
|---|---|
| Scope gate fails on absolute `/workspace/...` paths | Gate compared absolute spawn paths to repo-relative slice globs |
| Council emits `pkg/planning/` or other nonexistent package paths | Proposal was not grounded in the repo layout |
| New-package work is dropped before implementation | Guard treated legitimate new directories as fictional |
| Path-policy passes but review sees unrelated files | Slice glob is too broad or stale |

Diagnosis:

```bash
git diff --name-only origin/main...HEAD
find . -maxdepth 3 -type d | sort | sed -n '1,160p'
```

Compare every changed file with the canonical plan slice. For absolute spawn
paths, reduce to the repo-relative suffix before judging scope.

Remediation:

1. If the changed files are correct and only the gate path format is wrong, fix
   the gate or normalize the paths before retrying.
2. If the slice includes fictional paths, send the item back to council or patch
   the plan slice to real repository paths.
3. If the slice creates a legitimate new directory under an existing parent,
   keep the slice observable as speculative rather than silently dropping it.
4. Keep broad globs rare. Prefer exact files or narrow package paths.

Verification:

```bash
git diff --name-only origin/main...HEAD
```

Confirm the same file list against `agent_plan_get{"plan_id":"<plan_id>"}`.

The scope gate should pass for repo-relative suffixes and fail only on truly
unrelated files.

## Docs Guardrail And CI Documentation Failures

Use when CI fails `guardrails:docs-cli` after a code-facing change.

Common symptoms:

| Signal | Likely cause |
|---|---|
| CI says code-facing changes detected without doc updates | Branch touched `cmd/`, `internal/`, `pkg/`, scripts, CI, or Go module files without docs |
| Agent says tests passed but MR is red | Local run skipped the docs guardrail |
| Commit includes `[skip-docs-check]` | Bypass was used instead of documenting the change |

Diagnosis:

```bash
scripts/ci/check_docs_guardrails.sh
git diff --name-only origin/main...HEAD
```

Remediation:

1. Add a concise `CHANGELOG.md` entry under `## [Unreleased]` in the appropriate
   Keep a Changelog section.
2. If the change needs user or operator documentation, update the relevant file
   under `docs/` as well.
3. Do not use `[skip-docs-check]` for normal code changes.
4. Re-run the guardrail locally before pushing.

Verification:

```bash
scripts/ci/check_docs_guardrails.sh
```

The guardrail should pass before the MR stage waits on CI.

## Branch Push And MR Readiness

Use when the pipeline reaches MR creation but GitLab sees an empty or missing
branch.

Common symptoms:

| Signal | Likely cause |
|---|---|
| MR stage hangs or opens an empty MR | Branch was never pushed after commit |
| GitLab reports no source branch | Local branch has no upstream or push failed |
| Pipeline has commits locally but no MR diff | Commit exists only in the workspace |

Diagnosis:

```bash
git status --short --branch
git log --oneline origin/main..HEAD
git ls-remote --heads origin "$(git branch --show-current)"
```

Remediation:

1. Confirm the branch contains the intended commit range.
2. Push the current branch with upstream tracking:

   ```bash
   git push -u origin HEAD
   ```

3. If the push fails, fix credentials or branch protection before rerunning the
   MR stage.
4. Do not manually create an MR for a branch whose diff has not been verified.

Verification:

```bash
git ls-remote --heads origin "$(git branch --show-current)"
git diff --stat origin/main...HEAD
```

GitLab must see the branch and its commits before the downstream MR stage starts.

## Post-Incident Checklist

Before closing the incident:

- Link the failed run, MR, branch, and backlog item in the incident notes.
- Record the first failing stage and gate, not just the final escalation reason.
- Confirm whether the fault was isolated to one worktree or affected the shared
  substrate, prompt, gate, or policy.
- Add or update a regression test when the fix changes code.
- Update `CHANGELOG.md` and any operator docs touched by the remediation.
- Resume Mills only after a fresh status check shows `autonomy_ready=true`.
