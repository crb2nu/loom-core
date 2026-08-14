# Implementation Plan: Next Platform Enhancement Wave

> Date: 2026-04-16
> Status: Proposed
> Base: `main` fast-forwarded to `origin/main` after MR !177 (`0f1ef8e200afba74f7e43255f56d8aca014651e5`)

## Context

- The previous platform enhancement wave shipped through GitLab MR !177 and is now on `origin/main`.
- Local `main` is up to date with `origin/main`; pre-existing local planning/config edits remain unstaged.
- `.loom/00-workspace-snapshot.md` was regenerated after the fast-forward.
- Daemon health is good via `curl http://localhost:9876/health`, but MCP connector calls from this Codex session returned `Transport closed` for `codebase_memory`, `agent_context`, and `gitlab`. Treat live MCP index/issue status as unavailable until the transport recovers.
- Roadmap/status docs still identify these active gaps: devbox backend seam cleanup, daemon/runtime OTel depth, OpenAI Responses M2, Fleet orchestration follow-on, docs/onboarding consistency, and mobile scope discipline.

## Recommendation

Run a **runtime guardrails and automation wave** next. This wave is a better follow-up than another catalog/operator UI wave because the last round already improved catalog, deploy status, health status, and spawn telemetry. The next highest leverage is to harden the paths that future automation depends on: Responses context control, daemon trace fidelity, devbox backend reliability, and workflow quality automation.

## Slice 1: OpenAI Responses M2 Guardrails

- Branch: `feat/responses-m2-guardrails`
- Goal: finish the M2 token/context-control slice so Responses orchestration cannot silently exceed context/cost budgets.
- Primary files:
  - `pkg/openairesponses/preflight.go`
  - `pkg/openairesponses/compaction.go`
  - `pkg/openairesponses/orchestrator.go`
  - `pkg/openairesponses/*_test.go`
  - `cmd/loom/cmd_responses.go`
  - `cmd/loom/cmd_responses_runtime.go`
- Acceptance:
  - Explicit matrix tests cover `chain`, `conversation`, and `stateless` context modes.
  - `previous_response_id` and `conversation` remain mutually exclusive at validation boundaries.
  - Preflight returns structured estimates that callers can surface, not only a formatted error string.
  - Compaction behavior is observable in `LoopResult` or telemetry: strategy, before/after estimate, and whether compaction ran.
  - CLI runtime path exposes budget/compaction options consistently with config/env defaults.
- Tests:
  - `go test ./pkg/openairesponses/... -count=1`
  - `go test ./cmd/loom/... -run Responses -count=1`

## Slice 2: Daemon Trace Depth and Percentiles

- Branch: `feat/daemon-trace-depth`
- Goal: move from audit-backed trace summaries toward trace-grade daemon runtime observability.
- Primary files:
  - `internal/daemon/callpipeline.go`
  - `internal/daemon/callpipeline_stages.go`
  - `internal/daemon/daemon_call.go`
  - `internal/daemon/otel_metrics.go`
  - `internal/daemon/otel_metrics_test.go`
  - `internal/hud/app_routes_observability.go`
  - `internal/hud/frontend/src/lib/stores/traces.svelte.ts`
  - `internal/hud/frontend/src/lib/utils/traces.ts`
- Acceptance:
  - Tool-call pipeline stages emit span attributes or events for parse, policy, route, build, send, receive, scan, and cache.
  - OTel metrics include low-cardinality latency histograms suitable for p50/p95 reporting by server/tool/status.
  - HUD/API can return percentile-ready aggregates without forcing clients to calculate from raw trace rows.
  - Existing audit trace endpoints remain backward-compatible.
- Tests:
  - `go test ./internal/daemon/... -run 'CallPipeline|Otel|Trace' -count=1`
  - `go test ./internal/hud/... -run 'Trace|Observability|Otel' -count=1`

## Slice 3: Devbox K8s Backend Reliability Seams

- Branch: `feat/devbox-k8s-seams`
- Goal: reduce risk in the K8s backend by tightening test seams around build/runtime/wait behavior instead of doing a broad rewrite.
- Primary files:
  - `internal/devbox/backend/k8s.go`
  - `internal/devbox/backend/k8s_build.go`
  - `internal/devbox/backend/k8s_runtime.go`
  - `internal/devbox/backend/k8s_wait.go`
  - `internal/devbox/backend/k8s_objects.go`
  - `internal/devbox/backend/k8s_*_test.go`
- Acceptance:
  - Build pod orchestration has deterministic tests for retry, cleanup, log capture, and cache detection.
  - Runtime pod lifecycle tests cover image mismatch recreate, non-running cleanup, timeout cleanup, and status mapping.
  - Exec helper behavior is isolated enough to test shell command construction and timeout/exit-code mapping without a live cluster.
  - No large package reshuffle unless tests prove a seam cannot be isolated locally.
- Tests:
  - `go test ./internal/devbox/backend/... -count=1`
  - `go test ./cmd/mcp-devbox/... -run 'Devbox|Quality|Handler' -count=1`

## Slice 4: Workflow Auto-Verify and Session Retro Integration

- Branch: `feat/workflow-auto-verify-retro`
- Goal: turn the existing auto-verify/session-retro pieces into the default delivery loop for future parallel work.
- Primary files:
  - `pkg/agentcontext/workflow_executor.go`
  - `pkg/agentcontext/schema_workflow.go`
  - `cmd/mcp-agent-context/tools_workflows.go`
  - `.agents/workflows/*.yaml`
  - `mcp/context/skills-registry.yaml`
  - relevant workflow/skill docs under `docs/` or `.loom/`
- Acceptance:
  - Workflow YAMLs use `auto_verify` where the old human-only approval gate was just a quality check.
  - `auto_verify` preserves structured failure details and retry state for HUD/API consumers.
  - Session-retro can run as a non-blocking post-session step and record proposed follow-up tasks without blocking active hooks.
  - Tests cover green, red, retry-exhausted, and skipped/unavailable quality-gate states.
- Tests:
  - `go test ./pkg/agentcontext/... -run 'Workflow|AutoVerify|Retro' -count=1`
  - `go test ./cmd/mcp-agent-context/... -run 'Workflow|Tools' -count=1`

## Integration Order

1. Devbox K8s seams first: gives better confidence for auto-verify and sandbox-backed validation.
2. Responses M2 guardrails second: mostly isolated package/CLI work with clear tests.
3. Daemon trace depth third: touches shared runtime observability and should merge after any backend timing tests settle.
4. Workflow auto-verify/session-retro last: may depend on devbox quality-gate reliability and should absorb final docs/workflow updates.

## Quality Gate

- Per-slice targeted tests listed above.
- After integration:
  - `go test ./pkg/openairesponses/... ./internal/devbox/backend/... ./internal/daemon/... ./internal/hud/... ./pkg/agentcontext/... ./cmd/loom/... ./cmd/mcp-agent-context/...`
  - `make ci-contracts` if API payloads or golden contract surfaces change.
  - `pnpm install --frozen-lockfile && pnpm build` if HUD frontend files change.
  - `go test ./...` before push.

## Deferred Candidates

- Fleet merge-assistance UX and stronger conflict surfacing. Important, but better after trace and workflow reliability improve.
- Mobile scope-discipline enforcement. Useful and likely small, but less central to the platform runtime than this wave.
- Docs/onboarding consistency. Should be included as a final pass after the next implementation wave, not lead it.
- Cost-to-OTel parity and spawn budget UI. This can ride on the daemon trace/metrics slice if it remains small; otherwise split into the following wave.

## Decision Needed

Approve this four-slice runtime guardrails wave, or swap in one smaller product-facing slice:

- Mobile v1 scope gate.
- Fleet conflict/merge assistant.
- Cost-to-OTel parity and budget UI.
- Docs/onboarding canonical entrypoint refresh.
