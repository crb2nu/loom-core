# Technical Debt Priority Ranking

Scored using weighted model: impact 35%, risk reduction 30%, drag reduction 20%, effort inverse 15%.

| Rank | ID | Title | Component | Impact | Risk | Drag | Effort | Score |
|---:|---|---|---|---:|---:|---:|---:|---:|
| 1 | DEBT-070 | Replace hand-coded fi-accel race-test exclusion with dependency-graph computed list | .gitlab-ci.yml test:race + cmd/loom-mills-operator | 1.00 | 1.00 | 1.00 | 2.0 | 97.00 |
| 2 | DEBT-071 | Clear gosec G202 false positive blocking main (dao_workflow.go IN-clause) | pkg/mills/store/dao_workflow.go + CI security:gosec | 1.00 | 0.80 | 0.80 | 1.0 | 90.00 |
| 3 | DEBT-072 | Durable hub WS transport hardening (keepalive, liveness gating, backoff) | libs/mcp-go websocket + internal/daemon routing/callpipeline | 1.00 | 1.00 | 0.80 | 4.0 | 87.00 |
| 4 | DEBT-073 | Fix recurring Mills escalation classes and de-noise incident issues | pkg/mills/pipeline (ci_watch, merge, scope gate) + issue hygiene | 0.80 | 0.80 | 1.00 | 3.0 | 81.00 |
| 5 | DEBT-074 | Split internal/hud/spawn.go monolith (2206 LOC, top churn) | internal/hud/spawn.go | 0.60 | 0.60 | 0.80 | 3.0 | 64.00 |
| 6 | DEBT-075 | Split cmd/loom-mills-operator/main.go (1469 LOC) into handler modules | cmd/loom-mills-operator/main.go | 0.60 | 0.60 | 0.60 | 3.0 | 60.00 |
| 7 | DEBT-067 | Split cmd_sync.go into generate, sync, pull, and backup slices (carryover, grew to 1004 LOC) | cmd/loom/cmd_sync.go | 0.60 | 0.40 | 0.60 | 3.0 | 54.00 |
| 8 | DEBT-066 | Migrate largest remaining MCP server mains onto mcpscaffold (carryover, issue #50) | cmd/mcp-linear, mcp-terraform, mcp-github, mcp-argocd, mcp-neo4j, mcp-notion | 0.40 | 0.40 | 0.60 | 3.0 | 47.00 |
| 9 | DEBT-078 | Decompose HUD frontend monolith components (5 files over 1000 LOC) | frontend: mills.svelte.ts, App.svelte, SpawnDetailPanel, GraphFullView, MemoryPanel | 0.40 | 0.40 | 0.60 | 3.0 | 47.00 |
| 10 | DEBT-076 | Finish EPIC 2 migration of CLI tasks/sessions onto visibility contracts | cmd/loom/cmd_tasks.go, cmd/loom/cmd_sessions.go | 0.40 | 0.40 | 0.40 | 2.0 | 46.00 |
| 11 | DEBT-077 | Deslop sweep: banner comments, commented-out code, generation TODOs (343 candidates) | cmd/ (155 hits), internal/ (188 hits), frontend hooks | 0.40 | 0.40 | 0.40 | 2.0 | 46.00 |

## Suggested Cut Lines

- Wave 1: top 20-30% by score, low dependency risk
- Wave 2: next 30-40%, medium effort and moderate coupling
- Wave 3: remaining strategic refactors with cross-team dependencies
