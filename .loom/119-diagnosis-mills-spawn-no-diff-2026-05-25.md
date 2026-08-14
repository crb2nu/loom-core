# Mills spawn "implement stage produces no diff" — diagnosis (2026-05-25)

**Status:** Research-only. Root-cause analysis + minimal-fix proposal for the
dominant failure mode of the Mills `implement` stage: spawns reach
`status=completed` with `turn_count ≥ 1`, but `files_changed=0`,
`diff_patch=∅`, `commit_messages=nil`, and no branch ever appears on
origin. Every `implement` run that ever "succeeded" in the kill-test DB
(27/27 of the historical sample) shows this pattern.

**Audience:** the implementer slice that will land Fix A + Fix B without
re-reading this thread.

**Prior diagnosis (different bug, partly fixed):**
`.loom/118-diagnosis-mills-spawn-pod-not-found-2026-05-16.md` on branch
`docs/mills-spawn-diagnosis` proposed `ManagedByOverride` for the
reconciler label mismatch. That fix landed at
`internal/hud/spawn.go:551` but the reconciler is still poisoning state
records — see §4. That issue is downstream of this one; do not block on
it.

---

## 1. Root cause hypothesis

**Top-1 (high confidence):** the Mills operator and the spawn pod do not
share a filesystem in production. The operator allocates a worktree at
`<host>/.worktrees/<branch>` and passes its absolute path in
`pipeline.SpawnRequest.WorkingDir`. The HUD spawn API's wire format
(`hudSpawnRequestBody`) silently drops that field. The HUD-side
orchestrator creates the pod with `WorkDir` set from the canonical
project path. The K8s devbox backend, in production's `git-clone` sync
mode, hydrates the pod's emptyDir via a `git clone --depth 1 <url>`
init container with **no `-b <branch>`** flag. The agent therefore runs
in a pod-local, fresh, default-branch, shallow clone that has no
connection to `req.Branch` or to the operator's worktree allocator
path. When the spawn terminates, the operator's `attachGitContext`
reads `git diff baseBranch...HEAD` from its own worktree dir — which
was never touched by anyone — and records an empty diff.

This produces the observed symptom regardless of whether the agent
actually wrote files in the pod: even if it did, those changes never
land in the path the operator inspects.

**Top-2 (contributing, secondary):** the prompt template
(`cmd/loom-mills-operator/main.go:865`) tells the agent to "Write code +
tests in the allocated worktree" and to push with `git push -u origin
HEAD`. From inside a fresh default-branch clone, both instructions are
nonsensical: there is no allocated worktree visible to the agent, and
pushing the default branch is either blocked (protected branch) or
useless (no commits ahead). A reasonable agent — codex included — will
look at the prompt, look at the pod, conclude "I don't have a concrete
artifact to modify" and exit after one turn. This explains `turn_count
= 1, cost = $0.0X` with zero files touched.

**Top-3 (ruled out as primary):** prompt quality / agent capability. The
prior session attempted a prompt update at commit `55bb9841` to nudge
the agent toward pushing. The fix is necessary but insufficient — the
agent has nothing to push because (a) it's on the wrong branch and
(b) the prompt's referent "worktree" does not exist in the pod.

---

## 2. Evidence trail

### 2.1 Mills sends WorkingDir; HUD wire format drops it

`pkg/mills/pipeline/dispatcher.go:225-241` — Mills constructs the request:

```go
req := SpawnRequest{
    Prompt:          prompt,
    WorkingDir:      jc.Run.WorktreePath,   // <host>/.worktrees/<branch>
    Model:           w.Model,
    …
    Project:         project,
    Branch:          branch,
    BaseBranch:      w.BaseBranch,
    Namespace:       namespace,
}
```

`pkg/mills/clients/spawn.go:132-148` — the HTTP body type only carries
a subset:

```go
type hudSpawnRequestBody struct {
    AgentType       string ...
    Namespace       string ...
    Branch          string ...
    BaseBranch      string ...
    TaskDescription string ...
    Project         string ...
    TimeoutMinutes  int    ...
    MaxCostUSD      float64 ...
    MaxTurns        int    ...
    ParentSessionID string ...
    Metadata        map[string]string ...
    // ← no WorkingDir field
}
```

`internal/spawn/types.go:78-124` — and the HUD's own `spawn.Request`
type matches: no `WorkingDir`, only `Branch`, `BaseBranch`, `Project`.

So Mills' allocator path is local information only. The pod never sees
it.

### 2.2 The pod's WorkDir is canonical, not the worktree

`internal/hud/spawn.go:478-543`:

```go
projectDir, projectRel, resolveErr := resolveProjectPath(o.workspaceRoot, req.Project)
…
podProjectDir := "/workspace/" + projectRel    // e.g. /workspace/services/loom-core
…
startResult, err := o.backend.Start(ctx, backend.StartOpts{
    Name:    "spawn-" + spawnID,
    ImageTag: buildResult.ImageTag,
    WorkDir:  podProjectDir,                   // canonical project path
    …
})
```

The pod's working directory is always the canonical project path. The
HUD never reads any worktree-style field from `req`.

### 2.3 Sync mode and clone behavior

`internal/devbox/backend/k8s_workspace.go:16-36`:

```go
switch {
case k.syncMode == "tar-pipe":
    plan.volume = emptyDirWorkspaceVolume(emptyDirSizeLimit)
case k.gitEnabled():
    plan.volume = emptyDirWorkspaceVolume(emptyDirSizeLimit)
    plan.initContainers = []corev1.Container{k.gitCloneInitContainer(cloneTarget)}
default:
    plan.volume = pvcWorkspaceVolume(k.workspacePVC)
}
```

Three modes:
- `tar-pipe` — emptyDir hydrated by tar-pipe after pod start.
- `git-clone` — emptyDir hydrated by init container (the production path).
- default — NFS PVC (would be shared with operator).

`internal/devbox/backend/k8s_objects.go:244-255` — the clone script:

```go
cloneScript := fmt.Sprintf(
    `set -e
mkdir -p "$(dirname %q)"
git clone --depth 1 "%s://token:${GIT_TOKEN}@%s" %q
echo "git-clone: cloned %s into %s"`,
    cloneDest,
    scheme,
    hostAndPath,
    cloneDest,
    projectName,
    cloneDest,
)
```

Note: `--depth 1`, no `-b <branch>`, no checkout step. Pod ends up on
the default branch.

Negative search:

```
$ grep -rn "git checkout\|git switch" internal/devbox/backend/ \
    --include="*.go" | grep -v _test.go
(empty)
```

No production code anywhere in the devbox backend checks out a branch
after clone.

### 2.4 Production sync mode is git-clone

```
$ KUBECONFIG=$HOME/.kube/k3s.yaml kubectl -n loom-hub get deploy mobile-hud -o yaml \
    | grep -A1 SPAWN_
        - name: SPAWN_SYNC_MODE
          value: git-clone
        - name: SPAWN_GIT_BASE_URL
          value: http://192.168.50.218/services
        - name: SPAWN_GIT_SECRET
          value: gitlab-creds
```

Captured 2026-05-25. The cluster has been in git-clone mode since this
config was last changed; check the manifest history for the cutover
date if needed.

### 2.5 attachGitContext reads operator-local worktree

`pkg/mills/clients/spawn.go:473-492`:

```go
func (c *HUDSpawnClient) attachGitContext(ctx context.Context, resp *pipeline.SpawnResponse, workingDir, baseBranch string) {
    …
    diff := captureGitDiff(ctx, c.cfg.GitRunner, workingDir, baseBranch, c.cfg.MaxDiffBytes)
    commits := captureGitCommitMessages(ctx, c.cfg.GitRunner, workingDir, baseBranch, c.cfg.MaxCommitMessagesBytes)
    resp.DiffPatch = diff
    resp.CommitMessages = commits
}
```

`captureGitDiff` runs `git diff baseBranch...HEAD` in `workingDir`,
which is the operator's `<host>/.worktrees/<branch>` allocator path.
Nothing about that path ever receives the agent's work. Even if the
agent pushed to `origin/<branch>`, the operator's worktree has no
`origin/<branch>` ref until something fetches it — and nothing in
`attachGitContext` does.

### 2.6 Operator-recorded outcomes

From the prior session's verbal report against the kill-test DB
(`pkg/mills/store/stage_results`):

> Implement spawn artifacts: `agent_id=spawn-codex-09424d22c913`,
> `turn_count=1`, no diff_patch, no files_changed, no commit_messages.

Mills' own `stage_results` is the authoritative record (the HUD's
`loom-spawn-state` ConfigMap is being poisoned by §4's secondary
reconciler bug). The 27 historical "implement" successes the prior
session enumerated all share this exact shape: completed status, ≥1
turn, zero diff.

---

## 3. Fix proposal

Two coupled changes for the smallest workable fix. Both required; do
not ship in isolation.

### Fix A — init container checks out req.Branch (or creates it from base)

Change `internal/devbox/backend/k8s_objects.go:220-286`:

1. Drop `--depth 1` (full history is needed for diff capture and to
   make `git push` safe).
2. After clone, `cd` into the dest and either check out the existing
   remote branch or create a new branch off the base branch.
3. Wire `SPAWN_BRANCH` and `SPAWN_BASE_BRANCH` env vars into the init
   container from the orchestrator. New fields on `backend.StartOpts`
   to carry them; the orchestrator (`internal/hud/spawn.go`) populates
   them from `req.Branch` / `req.BaseBranch`.

Sketch:

```go
cloneScript := fmt.Sprintf(
    `set -e
mkdir -p "$(dirname %q)"
git clone "%s://token:${GIT_TOKEN}@%s" %q
cd %q
if [ -n "${SPAWN_BRANCH:-}" ]; then
  if git ls-remote --exit-code --heads origin "${SPAWN_BRANCH}" >/dev/null 2>&1; then
    git checkout "${SPAWN_BRANCH}"
  else
    git checkout -b "${SPAWN_BRANCH}" "origin/${SPAWN_BASE_BRANCH:-main}"
  fi
fi
echo "git-clone: ready %s on $(git rev-parse --abbrev-ref HEAD)"`,
    cloneDest, scheme, hostAndPath, cloneDest, cloneDest, projectName,
)
```

Test: `internal/devbox/backend/k8s_objects_test.go` — new case pins the
generated init-container command contains the checkout block with the
expected env-var references.

### Fix B — Mills reads diff from origin

Change `pkg/mills/clients/spawn.go:473-492` (`attachGitContext`):

1. Before diff capture, `git fetch origin <branch>` into the operator's
   worktree.
2. Replace `baseBranch...HEAD` with `baseBranch...origin/<branch>`.

Sketch:

```go
func (c *HUDSpawnClient) attachGitContext(ctx context.Context, resp *pipeline.SpawnResponse, workingDir, baseBranch, branch string) {
    if c == nil || resp == nil { return }
    if workingDir == "" || baseBranch == "" || branch == "" { return }
    if c.cfg.GitRunner == nil { return }
    _, _, _, _ = c.cfg.GitRunner.Run(ctx, workingDir, "git", "fetch", "origin", branch)
    diff := captureGitDiff(ctx, c.cfg.GitRunner, workingDir, baseBranch, "origin/"+branch, c.cfg.MaxDiffBytes)
    commits := captureGitCommitMessages(ctx, c.cfg.GitRunner, workingDir, baseBranch, "origin/"+branch, c.cfg.MaxCommitMessagesBytes)
    resp.DiffPatch = diff
    resp.CommitMessages = commits
}
```

`captureGitDiff` / `captureGitCommitMessages` get a third argument for
the head ref.

`HUDSpawnClient.pollSpawn` needs to thread `req.Branch` through the
existing `workingDir, baseBranch` channel — add a `branch` parameter
or stuff into `SpawnResponse` and read it back.

Test: `pkg/mills/clients/spawn_test.go` — new case pins `git fetch`
is invoked before `git diff` and uses the per-branch head ref.

### Optional Fix C — switch to NFS sync mode

Setting `SPAWN_SYNC_MODE=nfs` (or unsetting, since PVC is the default
at `internal/devbox/backend/k8s.go:103-105`) shares the filesystem
between operator and pod and removes the entire mismatch in one step.
Larger blast radius (NFS reliability, multi-pod concurrency, file
permissions) — defer until Fix A+B prove the loop works end-to-end.

### Why not "just fix the prompt"

The prompt was tweaked in commit `55bb9841` to instruct
`git push -u origin HEAD`. It did not change behavior because the pod
is on the wrong branch and pushing the default branch fails (protected)
or no-ops (no commits). The prompt is downstream of the workspace
problem. Land Fix A first.

---

## 4. Secondary issue (do not block on this slice)

All 9 entries currently in `loom-spawn-state` (devbox namespace) show
`status=failed, error="pod not found during reconciliation"` with all
request fields blanked:

```
$ kubectl -n devbox get cm loom-spawn-state -o jsonpath='{.data}' \
    | jq 'keys'
[
  "spawn-09424d22c913", "spawn-1f5d23ddf892", "spawn-42a6056984a2",
  "spawn-4273ed18877b", "spawn-960800bcb582", "spawn-ca645452cf74",
  "spawn-dc4dece6d755", "spawn-e87c89dad893", "spawn-f51dd33a5de4"
]
```

The 2026-05-16 fix (`ManagedByOverride` at `internal/hud/spawn.go:551`)
is present in main but the reconciler is still wiping records. Likely
candidates:
- A second pod-creation path (build, exec, …) labels with
  `mcp-devbox` and slips past the override.
- `completeSpawn` writes terminal status without clearing the
  poisoned `state.Error`, so subsequent reconciles re-fire.

Effect on this slice: hides post-mortem evidence in the HUD ConfigMap.
Does not invalidate Mills' own `stage_results` data (the authoritative
source the kill-test reads). Track as separate follow-up.

---

## 5. Acceptance criteria for the downstream fix slice

- A spawn for `project=loom-core, branch=mills-canary-fix-test,
  base_branch=main` results in a pod whose `git rev-parse
  --abbrev-ref HEAD` reports `mills-canary-fix-test`.
- After that spawn completes with the agent making a one-line edit and
  pushing, `pipeline.SpawnResponse.DiffPatch` is non-empty and
  `FilesChanged` lists the touched files.
- `glab api projects/47/repository/branches` shows
  `mills-canary-fix-test` exists on origin with the expected commit.
- The resulting MR has a non-empty `head_sha`.
- The 8 autonomy slices from the prior round (auto-merge, ranker, …)
  continue to operate; verify by replaying one canary cycle.

---

## 6. Sources

- Prior loop's verbal diagnosis (2026-05-25 RALPH argument).
- Mills autonomy plan: `.loom/43-plan-mills-autonomy-2026-05-24.md`.
- Prior reconciler-label diagnosis: `.loom/118-diagnosis-mills-spawn-pod-not-found-2026-05-16.md`
  (commit `89c55c6e`, branch `docs/mills-spawn-diagnosis`).
- Live cluster state: `kubectl -n loom-hub get deploy mobile-hud -o yaml`
  (2026-05-25).
- Spawn state ConfigMap: `kubectl -n devbox get cm loom-spawn-state`
  (2026-05-25).
- This loop's working artifact:
  `.loom/local/ralph-iteration-plan-mills-spawn-no-diff-2026-05-25.md`
  (gitignored).
