# Product spec — Mills implement-stage on Harvester KubeVirt VMs (`harvester-vm` devbox backend)

- **Date**: 2026-05-25
- **Lineage**: Converges from
  `.loom/brainstorm-vms-for-agents-2026-05-25.md` (Combo B recommendation:
  per-run mills VMs + sharded tests). Slice 0 kill-test PASSED (~46/56
  mills runs escalated at container-infra stages). User chose F1 (build on
  harvester) over F8 (rent exe.dev).
- **Scope**: F1 + F2 narrow to mills' implement+tests stages. Out of scope:
  F7 (sharded loom-core tests), F4 (NVMe for flexinfer), F6 (auto-batteries
  edges), local devbox migration. Separate specs if/when prioritized.

---

## Riskiest assumption + kill-test

**Load-bearing assumption (original)**: Harvester `harv-r730xd-01` can host
2–4 concurrent mills VMs at ≤90s cold-boot without starving the existing
k3s cluster sharing the box.

**Status**: **PASSED 2026-05-25**.
Full results: `.loom/local/handoffs/mills-harvester-vm-killtest-2026-05-25.md`.
4 concurrent VMs reached `Phase=Running` at T+53s wall-clock, cloud-init
complete at uptime 40.9s. Node pressure peaked at CPU 72% / MEM 78%. Zero
disruption to 16 existing VMs.

**Refined assumption (Slice 1.5)**: cloud-init + `qemu-guest-agent` is
sufficient for KubeVirt to report VMs' DHCPv4 IPs back to the operator;
SSH-from-outside works on the per-VM keypair.

**Status**: **PASSED 2026-05-25**.
Full results: `.loom/local/handoffs/mills-harvester-vm-slice15-2026-05-25.md`.
VM `mills-killtest-v4-1` allocated `192.168.50.87` via DHCP at T+110s,
`AgentConnected=True`, SSH worked. 130s total cold-boot (apt-get path); the
pre-baked image will close that to ≤60s.

---

## Why a new backend (not modify the existing `k8s` backend)

Existing `internal/devbox/backend/k8s_*.go` builds a per-project sandbox
image with buildah-in-pod, runs `sleep infinity` in a long-lived pod, and
mills `kubectl exec`s in. Two prod failure clusters (per Slice 0 kill-test):

- **22%** buildah-in-pod conflicts (Docker-in-Docker class)
- **21%** pod GC races during reconciliation
- **Structural 100%** (every implement spawn): pod-local git-clone is
  invisible to operator's worktree path. MR !525 partially fixed (added
  `-b <branch>`) but the cross-pod-boundary fragility remains.

KubeVirt VM is the natural fix for all three:
- Buildah-in-VM is just buildah-on-Linux (no nested-cgroup issues)
- VMI lifecycle is its own object (no pod-GC race)
- VM's disk IS the worktree (no cross-boundary mount)

**New backend alongside existing**, because:
- Both substrates coexist during canary rollout. Mills uses `harvester-vm`;
  agents using devbox locally keep `docker`/`k8s`.
- Lifecycle primitives differ (cloud-init vs init-container; SSH vs
  kubectl exec; VMI vs Pod). Abstracting would cost more than parallel
  implementations.

---

## Design

### Substrate

- **Cluster**: Harvester v1.5.1 on `harv-r730xd-01` (1 node, 40 vCPU /
  264 GB RAM), KubeVirt v1.4.0, CDI installed
- **Kubeconfig**: `~/workspace/platform/gitops/.kube/harvester-admin.yaml`
- **Namespace**: `default` (per Slice 1.5 evidence — cross-namespace CDI
  clone is RBAC-blocked; the Harvester pattern is per-VM PVC in same NS
  as the image)
- **Base VM image**: one curated Ubuntu 24.04 qcow2 with preinstalled:
  - Toolchain: Go 1.24, Python 3.12 + uv, Node 22 + pnpm, buildah, git,
    kubectl, glab
  - **qemu-guest-agent** enabled+started at boot (Slice 1.5 confirmed
    this is required for KubeVirt to see DHCPv4 IPs)
  - openssh-server enabled+started, password auth off
  - Stored as Harvester `VirtualMachineImage` named
    `mills-devbox-base-YYYYMMDD`. Auto-generated storage class
    `longhorn-image-mills-devbox-base` is what per-VM PVCs reference.
- **Per-run shape**: 2 vCPU / 4 Gi RAM / 20 Gi block-mode PVC
- **Networking**: `default/lan10g` plain bridge — NO Whereabouts.
  Slice 1.5 surfaced that the workspace's `192.168.50.0/24` LAN has no
  free space for dynamic IPAM (gateway + DHCP + static-VM range + MetalLB
  fill it). DHCP from the router + qemu-guest-agent reporting solves the
  end-to-end problem with zero new platform primitives.
- **Auth**: ed25519 keypair per-VM, public key via cloud-init, private
  key returned to operator. Lifetime = VM lifetime.
  - Fallback: serial-console writeback via
    `/var/run/kubevirt-private/<uid>/virt-serial0-log` (validated in
    Slice 0 memo as proof-of-life when SSH wasn't reachable).

### Backend interface implementation

New file `internal/devbox/backend/harvester_vm.go` implementing existing
`Backend` interface (`internal/devbox/backend/backend.go:14-50`).

| Interface method | Pod backend | KubeVirt backend |
|---|---|---|
| `Build` | `buildah build` pod + push | **no-op** — one shared base image |
| `Start` | Create Pod, wait Running | Create VMI, wait `Phase=Running` + `AgentConnected=True`, return SSH endpoint |
| `Exec` | `kubectl exec` | SSH using per-VM keypair |
| `Stop` | Delete Pod | Delete VMI (cascades PVC if owned) |
| `Status` | Pod phase | VMI phase |
| `Health` | `kubectl version` | KubeVirt API + CRDs reachable |
| `Pause/Resume` | unsupported | KubeVirt `virtctl pause/unpause` (free upgrade) |
| `ReadFile/WriteFile` | `kubectl exec cat`/`sh -c` | `scp` over same SSH |
| `CleanupBuilds` | delete old pods + ConfigMaps | delete orphan PVCs (24h+) |

Size estimate: ~600 LOC + tests. Reuses `backend.go` shared types verbatim.

### Backend selection

Two-line change in `cmd/mcp-devbox/main.go:55-60` to add `"harvester-vm"`
case → `NewHarvesterVMBackend(cfg)`. Config struct gets
`HarvesterKubeconfigPath`, `BaseImageName`, `Namespace`,
`DefaultVCPUs`, `DefaultMemMi`, `WarmPoolSize`.

### Mills integration

Per-stage substrate selection via new `policy.yaml` field:

```yaml
pipeline:
  stage_substrate:
    plan_slice:      k8s            # buildah image build, stays on k3s
    research:        k8s            # cheap read-only
    implement:       harvester-vm   # NEW — the high-value flip
    tests:           harvester-vm   # NEW — devbox quality_gate runs here
    pr_self_review:  k8s            # cheap LLM call
    mr:              k8s            # GitLab API call, no sandbox
```

Default-off; opt-in via canary item label first.

### Warm pool (Slice 3)

Cold-boot from pre-baked image projects to ~30-60s. To hit ≤30s warm-boot,
operator maintains `N=2` paused VMIs of the base image. On `Start`,
unpause one + hot-plug a fresh cloud-init seed disk. Saves ~30s.

Not Slice 1.

---

## Implementation plan

### Slice 0 — Capacity kill-test (no code) ✅ PASSED 2026-05-25

Memo: `.loom/local/handoffs/mills-harvester-vm-killtest-2026-05-25.md`

### Slice 1.5 — Base VM acceptance + plumbing ✅ PROVISIONAL PASS 2026-05-25

Memo: `.loom/local/handoffs/mills-harvester-vm-slice15-2026-05-25.md`.
Carved out of Slice 1 because original "use stock Ubuntu + virtctl ssh"
assumption broke. Live findings:
- Cross-namespace CDI clone blocked → use same-NS PVC pattern
- Whereabouts NAD was fundamentally misconfigured for this LAN → MRs
  !178 (CIDR fix) + !179 (NAD deprecated, resource removed)
- DHCP + qemu-guest-agent works end-to-end

**Remaining work for Slice 1.5**: pre-baked image build pipeline
(`platform/gitops/harvester/mills-devbox-base/`). Closes the 60s
boot-time spec target. ~2-4h of virt-customize iteration.

### Slice 1 — `harvester-vm` backend, cold-boot path only (NEXT)

- `internal/devbox/backend/harvester_vm.go` + tests
- Unit tests: `fake.NewSimpleClientset` against KubeVirt CRD scheme
- Integration test gated behind `HARVESTER_KUBECONFIG` env
- Wire into `cmd/mcp-devbox/main.go` + `manager.go`
- Exec primitive: SSH primary, `--exec-mode=serial-fallback` for
  network-degraded cases

### Slice 2 — Mills opts in for `mills-canary-*` items

- `pipeline.stage_substrate` field in policy ✅ **SHIPPED 2026-05-27**: `pkg/mills/policy.PipelinePolicy.StageSubstrate map[string]string` + `Policy.SubstrateForStage(stage) string` accessor + validation (keys ∈ {plan_slice, research, implement, tests, pr_self_review}, values ∈ {k8s, harvester-vm}) + tests. Default fallback is `k8s` (the prod baseline). YAML roundtrip pinned by `TestPolicy_StageSubstrate_Roundtrip`. **No runtime consumer yet** — the spawn dispatcher still picks backend at startup via `DEVBOX_BACKEND` env. Slice 2.5 wires `SpawnWorker` (the spawn-driven dispatcher, not `GitLabWorker` — that runs mr/ci_watch/merge/cleanup which have no sandbox) to thread the per-stage substrate hint through `SpawnRequest` so the HUD spawn API can pick the matching backend at pod-start time.
- **Slice 2.5 (next)**: thread `Policy.SubstrateForStage(jc.Stage)` through `SpawnWorker.Run` into `pipeline.SpawnRequest`, add a `Substrate string` field on `SpawnRequest` + `hudSpawnRequestBody` + `internal/spawn.Request`, and have the HUD spawn orchestrator pick the matching backend instance (requires multi-backend initialization in `cmd/mcp-devbox` + a backend lookup in `internal/hud/spawn.go`).
- Acceptance: 1 successful end-to-end auto-merge of a canary item via
  `harvester-vm` (prod currently 0/56)

### Slice 3 — Warm pool

- Operator controller maintains `N=2` paused VMIs
- Acceptance: `Start` p50 ≤30s, p95 ≤60s

### Slice 4 — Generalize substrate selection

- After 7 days of green canaries, flip defaults
- `k8s` stays as fallback when `harvester-vm` health fails
- Acceptance: 7-day prod escalation rate at `plan_slice`/`tests` <30%
  (was 82%)

---

## Explicit non-goals

- **Not migrating devbox for local dev**: claude-code on dev machines
  keeps `docker` backend.
- **Not building a second Harvester host**: Slice 0 confirmed capacity.
- **Not adding Kata RuntimeClass to k3s**: would change every k3s node's
  containerd config; out of scope.
- **Not solving F7 (sharded loom-core tests)**: same substrate could
  host it later; separate spec.
- **Not solving F5 (token-efficient MCP audit)**: zero-risk parallel
  work; ships independently.
- **Not introducing Whereabouts IPAM on `.50/24`**: LAN has no free
  space; new bridge + fresh subnet is the right shape if dynamic IPAM
  ever needed.

---

## Refinements from live testing (added after MRs !178/!179)

1. **No Whereabouts NAD**. Spec originally called for new
   `mills-devbox-lan10g` with whereabouts IPAM. Live test showed
   `192.168.50.0/24` is fully partitioned (gateway, DHCP, static VMs,
   MetalLB) — no room. Replaced with `default/lan10g` plain bridge.
2. **DHCP + qemu-guest-agent IS the IPv4 reporting story**. Not SSH +
   static IP. Bake qemu-guest-agent into the base image so KubeVirt sees
   the IP from the guest.
3. **Same-namespace PVC, not cross-namespace DataVolume**. CDI cross-NS
   clone is RBAC-blocked. Per-VM PVC with `longhorn-image-*` storage
   class is the Harvester pattern.
4. **Capacity question closed**. 4 concurrent VMs at 72% CPU / 78% MEM,
   zero existing-VM disruption. Slice 3 warm pool (N=2) is comfortable.

## Sources

- Brainstorm: `.loom/brainstorm-vms-for-agents-2026-05-25.md`
- Slice 0 (capacity) memo: `.loom/local/handoffs/mills-harvester-vm-killtest-2026-05-25.md`
- Slice 1.5 (base VM) memo: `.loom/local/handoffs/mills-harvester-vm-slice15-2026-05-25.md`
- Failure classification: `.loom/local/handoffs/mills-autonomy-killtest-2026-05-24.md`
- Structural bug evidence: `.loom/119-diagnosis-mills-spawn-no-diff-2026-05-25.md`
- Existing devbox K8s backend integration points:
  - `internal/devbox/backend/backend.go:14-50` (Backend interface)
  - `internal/devbox/backend/k8s_objects.go:13-179` (pod-spec shape)
  - `internal/devbox/backend/k8s_runtime.go:40-170` (Start/Exec)
  - `internal/devbox/backend/k8s_build.go:37-101` (build step we skip)
  - `cmd/mcp-devbox/main.go:55-60` (backend dispatcher)
  - `cmd/mcp-devbox/manager.go:178-228` (backend lifecycle)
- Platform MRs landed today:
  - https://gitlab.flexinfer.ai/platform/gitops/-/merge_requests/178
  - https://gitlab.flexinfer.ai/platform/gitops/-/merge_requests/179
