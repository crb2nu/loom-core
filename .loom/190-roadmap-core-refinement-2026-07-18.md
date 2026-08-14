# Roadmap — Core feature & architecture refinement wave (2026-07-18)

Owner: claude-code (session vigorous-faraday). Successor to
`187-sprint-plan-integration-mills-scaling-2026-07-14.md` (Sprints 0–2 landed) and
executor for `product-spec-mill-floor-views-2026-07-18.md` §5.

## Riskiest assumption + kill-test

**Load-bearing assumption**: Mill-floor S0 (!1128, rebased onto `16d9671e`) merges
green and its shared surface (`router.svelte.ts` sub-views, `panelRegistry` loaders,
`mills.svelte.ts` getters `backlogByPriority`/`escalatedRuns`/`boltRuns`/`lineageFor`/
`millFloorSpine`/`fetchArchiveRuns`, `LineageRibbon` spine+strand modes) is sufficient
for S1–S4 to build against without further edits to those shared files.

**Kill test**: !1128 merged to main; `cd internal/hud/frontend && pnpm svelte-check`
green on main; a scratch component importing all six getters + `LineageRibbon`
type-checks. ≤15 min.

**Failure mode if wrong**: four parallel view slices collide on shared-file edits and
serialize into conflict-resolution work (exactly what S0 exists to prevent).

**Status**: not run (blocks Wave 1 view dispatch; S5/H1/H2 are NOT blocked).

## State of the world (verified 2026-07-18 evening)

- main `16d9671e` green (pipeline 19909) and **deployed**: `loom-hub-servers`
  kustomization applied at `main@16d9671e`; images `mcp/loom-core:20260718-200635`,
  `mcp/loom-mills-operator:20260718-200637` running; all Flux kustomizations Ready.
- Open MRs all shepherded this session: !1128 (S0, rebased), !1098 (incident codes,
  rebased + changelog-fragment conversion), !1097/!1126 (docs, pipelines retried),
  !982 (renovate GH actions) — all with auto-merge armed.
- Recent-days trajectory now on main: Mills escalation dedup by failure class,
  bounded repo bootstrap, S4 pod-owned execution supervisor, plan objective/slice
  connective tissue, gitlab-mcp MR review tools, weaver/research kimi path,
  telemetry cache TTL, HUD TS ratchet + spin-runs poller refcount.

## Wave 1 — Mill-floor views + reliability hardening (dispatch now)

Parallel Opus slices; each in its own worktree, conventional commit, changelog.d
fragment, MR with auto-merge. Frontend slices copy spec §6 rules verbatim.

| Slice | Scope | Blocked by | Source |
|---|---|---|---|
| **S5** Mills backend B1+B2 | `cmd/loom-mills-operator` handlers: backlog nil→`[]` coercion; expose MR web URL on runs | — | spec §5 S5 |
| **H1** plan_slice timeout classification | `agent CLI exited 124`/`command timed out` + `Reading additional input from stdin` are today `class=code` → 8×1h retry burns (issues #356–#359, #351). Classify as retryable **infrastructure** with a distinct reason (`spawn-agent-timeout`), and bound per-stage wall-clock so an attempt can't sit at the 1h cap repeatedly | — | issues #356–359, #351 |
| **H2** rubric-judge empty-envelope resilience at gates | `post_review_gate` (`spec_conformance`, `pr_self_review`) still dies on `no parseable score envelope; raw=""` (#348) despite the kimi-k3 judge fix — apply the same reasoning-recovery/retry path at the gate call sites, and classify a persistent empty envelope as external-dependency, not code | — | issue #348, memory kimi-judge fix |
| **S1** Warps view | `WarpsPanel.svelte`, delete `BacklogPanel` | S0 merge | spec §5 S1 |
| **S2** Shuttles view | `ShuttlesPanel.svelte`, delete `PipelinesPanel` | S0 merge | spec §5 S2 |
| **S3** Sparks view | `SparksPanel.svelte` (escalations + requeue UX) | S0 merge | spec §5 S3 |
| **S4** Bolts view | `BoltsPanel.svelte` (archive/tartan/shift report) | S0 merge | spec §5 S4 |
| **S6** cleanup + docs | remove retired panels/redirect stragglers, AGENTS.md | S1–S4 | spec §5 S6 |

## Wave 2 — Autonomy throughput (after Wave 1 evidence)

1. **Escalation auto-close / bulk-triage** (DEBT-073 remainder): close mills
   escalation issues whose backlog item merged or whose MR is merged (e.g. #346 is
   already stale); fold audit-advisory issues (#339–#355 class) into a digest instead
   of one issue each. Unlocks unattended overnight shifts.
   - **Audit-advisory digest — SHIPPED (in flight 2026-07-20)**: new advisory
     findings now fold into a rolling per-UTC-day digest issue (one issue/day +
     a comment per finding) via `pkg/mills/audit/followup.go` +
     `FindOpenAuditDigest`. See `191-ralph-audit-advisory-digest-2026-07-20.md`.
     REMAINING here: (a) escalation-issue auto-close for merged backlog items
     (partly covered by ghost-spark sweep `5e82f409`); (b) one-time bulk-close of
     the pre-existing stale advisory issues (#339–#355 class), an ops sweep.
2. **Raise Mills concurrency** under the S4 supervisor + admission caps; watch KPI
   snapshot + shift reports.
3. **Dynamic workflows next slice** (Starlark; S1 kill-test PASSED) on the S4
   execution substrate.
4. **Pattern Loom**: A2 engram verification, cross-repo stamping.

## Wave 3 — Deferred (unchanged gates)

- Fleet S6 (hub child broker, clustered Postgres), S7 (MCP Tasks adapter) — gated on
  S3–S5 fleet-plan evidence.
- Weaver S6-full/S7 registry, squad planner (gap map).
- gpt-5.6 terra/sol re-flip — gated on #347–#351 class fixes + codexVersion bump.
- Off-LAN MCP-via-hub (Cloudflare Access).
- Zed `context_servers` surface + ICC tool-cap rebalance (tooling audit leftovers).

## Evidence

- MR/pipeline/deploy state: GitLab MRs !982/!1097/!1098/!1126/!1128, pipelines
  19389/19757/19913/19914/19915, `flux get kustomizations` + `kubectl get deploy`
  2026-07-18 ~22:10Z (this session).
- Escalation burn: issues #356–#359 stage tables (8 attempts, ~1h each, $0.00).
- Slice specs: `product-spec-mill-floor-views-2026-07-18.md` §5–§7.
