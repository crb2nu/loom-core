# Technical Debt Remediation Plan — Cycle 7

## Summary

- Planning date: 2026-06-12
- Scope: red-main CI repair, hub transport hardening, Mills escalation noise, structural monoliths, deslop sweep
- Total items: 11 (9 new + 2 carryovers)
- Inventory: `.loom/tech-debt-inventory-cycle7.md` / `.json`
- Ranking: `.loom/tech-debt-priority-cycle7.md`
- Scoring model: impact 35%, risk reduction 30%, drag reduction 20%, effort inverse 15%

## Riskiest assumption + kill-test

**Load-bearing assumption**: The chronic red `main` (27/30 failed pipelines) is fully explained by exactly two independent breakers — the `test:race` fi-accel exclusion gap for `cmd/loom-mills-operator` (`.gitlab-ci.yml:595`) and the single gosec G202 finding at `pkg/mills/store/dao_workflow.go:185-188` — and fixing both restores green main and unblocks image builds.

**Kill test**: Land DEBT-070 + DEBT-071 in one MR. Pass = the MR pipeline is fully green AND the first post-merge `main` pipeline is green through `build:image:*` (≤30 min observation). Job evidence already isolates exactly these two jobs as the only failures in pipelines 13784/13772.

**Failure mode if wrong**: A third intermittent breaker (e.g. flaky test or runner infra) keeps main red and Wave 1 doesn't restore deploys; re-triage from the next failed pipeline's job list before starting Wave 2.

**Status**: passed 2026-06-12 — MR !702 pipeline green; first post-merge `main` pipeline ([13830](https://gitlab.flexinfer.ai/services/loom-core/-/pipelines/13830), sha `41799b93`) green end-to-end: 18/19 jobs success including `build:image:{loom-core,custom-server,loom-mills-operator}`, `test:race`, `test:unit`, `security:gosec`; the single non-success job is `ios:archive-export` (manual deploy gate, expected). No third breaker surfaced. Wave 2 unblocked.

## Wave 1 — Restore green main (immediate, this week)

Goal: main green, image builds/rollouts unblocked. Everything else is queued behind this because a red main hides every new regression.

| Item | Slice | Effort |
|---|---|---|
| DEBT-071 | `#nosec G202` (with justification) on the placeholder-join query in `dao_workflow.go:185-188`; verify `security:gosec` green | S |
| DEBT-070 | Replace the `.gitlab-ci.yml:595` regex with a computed exclusion: `go list -deps -test` filtered for `gitlab.flexinfer.ai/libs/fi-accel`; apply the same mechanism to the unit-test exclusion at line 485 | S-M |

Acceptance: first post-merge main pipeline green end-to-end; kill-test above passes.
Dependencies: none. Risks: over-broad exclusion hides real races — mitigate by logging the computed exclusion list in the job output.
Not in this wave: any transport or Mills code change.

## Wave 2 — Reliability debt (next 1-2 weeks)

| Item | Slice | Effort |
|---|---|---|
| DEBT-072 | Execute `.loom/149` Slices 1-5: (1) WS keepalive ping loop + read deadline in libs/mcp-go; (2) liveness-gated `GetConnection`; (3) exponential backoff replacing flat 30s; (4) health-probe timeout fix; (5) hub per-connection instance dedup. Each slice independently shippable; libs/mcp-go changes need a loom-core go.mod bump + image redeploy to reach the cluster | L |
| DEBT-073 | Four sub-slices: (a) ci_watch: classify poll-timeout as infra, cap total wall-clock, surface MR pipeline URL; (b) merge stage: treat 405 as terminal config error (check MWPS/approvals), not retryable; (c) scope gate: allowlist `testdata/mills-canary/heartbeat.md` for canary items; (d) escalation-issue dedupe + auto-close on superseding run; bulk-close the ~60 stale ones once | M |

Acceptance: `transport closed` ~0 across 24h with hub routing re-enabled for one server; new canary run produces zero false-positive escalations; open `mills-escalation` count < 10.
Dependencies: DEBT-072 slices 1-2 land in `libs/mcp-go` (separate repo MRs) before the loom-core bump.
Not in this wave: re-enabling `--hub-prefer` globally (do after 72h soak).

## Wave 3 — Structural decomposition (rest of cycle)

| Item | Slice | Effort |
|---|---|---|
| DEBT-074 | Split `internal/hud/spawn.go`: extract command-building (`buildAgentCommand` + shellQuote already memory-hardened), config rendering, and lifecycle/telemetry into sibling files with no exported-API change | M |
| DEBT-075 | Split `cmd/loom-mills-operator/main.go`: wiring vs HTTP handlers vs reconciler setup | M |
| DEBT-067 | Split `cmd_sync.go` into cmd_sync_{generate,pull,backup}.go (carryover; do before next profile feature lands) | M |
| DEBT-077 | Deslop sweep, one MR per package group: `cmd/` banner comments; `internal/` banner comments + commented-out code (incl. dead Swift block in recovery_telemetry_test.go and dead snippets in frontend hooks); keep parser section markers in spawn_*_parser.go if they aid navigation — judge in context | S×3 |
| DEBT-076 | Migrate cmd_tasks.go/cmd_sessions.go onto visibility contracts; add the missing contracts subpackage tests | S-M |

Not in this wave (explicitly deferred): DEBT-066 (MCP main splits, issue #50 — only if touched for features), DEBT-078 (frontend monoliths — fold into the open HUD audit board slices 9-12 instead of a parallel effort).

## Sequencing constraints

1. Wave 1 blocks everything (red main hides regressions in all other waves' MRs).
2. DEBT-072 libs/mcp-go slices precede the loom-core go.mod bump (two-repo dance, see memory `reference_local_loom_core_image_build`).
3. DEBT-074/075 splits should land before Mills feature work piles more onto those files.
4. Deslop MRs (DEBT-077) are rebase-cheap; schedule them when no large refactor MR is open against the same package to avoid conflict churn.

## Backlog persistence

- Loom: tasks under session namespace `loom-core/tech-debt-planning` (session 51cb781a688063f5)
- GitLab: one issue per item, label `tech-debt,debt-cycle-7`; DEBT-066 = existing #50 (relabeled, not duplicated); #46 closed as done
