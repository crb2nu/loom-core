# Research Brief: Platform Enhancements

> Date: 2026-04-16
> Scope: Identify high-leverage Loom Core features/enhancements suitable for parallel implementation.
> Method: coordinator repo intake, loom-mode MCP inventory, external primary-source research, and four read-only Codex research subagents.

## Questions

1. Which enhancements best extend the existing Loom platform rather than duplicating shipped work?
2. Which items are concrete enough for parallel-slice implementation in independent file scopes?
3. Which items are aligned with current MCP/agent-platform direction?

## Local Facts

- Loom Core is the production backend for the local MCP runtime: CLI, daemon, proxy, Go MCP servers, HUD, and agent context/orchestration. Source: `ROADMAP.md:7`, `ROADMAP.md:14`.
- Current active priorities include devbox seam cleanup, daemon/runtime telemetry expansion, Fleet orchestration follow-on, OpenAI Responses M2, onboarding/docs consistency, and mobile scope discipline. Source: `docs/IMPLEMENTATION_STATUS.md:52`, `docs/IMPLEMENTATION_STATUS.md:63`.
- The roadmap still calls out catalog/discovery, security hardening, Fleet merge orchestration, and daemon OTel trace expansion as open or future work. Source: `ROADMAP.md:196`, `ROADMAP.md:205`, `ROADMAP.md:244`, `ROADMAP.md:263`, `ROADMAP.md:264`, `ROADMAP.md:270`.
- Catalog work is partially implemented already: `loom catalog` has `list`, `enable`, `disable`, and `status`, and the HUD has catalog list/enable/disable handlers. Source: `cmd/loom/cmd_catalog.go:24`, `cmd/loom/cmd_catalog.go:35`, `cmd/loom/cmd_catalog.go:39`, `cmd/loom/cmd_catalog.go:145`, `cmd/loom/cmd_catalog.go:156`, `cmd/loom/cmd_catalog.go:290`, `internal/hud/api_catalog.go:19`, `internal/hud/api_catalog.go:127`.
- Catalog state is currently a local disabled-server list under `~/.config/loom/catalog-state.yaml`; servers are enabled by default. Source: `pkg/registry/catalog_state.go:13`, `pkg/registry/catalog_state.go:23`, `pkg/registry/catalog_state.go:25`, `pkg/registry/catalog_state.go:103`.
- The HUD catalog panel already supports polling, filtering, table/card views, and enable/disable actions, but the data model is still basic: name, description, categories, enabled, running. Source: `internal/hud/api_catalog.go:11`, `internal/hud/api_catalog.go:17`, `internal/hud/frontend/src/lib/components/CatalogPanel.svelte:7`, `internal/hud/frontend/src/lib/components/CatalogPanel.svelte:16`, `internal/hud/frontend/src/lib/components/CatalogPanel.svelte:84`, `internal/hud/frontend/src/lib/components/CatalogPanel.svelte:260`.
- The April multi-sprint roadmap marks server catalog CLI, OTel trace explorer, session retro, and deploy/observability gaps as candidates, but parts of the catalog and trace story have already landed since those notes were written. Source: `.loom/86-multi-sprint-roadmap-2026-04-14.md:23`, `.loom/86-multi-sprint-roadmap-2026-04-14.md:31`, `.loom/86-multi-sprint-roadmap-2026-04-14.md:166`, `.loom/86-multi-sprint-roadmap-2026-04-14.md:235`.
- Codebase-memory indexing was attempted for `services/loom-core`, but this run failed during Qdrant upsert with a client timeout after collecting 13,567 files and partially indexing 4,699 chunks. Source: command `codebase_memory__codebase_index_start` job `686937bd6e753165`; poll result error `flush chunks: read response: context deadline exceeded`.
- Loom-mode MCP inventory is available: profile `full`, 47 servers, 514 tools, and paged tool inventory via `loom://tools/page/{page}`. Source: `loom://config` read on 2026-04-16.

## External Facts

- Current MCP authorization guidance is transport-level for HTTP-based transports; STDIO implementations should retrieve credentials from the environment instead. Source: https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization (accessed 2026-04-16).
- MCP authorization now relies on OAuth 2.1, protected resource metadata, authorization server metadata, dynamic client registration, and client ID metadata documents. Source: https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization (accessed 2026-04-16).
- MCP transport direction remains STDIO for local deployments and Streamable HTTP for remote deployments, with spec work moving toward stateless independent requests and simpler horizontal scaling. Source: https://blog.modelcontextprotocol.io/posts/2025-12-19-mcp-transport-future/ (accessed 2026-04-16).
- Docker's MCP Catalog positions catalog discovery as a trusted registry with verified/versioned metadata, profile/client management, local and remote server types, and OAuth handling for remote servers. Source: https://docs.docker.com/ai/mcp-catalog-and-toolkit/catalog/ and https://docs.docker.com/ai/mcp-catalog-and-toolkit/ (accessed 2026-04-16).
- GitHub Copilot cloud agent reflects the market direction toward background agents that research, plan, branch, test, and open PRs with visible logs. Source: https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent (accessed 2026-04-16).
- MCP-agent observability guidance treats structured logging, token/cost counters, OpenTelemetry spans, and metrics as core agent infrastructure. Source: https://docs.mcp-agent.com/mcp-agent-sdk/advanced/observability (accessed 2026-04-16).

## Subagent Findings

- Local platform slice: highest-value seams are catalog/sync, recall/workflow quality, daemon traces, devbox hardening, and quality-gate automation.
- External trend slice: registry/discovery, authorization consent, OTel-native lifecycle tracing, deferred tasks, and remote-agent handoffs are the main platform trends.
- Operations slice: deploy safety is weak compared to the rest of the platform; `deploy-status` should prove Flux convergence and rollout status, and deploy image mutation should become portable across macOS/Linux.
- Low-risk slice finder: Codex spawn telemetry accuracy, isolated MCP server handler hardening, and CLI compatibility tests are good early parallel slices.

## Recommended Opportunities

1. Catalog discovery upgrade.
   - Why: Loom has 47 servers and 514 tools, but discovery still lacks search/tool counts/env requirements in the shared CLI/HUD model.
   - Shape: add `loom catalog search`, richer catalog entries, env/config hints, and shared API/CLI behavior.
   - Implementation seam: `cmd/loom/cmd_catalog.go`, `internal/hud/api_catalog.go`, `internal/hud/frontend/src/lib/components/CatalogPanel.svelte`, `pkg/registry`.

2. Deploy safety and convergence reporting.
   - Why: GitOps deploys should prove validation, Flux reconciliation, rollout status, image convergence, and OTel/logging posture rather than just mutate tags and print pods.
   - Shape: portable image update helper, `make deploy-check`, richer `make deploy-status`.
   - Implementation seam: `Makefile`, optional `scripts/` helper, optional `cmd/loom/status.go`.

3. Operator health and observability report.
   - Why: daemon health has detailed restart/error/latency state, but user-facing status is still too aggregate for triage.
   - Shape: richer `loom status --json` or new `loom health` report including degraded servers, restart counts, last error, OTel status, and JSON log posture.
   - Implementation seam: `cmd/loom/status.go`, `internal/daemon/daemon_dispatch_status.go`, `internal/daemon/daemon_dispatch_otel.go`.

4. Spawn telemetry accuracy and contract guard.
   - Why: subagent/fleet orchestration depends on accurate telemetry, and a focused Codex parser fix has a small blast radius.
   - Shape: use model-aware cost estimates when available, keep clean fallback behavior, and add payload regression coverage.
   - Implementation seam: `internal/hud/spawn_codex_parser.go`, `internal/hud/spawn_parser.go`, `internal/hud/bridge/spawn_telemetry_delta.go`.

5. Agent workflow quality automation.
   - Why: automated verification and session retro loops compound across all future feature work.
   - Shape: finish `auto_verify`/quality-gate flow and session-retro integration after the operator-facing wave.
   - Implementation seam: `pkg/agentcontext/workflow_executor.go`, `pkg/agentcontext/schema_workflow.go`, `mcp/context/skills-registry.yaml`.

## Recommendation

Start with a four-slice operator/platform wave:

1. Catalog discovery upgrade.
2. Deploy safety and Flux convergence reporting.
3. Operator health/OTel status report.
4. Spawn telemetry accuracy and contract guard.

This wave is coherent from an operator-product standpoint, maps to current platform trends, and has mostly disjoint write scopes for parallel implementation.
