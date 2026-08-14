# Brainstorm: next most impactful sprint (loom-core / Loom platform)

**Date**: 2026-08-06
**Triggered by**: post-HUD-refresh review — "let's review, brainstorm, and plan out our next most impactful sprint"
**Constraints noted**: none stated; inferred from workspace practice — Mills factory available for dispatch, sprint mechanics proven (waves 1–4), homelab ops budget (no new heavy infra without cause)

## Review — where things stand (2026-08-06)

- **Just shipped this session**: HUD chrome refresh (!1469), status-bar honesty (!1473), Mill Staff report-cap + refetch-loop + error-copy fixes (!1474, auto-merging). Store-audit chip for the `$effect` loop class running separately.
- **Self-improving factory**: waves 1–4 SHIPPED (truth sweep, ground truth events, learning loops, surfaces + demand). Wave-5 candidates queued in memory: prompt-hash provenance, `operator` actor unification, ClickHouse offload, Grafana row for learning gauges, per-model calibration labels, exhaust-intake board rows, foreman's log.
- **Plan store**: 100 plans; 28 planned + 6 draft. Largest coherent loom-core cluster (4 plans): **external CI failure classification** → surface in consumer → suppression/fail-closed planning → dual-source verdict reconciliation + retry gating. Also queued: Mills run-integrity (conflicting terminal outcomes, durable council runs), HUD debt (DetailDrawer/MetricCard consolidation), config preflight validators, embedder-fallback telemetry.
- **Live pain signal**: foreman opened `escalation_storm:2026-08-07` **today** (alert.dryrun + file_issue.dryrun + anomaly_opened in Mill Staff recent actions). Memory corroborates: bogus code escalations from runner failures; escalated runs' green MRs orphaned; budget no-op DoS.
- **Scale signal**: the events firehose now exceeds 10k rows/336h — first cap trip (fixed in !1474 by filtering, but growth continues).
- **Deferred architecture**: S7 workflow-template registry (squads↔workflows convergence seam) still unbuilt; squads Planner/ensemble enforcement dead code (gap map 29d old — verify before acting).
- **Near-term date**: the 2026-08-08 Alley promotion review runs off the promotion report — unblocked by !1474.

## Phase 1 — Framings

### F1 — Finish wave 5: make the learning signals consumable

Waves 1–4 built evidence (judge calibration, regressions, config outcomes, demand log) but consumption is thin: no Grafana row, exhaust-intake events unemitted (board rows blocked), provenance prompt-hashes empty, two operator actor spellings split the audit trail. Sprint = close the loop from evidence to daily operator/factory behavior.

- **Bet**: compounding — every future decision (promotions, model pins, policy flips) gets cheaper when the signals are one glance away.
- **Risk**: polish on a working system; the signals may already be "consumable enough" via Mill Staff + /api reports.

### F2 — External-CI incident classification arc (the queue's biggest cluster)

Ship the four already-planned items as one arc: classify external CI failures with structured incident codes; surface classification in a consumer; enforce proposal suppression + fail-closed planning on classified incidents; reconcile dual-source failure classification into a persisted verdict that gates retry policy. Today's escalation storm is precisely the failure mode this kills.

- **Bet**: infra flakes misread as code failures are the #1 source of wasted escalations, orphaned MRs, and operator interrupts.
- **Risk**: classification taxonomies rot; plans may have drifted from main (truth-sweep era) — must be re-grounded before dispatch.

### F3 — Mills run-integrity hardening

Prevent conflicting terminal outcomes for a run; make operator-triggered council runs durable/edge-disconnect safe; sweep the zombie/husk/ghost-spark class (workflow zombie on terminal spawn is a known FREEZE). Correctness of the autonomy substrate over new features.

- **Bet**: every wedge burns the scarcest resource — operator attention — and un-trustworthy terminal states poison the learning loops.
- **Risk**: rare-event bugs; hard to reproduce, verification is expensive, and runbooks already contain the recoveries.

### F4 — S7 template registry: squads↔workflows convergence

Build the designed-but-deferred seam: `ManifestSpec.WorkflowTemplate` + `ResolveWorkflowTemplate` in the reconciler so squad manifests select declarative workflow templates (S6-full machinery becomes reachable beyond hardcoded canaries; budget envelopes get a home).

- **Bet**: architecture leverage — new autonomy patterns become manifest edits, not code sprints.
- **Risk**: 29-day-old gap map may be stale; big-bang architecture with little same-week operator payoff; it was deferred deliberately.

### F5 — HUD debt + honesty sweep

Ride the refresh: land the effect-loop audit results, execute the two queued consolidation plans (DetailDrawer, MetricCard), re-run the dark-endpoint audit (63 at last count), and generalize this week's honesty fixes (surface server error bodies everywhere; no surface asserts data it never fetched).

- **Bet**: the HUD is the cockpit; each lie or wedge discovered by the operator (like today's "Disconnected") taxes trust in everything else.
- **Risk**: mostly mechanical work a factory lane could absorb — spending the human sprint here has low marginal value.

### F6 — Get ahead of events-table scale

The 10k cap trip was the first growth symptom. Options ladder: retention/pruning policy for bookkeeping kinds → SQL-side aggregation for reports → ClickHouse offload (wave-5 candidate) only if query pressure demands.

- **Bet**: cheaper to shape data growth now than to firefight the next surface that scans the firehose.
- **Risk**: premature — !1474 removed the acute pain; ClickHouse adds ops burden the homelab doesn't need yet.

### F7 — Sprint = feed the factory, not code

The 28-planned backlog IS the sprint. Human time goes to grooming (re-ground drifted plans against main, split slices, set kill-tests) and dispatching to Mills; the factory implements. Measure: plans-to-merged throughput, escalation rate per dispatched plan.

- **Bet**: waves 1–3 proved 13-MRs-overnight; the constraint is groomed demand, not implementation capacity.
- **Risk**: dispatching drifted/undergroomed plans produces orphaned MRs and escalation noise — the very storm we saw today.

### F8 — Personal-product bet (familyforge / GTM engine)

Point the whole apparatus at a non-platform product for a sprint: familyforge (cycle/hormone tracking web+iOS) or the dogfooded GTM lead-gen engine. Cross-repo keystone is proven; the factory only matters if it ships products people use.

- **Bet**: platform value is realized (and its gaps exposed) only under real product load.
- **Risk**: splits attention across new repos lacking factory substrate; platform trust issues (storms, wedges) would interrupt anyway.

## Phase 2 — Cross-Pollinations & Tensions

### Combinations

- **F2 + F3 → "Trustworthy verdicts"**: these are one move, not two. The classification arc's capstone plan ("reconcile dual-source failure classification into a persisted verdict and gate retry policy") *is* run-integrity work: a single verdict pipeline from runner failure → classified incident → one persisted terminal outcome → retry/suppression policy → surfaced in Mill Staff + config-outcome reports. It simultaneously kills escalation storms (today's pain) and cleans the ground truth the wave-1–4 learning loops train on.
- **F7 + F5 (scheduling, not mechanism)**: the mechanical HUD-debt plans are ideal factory feedstock — groom and dispatch them as a parallel Mills lane while the human-led sprint does Trustworthy Verdicts. Two lanes, no contention.

### Tensions

- **F4 vs. F2/F3/F1**: build new capability vs. harden/exploit the live machine. That's the real axis. Waves 1–4 just landed and their surfaces are showing trust gaps (storms, misclassified failures, wedges); exploiting/hardening wins until template demand is felt concretely (e.g., a third hardcoded canary workflow would be the tell).
- **F8 vs. everything**: product pull vs. platform push. Worth scheduling deliberately — not as the default next sprint while the storm signal is live.

## Phase 3 — Convergence

### Recommended: F2 + F3 — the "Trustworthy Verdicts" sprint

Four reasons. (1) **Live pain**: the foreman opened an escalation-storm anomaly today; this arc is its structural fix, not a mitigation. (2) **Pre-groomed demand**: it's the largest coherent cluster the council already planned — the sprint starts at re-grounding, not ideation. (3) **Compounding**: clean persisted verdicts upgrade every learning loop shipped in waves 1–4 (calibration, config outcomes, regression attribution all join against terminal outcomes). (4) **Measurable**: escalation-noise rate, %-of-escalations-with-infra-classification, orphaned-MR count — all already emitted as events/metrics. Sprint shape: Phase 0 re-ground the four plans against main (plan-store drift is a known hazard) + run the kill-test below; then dispatch groomed slices through the proven scout→implementer lanes with the HUD-debt plans as a parallel low-risk lane (F7+F5).

### Runner-up: F1 — finish wave 5 (consumable learning signals)

Choose this instead if Phase 0 shows the classification cluster badly drifted (council plans superseded by shipped work), or if the 2026-08-08 Alley promotion review exposes that evidence surfaces — not verdict quality — are the binding constraint. It's a lighter sprint with guaranteed-shippable slices (Grafana row, actor unification, exhaust-intake events) and zero architectural risk.

### Open question

Sprint execution mode: **factory-led** (groom + dispatch to Mills lanes, human supervises — wave-style, high throughput, needs the storm quiet enough to trust escalations) or **human-led with factory assist** (Claude sessions implement the verdict pipeline core, Mills absorbs only the mechanical HUD-debt lane)? Given the verdict pipeline changes how escalations themselves are judged, there's a bootstrapping argument for human-led on the core.

## Riskiest assumption + kill-test

**Load-bearing assumption**: Recent escalated Mills runs carry enough machine-readable failure evidence (job `failure_reason`, `CI-INFRA-FAILURE:` sentinel lines, runner exit classes) that ≥80% of infra-caused escalations from the last 14 days can be classified by deterministic rules — i.e., the classification arc can be rule-based first, no model in the loop.

**Kill test** (≤30 min): pull the last 25 escalated runs from the operator (`GET /api/mills/... escalated set`), fetch each run's failing job trace via the GitLab API, and tally: (a) has `failure_reason` ≠ script_failure, (b) contains `CI-INFRA-FAILURE:` sentinel, (c) matches the known infra signatures from memory (gosec OOM 137, apiserver 502 wedge, runner SIGTERM 143, Harbor pull errors). If <80% of the human-judged-infra subset is covered by (a)∪(b)∪(c), the rule-based bet fails and the arc needs a signature-mining phase first (F2's scope grows ~2×).

**Failure mode if wrong**: we'd ship a classifier that labels only the easy minority, the suppression gate stays effectively off, escalation storms continue, and the sprint's headline metric doesn't move.

**Status**: **PASSED** (run 2026-08-06, ~20 min, cluster operator + GitLab API)

Evidence — 14-day window, 106 escalated runs (EscalationClass: code=60,
config=16, transient=15, infra=10, empty=5). Sampled the 12 most recent
escalated runs with MRs (11 classed `code`, all at ci_watch):

- 7/12 genuinely code — `failure_reason=script_failure` on lint/test/vet;
  two trace tails spot-checked (staticcheck finding; real unit-test failure).
- **5/12 falsely or stale-escalated as `code`** — and 100% rule-catchable:
  - 2 pipelines went green only after retrying `runner_system_failure` jobs
    (infra flake escalated as code; failure_reason is machine-readable);
  - 1 is the known `test:reliability` benchmark flake, retried to green
    (job-name quarantine signature);
  - 2 are stale escalations sitting on fully-green pipelines with zero failed
    attempts (the escalated-run/green-MR orphan class — exactly the F3
    "conflicting terminal outcomes" plan).

Conclusion: rule-based classification clears the ≥80% bar on the
misclassified subset, AND the sample empirically validates fusing F2+F3 —
three rule families fall out: (1) job `failure_reason` mapping, (2)
retried-to-green flake detection + quarantine signatures, (3) terminal-
outcome reconciliation against live MR/pipeline state.

## Decision (2026-08-06)

Operator chose: **Trustworthy Verdicts (F2+F3)**, execution mode **human-led
core + parallel Mills lane** (verdict pipeline core in Claude sessions —
bootstrapping argument: it changes how escalations themselves are judged;
Mills absorbs the mechanical HUD-debt plans). Kill-test passed same day.

## Handoff

- Next step: `plan-loom-core` — Phase 0 re-grounds the four queued plans
  against main (plan-store drift hazard), then slices around the three rule
  families the kill-test surfaced.
- Linked spec/plan doc: `.loom/135-spec-trustworthy-verdicts-2026-08-06.md`
  (Phase 0 grounding complete same day: 3 of 6 queued plans already shipped on
  main; sprint core narrowed to the persisted, supersedable verdict).
