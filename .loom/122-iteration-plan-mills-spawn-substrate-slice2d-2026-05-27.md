# RALPH Iteration Plan — HUD spawn orchestrator multi-backend init + per-spawn lookup (Slice 2d)

- **Date**: 2026-05-27
- **Lineage**: `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` (Slice 2). Slices 2a (`2a9c6c3a`), 2b (`a6979762`, !564), and 2c (`57052ce0`, !566) shipped today. Slice 2d closes the spec's "backend lookup in `internal/hud/spawn.go`" item so a request with `Substrate="harvester-vm"` actually routes pod-lifecycle calls to the HarvesterVMBackend instead of the K8sBackend. Slice 2e (mcp-devbox multi-backend init) + a HarvesterVMBackend env-propagation fix (see Risks) follow.

## Review

- Roadmap milestone: `.loom/45`, Slice 2 "Mills opts in for `mills-canary-*` items"
- Spec sections:
  - "Slice 2c (next, then 2d)": "have the HUD spawn orchestrator initialize the matching backend at pod-start time (requires multi-backend initialization in `cmd/mcp-devbox` + a backend lookup in `internal/hud/spawn.go`)"
  - Slice 2 acceptance: "1 successful end-to-end auto-merge of a canary item via `harvester-vm`" — this slice is necessary but not sufficient for that.
- Prior decisions to preserve:
  - `internal/spawn.Request.Substrate` (Slice 2c) is the canonical signal; persisted via `K8sConfigMapStore` already.
  - K8sConfigMapStore still needs a K8s `Clientset()` — wire it from the default backend, not per-spawn.
  - `streamExecCapable` is K8s-only; harvester-vm spawns fall through to buffered `Exec` (acceptable).
  - `agent_secret_*` Secret env vars (`agentSecretEnvVars`) are K8s-only on the wire; the harvester-vm backend currently ignores `StartOpts.SecretEnv`. **Not in scope to fix here** — call out as Risk + follow-up slice.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The pod-lifecycle hot path on `HarvesterVMBackend` — `Build` (no-op), `Start` (VM create + wait Running), `Exec` (SSH), `Stop` (VM delete) — works correctly when driven by the existing `SpawnOrchestrator.runSpawn` code paths that were written against the K8sBackend pod contract. Specifically: `Build` returns a `BuildResult.ImageTag` the orchestrator can hand back to `Start.ImageTag`; `Start.StartResult.ContainerID` round-trips to subsequent `Exec(ContainerID=…)` / `Stop(id=…)` calls; `Exec` honors `ExecOpts.Env` (it does, via the SSH shell prefix).

**Kill test** (post-merge, on a configured operator with both backends): construct a `pipeline.SpawnRequest{Substrate: "harvester-vm", …}` via `POST /api/mobile/v1/agent/spawn` with `substrate=harvester-vm` and a trivial prompt; observe (a) `kubectl get vm -n default` on the harvester cluster shows a freshly created VM matching the spawn name; (b) `kubectl get pod -n loom-spawn -l loom.dev/spawn-id=…` on the k3s cluster shows **no** matching pod; (c) `Stop` request from the spawn admin API deletes the VM. ~10 min including VM cold boot. Agent-runtime success (claude CLI executing inside the VM and reporting telemetry) is **NOT** in this kill test — the env-propagation gap means it may fail; that's Slice 2d.5.

**Failure mode if the assumption is wrong**: the orchestrator might call `Build` and then pass a Dockerfile-shaped image tag to the VM backend's `Start`, or pass a pod name to a VM `Stop` it can't resolve. Likely surface area: ContainerID mismatch between Start and subsequent Exec/Stop, or Build returning a tag that's not a registered VirtualMachineImage. Mitigation: explicit `BaseImageName` in the harvester-vm backend config; assert in tests.

**Status**: not run.

## Align

- Slice name: **HUD SpawnOrchestrator routes pod-lifecycle calls via `substrateBackend(substrate)` instead of a single `o.backend`**
- Scope in:
  1. `internal/hud/spawn.go`: replace `backend backend.Backend` with `backends map[string]backend.Backend` + `defaultSubstrate string`. Add `substrateBackend(substrate string) backend.Backend` helper that:
     - returns `o.backends[substrate]` when found,
     - falls back to `o.backends[o.defaultSubstrate]` when substrate is empty,
     - logs a warning + returns the default when substrate is unknown (so a misconfigured operator surfaces in logs without silently wedging).
  2. Update all 9 `o.backend.<Method>` callsites in `internal/hud/spawn.go` to use the helper:
     - `runSpawn` line 543/560/738/864/920 → `o.substrateBackend(req.Substrate)`
     - `runSpawn` line 723 (`streamExecCapable` cast) → cast on `o.substrateBackend(req.Substrate)` (harvester-vm fails the cast, falls through to buffered Exec — already the existing else-branch)
     - Stop paths line 997/1109/1230 → `o.substrateBackend(state.Request.Substrate)`
  3. Update `resumePreRuntimeSpawns` guard (line 268) to check `len(o.backends) > 0` instead of `o.backend == nil`.
  4. `NewSpawnOrchestrator` signature change: takes `backends map[string]backend.Backend, defaultSubstrate string` instead of `b backend.Backend`. The `streamExecCapable` cast for K8sConfigMapStore wiring (line 178) uses `backends[defaultSubstrate]`.
  5. Add `BackendDefaultSubstrate string` (default `"k8s"`) accessor for the K8sConfigMapStore / reconciler client init at line 178/582.
  6. `internal/hud/embed.go initSpawnOrchestrator`: keep K8sBackend construction; ADD optional HarvesterVMBackend construction when `cfg.HarvesterKubeconfig != ""` (new config fields below). Pass map `{"k8s": k8sBackend, "harvester-vm": hvmBackend}` (the latter conditional).
  7. `internal/hud/app.go Config`: new fields `HarvesterKubeconfig`, `HarvesterBaseImage`, `HarvesterNamespace`, `HarvesterStorageClass`, `HarvesterNetworkAttachDef`, `HarvesterDefaultVCPUs`, `HarvesterDefaultMemMi`, `HarvesterDefaultDiskGi`, `HarvesterSSHUser`. All optional; empty `HarvesterKubeconfig` means harvester-vm substrate isn't registered. (Same env-var shape as `cmd/mcp-devbox/main.go` so operators can copy-paste.)
  8. Tests:
     - `TestSubstrateBackend_FallbackBehaviors`: nil substrate / empty → default; explicit known → that backend; explicit unknown → default + warning log.
     - `TestNewSpawnOrchestrator_RegistersBackends`: map round-trip.
     - Update at least one existing orchestrator-tests fixture so it constructs via the new signature (smoke test the refactor didn't break anything).

- Scope out (separate follow-up slices):
  - **Slice 2d.5 (env propagation on HarvesterVMBackend)**: `HarvesterVMBackend.Start` currently ignores `opts.Env` and `opts.SecretEnv`. For a real harvester-vm spawn to actually run the agent CLI with API keys, the VM needs those env-vars present at exec time. Either (a) `HarvesterVMBackend.Start` writes a systemd env file via cloud-init, or (b) every `Exec` call layered above propagates env (already happens in `Exec`'s shell prefix — but the orchestrator's primary agent-exec call at line ~738 does NOT pass env). This is a HarvesterVMBackend internal concern + a runSpawn exec-env audit; tracked separately.
  - **Slice 2e (mcp-devbox multi-backend init)**: `cmd/mcp-devbox/main.go` still reads a single `DEVBOX_BACKEND` env at startup; for an in-pod mcp-devbox to switch backends at runtime, it needs the same `backends` map shape.
  - **Slice 2 acceptance kill-test**: canary auto-merge end-to-end on harvester-vm; blocked on Slice 2d.5 + the GitlabWorker canary-label gate (currently policy is global, not canary-scoped).

- Acceptance criteria:
  1. `go build ./...` clean.
  2. `go test ./internal/hud/... ./internal/spawn/... ./pkg/mills/...` green.
  3. `golangci-lint` clean on changed packages.
  4. With `cfg.HarvesterKubeconfig == ""` (the prod default), `o.backends == {"k8s": …}` and existing spawn behavior is byte-identical (no regressions).
  5. With `cfg.HarvesterKubeconfig` set, `o.backends["harvester-vm"]` is registered; a `runSpawn` call with `req.Substrate == "harvester-vm"` lands `Build`/`Start`/`Stop` on the HarvesterVMBackend (verified via unit test using a fake backend implementing the interface).
  6. `substrateBackend("")` and `substrateBackend("unknown")` both return the default backend; the latter emits a warning log.

- Dependencies/blockers: none (Slice 2c shipped, providing the wire field).

## Land

- Planned file areas:
  - `internal/hud/spawn.go` (orchestrator struct + helper + 9 callsite edits + signature change)
  - `internal/hud/embed.go` (initSpawnOrchestrator: optional harvester-vm init)
  - `internal/hud/app.go` (Config fields for harvester-vm)
  - `internal/hud/spawn_test.go` (helper tests, orchestrator construction smoke test)
  - `internal/hud/spawn_sdk_driver_test.go` (recordingBackend already; possibly extend or just update construction site)
  - `internal/hud/runtime.go` (if it has Config flag wiring — TBD)
  - Anywhere else `NewSpawnOrchestrator(` is called (grep showed only embed.go)
  - `.loom/122-iteration-plan-…` (this doc)
- Implementation order:
  1. Worktree allocate `feat/mills-hud-multi-backend` from `main`.
  2. Refactor `SpawnOrchestrator` struct + add `substrateBackend` helper (with logging).
  3. Update `NewSpawnOrchestrator` signature; keep a backward-compat single-backend shim if existing tests are noisy.
  4. Update all 9 callsites in `spawn.go`.
  5. Update `embed.go` + `app.go` for optional harvester-vm init.
  6. Update tests (existing construction sites + new helper tests).
  7. Build + targeted test + broader test.
  8. Commit, push, MR, auto-merge.

## Prove

- Tests to run:
  - `go test ./internal/hud -run "Substrate|SpawnOrchestrator|Backend" -v`
  - `go test ./internal/hud/...`
  - `go test ./internal/spawn/...`
  - `go test ./pkg/mills/...`
  - `go build ./...`
- Lint/static checks: `go vet ./...`, `golangci-lint run ./internal/hud/... ./internal/devbox/backend/...`
- Pre-commit hooks (the project runs these on commit).
- CI: GitLab pipeline reaches green.

## Handoff/Harvest

- Docs to update on land:
  - `.loom/00-index.md` — Mills VM substrate one-liner: "Slice 2d shipped YYYY-MM-DD as `<sha>` ([!MR]); next: Slice 2d.5 (HarvesterVMBackend env+secret propagation) → Slice 2e (mcp-devbox multi-backend) → Slice 2 acceptance kill-test".
  - `.loom/45-…` Slice 2d status line: append "Slice 2d shipped YYYY-MM-DD as `<sha>` ([!MR]); the orchestrator now routes Build/Start/Exec/Stop via `substrateBackend(req.Substrate)`; default substrate is `k8s` and the harvester-vm backend is registered only when `HarvesterKubeconfig` is configured."
  - `.loom/45-…` add explicit "Known gaps" subsection listing the env-propagation gap (Slice 2d.5) and the canary-label gate (Slice 2 acceptance).
- Agent-context entries (post-merge):
  - decision: "Substrate routing lives in `SpawnOrchestrator.substrateBackend(substrate)`, which falls back to the default backend on empty/unknown. Rationale: clean failure for misconfiguration (warn log + safe default) is preferable to fail-loud here — Mills already prints policy at startup, so an operator targeting harvester-vm without configuring it gets two signals (startup policy log + spawn-time warn log)."
  - finding: "HarvesterVMBackend.Start drops StartOpts.Env at VM-create time (env is only honored via Exec's shell prefix). The orchestrator's primary agent-exec call passes no Env, so harvester-vm spawns currently can't deliver API keys to the agent CLI. Slice 2d.5 closes this gap (cloud-init systemd env file + env passthrough in the orchestrator's agent-exec call)."
- Next-slice candidates:
  - **Slice 2d.5**: HarvesterVMBackend env propagation (cloud-init systemd env file for spawn-pod-equivalent env vars) + runSpawn agent-exec env passthrough.
  - **Slice 2e**: mcp-devbox multi-backend init in `cmd/mcp-devbox/main.go`.
  - **Slice 2 acceptance kill-test**: 1 successful end-to-end auto-merge of a canary item via harvester-vm — gated on 2d.5 + 2e + the canary-label gate.

## Sources

- Spec: `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` § "Slice 2c (next)" — explicit two-part decomposition.
- Slice 2c plan + outcome: `.loom/121-iteration-plan-mills-spawn-substrate-slice2c-2026-05-27.md`, commit `57052ce0` ([!566](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/566))
- HarvesterVMBackend interface impl: `internal/devbox/backend/harvester_vm.go:185-477`
- HarvesterVMBackend env-drop gap: `internal/devbox/backend/harvester_vm_objects.go:58-130` (buildVMManifest has no `opts.Env` consumer)
- HUD orchestrator `o.backend.*` callsites: `internal/hud/spawn.go:543,560,723,738,864,920,997,1109,1230`
- HUD orchestrator construction: `internal/hud/embed.go:535-598`
- Backend interface contract: `internal/devbox/backend/backend.go:14-50`
