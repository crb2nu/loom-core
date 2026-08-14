# RALPH Iteration Plan - mcp-devbox Multi-Backend Init (Slice 2e)

## Review

- Roadmap milestone: Mills harvester-vm devbox substrate.
- Spec section(s): `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` Slice 2e.
- Prior decisions to preserve:
  - Slice 2c carries `SpawnRequest.Substrate` to spawn pod env as `DEVBOX_BACKEND`.
  - Slice 2d made the HUD spawn orchestrator multi-backend and routes pod lifecycle calls per spawn.
  - Slices 2d.5 through 2d.5d already shipped env, SecretMount, home-dir parity, cloud-init Secret delivery, and the live codex home-parity kill-test. `ROADMAP.md` lagged behind this state.

## Align

- Slice name: mcp-devbox backend registry + harvester secret resolver wiring.
- Scope in:
  - Normalize backend names (`kubernetes` -> `k8s`) at manager init.
  - Keep the existing env-selected singleton behavior via `m.backend`.
  - Add a manager `backends` registry and `backendFor` lookup helper matching the HUD spawn orchestrator shape.
  - When `DEVBOX_BACKEND=harvester-vm`, opportunistically initialize a K8s backend and pass it to Harvester as `SecretResolver`; if K8s is unavailable, keep Harvester usable and warn.
- Scope out:
  - Adding per-tool `substrate` arguments.
  - Changing state keys or running two live sandboxes for the same project in one process.
  - Live canary auto-merge validation; that remains the Slice 2 acceptance kill-test.
- Acceptance criteria:
  - `mcp-devbox` still supports `docker`, `k8s`/`kubernetes`, and `harvester-vm`.
  - `harvester-vm` manager init can carry a K8s `SecretResolver` when companion K8s init succeeds.
  - Unit tests pin backend normalization and registry lookup.
- Dependencies/blockers: local `.codex/worktrees` checkout has a broken relative `go.work`; use `GOWORK=off` for local tests in this worktree.

## Land

- Planned file areas:
  - `cmd/mcp-devbox/manager.go`
  - `cmd/mcp-devbox/manager_test.go`
  - roadmap/spec progress docs
- Implementation steps:
  1. Extract backend constructors from `newManager`.
  2. Add backend registry fields and lookup helper.
  3. Wire optional K8s resolver into Harvester backend init.
  4. Add focused unit tests.

## Prove

- Tests to run:
  - `GOWORK=off go test ./cmd/mcp-devbox -run 'TestCanonicalBackendType|TestBackendFor|TestIsK8sBackend|TestBuildMounts' -count=1`
  - `GOWORK=off go test ./cmd/mcp-devbox -count=1`
- Lint/static checks:
  - `gofmt -w cmd/mcp-devbox/manager.go cmd/mcp-devbox/manager_test.go`
  - `git diff --check`
- CI checks:
  - Not run locally; this slice does not create/push an MR in the current thread.

## Handoff/Harvest

- Docs to update:
  - `ROADMAP.md`
  - `.loom/00-index.md`
  - `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md`
- Agent-context entries to add:
  - Decision: preserve singleton handler path while adding backend registry.
  - Finding: local `go.work` in this Codex worktree points at absent sibling libs; `GOWORK=off` works.
- Next-slice candidates:
  - Slice 2 acceptance: one real `mills-canary-harvester-vm` item auto-merges with a non-empty MR.
  - Phase B: curated Harvester base image to remove stock-image `apt-get qemu-guest-agent` boot tax.
