# Runbook: Mills Workflow S1c Deployed Dual-Crash Kill-Test

## Purpose

Prove the crash-recovery guarantees Mills requires on the real substrate: one
deterministic spawn identity, one Kubernetes pod incarnation, one durable spawn
record, one completion-wrapper/hold process pair, and one successful
workflow-journal row while BOTH state holders (operator journal and mobile-hud
spawn controller) restart. The gate requires **three consecutive runs** without
a second spawn/pod incarnation on the continuous Kubernetes watch, and with no
replacement or overlapping driver, zombie, or quarantine in the bounded
process samples.

This is deliberately **not** proof that the agent turn or its external tool
effects execute exactly once. The stronger S1c contract rejects a mobile-hud
restart that kills the exec session and re-drives the agent CLI in the same
pod/workspace. S1c therefore also requires a durable pod-owned execution
supervisor that survives controller replacement and reaps orphaned processes.
Autonomous S6-full/S7 effects still need durable turn/tool idempotency, or an
equally strong effect-level fence.

**Current status (2026-07-21): S4 DEPLOYED + a newly-found preflight
prerequisite FIXED; the canary is preflight-green except the window.** Two
updates since 2026-07-17:

1. **S4 is deployed.** The operator (`registry.harbor.lan/mcp/loom-mills-operator`,
   image tag `20260721-*`) and mobile-hud carry the pod-owned execution
   supervisor; `LOOM_SPAWN_SUPERVISED_EXECUTION` is default-on. The "do not run
   until S4 deploys" gate below is satisfied.
2. **Operator-authority prerequisite fixed (gitops `!469`, merged; operator
   rolled 2026-07-21).** A read-only preflight (`--phase preflight`,
   `--gitops-identity-mode protected-scope`) failed closed at **P2d/P3**: the
   operator's `/api/mills/policy` authority response carried empty
   `PodName/PodNamespace/PodUID`. Root cause: the operator reads those from the
   `POD_NAME/POD_NAMESPACE/POD_UID` env
   (`cmd/loom-mills-operator/operator_authority.go`), but the gitops operator
   Deployment (`platform/gitops/k3s/mills/deployment.yaml`) never set them.
   Confirmed empty on both the public route and a direct port-forward (not proxy
   stripping). Fixed by adding the three Downward API `fieldRef` env entries;
   after the Flux roll the operator emits populated `X-Loom-Mills-Pod-*` headers.

After the fix, the read-only preflight passes the **full provenance + operator
authority + Loki fence** (all four Flux Kustomizations `Ready` at their expected
revisions; live operator Deployment spec == reviewed render; policy ConfigMap
live == rendered; `loki_ready: true`). The **only** remaining preflight failures
are the deliberate window state — `workflows.enabled: false` (closed since
2026-07-13) and normal active Mills work — which Procedure Step 1 opens. The
destructive 3-run dual-crash canary is therefore **ready to run in a quiet
window** (it closes Mills admission cluster-wide and restarts the shared
mobile-hud, interrupting every exec-driven spawn). As of 2026-07-21 it is
**deferred pending a quiet window**, not blocked.

**Current status (2026-07-17):** the pod-owned execution supervisor/reaper has
**landed in code** (S4 slice `arch/fleet-s4-durable-execution`). A supervised
spawn now runs its agent turn + completion hold under a detached, PID-1-reparented
in-pod reaper; mobile-hud only tails status, so a mobile-hud restart **re-attaches**
to the surviving process pair instead of re-driving a replacement. With the
supervisor deployed, a fresh run is **expected to PASS** the process-continuity
contract: the original `(hold, wrapper)` `(PID, starttime)` survives both crashes,
no replacement wrapper/hold is created, and the mobile-hud replacement pod logs the
accepted dedupe phrase `idempotent spawn re-attach (already exists)` bound to the
exact spawn id. The gate stays **closed** until this runbook is run against the
DEPLOYED image carrying S4 and passes three fresh consecutive runs; the 2026-07-14
run remains a failed diagnostic and cannot be reused. The supervisor is gated by
`LOOM_SPAWN_SUPERVISED_EXECUTION` (default on in the deployed mobile-hud); verify it
is on before the run (`kubectl -n loom-hub set env deploy/mobile-hud --list |
grep -i LOOM_SPAWN_SUPERVISED_EXECUTION`, or confirm the default via the mobile-hud
startup log `pod-owned execution supervisor enabled (S4)`).

**Reconciliation (2026-07-17):** the loom-core#300 liveness recovery
(recover-before-reconcile boot ordering, keyed re-drive / unkeyed fail-fast, the
reconciler deadline backstop, terminal GC) is **merged and unit-verified green**
— see `.loom/188-reconciliation-workflow-turn-liveness-2026-07-17.md` for the
evidence bundle (`file:line`, commits, test runs). The gate remains closed **not**
because liveness is broken but because the process-continuity contract in this
runbook (same `(hold, wrapper)` `(PID, starttime)` across both crashes) requires
the **pod-owned execution supervisor/reaper**, which is the S4 durable-execution
slice (`arch/fleet-s4-durable-execution`, `.loom/187` Sprint 2) — not a re-fix of
#300. That supervisor has now **landed in code** (see the status block above); do
not attempt the live 3-run canary until S4 **deploys**. The pre-S4 re-drive path
is expected to fail this contract; the deployed supervisor's re-attach path is
expected to pass it.

Harness: `cmd/mills-workflow-killtest` (library: `pkg/mills/workflow/killtest`).

## Prerequisites

| # | Check | Command | Expect |
|---|-------|---------|--------|
| P1 | Cluster and evidence path reachable | `KUBECONFIG=~/workspace/platform/gitops/.kube/k3s.yaml kubectl get ns loom-mills loom-hub devbox logging` | all Active; Loki ready |
| P2 | Reviewed deployment identity recorded | capture the operator and mobile-hud namespace/UID/generation, image tags, running `imageID` digests, complete live/reviewed Deployment spec SHA-256 values, reviewed platform GitOps and loom-core baseline SHAs/digests, Flux renderer identity, both GitRepository source identities, exact policy source checksum, and spawn ConfigMap UID | both live Deployment specs equal the exact-commit, Flux-rendered, server-normalized desired specs; the stable workload/source tuple remains exact |
| P2a | Both Flux source checkouts and renderer available | `git -C ~/workspace/platform/gitops rev-parse --git-dir && git -C ~/workspace/services/loom-core rev-parse --git-dir && flux version --client` | all succeed; the harness can export exact commits and reconstruct both reviewed Deployments plus the policy ConfigMap from the same apps render, fetching `origin/main` only when an object is missing |
| P2b | Server-side normalization authorized | `kubectl auth can-i update deployment/loom-mills-operator -n loom-mills && kubectl auth can-i update deployment/mobile-hud -n loom-hub` | both `yes`; the harness sends only Kubernetes `dryRun=All` updates and never persists the reviewed object |
| P2c | Controller Pod admission replay authorized | `kubectl auth can-i create pods -n loom-mills && kubectl auth can-i create pods -n loom-hub` | both `yes`; Kubernetes authorizes dry-run CREATE through the normal `create pods` permission, but the harness always sends `dryRun=All` and persists no Pod |
| P2d | Operator authority identity is reviewed and routed intact | inspect the reviewed operator Deployment for Downward API `POD_NAME`, `POD_NAMESPACE`, and `POD_UID`; request the stable route and inspect `X-Loom-Mills-*` headers | all three env fields use `fieldRef` (`metadata.name`, `metadata.namespace`, `metadata.uid`); every response carries the exact Pod/Deployment fields and one 64-lowercase-hex boot ID without proxy rewriting |
| P2e | Cluster anchor read is authorized | `kubectl auth can-i get namespace/loom-mills` | `yes`; the harness binds the frozen kubectl source and client-go DELETE client to the same active Namespace UID |
| P3 | Audited admission barrier active | reconstruct `loom-mills/loom-mills-policy` from the reviewed `apps` render, compare its complete payload with the live ConfigMap, then compare `policy.yaml` with `GET /api/mills/policy` | reviewed/live `data`, `binaryData`, and `immutable` hashes match; exact source-file SHA matches the reviewed/live Deployment annotation; global `enabled: false`; `workflows.enabled: true`; `substrate_k8s_only: true`; ConfigMap and effective policy match |
| P4 | Selected canary harness confirmed | recent successful Mills spawn using the selected `agent_type` with cost telemetry | auth works; `claude-code` reports `cost_source=real`, while `codex` reports `cost_source=estimated` |
| P5 | Substrate is k8s | structural — the runtime hardcodes `Substrate: "k8s"` (`pkg/mills/workflow/runtime.go`) | n/a |
| P6 | Spawn-state ConfigMap durable | `kubectl -n devbox get configmap loom-spawn-state` | exists (NOT in loom-hub) |
| P6a | Every controller sharing spawn state is owner-aware | record the cluster mobile-hud image, `SPAWN_CONTROLLER_ID`, and `SPAWN_RECOVERY_AUTHORITY`; inspect every desktop HUD with `pgrep -lf 'loomd|loom hud'` | cluster is `loom-hub/mobile-hud` and the sole recovery authority; every desktop HUD was rebuilt/reloaded from the merged fix, reports a distinct role-qualified `local/<role>/*` owner, and has recovery authority disabled (or is disabled for the full window); two live processes must never share one owner ID |
| P6b | Runtime identity migration is authorized | run the `kubectl auth can-i` checks below | mobile-hud may update pods in `devbox`; `mills-spawn` may update VMs in Harvester `default` |
| P6c | Exact canary process proof is authorized | `kubectl auth can-i create pods/exec -n devbox` | `yes`; the local harness can inspect the exact foreground hold without granting this permission to a deployed controller |
| P7 | Unrelated work drained | `GET /api/mills/safety/quiescence` plus `devbox` spawn state | all durable and in-memory counts are zero before launch; after launch the canary is the only active workflow/spawn |

Pinned selectors (verified live 2026-07-08):

- Operator pod: `-n loom-mills -l app.kubernetes.io/name=loom-mills-operator`
- mobile-hud pod: `-n loom-hub -l app=mobile-hud`
- Spawn pods: `-n devbox`, name `spawn-<spawn_id>`

## Procedure

### 1. Open the audited canary window and admission barrier

As of 2026-07-13, GitOps has both global admission and
`workflows.enabled` closed after the failed S1c window. Open only the workflow
gate for the audited run below. An idle snapshot alone cannot stop new council,
pipeline, spin, audit, or intake work from starting between preflight and
deletion.

Stage the opening so the operator cannot restart before existing work drains:

1. Edit `~/workspace/platform/gitops/k3s/mills/configmap-policy.yaml`: set the
   global `enabled: false`, keep `workflows.enabled: true`, and keep
   `workflows.substrate_k8s_only: true`. **Do not change the Deployment checksum
   yet.** Commit, push, and reconcile. The ConfigMap hot reload closes admission
   without restarting the current operator.
2. Verify the effective policy matches the ConfigMap, then wait for the full
   quiescence proof below. Any nonzero or unknown count blocks the window.
3. Only after drain, update `loom.flexinfer.ai/policy-checksum` in
   `deployment.yaml` to the checksum of the already-active closed policy.
   Commit, push, reconcile, and wait for the replacement operator to become a
   stable singleton. The rollout now happens behind the closed barrier.

For each pushed stage:

```bash
flux reconcile kustomization apps -n flux-system --with-source
```

The global switch is the admission barrier: work-creating REST handlers and
background schedulers reject new admissions, while the imperative workflow
scheduler remains enabled for the single canary. The operator exposes any
request/scheduler admitted before the hot reload as an in-memory activity
count; the harness waits for zero. `POST /workflow/canary` is the sole
exception and atomically refuses a second active workflow. The caller supplies
a stable run id, so a lost response is an idempotent retry rather than an
orphaned second run.

Require all four render-owning Flux Kustomizations to report `Ready=True` for
their current metadata generations: `apps`, `bootstrap`, `system`, and
`loom-hub-servers`. Each owner's applied and attempted revisions must match.
The v5 provenance contract also requires exactly the two GitRepositories those
owners reference:

| GitRepository | Reviewed routing fields | Kustomizations |
|---------------|-------------------------|----------------|
| `flux-system/gitops-gitlab` | URL `http://gitlab-vm.gitlab.svc.cluster.local/platform/gitops.git`; branch `main`; Secret reference `gitops-gitlab` | `apps`, `bootstrap`, `system` |
| `flux-system/loom-core` | URL `http://gitlab-vm.gitlab.svc.cluster.local/services/loom-core.git`; branch `main`; Secret reference `gitops-gitlab` | `loom-hub-servers` |

The reviewed source manifests are
`clusters/k3s/flux-system/gitrepository-gitlab.yaml` and
`clusters/k3s/flux-system/gitrepository-loom-core.yaml`. The reviewed render
manifests are the `kustomization-apps.yaml`, `kustomization-bootstrap.yaml`,
`kustomization-system.yaml`, and `kustomization-loom-hub-servers.yaml` files in
that same directory.

The harness reconstructs each complete GitRepository and Kustomization `.spec`
from its defining manifest under the reviewed platform GitOps baseline. It
compares a canonical digest of the complete live API spec, including contract
handling for API defaults, rather than trusting selected routing fields. A full
gate therefore fails closed on any source URL, ref, `secretRef`, render path,
or other spec drift. Only the Secret reference name is evidence; the harness
never reads or serializes repository Secret contents.

Deployment provenance uses the independent
`mills-s1c-deployment-provenance/v1` contract. The harness exports only the
exact reviewed Git objects into private temporary directories, then runs:

- `flux build kustomization apps --dry-run` against the reviewed platform
  `k3s/flux/apps` tree and `kustomization-apps.yaml`, selecting exactly
  `loom-mills/loom-mills-operator`;
- `flux build kustomization loom-hub-servers --dry-run` against the reviewed
  loom-core `k8s/base` tree and reviewed platform
  `kustomization-loom-hub-servers.yaml`, selecting exactly
  `loom-hub/mobile-hud`.

For each selected object, a server-side dry-run Deployment UPDATE applies the
same Kubernetes defaults and admission as the live cluster. The harness hashes
the complete normalized `.spec` and requires equality with the complete fresh
live `.spec`; no selected-field allowlist is used. This catches pre-existing
command, args, environment, service-account, volume, init-container, security,
affinity, probe, and other spec drift even when Flux reports Ready and
server-side apply preserved a foreign-owned field. Dynamic
`postBuild.substituteFrom` fails closed because an offline render cannot prove
its Secret/ConfigMap inputs. Exact-baseline renders are cached by both reviewed
revisions/digests, the relevant Flux spec digests, binary path, and renderer
version; every preflight still repeats the server normalization and live
comparison. The server request uses `dryRun=All` and persists no mutation.

Controller Pod execution provenance uses the independent
`mills-s1c-pod-execution-provenance/v1` contract. After binding the selected
Pod to an exact stable ReplicaSet and reviewed Deployment, the harness rebuilds
the same Pod CREATE shape as the ReplicaSet controller: template labels,
annotations, finalizers, complete PodSpec, generated-name prefix, and the exact
ReplicaSet controller owner reference. It submits that object with a
server-side `dryRun=All` CREATE and requires the complete admitted PodSpec
SHA-256 to equal the complete live PodSpec SHA-256. The live Pod must also
satisfy the full ReplicaSet selector and retain the exact Pod -> ReplicaSet ->
Deployment controller lineage.

Each controller namespace census is exactly one raw Pod API List with
`limit=5000`. A continuation token or more than 5000 returned items rejects the
snapshot before ReplicaSet selection; kubectl is not allowed to auto-follow and
merge pages. All generic kubectl invocations also cap stdout at 64 MiB and
stderr at 64 KiB. Overflow, truncation, or command failure returns no stdout to
the parser, so a partial Kubernetes object can never authorize the gate.

Each namespace Pod List resourceVersion is retained as observation provenance,
but it is not an incarnation identifier and may advance between otherwise
identical censuses. Cross-observation continuity therefore ignores only that
List resourceVersion. The selected Pod resourceVersion, ReplicaSet
resourceVersion, UIDs, container identity, execution digests, and lineage stay
exact and any drift in them closes the gate.

The v1 PodSpec comparison normalizes only two create-time differences. A live
scheduler-assigned `nodeName` is cleared only when the dry-run result leaves
`nodeName` empty; an explicit template/admission nodeName remains exact. The
random suffix of the single projected `kube-api-access-*` volume and all of its
matching regular, init, and ephemeral-container mounts are replaced by one
sentinel name; the projected source and every mount field remain hashed. Zero
such volumes is valid for automount-disabled Pods, while multiple or unmatched
volumes fail closed. Command, environment, service account, volume source,
security context, init/ephemeral containers, scheduling, and every other
PodSpec field receive no normalization.

This proof replays the cluster's current API defaulting and admission; it does
not independently source-review admission webhook configuration. The current
audited environment has no Pod-mutating webhook or namespace LimitRange for
these controller namespaces. Before introducing either, bind its reviewed
configuration and version the renderer/evidence contract; otherwise the S1c
gate must remain closed. The persisted execution digests deliberately cover
the complete PodSpec. Arbitrary live metadata is outside that digest, while
selector labels and controller ownership are validated by the separate exact
lineage checks above.

Authority-plane evidence uses the independent `mills-s1c-authority-plane/v1`
contract. At startup the harness loads the selected kubeconfig once, minifies
and flattens it, writes one private frozen copy, and derives both kubectl and
client-go from that exact source. The serialized evidence contains only a
public-authority digest (normalized HTTPS API endpoint, server name, embedded
CA digest, and context), the individual endpoint/CA hashes, and the live
`loom-mills` Namespace UID; it never serializes bearer, client-key, or exec
credential material. API URLs with non-HTTPS schemes or userinfo fail closed.
Immediately before the final time-sensitive delete hook, client-go must reread
that same active Namespace UID. After the hook, its only Kubernetes operation
is the exact Pod DELETE with UID and resourceVersion preconditions.

Every gate-relevant operator response must carry the authority contract,
Downward-API Pod name/namespace/UID, Deployment name, and a per-process random
boot ID. The harness binds those response fields to its independently reviewed
Pod -> ReplicaSet -> Deployment chain. Same-Pod boot-ID drift detects an
unplanned container/process restart and is terminal; authority mismatch is
never treated as a lost-response retry. These headers are fail-closed
operational routing/incarnation identity, **not cryptographic authenticity**
against an ingress or workload able to forge them.

Automatic failure cleanup does not relax this contract. If an unplanned
operator replacement caused the gate failure, cleanup creates a separate,
ephemeral mitigation harness over the same frozen kubeconfig. It freshly proves
the original active Namespace UID, the unchanged exact reviewed operator
Deployment, and the current Deployment -> ReplicaSet -> Pod chain before one
side-effect-free response must attest that Pod and a valid boot ID. Only that
ephemeral harness may issue `/fail`; its replacement identity is never copied
into gate evidence or made eligible for PASS. Any further authority or boot-ID
change returns a typed terminal error immediately instead of polling for two
minutes.

This loom-core change does not modify the external `platform/gitops` repo. Its
reviewed operator Deployment must first inject these exact fields:

```yaml
env:
  - name: POD_NAME
    valueFrom: {fieldRef: {fieldPath: metadata.name}}
  - name: POD_NAMESPACE
    valueFrom: {fieldRef: {fieldPath: metadata.namespace}}
  - name: POD_UID
    valueFrom: {fieldRef: {fieldPath: metadata.uid}}
```

Until that reviewed GitOps prerequisite is deployed and the stable route is
shown to preserve every authority header, the new loom-core binary alone cannot
satisfy this contract. Keep S1c closed; do not claim a live PASS.

Policy ConfigMap provenance uses the independent
`mills-s1c-policy-configmap-provenance/v1` contract. From the exact same
`flux build kustomization apps --dry-run` output used for the reviewed operator
Deployment, the harness requires exactly one
`v1/ConfigMap loom-mills/loom-mills-policy`. It computes a deterministic
SHA-256 over a canonical JSON object containing all three policy-bearing API
fields: `data`, decoded `binaryData`, and the presence/value of `immutable`.
Map keys are canonicalized; absent and empty maps share the Kubernetes semantic
representation `{}`, while absent `immutable` remains distinct from explicit
`false`. Any extra live data key, binary entry, changed value, or immutable
drift therefore changes the payload digest and closes the gate.

The rollout checksum is a separate exact-source proof. The harness computes
lowercase hexadecimal SHA-256 directly over the committed blob bytes at
`<reviewed-platform-revision>:k3s/mills/configmap-policy.yaml`. It does not
parse or reserialize YAML and does not normalize line endings or the final
newline. That digest must equal
`spec.template.metadata.annotations["loom.flexinfer.ai/policy-checksum"]` on
both the reviewed rendered Deployment and the fresh live Deployment. The
serialized policy review also binds the exact ConfigMap name/namespace, apps
Flux full-spec digest, platform baseline revision/protected-scope digest,
source path/source SHA, renderer/version, and rendered/live payload SHAs.

Each crash also carries a `mills-s1c-policy-delete-boundary/v1` proof. After
the final target Deployment -> ReplicaSet -> Pod reread, the delete-start hook
makes the last foreground gate reads before DELETE in this exact order: policy
ConfigMap A, `GET /api/mills/policy`, the live operator Deployment, then policy
ConfigMap B. A and B must retain the immediate preflight ConfigMap's exact
namespace, name, UID, resourceVersion, complete payload SHA, and parsed policy.
The effective response must equal that policy with global admission closed,
workflows enabled, and Kubernetes-only substrate. The operator Deployment must
be a stable singleton with the same complete spec, pod-template, selector,
reviewed-render identity, and policy checksum as immediate preflight.

Every boundary observation timestamp is captured when its request starts, so
request latency consumes the freshness budget. The evidence records completion
only after ConfigMap B returns; strict validation requires A -> effective ->
operator Deployment -> B -> completion ordering, completion before the
recorded DELETE request, and B's request start no more than ten seconds before
that request. After B, the main delete path performs only in-memory
authorization and freshness validation before the
UID-and-resourceVersion-preconditioned DELETE. The independent process observer
is already active and may continue its bounded sampling concurrently. The
bracket is bounded and ordered, not a cross-object transaction.

Each v5 observation is a
GitRepository-List A -> Kustomization-List -> GitRepository-List B bracket.
Evidence persists the completion timestamps for A, the Kustomization List, and
B and requires them to be ordered. Consecutive brackets may not overlap.
Both source samples must contain the exact two referenced objects with stable
UID, generation, and resourceVersion. For each object,
`status.observedGeneration` and the `Ready` and `ArtifactInStorage` condition
observed generations must equal `metadata.generation`; both conditions must be
`True`; and the artifact revision and digest must be non-empty and stable.
`apps`, `bootstrap`, and `system` must apply the `gitops-gitlab` artifact
revision. `loom-hub-servers` must apply the `loom-core` artifact revision.
Every required Kustomization and GitRepository must have no deletion timestamp
and must serialize `terminating=false`. A source artifact digest may change
only when its normalized artifact revision also changes; an unchanged revision
with different bytes is rejected even in protected-scope mode.
Any source-object, artifact-revision, artifact-digest, or cross-binding drift
invalidates the observation.

The effective operator policy must match the ConfigMap before launch. The
harness deliberately never changes policy. Closing the window after PASS or
FAIL is mandatory.

A routine Flux scan can temporarily report `Ready=False` without changing the
accepted source identity. The harness checks all four owners and waits at most
two minutes only when that owner's applied and attempted revisions are
identical. `apps`, `bootstrap`, and `system` are each resolved independently
against the reviewed platform GitOps protected-scope contract.
`loom-hub-servers` is resolved against the independent loom-core contract.
Every protected digest must still equal its reviewed baseline digest. An
observed SHA may advance only to a descendant whose protected Git objects are
unchanged at every intervening commit. The timeout context bounds every probe,
and the target run must remain `running` throughout the wait. A non-descendant
commit, protected-source drift, applied/attempted mismatch, unrelated probe
error, or terminal target fails immediately. No pod deletion occurs until all
four owners are `Ready=True` again.

The protected source identities do not relax live deployment identity. The
operator and HUD Deployment namespaces, UIDs, generations, complete spec
hashes, reviewed render/source bindings, image tags, running image digests,
policy checksum, and spawn ConfigMap UID remain an exact tuple across all
preflight samples. Deployment resourceVersions are retained as evidence but
are not compared across planned pod restarts because status-only updates may
advance them. Protected digests permit unrelated commits; they do not permit
an operator, HUD, policy, spawn, or reconciliation-input change.

The harness uses raw, unpaginated, namespace-wide Lists for every part of the
v5 bracket. It selects the four Kustomizations and two GitRepositories from
those responses instead of issuing independent object GETs. Each preflight
takes one bracket at its start and another at its end. The three platform
owners must retain the reviewed platform protected identity. The loom-core
owner must retain its independently reviewed identity. A render-owner or
source-object change cannot hide inside a mixed workload/source snapshot.

The final workload callback first prepares the complete source fence, including
any slow Git identity resolution. Its closing source operation is the
GitRepository B List that completes one final v5 bracket. Every required
Kustomization and GitRepository UID, generation, resourceVersion, reviewed
spec, condition generation, and revision/artifact identity must match the
prepared snapshot exactly. This rejects mutation and change/revert even when a
revision returns to an earlier value. List resourceVersions are retained as
provenance, but unrelated Flux objects may advance them. Any render-owner or
source-object uncertainty aborts the crash. The final target controller reread
then runs, followed by the policy delete-boundary bracket described above as
the last foreground gate I/O before DELETE. The independent process observer
may continue sampling concurrently.

This is an ordered, bounded-freshness proof, not an atomic transaction across
Flux, Deployment, ReplicaSet, and Pod reads. Drift after any read invalidates
the evidence, and later brackets/preflights reject drift they observe, but the
API server does not lock those objects into one snapshot. Only the Pod DELETE
is atomic with respect to the selected object identity: its preconditions bind
both UID and resourceVersion. Preventing every possible post-read cross-object
race would require an admission-side fence (or equivalent server-side
transaction) that validates the reviewed gate identity at DELETE time.

The durable spawn store uses resourceVersion-based read/modify/update retries,
a same-key merge fence, and a stable logical driver owner. Every initial record
is stamped with `driver_owner_id` before dispatch. Every pod or VM carries the
spawn ID, agent ID, label-safe owner fingerprint, and a generation derived from
the immutable record-before-dispatch `started_at`. Startup recovery
loads/redrives only matching-owner rows and is retried before reconcile or spawn
routes become available. Exactly one configured recovery authority may
atomically claim pre-owner legacy rows or stamp missing runtime identity labels;
ordinary peers leave them untouched. A controller that discovers a pod absent
from its own memory first reads the durable row: an existing row is peer-owned
and is not adopted or rewritten; an unreadable store fails closed; only an
owner-authorized positive miss permits lossy label reconstruction. Keyed
registration also probes the deterministic runtime before installing Pending,
so a rowless foreign pod or VM cannot be reused or replaced by name.

For keyed records, request identity, driver owner, and start time are immutable,
stop/cleanup intent is monotonic, and the first terminal result is sticky across
conflict retries. Reuse, reconcile, stop, and terminal reap all validate the
runtime identity; deletes use UID and resourceVersion preconditions. Durable
pruning matches owner, idempotency key, and start generation inside each
ConfigMap conflict retry, so a stale controller cannot remove or acknowledge
cleanup on a peer replacement.
Generated desktop owners are scoped to their daemon socket and HUD endpoint,
and a nonblocking host-local owner lock rejects a same-domain second process.
The cluster deployment uses an explicit stable owner and is safe because it is
`replicas: 1` with `strategy: Recreate`. It is not a multi-replica execution
lease: scaling one explicit logical owner beyond one live process requires
leader election or a fenced owner epoch first.

The ConfigMap store rejects a candidate above its 896 KiB serialized safety
budget, leaving 128 KiB below the Kubernetes 1 MiB object ceiling. Delete and
prune may progressively shrink an already oversized legacy object, but no
normal lifecycle transition may grow or retain it above the budget.

Delete/recreate of the shared ConfigMap is forbidden. Before launch, the
harness requires exact-object update permission for the HUD service account
and records the ConfigMap UID as immutable gate identity. The least-privilege
RBAC check should return `yes` while a namespace-wide update check returns
`no`:

```bash
kubectl auth can-i update configmap/loom-spawn-state -n devbox \
  --as=system:serviceaccount:loom-hub:loom-hub
kubectl auth can-i update configmaps -n devbox \
  --as=system:serviceaccount:loom-hub:loom-hub
kubectl auth can-i update pods -n devbox \
  --as=system:serviceaccount:loom-hub:loom-hub
KUBECONFIG=~/workspace/platform/gitops/.kube/harvester-admin.yaml \
  kubectl auth can-i update virtualmachines.kubevirt.io -n default \
  --as=system:serviceaccount:default:mills-spawn
```

### 2. Use a restart-stable operator URL

Prefer the stable external operator route so CRASH A cannot sever the harness's
REST connection:

```bash
export S1C_OPERATOR_URL="${LOOM_MILLS_OPERATOR_URL:-https://mills.flexinfer.ai}"
curl -fsS "$S1C_OPERATOR_URL/api/mills/status" | jq .
```

The public route must accept the harness's raw bearer-authenticated requests
from the current network path. A local port-forward is a fallback only:

```bash
# Dedicated terminal; restart it if CRASH A terminates the selected pod.
kubectl -n loom-mills port-forward svc/loom-mills-operator 8090:8090
```

```bash
export S1C_OPERATOR_URL=http://localhost:8090
```

`kubectl port-forward` selects a backing pod and may exit when CRASH A deletes
that pod. Do not use an unsupervised port-forward for the full gate; otherwise
post-crash polling can fail for a transport reason rather than a Mills defect.

Before launch, prove the barrier has drained both durable and in-memory work.
CRASH B restarts the shared mobile-hud and interrupts every exec-driven spawn,
not only the canary:

```bash
curl -fsS "$S1C_OPERATOR_URL/api/mills/safety/quiescence" | \
  jq -e '.quiescent == true and
    ([.counts[]] | all(. == 0)) and
    (.in_memory.admission_closed == true) and
    (.in_memory.crash_lease_active == false) and
    (.in_memory.sources_ready == true) and
    (.in_memory.sample_stable == true) and
    (.in_memory.wiring_required == true) and
    (.in_memory.activity_sources >= 6) and
    (.in_memory.source_generation > 0) and
    (.in_memory.active_admissions == 0) and
    (.in_memory.canary_admissions == 0) and
    (.in_memory.background_operations == 0) and
    (.in_memory.spin_workers == 0) and
    (.in_memory.audit_outstanding == 0) and
    ([.in_memory.source_operations[]] | all(. == 0)) and
    ([.in_memory.source_run_ids[] | length] | all(. == 0))'
kubectl -n devbox get pods -l app.kubernetes.io/managed-by=loom-spawn
```

### 3. Load both admin tokens

```bash
export LOOM_MILLS_ADMIN_TOKEN="$(
  kubectl -n loom-mills get secret loom-mills-admin \
    -o jsonpath='{.data.admin-token}' | base64 -d
)"
export HUD_ADMIN_TOKEN="$(
  kubectl -n loom-hub get secret loom-secrets \
    -o jsonpath='{.data.HUD_ADMIN_TOKEN}' | base64 -d
)"
export S1C_HUD_URL="${S1C_HUD_URL:-https://hud.flexinfer.ai}"
```

The harness reads `LOOM_MILLS_ADMIN_TOKEN`, not `LOOM_ADMIN_TOKEN`. The
operator token authorizes canary launch and crash leases. The HUD token is
mandatory for fail-safe cleanup of the exact spawn driver and pod.

### 4. Record both reviewed source baselines and protected scopes

```bash
mkdir -p .loom/local
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
export S1C_GITOPS_REPO=~/workspace/platform/gitops
export S1C_LOOM_CORE_REPO=~/workspace/services/loom-core
git -C "$S1C_GITOPS_REPO" fetch --quiet --no-tags origin main
git -C "$S1C_LOOM_CORE_REPO" fetch --quiet --no-tags origin main
git -C "$S1C_GITOPS_REPO" ls-remote origin refs/heads/main | \
  awk '{print $1}' | \
  tee ".loom/local/s1c-gitops-$STAMP.txt"
git -C "$S1C_LOOM_CORE_REPO" ls-remote origin refs/heads/main | \
  awk '{print $1}' | \
  tee ".loom/local/s1c-loom-core-$STAMP.txt"
export S1C_EXPECTED_GITOPS_REVISION="$(tr -d '[:space:]' < ".loom/local/s1c-gitops-$STAMP.txt")"
export S1C_EXPECTED_LOOM_CORE_REVISION="$(tr -d '[:space:]' < ".loom/local/s1c-loom-core-$STAMP.txt")"
git -C "$S1C_GITOPS_REPO" cat-file -e \
  "$S1C_EXPECTED_GITOPS_REVISION^{commit}"
git -C "$S1C_LOOM_CORE_REPO" cat-file -e \
  "$S1C_EXPECTED_LOOM_CORE_REVISION^{commit}"
S1C_FLUX_GITREPOS_A_PATH=".loom/local/s1c-flux-gitrepositories-a-$STAMP.json"
S1C_FLUX_LIST_PATH=".loom/local/s1c-flux-kustomizations-$STAMP.json"
S1C_FLUX_GITREPOS_B_PATH=".loom/local/s1c-flux-gitrepositories-b-$STAMP.json"
kubectl get --raw \
  '/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories?limit=0' \
  > "$S1C_FLUX_GITREPOS_A_PATH"
kubectl get --raw \
  '/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations?limit=0' \
  > "$S1C_FLUX_LIST_PATH"
kubectl get --raw \
  '/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories?limit=0' \
  > "$S1C_FLUX_GITREPOS_B_PATH"
jq -e \
  --arg gitops_revision "$S1C_EXPECTED_GITOPS_REVISION" \
  --arg loom_core_revision "$S1C_EXPECTED_LOOM_CORE_REVISION" '
    .metadata.resourceVersion as $list_rv |
    ([.items[] |
      select(.metadata.name == "apps" or
        .metadata.name == "bootstrap" or
        .metadata.name == "system" or
        .metadata.name == "loom-hub-servers") |
      ([.status.conditions[]? | select(.type == "Ready")]) as $ready |
      {
        name: .metadata.name,
        uid: .metadata.uid,
        resource_version: .metadata.resourceVersion,
        generation: .metadata.generation,
        ready_condition_count: ($ready | length),
        ready_status: ($ready[0].status // ""),
        ready_observed_generation:
          ($ready[0].observedGeneration // 0),
        last_applied_revision: .status.lastAppliedRevision,
        last_attempted_revision: .status.lastAttemptedRevision,
        desired_revision:
          (if .metadata.name == "loom-hub-servers"
           then $loom_core_revision else $gitops_revision end)
      }] | sort_by(.name)) as $targets |
    {
      list_resource_version: $list_rv,
      targets: $targets
    } |
    select(
      (.list_resource_version | type == "string" and length > 0) and
      (.targets | length == 4) and
      ([.targets[].name] ==
        ["apps", "bootstrap", "loom-hub-servers", "system"]) and
      all(.targets[];
        .desired_revision as $desired |
        (.uid | type == "string" and length > 0) and
        (.resource_version | type == "string" and length > 0) and
        (.generation | type == "number") and .generation > 0 and
        .ready_condition_count == 1 and .ready_status == "True" and
        .ready_observed_generation == .generation and
        (.last_applied_revision | type == "string" and length > 0) and
        .last_applied_revision == .last_attempted_revision and
        ($desired | type == "string" and length > 0) and
        (.last_applied_revision | endswith($desired))))
  ' "$S1C_FLUX_LIST_PATH" | \
  tee ".loom/local/s1c-flux-targets-$STAMP.json"
```

Use these exact raw collection endpoints and A -> Kustomization -> B order for
every manual Flux check during the window. The `limit=0` query disables
pagination. Do not replace the Lists with resource-named GETs or object-printer
output; the latter clears the List metadata resourceVersion in the current
client path. The source Lists must contain stable
`flux-system/gitops-gitlab` and `flux-system/loom-core` object/spec/status
identities, current `Ready` and `ArtifactInStorage` conditions, and identical
artifact revisions and digests across A and B. The Kustomization capture must
cross-bind the three platform owners to the first artifact revision and
`loom-hub-servers` to the second.

Review both baseline SHAs before using them. The harness records them as
`gitops_identity.baseline_revision` and
`loom_core_identity.baseline_revision`. The platform
`mills-s1c-gitops-scope/v1` contract covers the complete
`clusters/k3s/flux-system` directory. It also covers the transitive render
closure referenced by `k3s/flux/system`: `k3s/system`, `k3s/net`,
`k3s/coredns`, and `k3s/kube-vip`. Mills, loom-hub, devbox,
security-posture, and relevant image-automation inputs remain protected. The
independent `mills-s1c-loom-core-scope/v1` contract covers
`k8s/base`, the complete render root consumed by `loom-hub-servers`. Each
baseline must be an ancestor of every observed SHA, and every commit on each
ancestry path must preserve its baseline digest. Endpoint-only change/revert is
not accepted. Every protected path must exist at the baseline. Missing commits
trigger one bounded fetch from that repository's `origin/main` without
changing either working tree; missing paths, ancestry failure, or digest
mismatch fail closed.

The full gate requires all four source inputs plus the Flux renderer. The flags
`--expected-loom-core-revision` and `--loom-core-repo` map to
`S1C_EXPECTED_LOOM_CORE_REVISION` and `S1C_LOOM_CORE_REPO`, respectively. The
platform equivalents are `--expected-gitops-revision` and `--gitops-repo`.
`--flux-bin` maps to `S1C_FLUX_BIN` and defaults to `flux`.

Each run's preflight evidence also captures the exact operator and mobile-hud
Deployment UID/generation/full-spec identity, raw and server-normalized
reviewed render digests, renderer version, image tags, running `imageID`
digests, live Flux applied/attempted revisions, both complete reviewed
GitRepository specs and artifact identities, deployment policy checksum, and
spawn ConfigMap UID.
At initial launch, compare both reviewed SHAs above with their Flux
`.status.lastAppliedRevision` suffixes. Later unrelated descendant commits may
advance either suffix only when that source's protected digest is unchanged at
every intervening commit. The harness fails closed on policy or protected-source
drift, any unrelated durable/in-memory work, an active spawn, a rollout
overlap, or an unavailable Loki evidence path.

### 5. Run the automated three-run gate

Choose one canonical agent identity for the complete three-run window. The
default is `claude-code`; use `codex` to exercise the Codex harness. Unsupported
values are rejected before preflight or launch:

```bash
export S1C_AGENT_TYPE=claude-code
export S1C_CRASH_DELAY=15s
export S1C_FLUX_BIN="${S1C_FLUX_BIN:-$(command -v flux)}"
# Codex gate instead:
# export S1C_AGENT_TYPE=codex
# export S1C_CRASH_DELAY=0s
```

```bash
KUBECONFIG=~/workspace/platform/gitops/.kube/k3s.yaml \
  go run ./cmd/mills-workflow-killtest \
  --operator-url "$S1C_OPERATOR_URL" \
  --expected-gitops-revision "$S1C_EXPECTED_GITOPS_REVISION" \
  --expected-loom-core-revision "$S1C_EXPECTED_LOOM_CORE_REVISION" \
  --gitops-identity-mode protected-scope \
  --gitops-repo "$S1C_GITOPS_REPO" \
  --loom-core-repo "$S1C_LOOM_CORE_REPO" \
  --flux-bin "$S1C_FLUX_BIN" \
  --hud-url "$S1C_HUD_URL" \
  --agent-type "$S1C_AGENT_TYPE" \
  --crash-delay "$S1C_CRASH_DELAY" \
  --runs 3 \
  --evidence ".loom/local/s1c-evidence-$STAMP.json"
```

Use `protected-scope` with both local source repositories for the full S1c
gate. The `exact-revision` mode remains a stricter diagnostic compatibility
mode; it does not replace the two reviewed protected-scope contracts for this
runbook.

The harness runs sequentially and stops on the first failure. It writes
`...-run-01.json`, `...-run-02.json`, and `...-run-03.json`, inserting the
suffix before `.json`. The original path receives the aggregate summary.

Each run executes: four-owner-fenced preflight → allocate/checkpoint a stable
run id → perform one complete pre-launch namespace Pod List page with
`limit=5000`, rejecting continuation/overflow and every active spawn-related
Pod while retaining terminal history → anchor the namespace-wide watch to that
accepted List resourceVersion before launch → idempotent canary launch carrying the immutable `agent_type` → require
one exact Running+Ready pod and HUD
status `running` → inspect the exact `devbox` container at one-second cadence
until precisely one non-zombie `sleep 90` process and its unique live
completion-wrapper parent are running → acquire and renew the target-bound crash
lease → recheck all four render-owner identities and those same hold/driver
PID-plus-starttime identities in the final pre-delete callback
→ derive the sole allowed active spawn from immutable journal identity (never
from mutable request-branch metadata) and fail on every other active spawn
→ close the final source bracket and activate the previously paused process
observer
→ reread the final target Deployment -> ReplicaSet -> Pod identity → take the
ordered policy ConfigMap A -> effective policy -> live operator Deployment ->
policy ConfigMap B boundary proof with no later foreground network I/O in the
delete path
→ **CRASH A** (operator Pod DELETE with UID and resourceVersion preconditions)
→ repeat the source/lease/process proof while retaining active bounded process
sampling → take the final ordered GitRepository A -> Kustomization ->
GitRepository B bracket → freshness-check the latest completed sample without
synchronous probe I/O → keep the already-active independent observer running → reread the final
target controller identity → take the same final policy boundary proof →
**CRASH B** (mobile-hud Pod DELETE with UID and resourceVersion
preconditions), with observation spanning request latency →
sample the original process identities, all zombies, and the complete live
hold/driver inventory until the exact pod is confirmed deleted → await `done` →
collect Loki and all-record ConfigMap
evidence → prove final zero-work/immutable identity → stop the watch → verdicts.
Aggregate PASS requires three consecutive runs with `overall=true` and final
state `done`.

## S6-full merging canary (PASS-3)

`-merging` launches the template-v3 canary: the same dual-crash choreography,
plus one journaled `merge('canary')` effect after the gate. The operator's
merge executor is end-to-end idempotent (deterministic branch
`mills-wf-merge/<run-id>` from main, lookup-first file commit under
`.mills-canary/`, adopt-first MR, merged-state-reconciling merge), so a replay
after any crash converges on exactly one landed merge. PASS-3 is evaluated
from real evidence collected after the run reaches done: exactly one MR (any
state) for the deterministic merge branch, in state merged with a merge
commit, and exactly one journal merge success row. Merging mode requires
`-gitlab-token` (or `$GITLAB_TOKEN`) for the verification reads, and the
merge waits on the canary MR's CI pipeline — raise `-terminal-timeout`
accordingly (the loom-core pipeline runs ~25–50 minutes).

Each observation call inside a sample (exact-pod read, probe exec,
post-failure rechecks) is individually bounded by
`ProcessProbeAttemptTimeout` (default 1.2s; healthy in-cluster calls run
100–300ms) under the sample deadline, so a hung call dies early instead of
consuming the whole sampling-gap budget and starving the retry.

A sample attempt that fails on the observation transport — a kubectl read or
exec killed or timed out while the cluster absorbs crash churn — retries on
the next poll tick instead of aborting the window; the sampling-gap contract
(`ProcessMaxSampleGap`) remains the fail-closed arbiter, so a persistent
outage still fails the run, now naming the last underlying transport cause.
Retried attempts never append evidence and are recorded separately as
`post_crash_process_transient_failures` (capped list, exact count). Both v5
gate attempts died 2/3 on exactly one such call each during run 3.

Each process sample's inventory enumeration re-visits an absent original
hold/driver once, through the same full match contract (snapshot, exact argv,
snapshot), before the sample is emitted. A process-tree transition — e.g. the
in-pod reaper exiting ~78s after CRASH B when the recovered mobile-hud
re-attaches — can straddle the loop's multi-read snapshot and drop a
provably-alive original from the inventory for exactly one sample while the
same sample's known-identity read still binds the original (PID,starttime)
alive (v5 run 1 died at sample 313: driver `state="S" live=[]`). The recheck
runs after the enumeration, when the transition has settled, so a live
original re-matches; a dead, replaced, argv-mismatched, or identity-drifted
process still fails every branch, and the recheck never fires when the
inventory already holds any candidate, keeping replacement and overlap
detection intact.

After each replacement becomes Kubernetes Ready, the external route can still
return a transient transport error or 5xx while ingress endpoints converge.
Lease release retries only those failures, with context-aware exponential
backoff and a hard 30-second deadline; the harness does not advance until the
release is confirmed. `204` means released and `404` means the lease is already
absent or expired. A different active token returns `409`, and every other 4xx
is terminal, so authorization or concurrent-gate conflicts remain fail-closed.

After the selected agent returns successfully, the spawn driver owns a bounded,
foreground `sleep 90` completion hold and does not terminalize the durable spawn
until that hold exits. This keeps the crash window structural and independent
of whether an agent understands yielded shell-session semantics. Pod readiness
and HUD `running` are still insufficient: they can become true while the agent
is starting. The harness scans `/proc` in the exact spawn pod and authorizes
CRASH A only after it sees exactly one live process whose argv is precisely
`sleep 90` and exactly one live completion wrapper that is its parent. Process
identity is the pair `(PID, /proc/<pid>/stat starttime)`, not PID alone, so PID
reuse cannot masquerade as continuity. The harness requires the same hold and
wrapper identities immediately before CRASH A and CRASH B.

The post-CRASH B observer takes one synchronous process sample before the final
source bracket, then pauses without issuing Kubernetes reads. Each sample's
`observed_at` is conservatively fixed at probe start, so probe latency consumes
the same bounded gap instead of making evidence appear fresher. After the
bracket closes, a no-I/O freshness check requires the sample to remain inside
the configured maximum gap. A slow source fence therefore fails before deletion.
For CRASH A, the observer activates immediately after the final source bracket
closes and its paused sample passes freshness. It therefore remains paused—and
performs no observer I/O—through the source bracket, then samples actively
through the final target controller census, policy bracket, DELETE latency, and
replacement rollout. CRASH B reuses that already-active observer.

The default poll interval is 250 ms. The versioned evidence contract hard-caps
`post_crash_process_max_gap_ms` at 3000 ms; configuration may tighten that bound
but cannot relax it. Each probe reads the original PID states and starttimes
before command lines, so Linux zombies remain visible even when
`/proc/<pid>/cmdline` is empty. It also records every zombie PID in the
container, not only zombies matching the canary argv. The same sample
inventories every live exact hold and completion wrapper. Any zombie, unknown
state, PID/starttime drift, overlap, or replacement process fails immediately
when observed.

This process evidence is deliberately a bounded sampling proof, not an
uninterrupted trace. A process whose entire lifetime falls between two completed
inventories can be missed. The recorded maximum gap is therefore the explicit
worst-case blind window (never more than 3 seconds; the default scheduling
target is 250 ms). PASS proves that every retained sample satisfies the process
invariants and that no sampling gap exceeds that bound; it does not prove the
absence of an arbitrarily short-lived process between samples. Pod-incarnation
coverage is stronger because it uses the separate Kubernetes resourceVersion
watch described below.

The synchronous first sample must still prove both original process identities
non-`MISSING` and live; otherwise CRASH B is not authorized. Later normal
completion is represented explicitly. A `MISSING` process must have starttime
zero and an empty corresponding live inventory. A missing driver also requires
the hold to be missing; a live driver may briefly remain after its hold exits.
Once either original identity is `MISSING`, every later sample must keep that
identity `MISSING`; an apparent resurrection is invalid evidence.
Sampling ends only when Kubernetes confirms deletion of the exact pod. Every
gap from CRASH B through that confirmation must stay within the recorded bound.
Eventual cleanup does not erase an earlier violation.

The exact-pod resourceVersion watch is a separate continuous proof. Its initial
List resourceVersion must be non-empty, it must start no later than the initial
hold proof, and it must end no earlier than the process observer's exact-pod
deletion confirmation.

This stronger process contract requires the pod-owned execution supervisor/reaper
(S4) to preserve or safely complete the original process pair without
controller-owned replay. With S4 deployed and `LOOM_SPAWN_SUPERVISED_EXECUTION`
on, the agent turn runs under a detached in-pod reaper and mobile-hud RE-ATTACHES
on restart (it tails the reaper's log and collects the durably recorded outcome),
so the original `(hold, wrapper)` `(PID, starttime)` is preserved across both
crashes and the run is expected to PASS. The pre-S4 HUD recovery path re-drove the
CLI after controller restart (replacing the wrapper/hold with new identities) and
is expected to FAIL this contract; a run against an image WITHOUT S4, or with the
supervisor toggled off, cannot count toward the gate.

Expected output ends with `S1c gate PASSED — 3 consecutive runs; summary: …`
and exit 0. `--run-id ... --runs 1` is recovery-only and can never emit a gate
PASS. Before printing the gate PASS, the live CLI reopens the summary and all
three run artifacts through the canonical verifier. If that verification
fails, it atomically rewrites the summary with `overall=false` and exits with
an error.

The frozen kubeconfig is credential-bearing even though evidence contains only
public hashes. Its temporary directory is mode `0700` and file mode `0600`.
Normal exit removes it; removal failure is fatal and the CLI cannot print final
gate PASS. `SIGKILL` cannot execute deferred cleanup, so after a killed harness
verify that no run remains active and explicitly remove any private
`$TMPDIR/mills-s1c-kubeconfig-*` residual before another attempt.

### 6. Verify the serialized three-run gate

Do not use a file glob or a hand-written `jq` expression as gate authority.
The canonical verifier starts from the exact summary path, opens only its three
derived sibling evidence paths, and treats every serialized field as untrusted:

```bash
SUMMARY=".loom/local/s1c-evidence-$STAMP.json"

go run ./cmd/mills-workflow-killtest \
  --phase verify \
  --evidence "$SUMMARY"
```

A qualifying result prints `S1c gate evidence VERIFIED — three distinct runs`
and exits 0. Any verifier error keeps the gate closed; do not rename individual
run files, replace a missing run, or assemble a summary with a glob.

The verifier fails closed unless all of the following are re-derived from the
four exact JSON files:

- the summary is a regular, non-symlink strict JSON file for exactly three
  completed, clean runs;
- the summary carries the current gate-binding contract, one 32-character
  lowercase hexadecimal gate ID, and a non-zero gate start timestamp;
- each exact `run-01` through `run-03` file is a regular, non-symlink file at
  the canonical path derived from the summary;
- every run is bound to that exact gate, start time, run count, and position;
  its server-persisted run ID must be the canonical child ID for that gate;
- each summary SHA-256 must match the exact final bytes parsed from the same
  single regular-file open of its run file. Run 1
  has no predecessor, while runs 2 and 3 must name the actual preceding file's
  SHA-256 in both the summary and their embedded gate binding;
- every wrapper preflight and serialized verdict equals its evidence-backed
  pure recomputation, including durable delete intent/receipt, renewed lease,
  process authorization, duplicate, zombie, provenance, and crash-safety checks;
- run IDs and spawn IDs are non-empty and distinct, while agent, workload,
  policy, spawn ConfigMap, all four Flux owner identities, and both
  GitRepository UID/generation/resourceVersion, reviewed complete spec,
  condition generation, artifact revision/digest, and protected-source
  identities remain stable and correctly cross-bound;
- both Deployment namespace/UID/generation/full-spec identities and their
  exact reviewed render, Flux transform, source baseline, and renderer
  identities remain stable. Deployment resourceVersion may advance only as
  non-authorizing status evidence;
- the explicit `loom-mills/loom-mills-policy` review remains stable; its
  rendered/live complete-payload SHAs are equal, its source SHA equals the
  report and operator Deployment checksums, and its apps Flux/platform
  baseline/renderer bindings match the operator Deployment review;
- both crash records contain the current policy delete-boundary contract with
  strictly ordered A/effective/operator/B request-start observations and a
  later completion before DELETE; both ConfigMap samples retain the exact
  reviewed live identity/payload and parsed policy, the effective policy stays
  closed/k8s-only, the stable live operator Deployment remains identical to
  immediate preflight, and ConfigMap B starts within the ten-second freshness
  budget;
- every initial, CRASH A, CRASH B, and final source proof uses
  `protected-scope`; `exact-revision` evidence is diagnostic-only and rejected;
- run 1 begins no earlier than, and within the bounded gap after, gate
  allocation. The three evidence windows are strictly ordered,
  non-overlapping, and each next run begins within that same bounded gap. Each
  window begins at its initial GitRepository-A completion and ends at its final
  GitRepository-B completion;
- terminating Kustomizations or GitRepositories, overlapping
  A/Kustomization/B brackets, and an artifact digest change at an unchanged
  normalized revision are rejected.

Unknown JSON fields, trailing JSON values, copied identities, summary/evidence
mismatches, broken predecessor links, mixed historical gates, reordered or
out-of-window runs, symlink substitution, and hand-edited verdicts all
invalidate the gate. A path containing redundant segments cannot redirect the
verifier away from the canonical sibling file.

This verifier proves internal consistency and resists accidental artifact
mixing; the JSON files are not signed attestations. Archive the summary and
three run files together in write-once storage if the threat model includes a
party able to rewrite every artifact and recompute all derived values.

### 7. PASS criteria (all must hold — plan §3.4)

- **PASS-1**: the exact same non-zombie `sleep 90` PID/starttime and its unique
  live completion-wrapper parent PID/starttime are proven in the exact spawn
  pod before CRASH A and revalidated immediately before both deletions.
  CRASH B's active authorization sample remains fresh after the final source bracket. Post-crash
  observation stays within a recorded maximum gap no greater than 3000 ms until
  exact pod deletion. No retained sample observes a zombie PID, overlapping
  pair, or replacement hold/driver identity. Exactly one pod UID incarnation
  for the spawn id is observed across a continuous Kubernetes resourceVersion
  watch spanning both crashes and final proof, AND a durable-path dedupe log line (operator
  `re-attaching to in-flight spawn`, mobile-hud `idempotent spawn … already
  exists`, or deterministic-name Kubernetes `AlreadyExists` adoption) is
  attributed by Loki to the exact replacement pod at or after that component's
  crash timestamp and bound to the exact spawn id.
  "One pod" alone is insufficient — a failed racing Create also leaves one.
- **PASS-2**: exactly one `success` journal row for the agent step key; no
  quarantine.
- **PASS-3**: not exercised — the S6-min canary stops at the gate, pre-merge.
  Asserted by S6-full's merging re-run.
- **PASS-4**: the success row carries the provenance required by the immutable
  agent identity: `claude-code` requires `cost_source=real`; `codex` requires
  `cost_source=estimated`. An empty or different value fails the gate.
- **PASS-5**: distinct spawn success rows == 1 (`agent()` calls in the canary).
- **PASS-6**: all three runs finish in state `done`; every ConfigMap idempotency
  key maps to one completed spawn, with no live pod, zombie, replacement
  completion wrapper, or replayed hold.
- **PASS-7**: agent identity, operator and mobile-hud image tags/digests, the
  exact policy source checksum and complete reviewed/live ConfigMap payload,
  Deployment namespace/UID/generation/complete live-and-reviewed
  spec identities and render bindings, and each selected controller's complete
  Pod namespace/name/UID/resourceVersion, container name/ID/restart count/start
  time, and controller ReplicaSet name/UID/resourceVersion -> Deployment
  name/UID lineage remain bound. The spawn ConfigMap UID, both reviewed
  source baselines, both reviewed GitRepository specs and artifact identities,
  and both protected-scope digests remain identical across the full window.
  Each observed source SHA may advance only through descendants whose entire
  ancestry preserves its protected digest. `apps`, `bootstrap`, and `system`
  each pass the platform contract and bind to `gitops-gitlab`;
  `loom-hub-servers` passes the loom-core contract and binds to `loom-core`.
  Start/end preflight and immediate pre-delete v5 source brackets must all pass.
- **PASS-8**: the global admission barrier stays closed, the workflow flag stays
  open, and every durable/in-memory activity count except the single target
  workflow/spawn is zero immediately before both deletions. A server-side
  lease blocks all other admissions and is renewed immediately before each
  bounded UID-and-resourceVersion-preconditioned Pod DELETE. After each final
  target controller reread, an ordered policy A/effective/operator/B bracket
  remains exact, closed, stable, and fresh, with no later foreground gate I/O
  before that DELETE; independent bounded process sampling continues.

These criteria prove stable spawn and process identity plus one durable journal
effect. They reject controller-owned agent-turn redrive. They do not establish
effect-level exactly-once semantics for external tools.

### 8. Record the result

- Attach the evidence JSON to the plan. Record the spawn/journal crash proof as
  passed or failed, and keep the separate effect-level exactly-once assumption
  open until durable agent-turn/tool idempotency exists.
- Record the 2026-07-14 diagnostic as failed: it observed the original hold as
  a zombie and a replacement wrapper/hold after CRASH B. It cannot be reused as
  one of the three S1c runs after the supervisor/reaper change.
- Any failed or invalidated run keeps the gate closed. A duplicate spawn,
  duplicate journal effect, or deterministic-replay divergence rejects the
  Layer-3 bet and triggers the declarative-DAG fallback (`.loom/132 §7` option
  b). Transport, auth, or deployment-identity failures require remediation and
  a fresh three-run gate; they are not effect-level exactly-once evidence.

## MR-awareness and queued-proof scenarios

The evidence-only scenarios are additive to the deployed S1c gate. Run them
with `-scenario=mr-awareness` or `-scenario=queued-proof` and pass the JSON
document with `-evidence`. Both documents bind `scenario`, `captured_at`, and
`deadline`; MR awareness additionally binds `repo`, positive MR `iid`, and
`source_branch`, while queued proof binds one `run_id` and `backlog_id` across
the ordered `queued`, `picked_up`, and `paused` transitions. Every observation
timestamp must be ordered and no later than `captured_at`.

The decoder rejects unknown fields, malformed or trailing JSON, mixed scenario
payloads, missing or contradictory identities, duplicate observations, stale
captures, and expired deadlines. Success and failure both emit one JSON report:
`verdict` is `PASS` or `FAIL`; failures also include a stable `reason_code` and
detail. Duplicate detection is intentionally local to the supplied document;
these deterministic checks do not create or migrate stored state.

To roll back these scenarios, revert the harness commit and redeploy the CLI.
No data cleanup or store migration is required. Existing S1c evidence remains
valid because scenario dispatch is isolated behind the explicit `-scenario`
flag. If a scenario fails, preserve its FAIL report, correct the producer, and
collect fresh evidence; never reuse or manually edit failed evidence to obtain
a PASS.

## Rollback

1. **Keep global `enabled: false` while cleaning up.** Do not reopen admission
   first. If the harness did not finish its automatic cleanup, stop the exact
   HUD spawn before terminalizing the workflow:

   ```bash
   curl -fsS -X POST \
     -H "X-Admin-Token: $HUD_ADMIN_TOKEN" \
     -H "Authorization: Bearer $HUD_ADMIN_TOKEN" \
     "$S1C_HUD_URL/api/agent/spawn/<spawn_id>/stop"
   ```

2. There is no workflow-run DELETE API. If a run is still `running` or
   `paused`, terminalize it with the admin endpoint rather than editing SQLite:

   ```bash
   curl -fsS -X POST \
     -H "Authorization: Bearer $LOOM_MILLS_ADMIN_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"reason":"S1c cleanup: run did not reach done"}' \
     "$S1C_OPERATOR_URL/api/mills/workflow/runs/<run_id>/fail"
   ```

   Keep the terminal journal row as evidence.
3. Require the exact ConfigMap record to be terminal, no managed spawn pods,
   and `GET /api/mills/safety/quiescence` to report all-zero twice. The stop
   endpoint waits for the owned driver to exit; a failed late-pod cleanup is a
   failed rollback, not permission to reopen admission.
4. With the fleet drained, change the ConfigMap to global `enabled: false` and
   `workflows.enabled: false` **without restarting**, commit/push/reconcile,
   and verify the effective policy. Then bind the Deployment checksum to that
   exact closed policy and roll while both switches remain closed. Resume any
   image automation suspended for the bounded window. A failed or invalidated
   gate stops here: do not reopen global admission. Only the separate PASS
   closure may stage the normal `enabled: true`, workflows-disabled checksum
   behind the closed barrier and then apply that final ConfigMap-only reopen.
5. The canary branch `mills-wf/<run_id>` in loom-core can be deleted; the
   canary never opens an MR or merges.
6. Both crashed deployments self-heal as stable singletons. The operator uses
   `Recreate`; mobile-hud also uses `Recreate`, and the harness refuses to delete
   during any unexpected old/new overlap. Confirm `kubectl -n loom-mills get pods` and
   `kubectl -n loom-hub get pods -l app=mobile-hud` each select exactly one
   non-terminating Ready pod.
7. **Supervisor rollback tradeoff (S4).** Two rollback/degradation shapes can
   produce a **duplicate concurrent agent turn** via the liveness-preserving
   re-drive fallback; this is an inherent tradeoff of keeping liveness when
   continuity is lost, and operators should expect it rather than treat it as a
   new bug:
   - **Forward-rollback:** pods spawned by an S4 controller carry a live in-pod
     reaper, but a rolled-back (pre-S4, or `LOOM_SPAWN_SUPERVISED_EXECUTION=false`)
     controller does not know how to re-attach — its recovery re-drives a fresh
     turn in the same pod while the original reaper-owned turn may still be
     running. The turns share one workspace; the journal's success-only
     `readThrough` still delivers at most one workflow outcome, but external
     side effects of the overlapped turns can duplicate.
   - **Reaper-only death (e.g. OOM-kill of the reaper process while the agent
     pod survives):** recovery probes the reaper as dead with no recorded
     outcome and re-drives; if the agent/hold process pair somehow outlived the
     reaper, the same overlap applies.

   During rollback, prefer draining (step 3's quiescence proof) before flipping
   the controller image/flag, and treat any spawn that was `running` across the
   flip as suspect: verify its pod is gone (or stop it via step 1) before
   reopening admission. Outcome exactly-once at the workflow layer is preserved
   in all cases; the duplication risk is confined to the agent turn's external
   side effects, the same at-least-once semantics the legacy re-drive path
   always had.

## Contacts

- Owner: Cody Blevins (cody.r.blevins@gmail.com)
- Plan: `.loom/134-plan-mills-workflow-runtime-sequencing-2026-06-06.md`
- Runtime: `pkg/mills/workflow/` (`runtime.go`, `scheduler_min.go`)
- Spawn dedupe: `internal/spawn/controller.go` (K8sConfigMapStore, ns `devbox`)
