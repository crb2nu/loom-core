# Plan: Wrap-Up + Next Killer Wave

> Date: 2026-04-25
> Author: Claude Opus 4.7 (worktree `condescending-mendeleev-6e86e1`)
> Status: Proposed — awaiting review
> Anchors: `.loom/86-multi-sprint-roadmap-2026-04-14.md`, `.loom/90-implementation-plan-next-platform-wave-2026-04-16.md`, `ROADMAP.md`

## TL;DR

1. **Ship the easy wins first** (≤1 day): merge MR `!161` (auto-verify base) and open an MR for `feat/responses-m2-guardrails`. Both are clean, single-commit, and align with Sprint 1 / Plan 90 Slice 1.
2. **Garbage-collect 8+ stale codex/* sibling worktrees** plus the dirty `.worktrees/feat-*` leftovers — they map to closed issues or already-merged work and are now noise on disk and in `git branch`.
3. **Then run the "Spawn Multi-Turn + Sprint 2 Observability" wave** — three parallel slices that build directly on the just-shipped iOS Spawn Phase 1 (Slices A–D + Budget Widget) and complete Sprint 2 of the active multi-sprint roadmap.

## Current State Snapshot

### What just shipped (last 2 weeks, main)

| Theme | Commits | Status |
|---|---|---|
| **iOS Spawn Phase 1** | `e18b87e8` → `d4d2f389` (8 commits) | Slice A (top-level Spawn tab), B (Quick Spawn), C (Templates), D (Telemetry + swipe), Budget Widget, file-change line counts | ✅ Complete |
| **Sprint 1: Codex keepalive + recall meta** | landed via `cfb8f49e` and follow-ups | ✅ Shipped (per ROADMAP entries) |
| **HUD agent lifecycle hardening** | `0c294311`, `38980b63`, `9a059a7e`, `f0ee7160` | Orphan reaping, canonical agent↔session join, hub teardown reconnect fix, lifecycle contract docs |
| **Spawn (Weaver) backend** | MRs `!204`–`!210` | SubAgent backend discriminator, Daemon spawn bridge, dispatch metrics + sessions endpoint, project-name resolution, Claude OAuth token wiring |
| **Brand kit MCP** | `eb65cce3` → `f82b79db` | New wrapper |
| **Generator: Monitor tool permission** | `7a938cb1` | Allows hook-driven Monitor in Claude Code settings |

### Open MRs

| MR | Branch | State | Action |
|---|---|---|---|
| `!161` | `feat/auto-verify` | **CI green, 0 comments, 1 commit** (Pipeline `6800` success across all 13 stages) | **Merge now** |
| `!105` | `renovate/configure` | Renovate onboarding, low priority | Defer or close |

Source: `glab mr list`, `glab mr view 161`, `glab ci status -b feat/auto-verify`.

### In-flight branches with potential

| Branch | Ahead of main | Dirty? | Recommendation |
|---|---|---|---|
| `feat/auto-verify` | 1 | clean | **Merge MR !161 now** |
| `feat/responses-m2-guardrails` | 1 | clean | **Open MR** (Plan 90 Slice 1, has tests) |
| `codex/issue-21-cli-contracts` | 1 | clean | **Delete** — Issue #21 is ✅ Complete on ROADMAP |
| `codex/issue-21-hud-contracts` | 1 | clean | **Delete** — same |

### Stale worktrees worth pruning

Sibling worktrees under `~/workspace/` and `~/workspace/services/` (per workspace layout policy these are legacy linked trees and should not exist for new work):

| Worktree | Last commit | State | Reason |
|---|---|---|---|
| `loom-core-conn-limit-slice-2/3` | 2026-03-26 | ahead=0, dirty | Already merged |
| `loom-core-devbox` (`codex/devbox-maturity-4`) | 2026-03-24 | ahead=12, dirty | Superseded by `feat/devbox-k8s-seams` (now merged via Plan 90) |
| `loom-core-gemini` | 2026-03-23 | ahead=0, dirty | Merged |
| `loom-core-hud` (`codex/hud-rbac-audit-53b`) | 2026-03-24 | ahead=14, dirty | Superseded by RBAC issue close-out |
| `loom-core-rbac` + `loom-core-rbac-24-merge` | 2026-03-23/26 | ahead=11/12, dirty/clean | RBAC issue #27 closed |
| `loom-core-scope` | 2026-03-24 | ahead=12, dirty | Mobile scope-discipline #37 still open but March work is stale |
| `loom-core-issue-21-cli`, `loom-core-issue-21-hud` | 2026-03-26 | ahead=1, clean | Issue #21 closed |

Repo-local `.worktrees/feat-*` and `fix-*` mostly at `ahead=0` (already merged) but with stray dirty files:

| Worktree | Notes |
|---|---|
| `feat-daemon-trace-depth` | ahead=0, 11 dirty — leftover after `feat/platform-enhancement-wave` merge |
| `feat-workflow-auto-verify-retro` | ahead=0, 15 dirty — leftover |
| `feat-presence-session-telemetry` | detached HEAD, 5 dirty |
| `fix-claude-desktop-proxy-config` | ahead=0, 1 dirty |

Plus 17 `.claude/worktrees/` directories at various commits — Claude-managed sandboxes. Keep the 1–2 active ones, reap the rest with `agent_worktree_cleanup` / `git worktree remove`.

## Wrap-Up Wave (≤1 day)

### W1: Merge MR !161

```bash
glab mr merge 161
```

CI is green at pipeline 6800 (lint, vet, fmt, unit, integration, enterprise-smoke, guardrails, build, build:image:loom-core, build:image:custom-server). No comments. Zero risk merge.

### W2: Open MR for feat/responses-m2-guardrails

This branch is a single commit (`635caf3e`, Apr 16) that delivers exactly Plan 90 Slice 1:
- `pkg/openairesponses/preflight.go` — structured estimates
- `pkg/openairesponses/compaction.go` — observable strategy
- `pkg/openairesponses/orchestrator.go` — preflight wiring
- `cmd/loom/cmd_responses.go` — CLI surface
- 469 LOC added across 10 files, with tests in `*_test.go`

```bash
git -C /Users/cblevins/workspace/services/loom-core/.worktrees/feat-responses-m2-guardrails push -u origin feat/responses-m2-guardrails
glab mr create --source feat/responses-m2-guardrails --target main --title "feat: OpenAI Responses M2 token/context guardrails"
```

### W3: Worktree garbage collection

Use the workspace policy (`~/workspace/CLAUDE.md` → "Workspace Layout Policy") to clear:

```bash
# Sibling worktrees: superseded / merged
for wt in loom-core-conn-limit-slice-2 loom-core-conn-limit-slice-3 loom-core-gemini loom-core-rbac loom-core-rbac-24-merge loom-core-issue-21-cli loom-core-issue-21-hud; do
  git -C /Users/cblevins/workspace/services/loom-core worktree remove --force ../$wt 2>/dev/null || \
  git -C /Users/cblevins/workspace/services/loom-core worktree remove --force ../../$wt
done
git -C /Users/cblevins/workspace/services/loom-core worktree prune --expire now

# Then evaluate (one at a time, since dirty):
#   loom-core-devbox, loom-core-hud, loom-core-scope
# Each has 11–14 commits; review for any unmerged value before removing.

# Repo-local already-merged trees with stray dirt:
for wt in feat-daemon-trace-depth feat-workflow-auto-verify-retro feat-runtime-guardrails-wave feat-devbox-k8s-seams feat-hud-traces fix-hud-catalog-registry fix-claude-desktop-proxy-config issue-23-rebase devbox-tool-pin; do
  git -C /Users/cblevins/workspace/services/loom-core worktree remove --force .worktrees/$wt
done

# Closed-issue branches:
for br in codex/issue-21-cli-contracts codex/issue-21-hud-contracts codex/conn-limit-slice-2 codex/conn-limit-slice-3; do
  git -C /Users/cblevins/workspace/services/loom-core branch -D $br
done
```

⚠️ **User confirmation needed before running W3** — these are destructive and dirty trees may hold uncommitted work.

### W4: Update `.loom/00-index.md`

Append a "Current Planning Addendum (2026-04-25)" pointing at this doc and noting that Sprint 2 work supersedes Plan 90 (which only ever shipped Slices 1+ partial 2). Mark Plan 90 as "absorbed into Sprint 2" so future agents stop opening it as the active plan.

## Sprint Status Reconciliation

The 4-sprint roadmap (`86-multi-sprint-roadmap-2026-04-14.md`) is the active program. Reconciled status as of today:

| Slice | Plan | Status | Evidence |
|---|---|---|---|
| **S1.1** Codex keepalive | Sprint 1 | ✅ Shipped | ROADMAP.md "Recently Shipped → 2026-04-14: Codex keepalive bootstrap" |
| **S1.2** Recall quality + observability | Sprint 1 | ✅ Shipped | ROADMAP.md same entry; `recall_meta` surfaced |
| **S1.3** Auto-quality-gate | Sprint 1 | 🟡 Base shipped, integration pending | MR !161 adds `auto_verify` step to executor + tests; feature-dev/bugfix workflows still use human approval gates |
| **S1.4** Test coverage (compaction, export/import, hybrid_search, codebase_sync) | Sprint 1 | ❌ Not started | Files still 0% coverage |
| **S2.1** OTel trace explorer | Sprint 2 | 🟡 Audit-backed traces shipped, daemon-grade depth pending | HUD Traces panel live; Plan 90 Slice 2 (`feat/daemon-trace-depth`) abandoned with dirty worktree |
| **S2.2** Server catalog CLI | Sprint 2 | ❌ Not started | `cmd/loom/cmd_catalog.go` does not exist |
| **S2.3** Session retro automation | Sprint 2 | ❌ Not started | `mcp/skills/session-retro/` referenced but no `postSessionEnd_retrospective` hook |
| **S2.4** Spawn↔Session bridge | Sprint 2 | 🟡 Hierarchy + cross-nav shipped; mobile parity pending | Multiple recent merges (`!211`, `!218`); mobile spawn detail still one-shot |

## Killer Next Wave: "Spawn Multi-Turn + Sprint 2 Catalog/Retro"

Three parallel slices. They directly extend the Phase 1 iOS spawn momentum, complete Sprint 1 (S1.3, S1.4), and clear two of the four unstarted Sprint 2 surfaces.

### Slice A — Multi-Turn Spawn (S3.3 pulled forward)

**Why now:** iOS Spawn Phase 1 just shipped. Operators can launch and monitor — but cannot redirect. The SDK drivers in `tools/spawn-driver/` already support multi-turn; only the API surface and UI are missing.

**Scope (1 worktree, ~3–4 days):**

1. **Backend:** `POST /api/agent/spawn/{id}/message` — accepts free-text, pipes to existing SDK driver via daemon spawn bridge.
2. **Backend:** `POST /api/agent/spawn/{id}/interrupt` — soft-cancel current turn without killing the spawn.
3. **HUD:** message-input row in `SpawnDetailPanel.svelte`, with optimistic display and SSE confirmation.
4. **iOS:** message-input affordance in spawn detail view + "Interrupt" swipe action (parity with Slice D telemetry pattern).
5. **Mobile API:** `/api/mobile/v1/agent/spawn/{id}/message` for parity.

**Acceptance:**
- Operator can send "now also update the docs" mid-run and the spawn picks it up next turn.
- Interrupt cleanly stops the current turn and surfaces a `agent.spawn.interrupted` SSE event.
- iOS SwiftUI tests cover the message-send flow end-to-end against a fake backend.

**Files (predicted):**
- `internal/hud/domain/spawn/handler_message.go` (new)
- `internal/hud/domain/spawn/handler_interrupt.go` (new)
- `internal/hud/spawn.go` — pipe through to SDK driver
- `internal/hud/frontend/src/lib/components/SpawnDetailPanel.svelte`
- `apps/loom-companion-ios/Sources/LoomCompanion/Views/SpawnDetailView.swift`
- `internal/hud/api_mobile.go` — mobile parity endpoint

### Slice B — Server Catalog CLI (S2.2)

**Why now:** With 47 servers + 500+ tools, registry-as-source-of-truth is now an operator burden. CLI parity with the existing HUD catalog framing is the smallest unlock with the highest discovery payoff.

**Scope (1 worktree, ~3 days):**

1. `loom catalog list [--status active|inactive]` — table of servers with tool count, status, missing env hints.
2. `loom catalog enable <server>` — adds to registry, regenerates configs, returns env to set.
3. `loom catalog disable <server>` — clean removal.
4. `loom catalog search <query>` — fuzzy match across name, description, tool names.
5. Reuse `mcp/context/registry.yaml` reader in `pkg/generator/registry.go`. New `pkg/catalog/` for shared logic between CLI and existing HUD catalog API.

**Acceptance per Sprint 2 spec:** see `.loom/86-multi-sprint-roadmap-2026-04-14.md` lines 226–230.

### Slice C — Session Retro Automation (S2.3) + S1.4 Test Coverage

**Why combined:** Both are "polish/safety net" work and orthogonal to A and B. Retro automation needs test coverage in `pkg/agentcontext` to land safely; bundling them keeps the sandbox warm.

**Scope (1 worktree, ~3–4 days):**

1. Add `postSessionEnd_retrospective` hook entry in `pkg/generator/configs_hooks.go`.
2. Implement `mcp/skills/session-retro/scripts/session-retro.sh` — extracts failures, novel solutions, friction from session log; writes to `.loom/local/retro-<session-id>.md`.
3. Append finding pointers to `.loom/local/retro-queue.md` for batch review.
4. Add S1.4 test coverage: `compaction_execution_test.go`, `memory_export_import_test.go`, `hybrid_search_test.go`, `codebase_sync_test.go` (3–5 tests each, target 0% → >60%).

**Acceptance:**
- Session end triggers retro extraction async (non-blocking).
- `go test -race ./pkg/agentcontext/...` clean and 4 new test files cover the previously-untested code.

### Why this set?

| Lens | Slice A | Slice B | Slice C |
|---|---|---|---|
| **Builds on hot momentum** | Yes — iOS Spawn Phase 1 | No | No |
| **Roadmap leverage** | Sprint 3 pulled forward | Sprint 2 unblock | Sprint 1 finish + Sprint 2 unblock |
| **Risk** | Low (drivers exist) | Low (data-driven) | Low (additive) |
| **Parallelizable** | Independent | Independent | Independent |
| **Effort** | 3–4 days | 3 days | 3–4 days |

All three are independent — ship as parallel worktrees per `parallel-slice-ship` workflow.

## Out of Scope (Defer Explicitly)

- **Embedded Orchestra in daemon (S3.1)** — 5–7 day critical path with FlexInfer vLLM dependency. Defer until vLLM is validated.
- **Recall reranking (S3.2)** — adds latency and cross-encoder dependency. Worth doing but sequence after Slice C lands a clean test surface.
- **TDD-first workflow (S3.4)** — depends on `auto-quality-gate` integration into feature-dev. Sequence after Slice C and the auto-verify integration.
- **cmd_sync split (S4.1)** + data-driven profiles (S4.2) — Sprint 4. Important debt but no user-visible win this wave.
- **Plan 90 Slice 2 (daemon trace depth)** — keep deferred. The `feat/daemon-trace-depth` worktree is dirty/abandoned. Re-plan as a focused effort once HUD Traces P50/P95 demand emerges.
- **Mobile scope-discipline #37** — open issue but no concrete pressure right now; keep gating mobile mutations to monitoring + lifecycle controls.

## Validation Checklist

Per slice:
- [ ] `devbox_quality_gate(project="loom-core", agent_id="claude-code")` green
- [ ] `go test -race ./pkg/...` clean (where touched)
- [ ] `pnpm --dir internal/hud/frontend build` (Slice A only)
- [ ] iOS Xcode build (Slice A only): `xcodebuild ... -scheme LoomCompanion -sdk iphonesimulator`
- [ ] MR created, CI green, auto-merge requested

## Sources

- `git log --oneline main -30` — recent main commits
- `git worktree list` — 39 worktrees enumerated
- `glab mr list` — 2 open MRs
- `glab mr view 161` — MR !161 ready, CI green
- `glab ci status -b feat/auto-verify` — pipeline 6800 success
- `git show --stat feat/responses-m2-guardrails` — 1 commit, 469 LOC, 10 files
- `.loom/86-multi-sprint-roadmap-2026-04-14.md` — active 4-sprint program
- `.loom/90-implementation-plan-next-platform-wave-2026-04-16.md` — runtime guardrails wave
- `ROADMAP.md` — Tier 1–4 status (last updated 2026-04-14)
- `~/workspace/CLAUDE.md` → "Workspace Layout Policy" + "Auto-Ship Policy"
