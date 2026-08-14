# Mills spawn "pod not found during reconciliation" — diagnosis (2026-05-16)

**Status:** Research-only. Diagnostic + fix proposal for the dominant canary
failure mode (24 / 48 escalated pipelines killed at `plan_slice` with the
exact text `hud spawn spawn-<id> status=failed: pod not found during reconciliation`).

**Audience:** the implementer slice that will land the fix without re-reading
this thread.

---

## 1. Root cause hypothesis

**Top-1 (high confidence):** the spawn reconciler queries Kubernetes for pods
labeled `app.kubernetes.io/managed-by=loom-spawn`, but the K8s devbox backend
that actually creates spawn pods labels them with
`app.kubernetes.io/managed-by=mcp-devbox`. The label selector therefore returns
the empty set, every tracked spawn whose `state.PodName` has been populated is
reclassified as "pod not found", and Reconcile falsely stamps
`Status=failed, Error="pod not found during reconciliation"` on it. The Mills
HUD spawn client polls until terminal, sees that state, and burns the stage
attempt.

A 30 s reconcile cadence vs. multi-minute `plan_slice` LLM runs makes this
fire on every long stage. Build-only failures and short stages (where the
spawn becomes terminal before the next reconcile tick) escape. That is exactly
the ~50 % failure split observed on the canary fleet.

**Top-2 (lower confidence, contributing):** even when the agent run completes
successfully, Reconcile's poison write of `state.Error` is never cleared by
`completeSpawn`. Mills only inspects `state.Status`, so `status=completed`
spawns are fine for the operator, but the in-cluster `loom-spawn-state`
ConfigMap is full of misleading "completed but errored" rows — useful evidence
in §2, and a developer-confusion footgun until the root cause lands.

**Top-3 (ruled out, but historically attacked):** HUD rollouts / transient
HTTP failures / orphan reaper races. The 7 prior fix commits all attacked
these surfaces. None touched the label mismatch, which is why the failure
persists.

---

## 2. Evidence trail

### 2.1 Code: error string lives in the reconciler

`internal/spawn/controller.go:336-344` (Reconcile, "pod gone" branch):

```go
for spawnID, state := range c.spawns {
    pod, exists := livePods[spawnID]
    if !exists {
        if state.PodName == "" && isPreRuntimeStatus(state.Status) {
            continue
        }
        // Pod gone -- if a runtime pod existed for a non-terminal spawn,
        // mark it as failed.
        if !IsTerminal(state.Status) {
            state.Status = StatusFailed
            state.Error = "pod not found during reconciliation"
            ...
```

The pre-runtime guard at line 332 was added by `d5ec488a fix(mills): keep
spawns alive during builds` and protects Pending/Building states only. Once
the orchestrator sets `state.PodName` (line 530 of `internal/hud/spawn.go`),
the guard no longer applies and Reconcile flips the spawn to failed on the
next tick.

### 2.2 Code: reconciler's label selector

`internal/spawn/controller.go:302-308`:

```go
selector := fmt.Sprintf("%s=%s", ManagedByLabel, ManagedByValue)
pods, err := c.client.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
    LabelSelector: selector,
})
```

Constants: `internal/spawn/types.go:181-192` define
`ManagedByLabel = "app.kubernetes.io/managed-by"`,
`ManagedByValue = "loom-spawn"`, plus `SpawnIDLabel = "loom.dev/spawn-id"`
and `AgentIDLabel = "loom.dev/agent-id"`.

**These constants are referenced only from `internal/spawn/controller.go`
and the test suites in `internal/spawn/`. No production code writes them.**

Confirmed via:

```
$ grep -rn "loom-spawn\|loom\\.dev/spawn-id\|loom\\.dev/agent-id" --include="*.go" .
internal/spawn/controller.go:295: // app.kubernetes.io/managed-by=loom-spawn ...
internal/spawn/controller.go:314: spawnID := pod.Labels[SpawnIDLabel]
internal/spawn/types.go:186: const ManagedByValue = "loom-spawn"
internal/spawn/types.go:189: const SpawnIDLabel = "loom.dev/spawn-id"
internal/spawn/types.go:192: const AgentIDLabel = "loom.dev/agent-id"
```

### 2.3 Code: where spawn pods are actually labeled

`internal/devbox/backend/k8s_objects.go:100-106` (`buildPodSpec`, used by
`K8sBackend.Start` at `internal/devbox/backend/k8s_runtime.go:40-72`):

```go
labels := map[string]string{
    "app.kubernetes.io/managed-by": "mcp-devbox",
    "devbox/project":               opts.Name,
}
if opts.AgentID != "" {
    labels["devbox/agent-id"] = opts.AgentID
}
```

The HUD spawn orchestrator calls this backend (`internal/hud/spawn.go:512-524`):

```go
startResult, err := o.backend.Start(ctx, backend.StartOpts{
    Name:         "spawn-" + spawnID,
    ImageTag:     buildResult.ImageTag,
    ...
    AgentID:      state.AgentID,
})
...
state.PodName = startResult.ContainerID
```

So every HUD-spawned pod ends up with `managed-by=mcp-devbox`, never
`managed-by=loom-spawn`. The reconciler's `List` always returns zero rows.

### 2.4 Code: Mills surfaces the poisoned error

`pkg/mills/clients/spawn.go:223-228` (`pollSpawn`):

```go
if isTerminalSpawnStatus(state.Status) {
    resp := mapTelemetryToResponse(state)
    if state.Status != "completed" {
        return resp, fmt.Errorf("hud spawn %s status=%s: %s", spawnID, state.Status, state.Error)
    }
    return resp, nil
}
```

Format produces the exact canary error: `hud spawn spawn-<id> status=failed:
pod not found during reconciliation`. The condition guards on
`!= "completed"`, so this only fires when Reconcile flipped Status before
`completeSpawn` could.

### 2.5 Cluster: zero pods carry the spawn label

`KUBECONFIG=/Users/cblevins/workspace/platform/gitops/.kube/k3s.yaml`
(taken 2026-05-16, k3s app cluster):

```
$ kubectl get pods -A -l 'app.kubernetes.io/managed-by=loom-spawn' --show-labels
No resources found
$ kubectl get pods -A -l 'app.kubernetes.io/managed-by=mcp-devbox' --show-labels
No resources found
```

Both selectors are empty in the current quiescent state — no live spawns at
the moment. Spawn pods do get created though; they are named `spawn-spawn-<id>`
in the `devbox` namespace (the `SpawnNamespace` default in
`internal/hud/app.go:87`). The `buildah-build-loom-core-spawn-<id>` builder
pods are still visible in events from 30-45 minutes ago.

### 2.6 Cluster: the spawn-state ConfigMap is the smoking gun

`kubectl get configmap -n devbox loom-spawn-state -o json` (19 entries, recent
canary batch):

| Count | Error text (truncated) | Sample status / pod |
|-------|------------------------|---------------------|
| 9     | `image build failed: buildah build failed: watch closed ...` | `failed` / empty PodName |
| **6** | **`pod not found during reconciliation`** | **3 of 6 also show `status=completed` with non-empty PodName** |
| 2     | `image build failed: buildah build failed: build pod failed: exit_code=125 ...` | `failed` / empty PodName |
| 1     | `pod creation failed: pod not ready: watch closed ...` | `failed` / empty PodName |
| 1     | (none) | `completed` |

Three entries that are particularly damning — `status=completed` but
`error="pod not found during reconciliation"`:

```
spawn-43485903baef: status=completed pod=spawn-spawn-43485903baef err=pod not found during reconciliation
spawn-74d09ff04469: status=completed pod=spawn-spawn-74d09ff04469 err=pod not found during reconciliation
spawn-dfeb98a5c65b: status=completed pod=spawn-spawn-dfeb98a5c65b err=(none)
```

These tell the story: `Reconcile` poisons `state.Error` while the spawn is
still `Running`; the agent then finishes successfully and `completeSpawn`
overwrites `Status` but does not touch `Error`. The lucky `dfeb98a5c65b` ran
to completion in less than 30 s and beat the reconcile tick.

Of the six `pod not found` rows, three retained `status=failed` — those are
the actual canary stage failures Mills surfaced.

### 2.7 Prior fix commits — none addressed the label mismatch

| Commit | Surface attacked | Why it left the bug open |
|--------|------------------|--------------------------|
| `a433ce06 fix(mills): redial broken MCP hub transport on close 1006` | gateway WS retries | Different layer (MCP hub), unrelated to spawn reconcile |
| `5ecdda2f fix(mills,hud): reap orphan spawns + give Mills UI actual signal` | added TerminalHook, UI signal | Touched controller.go terminal-cleanup path only; selector unchanged |
| `1571d923 fix(mills): tolerate hud rollout spawn gaps` | HTTP retries on POST/GET | Masked transient HUD rollout 5xx; doesn't help once `state.Status=failed` is fixed |
| `a08c8f7d fix(mills): keep accepted spawns pending on poll interruption` | poll-context guard | Stops spawn from being lost on context cancel; reconcile still runs |
| `f2dec2cf fix(mills): guard pending spawn ownership` | dispatcher ownership | Pipeline-side; spawn flips Failed regardless |
| `f55f8cfe fix(mills): resume accepted HUD spawns` | resume after rollout | Resume can't fix a `state.Error` already poisoned |
| `895343b6 fix(mills): decode HUD spawn envelopes` | JSON envelope decode | Unrelated |
| `6cb0dcd1 fix(mills): unstick pipeline runs by propagating ResumeSpawnID` | dispatcher resume path | Same as f55f8cfe |
| `d5ec488a fix(mills): keep spawns alive during builds` | added `isPreRuntimeStatus` | Only protects Pending/Building; once `PodName` is set, bug fires |

Every recent fix attacked a neighbour. None changed `internal/devbox/backend/k8s_objects.go` labels nor the reconciler selector.

### 2.8 Pre-runtime guard's narrow scope

`internal/spawn/controller.go:332-334` (added in `d5ec488a`):

```go
if state.PodName == "" && isPreRuntimeStatus(state.Status) {
    continue
}
```

`isPreRuntimeStatus` returns true only for `StatusPending` / `StatusBuilding`
(controller.go:426-433). After `state.PodName = startResult.ContainerID` runs
at `internal/hud/spawn.go:530`, this guard no longer matches and every
subsequent reconcile tick takes the poison branch.

---

## 3. Proposed fix slice

### 3.1 Branch + commit shape

- Branch: `fix/mills-spawn-pod-label-mismatch`
- Commit message subject: `fix(spawn): label HUD-spawned pods with managed-by=loom-spawn`
- Single MR, single commit if possible.

### 3.2 Files to change

| File | Change |
|------|--------|
| `internal/devbox/backend/backend.go` | Extend `StartOpts` with two optional fields: `ExtraLabels map[string]string` and `ManagedByOverride string`. Keep zero-value behaviour identical (devbox label set unchanged) so mcp-devbox sandboxes don't break. |
| `internal/devbox/backend/k8s_objects.go` | In `buildPodSpec`, after the default `labels` map is constructed, apply `opts.ManagedByOverride` (replacing the `managed-by` value) and merge `opts.ExtraLabels` in. Document that overrides come after defaults so the caller wins. |
| `internal/devbox/backend/docker.go` | Equivalent merge for the docker backend's container label assembly (same name in the Docker label map). The docker backend is not used in cluster runs but keeps the two backends symmetric. |
| `internal/hud/spawn.go` | In `runSpawn` where `backend.StartOpts{...}` is constructed (around line 512-524), populate the new fields with `ManagedByOverride: spawn.ManagedByValue` and `ExtraLabels: { spawn.SpawnIDLabel: spawnID, spawn.AgentIDLabel: state.AgentID, "loom.dev/agent-type": req.AgentType, "loom.dev/project": req.Project }`. Import `internal/spawn` for the label constants. |
| `internal/devbox/backend/k8s_test.go` (or new `k8s_objects_test.go`) | Unit test confirming a `StartOpts` carrying override+extras produces a pod with `managed-by=loom-spawn`, `loom.dev/spawn-id=<id>`, `loom.dev/agent-id=<agent>`, while a zero-override `StartOpts` still produces `managed-by=mcp-devbox`. |
| `internal/spawn/controller_test.go` | Already covers `ManagedByLabel/SpawnIDLabel` round-trip; verify it still passes. Add a regression test that mocks a `kubernetes.Interface` returning a pod labeled `managed-by=loom-spawn` matching a tracked `Running` spawn and asserts Reconcile does **not** mutate `state.Status` or `state.Error`. |
| `internal/hud/spawn_persist_test.go` or sibling | Optional: integration-style test that wires a fake K8sBackend + spawn controller and asserts the orchestrator's labeling produces pods that survive `Reconcile` without false failures. |
| `CHANGELOG.md` | One line under `Fixed`: `Mills canary pipelines no longer fail with "hud spawn ... pod not found during reconciliation"; HUD-spawned pods now carry the labels the spawn reconciler queries.` |

### 3.3 Out-of-scope (do NOT touch in this fix)

Per coordination notes:
- `pkg/mills/pipeline/`
- `pkg/mills/store/`
- `cmd/loom/cmd_mills_pipelines.go`
- `internal/hud/frontend/src/lib/components/Mills/`

Also leave alone:
- The reconciler's "non-terminal spawn → mark failed" branch. The fix above
  makes `livePods[spawnID]` populated, so the branch becomes unreachable for
  healthy spawns — no logic change needed there.
- The `state.Error` poison-clearing question. Once the bug is gone the
  poisoned-error rows can't be generated. A separate cleanup MR can null out
  `state.Error` in `completeSpawn` for hygiene; not required for canary recovery.
- The optional `ParentSessionID` / `Metadata` label-propagation discussion —
  belongs to a follow-up "spawn observability" slice.

### 3.4 Acceptance criteria

1. New `go test ./internal/devbox/backend/...` passes with the labelling test
   added in 3.2.
2. New `go test ./internal/spawn/...` passes including the regression test
   asserting Reconcile does not poison a `Running` spawn whose pod carries
   the new labels.
3. Manually verifiable post-deploy:
   - `kubectl get pods -n devbox -l app.kubernetes.io/managed-by=loom-spawn`
     returns rows during a live spawn.
   - `kubectl get configmap -n devbox loom-spawn-state -o json` after a clean
     canary run shows **zero** entries with
     `error="pod not found during reconciliation"`.
4. A fresh canary pipeline reaches at least the `implement` stage without the
   `pod not found` error in `stage_results.log_tail`.
5. Existing mcp-devbox sandbox pods continue to come up with
   `managed-by=mcp-devbox` (no regression for the existing devbox MCP server).

---

## 4. Risk + rollback

### 4.1 Risks

- **Low blast radius.** The fix only changes pod labels; pod *spec* (image,
  command, mounts, security context) is unchanged. No new RBAC or CRDs.
- **Existing mcp-devbox sandboxes share `buildPodSpec`.** Mitigated by making
  the new fields opt-in (`ManagedByOverride == ""` → preserve current
  `mcp-devbox` value; `ExtraLabels == nil` → no merge). The mcp-devbox call
  site is untouched.
- **In-flight spawns at deploy time** will not be retroactively labeled. The
  reconciler's 30 s tick will still mark them failed once. Acceptable: they
  were already failing. The first **new** spawn after rollout is labeled
  correctly.
- **`internal/hud` importing `internal/spawn`.** Already imported (see
  `internal/hud/spawn.go:179` for `spawn.NewK8sConfigMapStore` etc.) — no new
  package graph dependency.
- **The `loom.dev/agent-type` / `loom.dev/project` extra labels** are
  technically a new schema. Mitigation: scope the fix to just the three
  reconciler-required labels (`managed-by`, `spawn-id`, `agent-id`) and
  defer the optional ones to the observability slice.

### 4.2 Rollback

- Revert the single commit. Spawn pods go back to `managed-by=mcp-devbox`,
  reconciler returns to its empty-list false-failure mode. Mills canary
  pipelines return to the current 50 % failure rate, no worse than before.

### 4.3 Migration / orphan handling

- After deploy, the existing `loom-spawn-state` ConfigMap in `devbox` namespace
  contains 19 stale entries with the poisoned error. The 24 h prune
  (`StartPruneLoop` at `internal/hud/embed.go:576`) will clear them within a
  day. No manual migration required. If the operator wants a clean slate
  immediately, `kubectl delete configmap -n devbox loom-spawn-state` is safe
  — the controller will recreate it via `K8sConfigMapStore.Save` on the next
  spawn.

---

## 5. Verification plan

The follow-up implementer slice can verify cold (no need to re-read this doc)
by running the sequence below against the deployed HUD/Mills stack.

### 5.1 Pre-deploy local verification

```bash
cd /Users/cblevins/workspace/services/loom-core
go test ./internal/devbox/backend/... ./internal/spawn/... ./internal/hud/...
go vet ./...
```

Expected: all tests pass, new label-assertion tests included.

### 5.2 Post-deploy cluster verification

```bash
export KUBECONFIG=/Users/cblevins/workspace/platform/gitops/.kube/k3s.yaml

# 1. Confirm a fresh spawn pod carries the expected labels.
#    (Trigger via mobile API or the loom CLI.)
kubectl get pods -n devbox -l app.kubernetes.io/managed-by=loom-spawn --show-labels

# Expected: at least one pod listed during an active spawn, with labels
#   app.kubernetes.io/managed-by=loom-spawn
#   loom.dev/spawn-id=<id>
#   loom.dev/agent-id=spawn-<type>-<short-id>
#   devbox/project=spawn-<id>

# 2. Confirm reconcile sees the pods and does NOT poison state.
kubectl get configmap -n devbox loom-spawn-state -o json | \
  jq -r '.data | to_entries[] | "\(.key): \(.value | fromjson | .status) \(.value | fromjson | .error)"' | \
  grep "pod not found"

# Expected: no matches for spawns started after the deploy.

# 3. Confirm Mills canary reaches `tests` stage.
kubectl logs -n loom-mills deploy/loom-mills-operator --tail=200 | \
  grep -E "stage=plan_slice|stage=implement|stage=tests|pod not found"

# Expected: stage transitions present, "pod not found" absent.
```

### 5.3 Long-running soak

Enqueue 3 canary pipelines via `loom mills pipeline start --canary` and let
them run for 45+ minutes. Pass criteria:

- All three reach at least `implement`.
- `loom-spawn-state` shows `status=completed` rows with empty `error` field.
- Zero new `hud spawn ... status=failed: pod not found ...` lines in the
  operator logs.

### 5.4 Mcp-devbox regression check

```bash
# Trigger a normal devbox quality gate.
loom devbox quality-gate --project loom-core --agent-id verify-fix

# Inspect the pod that came up — must NOT carry the loom-spawn labels.
kubectl get pods -n devbox -l app.kubernetes.io/managed-by=mcp-devbox \
  --show-labels | head -3
```

Expected: the devbox pod is labeled `managed-by=mcp-devbox`, NOT
`loom-spawn`. Confirms the override is opt-in and non-spawn paths are
untouched.

---

## 6. Coordination notes for the implementer

- Branch from `main` (currently `76ca2eb2`). Avoid `codex/mills-canary-start`
  and the parallel slice branches enumerated in §3.3.
- The fix touches three packages but is mechanically small (~40 lines of
  production code plus tests). Stay disciplined; do not let it grow into a
  "rework the spawn observability" rewrite.
- If the implementer needs to validate quickly without a full cluster
  re-deploy, they can:
  1. `kubectl edit pod -n devbox spawn-spawn-<id>` and manually patch labels
     onto a live spawn pod. The next 30 s reconcile tick will see it and
     skip the false-failure branch. This is a *diagnostic confirmation
     hack only* — do not ship it.
  2. Or replay the failing canary with the patched binary deployed to
     `mobile-hud` (`registry.harbor.lan/mcp/loom-core:<tag>`); see local
     image-build runbook at
     `/Users/cblevins/.claude/projects/-Users-cblevins-workspace-services-loom-core/memory/reference_local_loom_core_image_build.md`.

---

## 7. References

- Workspace: `/Users/cblevins/workspace/services/loom-core`
- Worktree this diagnosis was produced in:
  `.claude/worktrees/compassionate-benz-50a60a` (branch
  `docs/mills-spawn-diagnosis` off `main` @ `76ca2eb2`)
- k3s cluster kubeconfig:
  `/Users/cblevins/workspace/platform/gitops/.kube/k3s.yaml`
- Spawn-state ConfigMap (live evidence):
  `kubectl get configmap -n devbox loom-spawn-state`
- Mills canary pipeline runner: `pkg/mills/pipeline/runner.go`
  (stage list at line 56)
