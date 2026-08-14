# Implementation Plan: Platform Enhancement Wave

> Date: 2026-04-16
> Mode: `parallel-slice-ship` candidate.
> Coordinator: Codex.

## Current State

- Repo branch: `main`, behind `origin/main` by 14 commits at intake.
- Pre-existing dirty files observed before this plan: `.loom/00-index.md`, `.opencode/opencode.json`, `.zed/mcp.json`, `AGENTS.md`, `mcp/context/skills-registry.yaml`, and untracked `.loom/86-multi-sprint-roadmap-2026-04-14.md`.
- Context pack was initialized without overwriting existing templates, and `.loom/00-workspace-snapshot.md` was regenerated.
- Worktree audit was started with `/Users/cblevins/workspace/bin/workspace-clean --report --worktrees`; it reported substantial noncanonical workspace worktree sprawl before being stopped after the needed signal. New implementation worktrees must be created only under `services/loom-core/.worktrees/<branch>`.
- Codebase-memory indexing job `686937bd6e753165` failed during Qdrant upsert timeout. Use direct file reads/`rg` as primary implementation context until the index is refreshed.

## Integration Branch

- Branch: `feat/platform-enhancement-wave`
- Worktree: `services/loom-core/.worktrees/feat-platform-enhancement-wave`
- Base: current `main` after explicitly deciding whether to pull/rebase the 14 remote commits.

## Slice 1: Catalog Discovery Upgrade

- Branch: `feat/platform-enhancement-catalog`
- Worktree: `.worktrees/feat-platform-enhancement-catalog`
- Goal: turn the existing catalog CLI/HUD surface into a real discovery surface.
- Primary files:
  - `cmd/loom/cmd_catalog.go`
  - `cmd/loom/cmd_catalog_test.go`
  - `internal/hud/api_catalog.go`
  - `internal/hud/api_catalog_test.go`
  - `internal/hud/frontend/src/lib/components/CatalogPanel.svelte`
  - `internal/hud/frontend/src/lib/stores/catalog.svelte.ts`
  - shared helper under `pkg/registry` or new `pkg/catalog` only if duplication becomes meaningful
- Acceptance:
  - `loom catalog search <query>` exists.
  - catalog JSON entries expose tool count and env/config hints where available.
  - CLI and HUD use compatible enriched entry fields.
  - Existing list/enable/disable/status tests still pass.
- Tests:
  - `go test ./cmd/loom/... -run Catalog -count=1`
  - `go test ./internal/hud/... -run Catalog -count=1`
  - frontend type/build check if HUD files change.

## Slice 2: Deploy Safety and Flux Convergence

- Branch: `feat/platform-enhancement-deploy-safety`
- Worktree: `.worktrees/feat-platform-enhancement-deploy-safety`
- Goal: make deploy mutation portable and make deploy status prove convergence.
- Primary files:
  - `Makefile`
  - optional helper under `scripts/`
  - optional docs snippet in `docs/DEV_BUILD_LIFECYCLE.md` or `docs/DEVELOPER_GUIDE.md`
- Acceptance:
  - image tag mutation works without BSD-only `sed -i ''`.
  - `make deploy-check` runs validation gates before deploy mutation.
  - `make deploy-status` reports Flux readiness and rollout/image convergence where local tools are available.
  - missing cluster/tooling is reported as a clear unavailable state.
- Tests:
  - shellcheck or dry-run helper tests if a script is introduced.
  - `make deploy-check` in dry/local mode where possible.
  - targeted `make -n deploy-update-images deploy-status` to prove command expansion.

## Slice 3: Operator Health and OTel Status

- Branch: `feat/platform-enhancement-health-status`
- Worktree: `.worktrees/feat-platform-enhancement-health-status`
- Goal: expose actionable health/observability status without forcing operators into raw daemon APIs.
- Primary files:
  - `cmd/loom/status.go`
  - `cmd/loom/status_test.go`
  - `internal/daemon/daemon_dispatch_status.go`
  - `internal/daemon/daemon_dispatch_otel.go`
  - existing bridge/HUD files only if a backend shape must be mirrored
- Acceptance:
  - JSON status includes degraded servers, restart counts, last error, latency/readiness, and OTel/log state.
  - human status highlights degraded server names and OTel/log warnings.
  - existing status output remains backward-compatible for scripts.
- Tests:
  - `go test ./cmd/loom/... -run Status -count=1`
  - `go test ./internal/daemon/... -run 'Status|Otel|Health' -count=1`

## Slice 4: Spawn Telemetry Accuracy

- Branch: `feat/platform-enhancement-spawn-telemetry`
- Worktree: `.worktrees/feat-platform-enhancement-spawn-telemetry`
- Goal: make Codex spawn telemetry model/cost reporting accurate and guarded by tests.
- Primary files:
  - `internal/hud/spawn_codex_parser.go`
  - `internal/hud/spawn_parser.go`
  - `internal/hud/bridge/spawn_telemetry_delta.go`
  - `internal/hud/frontend/src/lib/stores/spawn.svelte.ts` only if payload compatibility requires a frontend adjustment
- Acceptance:
  - Codex cost estimation uses model metadata when available.
  - fallback remains deterministic when model data is missing.
  - SSE telemetry delta payload remains compatible with the frontend consumer.
- Tests:
  - `go test ./internal/hud/... -run 'CodexJSONLParser|SpawnTelemetry' -count=1`
  - `go test ./internal/hud/... -count=1`

## Integration Order

1. Deploy safety first, because it is mostly isolated in `Makefile`/scripts.
2. Spawn telemetry second, because it is isolated under HUD spawn parsing.
3. Operator health third, because it may touch daemon/CLI status shape.
4. Catalog last, because it has the broadest CLI/HUD surface and may need final frontend/API alignment.

## Quality Gate

- Run targeted tests per slice before merge.
- After integration, run:
  - `go test ./cmd/loom/... ./internal/daemon/... ./internal/hud/...`
  - `make ci-contracts` if status/HUD contract shapes change.
  - HUD frontend check/build if Svelte files change.

## Delegation Plan

Use four worker subagents after approval. Each worker owns one slice and must edit only its listed files unless it records the reason for widening scope. Workers are not alone in the codebase; they must not revert unrelated edits and must accommodate pre-existing dirty files.

## Decision Needed

Approve this wave as the first `parallel-slice-ship` implementation target, or swap in one of the deferred candidates:

- Agent workflow quality automation (`auto_verify` and session retro).
- Devbox K8s backend seam cleanup.
- OpenAI Responses M2 token preflight and compaction controls.
- Full MCP Registry/subregistry ingestion.
