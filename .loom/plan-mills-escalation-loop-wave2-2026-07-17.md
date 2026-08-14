# Plan: Mills wave 2 — close the escalation loop + review cost

Date: 2026-07-17 · Branch: `claude/mills-escalation-loop-wave2` · Follows
`.loom/plan-mills-telemetry-optimization-2026-07-16.md` (shipped !1109, merged e2c8e6cc).

## Riskiest assumption + kill-test

**Load-bearing assumption**: Escalated backlog items whose run has an `mr_iid`
that GitLab reports as `merged` are safe to transition escalated→merged without
violating the one-way terminal-state guard (2b31f918) or racing the reconciler's
aggregate-version optimistic concurrency — i.e. `TransitionState` supports
escalated→merged and nothing else consumes escalated items concurrently.

**Kill test**: W1's unit test seeds an escalated item + escalated run with an
MRIID, points the fake GitLab client at state=merged, runs one reconcile sweep,
and asserts the item lands merged with version bump and the run is annotated —
plus a second sweep asserting idempotence (no double transition, no second issue
close). Runnable via `GOWORK=off go test ./pkg/mills/ -run TestGhostSpark`.

**Failure mode if wrong**: The sweep either wedges on version conflicts (harmless
but inert) or, worse, resurrects terminal states. The state-machine test catches
the latter before ship.

**Status**: not run (W1's first deliverable).

## Live evidence (2026-07-17, cluster)

- 91 of 141 backlog items sit in `escalated`. Spot-checks: `MILLS-2026-07-10-002`
  escalated on a merge-422 while its MR !1037 merged 2026-07-10T23:38 via MWPS;
  same for !1044. The runs list shows `merge:error` escalations whose MRs are
  `merged` — ghost sparks. Requeue is wrong (work exists); nothing reaps them.
- Issue auto-close exists only on the "later run succeeds" path
  (`pkg/mills/pipeline/escalate.go:121-131,387-421`); the MWPS-merged-later path
  never fires it.
- `pr_self_review` is a full SpawnWorker agent session
  (`pkg/mills/pipeline/dispatcher.go:1205`) on the global spawn agent
  (claude-opus-4-8 via anthropic backend). 7d cost: $12.34 vs implement $17.59 —
  36% of pipeline spend for a review pass. Policy has per-stage devbox substrate
  (`pkg/mills/policy.go:295-358` StageSubstrate/SubstrateForStage) but no
  per-stage agent/model override; `LOOM_MILLS_SPAWN_AGENT` flips ALL stages.
- The HUD run drawer now shows EscalationRetryable etc. (!1109) but offers no
  action; requeue exists only as an admin curl
  (`POST /api/mills/pipeline/runs/{backlog_id}/start?requeue=1`, proxied at
  `internal/hud/domain/mills/mills.go:120` as an admin POST).

## Slices (3, parallel, Opus implementers, isolated worktrees)

No slice touches CHANGELOG.md or docs/ (consolidated at integration). Go
verification: `GOWORK=off CGO_ENABLED=0 go build/test ./pkg/mills/... ./cmd/loom-mills-operator/...`.

### W1 — ghost-spark-auto-close (backend)
Own: `pkg/mills/reconciler.go` (new sweep), `pkg/mills/store/` (any new DAO
query), reuse of `pkg/mills/pipeline/escalate.go` auto-close helper, metrics.
- Reconciler tick sweep (capped, e.g. 10 GitLab lookups/tick, oldest-first):
  for escalated backlog items whose most recent run carries an MRIID, ask GitLab
  for the MR state; `merged` ⇒ transition item escalated→merged (aggregate
  version respected; one-way guard respected), annotate an event
  (`reconciler.ghost_spark_closed`), close the dedup'd escalation issue via the
  existing helper, bump a new `mills_ghost_sparks_closed_total` counter.
- MR states `closed` (abandoned) ⇒ leave escalated (human decision), but count
  via metric label so the pile is measurable.
- Idempotent; GitLab errors skip the item (retry next tick), never fail the tick.

### W2 — per-stage spawn agent override (backend)
Own: `pkg/mills/policy.go` (new `StageAgents map[string]string` +
`AgentForStage` accessor mirroring the StageSubstrate pattern incl. validation
against the spawn-driven stage set and allowed agent values), the SpawnWorker
wiring in `pkg/mills/pipeline/dispatcher.go` (`AgentFor func(stage) string`
alongside SubstrateFor), and the spawn request path so a per-stage agent
override reaches the spawn API (find where LOOM_MILLS_SPAWN_AGENT is consumed
and thread the per-stage value with global-env > policy-stage > default
precedence documented in the field comment — env stays the break-glass).
- Default behavior byte-identical when the map is empty.
- Purpose: run `pr_self_review` on a cheaper agent (e.g. sonnet-class) by policy
  alone; actually flipping production policy is a separate gitops change (needs
  the deployment policy-checksum bump), NOT part of this slice.
- Tests: policy parse/validation, precedence, dispatcher threading.

### W3 — HUD requeue action (frontend)
Own: `internal/hud/frontend/src/lib/components/mills/PipelineRunDetail.svelte`,
small store addition in `stores/mills.svelte.ts`, vitest for any pure helper.
- "Requeue" button in the run drawer for escalated runs (show when
  `run.State === 'escalated'`; annotate with the retryable chip state), calling
  the existing proxied admin POST `/api/mills/pipeline/runs/{backlog_id}/start?requeue=1`
  via the store's existing admin-token machinery (see mills.adminToken tests).
- Response handling: 201 ⇒ toast/inline "requeued as <run_id>" + refresh; 409 ⇒
  show `reason` (e.g. "state is merged" — the ghost-spark case reads as
  already-done); 403 ⇒ show the admin-token hint the other mutations use.
- Frontend verification: `pnpm build && pnpm test`; restore `dist/.gitkeep`.

## Integration & ship
Order W1 → W2 → W3; consolidated CHANGELOG; full Go + frontend gates; single MR
from this branch; auto-merge per Auto-Ship policy.
