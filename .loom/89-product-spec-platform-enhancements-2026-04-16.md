# Product Spec: Platform Enhancement Wave

> Date: 2026-04-16
> Status: Proposed for parallel-slice implementation.

## Goal

Improve Loom Core's operator-facing platform experience so users can discover MCP servers, deploy safely, diagnose daemon/server health, and trust subagent telemetry without dropping into manual registry edits or raw logs.

## Non-Goals

- Do not redesign the full HUD information architecture.
- Do not replace the registry schema or the `fi-mcp-kit` compatibility layer.
- Do not implement full upstream MCP Registry mirroring in this wave.
- Do not refactor devbox K8s backend concerns in this wave.
- Do not change existing `loom proxy` or MCP server protocol behavior.

## Users

- Operators running Loom locally or as a team daemon.
- Agents and developers who need server discovery and consistent generated configs.
- Maintainers triaging deploys, OTel state, server health, and subagent fleet activity.

## Requirements

### R1: Catalog Discovery Upgrade

- `loom catalog list --json` includes searchable metadata beyond name/category/description: tool count, known command, enabled/running state, and env/config hints when available.
- `loom catalog search <query>` searches server names, descriptions, categories, and available metadata.
- HUD catalog API returns the same enriched entry model as CLI where practical.
- Existing enable/disable behavior remains backward-compatible.

### R2: Deploy Safety and Convergence

- Add a preflight deploy check that runs config/schema/RBAC validation before GitOps mutation.
- Replace macOS-specific image mutation with a portable approach.
- `make deploy-status` reports Flux Kustomization readiness, relevant rollout status, image tag convergence, and OTel/log format state when available.
- The status output should fail clearly when convergence cannot be proven.

### R3: Operator Health and OTel Status

- Expose actionable daemon health in CLI JSON/human output: degraded servers, restart counts, last error, latency, and readiness.
- Include OTel runtime status and JSON logging posture in an operator-visible path.
- Keep existing `loom status` behavior compatible for scripts; additive fields are acceptable.

### R4: Spawn Telemetry Accuracy

- Codex spawn telemetry uses a real model identifier when available.
- Cost estimates fall back cleanly when model metadata is missing.
- The `agent.spawn.telemetry.delta` payload keeps compatibility with HUD stores.
- Add focused tests for Codex parser behavior and telemetry delta shape.

## Acceptance Criteria

- Catalog: `go test ./cmd/loom/... -run Catalog -count=1` passes, and `loom catalog search kubernetes --json` returns matching servers with enriched fields.
- Deploy: `make deploy-check` and `make deploy-status` are deterministic on macOS and Linux shell environments.
- Health: `loom status --json` or `loom health --json` exposes degraded server/OTel details without breaking existing fields.
- Spawn telemetry: `go test ./internal/hud/... -run 'Codex|SpawnTelemetry' -count=1` passes.
- Combined: `go test ./cmd/loom/... ./internal/hud/...` passes for touched packages.

## Risks

- Catalog metadata may require shared code to avoid CLI/HUD drift.
- Deploy checks may depend on local cluster access; commands must degrade clearly when Flux or Kubernetes is unavailable.
- Status JSON additions should avoid breaking existing consumers.
- HUD frontend changes must keep table/card layout stable on narrow viewports.

## Open Questions

- Should catalog env hints come from registry template references, embedded metadata, or both?
- Should deploy convergence live only in `Makefile`, or should `loom deploy status` become a first-class CLI command?
- Should the health report be an enriched `loom status` or a new `loom health` command?
