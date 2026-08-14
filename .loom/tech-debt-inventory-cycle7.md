# Technical Debt Inventory — Cycle 7

## Scope

- Product/Service: `services/loom-core`
- Planning date: 2026-06-12
- Time horizon: next 2-4 delivery cycles
- Owners: CI/platform, daemon/transport, Mills autonomy, HUD/runtime, CLI/config generation
- Includes: deslopper sweep findings (AI-slop remediation) as first-class debt items

## Cycle 6 Closeout

| ID | Status | Evidence |
|---|---|---|
| DEBT-062 | **RECURRED** as DEBT-070 | Same failure mode, new package: `cmd/loom-mills-operator` now trips the fi-accel race exclusion gap (`.gitlab-ci.yml:595` regex omits it) |
| DEBT-063 | Unverified | No fresh evidence this cycle; `ios:archive-export` canceled on red mains, not independently failing |
| DEBT-064 | **DONE** | `internal/hud/app.go` now 302 LOC (was 730) |
| DEBT-065 | Likely done | `guardrails:docs-cli` green in recent pipelines (#13784) |
| DEBT-066 | **OPEN** (carryover) | `cmd/mcp-linear/main.go` 939, `mcp-terraform` 924, `mcp-github` 887, `mcp-argocd` 885, `mcp-neo4j` 876, `mcp-notion` 875. Tracked as GitLab issue #50 |
| DEBT-067 | **OPEN, grew** (carryover) | `cmd/loom/cmd_sync.go` now 1004 LOC (was 880) |
| DEBT-068 | **DONE** | `pkg/agentcontext/svc_sessions.go` now 238 LOC (was 796) |
| — | GitLab issue #46 (codebase service.go monolith) **DONE** | `pkg/codebase/service.go` now 142 LOC; close the issue |

## Items

| ID | Component | Debt Statement | Evidence | Impact | Risk | Drag | Effort |
|---|---|---|---|---:|---:|---:|---:|
| DEBT-070 | `.gitlab-ci.yml:592-596` `test:race` | **Main is red**: race-test fi-accel exclusion is a hand-maintained regex; `cmd/loom-mills-operator` now transitively links `fiaccel` cgo and is not excluded → `fatal error: fi_accel.h: No such file or directory` → `FAIL github.com/crb2nu/loom/cmd/loom-mills-operator [build failed]`. Second recurrence of this exact mode (cycle 6 DEBT-062 was `cmd/mcp-orchestra`). Durable fix: compute the exclusion from `go list -deps` instead of a regex. | 27 of last 30 `main` pipelines failed (`glab api 'projects/47/pipelines?ref=main'` 2026-06-12); job 137381 trace; `.gitlab-ci.yml:595` | 5 | 5 | 5 | 2 |
| DEBT-071 | `pkg/mills/store/dao_workflow.go:185-188`, `security:gosec` | **Main is red (2nd breaker)**: gosec G202 (SQL string concat) flags a safe `?`-placeholder IN-clause join. One MEDIUM finding fails the job and (with DEBT-070) blocks all image builds/rollouts (govulncheck/gosec gate the deploy stage). Fix: `#nosec G202` with justification comment, or query-builder restructure. | gosec-report.json artifact from job 137375: exactly 1 issue, G202 MEDIUM at dao_workflow.go:185-188 | 5 | 4 | 4 | 1 |
| DEBT-072 | `libs/mcp-go/websocket.go`, `internal/daemon/{routing,callpipeline_routing,health}.go`, hub gateway | Hub WS transport has no keepalive (`Ping()` defined at websocket.go:181, zero callers), no liveness gating on conn reuse (websocket.go:203-215), flat 30s backoff (routing.go:114-124), 10s health-probe timeout that false-positives under load, and the hub spawns a full agent-context instance per WS connection. Caused the chronic `muxstdio: transport closed` storm (~800-1,340 events/hr). Slice 0 mitigation (config pin `agent_context: local-only`) PASSED 2026-06-12 but is a routing bypass, not a transport fix; cluster agents still ride the unhardened path. | `.loom/149-plan-backend-hub-transport-stability-2026-06-12.md` (full evidence, file:line); memory `project_hub_ws_transport_storm` | 5 | 5 | 4 | 4 |
| DEBT-073 | `pkg/mills/pipeline/runner.go` (ci_watch, merge stages), scope gate, escalation issue lifecycle | 67 open `mills-escalation` incident issues bury real signal. Recurring classes: (a) `ci_watch` 30m poll timeout × up to 8 attempts = 4h burn per run (issues #149, #153); (b) `merge` stage 405 Method Not Allowed retried verbatim 3× (#148, #150); (c) `testdata/mills-canary/heartbeat.md` scope-gate false positive across canaries (#151, #163); (d) infra 502s escalate as P2 code-class (#157-#160). Debt: classify-and-handle each class + auto-close/dedupe stale escalation issues. | `glab api projects/47/issues`: 67/100 open issues are `mills-escalation`; per-issue stage histories cited | 4 | 4 | 5 | 3 |
| DEBT-074 | `internal/hud/spawn.go` | 2,206 LOC monolith; top-churn Go file (61 touches/3mo). Owns spawn orchestration, agent command building, config rendering, lifecycle, and telemetry in one file. Parsers were already split out (spawn_codex_parser.go etc.) — finish the decomposition. Includes TODO at spawn.go:1139. | `wc -l`; `git log --since='3 months ago'` churn scan | 3 | 3 | 4 | 3 |
| DEBT-075 | `cmd/loom-mills-operator/main.go` | 1,469 LOC operator main; 27 touches/3mo. Mixes flag/env wiring, reconciler setup, and HTTP handlers. Also carries TODO(slice 4.4 followup) in handlers_crossrepo.go:165. | `wc -l`; churn scan | 3 | 3 | 3 | 3 |
| DEBT-067 | `cmd/loom/cmd_sync.go` | Carryover from cycles 5-6, **grew** 880 → 1,004 LOC. Aggregates generate, backup, pull, and sync command wiring; every profile/config feature lands here and conflicts. | `wc -l cmd/loom/cmd_sync.go` = 1004 | 3 | 2 | 3 | 3 |
| DEBT-066 | `cmd/mcp-{linear,terraform,github,argocd,neo4j,notion}/main.go` | Carryover (GitLab issue #50): six MCP server mains at 875-939 LOC of repeated lifecycle boilerplate; mcpscaffold migration unfinished since cycle 5. | `wc -l` large-file scan | 2 | 2 | 3 | 3 |
| DEBT-076 | `cmd/loom/cmd_tasks.go:8`, `cmd/loom/cmd_sessions.go:8` | EPIC 2 follow-up TODOs: CLI tasks/sessions still on legacy types instead of `internal/visibility/contracts/{tasks,sessions}`. Contracts packages exist and ship with no test files — migration would also close that gap. | TODO markers; `test:race` trace shows `[no test files]` for contracts subpackages | 2 | 2 | 2 | 2 |
| DEBT-078 | HUD frontend | Five files >1,000 LOC: `stores/mills.svelte.ts` 1134, `App.svelte` 1122, `SpawnDetailPanel.svelte` 1083, `graph/GraphFullView.svelte` 1017, `MemoryPanel.svelte` 1003. HUD is the highest-churn frontend area; monoliths amplify the `:global()` scope-drop and dist-rebase gotchas already in memory. | `wc -l` frontend scan | 2 | 2 | 3 | 3 |
| DEBT-077 | repo-wide (deslopper) | AI-slop debt: 343 candidate hits — `cmd/` 155 (148 banner comments), `internal/` 188 (177 banner comments, 8 commented-out code blocks incl. dead Swift in `recovery_telemetry_test.go:13-15` and dead snippets in `useStreamScroll.svelte.ts`/`useAction.svelte.ts`), 5 generation TODOs, 2 tonal markers. Behavior-preserving cleanup, one package per slice. | `mcp/skills/deslopper/scripts/scan_slop.sh {cmd,internal}` 2026-06-12 | 2 | 2 | 2 | 2 |

## Source Links

- CI: `glab api 'projects/47/pipelines?ref=main&per_page=30'` → 27 failed / 30; failing jobs 137381 (`test:race`), 137375 (`security:gosec`)
- gosec artifact: `glab api 'projects/47/jobs/137375/artifacts/gosec-report.json'`
- Transport: `.loom/149-plan-backend-hub-transport-stability-2026-06-12.md`
- Mills noise: `glab api 'projects/47/issues?state=opened&per_page=100'` → 67 `mills-escalation`
- Markers: `rg -n "TODO|FIXME|HACK|XXX" --glob '!vendor/**' .` → 8 real code TODOs (repo is clean of untracked FIXME/HACK)
- Hotspots: `git log --since='3 months ago' --pretty=format: --name-only | sort | uniq -c | sort -rn`
- Large files: `find ./internal ./pkg ./cmd -name '*.go' -not -name '*_test.go' | xargs wc -l | sort -rn`
- Slop scan: `bash mcp/skills/deslopper/scripts/scan_slop.sh {internal,cmd,pkg}`
