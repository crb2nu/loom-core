# Spec: Trustworthy Verdicts — Phase 0 grounding + slice plan

**Date**: 2026-08-06
**Lineage**: [.loom/brainstorm-next-sprint-2026-08-06.md] (decision + passed kill-test)
**Mode**: human-led core, parallel Mills lane for HUD-debt plans

## Phase 0 — re-grounding the six queued plans against main

Plan-store drift was the stated hazard; it is real. Verdict per plan:

| Plan (store id) | Verdict | Evidence on main |
|---|---|---|
| `plan-council-classify-external-ci-failures-…` | **DONE — supersede** | Closed taxonomy `pkg/mills/pipeline/failure_classifier.go` (transient/transient_quota/infrastructure/code/configuration + Retryable/FreeRetry/Terminal); `ClassifyCIFailureSignature` (`runner_system_failure` → transient, wired 08-04 `33893048`+`223ed1d6`); `mcperror.ClassifyExternalCIIncident` + `telemetry.LookupIncidentCode` (stable incident codes); `external_dependency_incident` classification class + `store.IncidentRecord`. |
| `plan-council-surface-external-incident-…-consumer` | **DONE — supersede** | `retry_policy.go` gates on `store.ExternalDependencyIncidentClassification`; `reconciler_auto_requeue.go` counts recent incidents; EscalationClass/FailureClass on the run wire contract, rendered in HUD. |
| `plan-mills-gl-47-334` (durable council runs) | **DONE — supersede** | `POST /api/mills/council/async` (#334): 202 + committed run id, pass detached from the request. |
| `plan-council-enforce-proposal-suppression-…` | **MOSTLY DONE — small residual slice** | `pkg/mills/council/classification_policy.go`: deterministic suppression + storage-health fail-closed planning, parity-pinned to contracts. Residual: verify enforcement coverage at every planner entry + suppression on *verdict* (below), not just first classification. |
| `plan-council-reconcile-dual-source-…-persisted-verdict` | **LIVE — the sprint core** | Nothing reconciles a run's persisted classification after the world moves. |
| `plan-council-prevent-conflicting-terminal-outcomes-…` | **LIVE — merges into core** | Same gap, state-machine face of it. |

**The surviving gap, precisely.** The kill-test's five false escalations decompose
against today's main as: 2 fixed by the 08-04 classifier wiring (historical); the
!1390/!1391 green-MR class fixed by `GreenMRAdopter` (its comment cites exactly
those MRs). But in every one of those cases — and in the two post-fix stale
escalations (ended 08-04) — **the run's terminal record still says
`escalated/code` forever**. Adoption merges the MR; nothing amends the verdict.
Consequences: config-outcome and judge-calibration reports count these runs as
escalations (learning loops train on falsehoods); escalation reviews re-litigate
resolved incidents; storm detection counts corrected runs; retry/suppression
policy reads the stale class.

## Sprint core: the persisted, supersedable verdict

One principle: a run's verdict is an append-only history whose HEAD is what
every consumer reads — never a frozen copy of the first classification.

### S1 — Verdict record + reconciliation sweep (core)
Append-only `run_verdicts` (or verdict events on the existing events table —
decide in-slice; events table already has the DAO/index substrate): initial
entry = escalation classification at terminal time; superseding entries carry
`source` (green-mr-adopter, mrwatch merged, regression.attributed, operator
override) + `reason` + evidence ref. Reconciler pass (rides the existing
ghost-spark sweep cadence): escalated run whose MR is merged/green →
`superseded: false_escalation` with the class the evidence supports.
Guards: append-once per (run, source, evidence); no verdict flapping (a
supersede requires strictly newer evidence).

### S2 — Learning loops read the verdict HEAD
`RunTerminalOutcome` gains verdict; config-outcomes / judge-calibration /
promotion partition by current verdict (label corrected runs distinctly —
`merged_after_escalation`, never silently folded into `merged`); regression
attribution unchanged (it is itself a verdict source).

### S3 — Policy reads the verdict
Retry policy + auto-requeue + council suppression consult verdict HEAD instead
of the frozen EscalationClass; storm/anomaly detection (foreman) discounts
superseded escalations — closes the 08-07 `escalation_storm` class where
corrected runs keep feeding the storm counter.

### S4 — Surfaces
Run drawer evidence block + Mill Staff: verdict chip with supersede history;
demand-log/escalation review shows current verdict; changelog + docs.

### Residual slice R1 (from plan 3)
Audit planner entry points for classification-policy coverage; extend
suppression to consult verdicts (S3 dependency).

**Order**: S1 → S2/S3 in parallel → S4 + R1. Each slice: branch off main,
changelog fragment, MR, auto-merge (standard Landing Procedure).

## Mills lane (parallel, dispatch as-is)
`plan-mills-bl-hud-mills-drawer-unify-20260725` (DetailDrawer collapse) and
`bl-hud-mills-metriccard-unify-20260725` (MetricCard unification) — mechanical,
well-bounded, no contention with the core (drawer file will be touched by S4;
sequence Mills drawer-unify BEFORE S4 or accept a small rebase).

## Plan-store hygiene (Phase 0 actions)
Mark the three DONE plans superseded with evidence links; retitle the two core
plans to reference this spec; note residual slice on the suppression plan.

## Success measures (already emitted)
- Rate of escalations later superseded (new, from S1) — the honesty metric.
- Foreman storm anomalies counting only non-superseded escalations (S3).
- Config-outcome/judge-calibration escalation counts excluding corrected runs (S2).

## Riskiest assumption (inherited, PASSED)
See brainstorm doc — rule-based classifiability validated 2026-08-06 against
cluster operator + GitLab. S1's own residual assumption: the events/DAO
substrate supports the verdict-history read cheaply (mitigated by !1474's
filtered listers riding `idx_events_occurred`).
