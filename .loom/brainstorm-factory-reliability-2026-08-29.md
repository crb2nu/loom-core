# Brainstorm: Factory reliability, self-healing, telemetry & alerting

**Date**: 2026-08-29
**Triggered by**: Operator ask after two days of demand-sourcing revival ops in which every stall was found by a human asking, and every fix was a hand-run recipe.
**Constraints noted**: Build on the existing substrate (overseers, gates, merge queue, KPI writer, :9090 metrics, Prometheus/Grafana/Alertmanager, HUD + mobile attention lanes); GitOps-first; GitLab CE (no merge trains); operator attention is the scarcest resource. **loom-flightdeck** (Tier-1 agent flight recorder: append-only Postgres event store with live Claude/Codex ingest, Stall Board, session→GitLab-MR outcome join, PromEx metrics) is standing fleet infrastructure — prefer extending it over building parallel planes.

## Grounding — the last 48h incident corpus

1. **Branch-contract ref collision ×2** (loom-core issues #621, #623): both demand-sourcing relaunches did full in-scope work on every implement attempt, then failed `branch_pushed.missing_ref` because a June-era item-named ref blocked the nested contract branch. Agents improvised husk branches; ~$7.5 of model spend reached escalation; two manual rescues (delete legacy ref → plant contract branch at best attempt SHA → requeue) both worked first try. The second occurrence happened because the dependency-gate auto-start path shares no preflight with manual start. Class fix in flight (`bl-mills-branch-contract-ref-collision-20260828`, factory draft MR !1759).
2. **Silent red main, ~10h**: upstream advisory GO-2026-6303 (x/crypto v0.54.0) failed `security:govulncheck` on 8 consecutive main pipelines (04:29→09:48Z). No alert existed; the factory kept starting runs onto the red base; every operator image build was blocked, so the just-merged multi-repo importer (!1748, 01:57Z) sat undeployed. The fix (!1762) was a fully mechanical version bump a script could have authored.
3. **Stale-base ripple**: S4's MR !1763 branched from pre-fix main and would have burned CI red on the same advisory; a human rebased it preemptively.
4. **Image-pin lag is invisible**: deployment pinned `20260828-101417` all morning while main advanced; live policy keys (`intake.gitlab.projects`) sat armed-but-inert with no gauge saying so.
5. **Watchers are hand-rolled**: five ad-hoc curl loops this session (two item watchers, a chain watcher, a flap-catcher) because nothing operator-facing implements "watch this subject to terminal and notify" — even though `store.Watch{SubjectKind, SubjectID, TerminalCondition, ExpiresAt}` already exists.
6. **Evidence archaeology**: the run ledger can't serve historical runs (`runs?backlog_id=` ignored; 200-item window); postmortem truth lives in escalation-issue bodies (#218/#219/#621/#623). Related historical family: sliceless-scope hard-fails that orphaned green June work, ghost/husk sparks, MWPS dropping on push, two-pass sweep blind spots, deferral reasons buried in event payloads.

## Phase 1 — Framings

### F1 — Work conservation (never lose finished work)

Treat "the work was good but couldn't land" as a distinct, machine-repairable failure class. Split every post-implement failure into *semantic* (bad diff) vs *mechanical* (ref collision, push identity, MWPS drop, merge-405). Mechanical failures route to deterministic repair verbs — starting with `branch_rescue`, which is exactly the hand-run recipe that worked twice this week — plus an attempt-ref registry on the run so no husk needs archaeology.

- **Bet**: A large share of escalation cost is landing mechanics, not bad work — conserve it and both cost/merge KPIs jump without any smarter agents.
- **Risk**: Automated git-ref mutation; one wrong rescue deletes a ref a human wanted or launders a bad diff past review.

### F2 — Environment as a first-class admission input

The factory currently assumes its base is healthy. Make base health explicit: a `base_red` admission hold for code-class items when main's latest real pipeline is red (remediation-labeled items exempt), an auto-minted remediation item for known-mechanical red causes (the govulncheck advisory template: parse report → bump module → MR through normal CI), and a deployed-vs-merged drift signal so "armed-but-inert" states are visible.

- **Bet**: The environment (base repo, deps, image) breaks more often than factory logic; gating on it prevents cascade waste and the factory can heal the commonest breakage itself.
- **Risk**: False-positive holds starve the factory — skipped/path-gated pipelines already produced one wedge class; a naive "is main green" check fails closed on phantom signals.

### F3 — Every stall chirps (alerting on the factory's own SLOs)

Instrument stalls, not just successes: main-red duration, image-pin lag hours, queue-age per item with its deferral reason as a labeled counter (reasons exist today but only inside event payloads), new-escalation events with failure class and cost, north-star zero-crossings. Route by severity: page-worthy signals through the existing HUD mobile attention lanes / push; everything else renders as a Factory lane beside loom-flightdeck's Stall Board (which already surfaces stalled/blocked agent sessions and exports PromEx metrics), honoring the known PrometheusRule pitfalls (release label, counter-born-nonzero offset).

- **Bet**: Minutes-to-operator-awareness dominates MTTR; today's 10h red-main would have been a 10-minute page.
- **Risk**: Alert fatigue mutes the channel; mislabeled rules go dark while pretending to cover.

### F4 — Drill the healing paths (chaos canaries)

Healing code that isn't exercised rots (two-pass sweep blind spots were found by accident). Extend the daily heartbeat canary into scheduled fault drills in a sandbox project: inject a colliding legacy ref, a husk MR, a red-dep base; require sweeps/shepherd/rescues to clear them unaided; alert when a drill fails. The drill suite is also the regression harness for every new healing verb.

- **Bet**: Continuously-proven healing is the only healing that works during a real incident.
- **Risk**: Drill machinery adds its own surface area and noise; sandbox fidelity drifts from prod and buys false confidence.

### F5 — One admission door

Manual start, dependency auto-start, auto-requeue, shepherd relaunch, and canary autopilot are separate code paths with divergent preflights — the S4 recurrence was exactly a path-divergence bug. Consolidate every "a run may begin" decision into one admission function carrying all preflights (ref-collision probe, base health, scope, deps, budget) and one event vocabulary, so a preflight added once protects every path forever.

- **Bet**: Most "weird" factory failures are path-divergence bugs; collapsing paths deletes the class, not instances.
- **Risk**: Refactor of the reconciler's most load-bearing code; regression risk plus a velocity pause while it lands.

### F6 — The run is a flight recorder

The fleet already *has* a flight recorder: loom-flightdeck ingests Claude/Codex hook events, tool calls, stalls, and tokens into an append-only Postgres store within seconds of emission, and already joins sessions to their GitLab MRs for quality outcome labels. What's missing is the factory half of the join — Mills runs are invisible to it (zero mills/backlog references in its code). Stamp `run_id`/`backlog_id`/`stage` into spawned agents' environment so their hook events carry the keys; teach the Mills API to serve historical runs (`runs?backlog_id=` is ignored today; cursor past the 200-cap); generate escalation issues *from* the joined evidence. Triage, grading, dedup, and any future auto-healer become queries instead of archaeology — and attempt-level behavior ("what did attempt 2 actually do for 51 minutes?") comes free from data flightdeck already stores.

- **Bet**: The recorder exists and ingests reliably; correlation keys turn two half-ledgers into one whole — mostly integration, not construction.
- **Risk**: Cross-service schema coupling (Mills ↔ flightdeck); the existing MR-join only covers runs that reach an MR, so pre-MR failures still need the stamped keys.

### F7 — Interventions-per-week is the north star

Directly measure autonomy: count and classify every manual operator touch (this week: two branch rescues, one dep bump, one MR rebase, five hand-rolled watchers). Each intervention gets attributed to a missing capability; the weekly intervention report *is* the reliability backlog. Ship nothing that doesn't retire a recurring intervention.

- **Bet**: Optimizing the true goal (hands-off weeks) beats optimizing proxies like alert coverage or verb count.
- **Risk**: Goodharting — pressure to automate judgment calls that should stay human, or to stop touching the factory to keep the number pretty.

### F8 — Kill the custom, buy the boring

Replace bespoke subsystems with maintained upstream equivalents where they exist: Renovate already handles advisory-driven dep bumps; Flux notification-controller already emits rollout/image events to Alertmanager; GitLab MR features cover parts of ci-watching. Each replacement permanently deletes a failure surface the factory currently owns.

- **Bet**: The cheapest reliability is code you no longer run.
- **Risk**: CE feature gaps (no merge trains), integration loss (verdicts/taste/keyed spawns don't exist upstream), and migrations that cost more than the surface they delete.

## Phase 2 — Cross-Pollinations & Tensions

### Combinations

- **F2 + F3 + F6 → the Factory Health Plane**: one exporter/poller owning base health, image drift, deferral reasons, and ledger-derived stall ages as a single metrics surface, one alert pack, one HUD lane. Each framing alone builds a partial dashboard; together they make "is the factory healthy and if not why" a single glance — and the admission hold (F2) becomes just another consumer of a signal that's already proven.
- **F1 + F4 → verbs ship with their drills**: `branch_rescue` lands only alongside an injected-collision drill that proves it weekly. Converts F1's scariest property (automated ref mutation) into F4's continuously-verified guarantee, and sets the precedent for every future healing verb.
- **F8 inside F2**: the advisory-remediation half of F2 might be *Renovate config*, not factory code — try the boring tool first, keep the factory template as fallback.
- **Health Plane × flightdeck (delivery detail for F3+F6)**: HP-1's factory events sink into flightdeck's append-only store and render as a Factory lane beside the agent Stall Board, while PromEx exports the same signals for the alert pack. One already-deployed pane, no parallel plane — and the existing session→MR join means run↔session correlation works for MR-bearing runs on day one, before any key-stamping lands.

### Tensions

- **F5/F8 (subtraction) vs F1/F2/F3 (addition)**: the real budget decision. Consolidating admission paths and deleting custom surface compounds forever but pauses velocity; adding health/healing capability pays this week but accretes surface. You cannot lead with both.
- **F7 vs F3**: a page *is* an intervention. If the factory's answer to every anomaly is "alert the human," the intervention metric never improves — F3's severity routing must default to digest, paging only on SLO breach.

## Phase 3 — Convergence

### Recommended: Factory Health Plane (F2+F3+F6), two slices

Slice **HP-1 (observe)**: the exporter + alert pack + push wiring — main-pipeline health poller (skipped-aware), image-pin drift gauge, deferral-reason counters promoted out of event payloads, new-escalation push with class+cost through the existing mobile attention lanes, and `watch→notify` on the existing `store.Watch` type so ad-hoc curl loops die. Slice **HP-2 (act, conservatively)**: the `base_red` admission hold (policy-gated, default-off, soaked like every overseer verb) and the govulncheck advisory auto-bump (Renovate first if it fits, factory template else). This wins because every incident in the corpus was a *visibility* failure before it was anything else; HP-1 is pure observation with near-zero blast radius, and HP-2 adds exactly one hold and one mechanical remediation whose manual versions were both proven this week. Delivery detail from the flightdeck cross-poll: HP-1 sinks into loom-flightdeck (Factory lane on the Stall Board, PromEx → Prometheus) with HUD mobile push reserved for page severity — the plane rides Tier-1 infrastructure that already ingests the whole fleet, and F6 shrinks to a correlation-stamping integration. The plane is also the measurement floor F7 needs before an autonomy north-star means anything.

### Runner-up: verbs-with-drills (F1+F4, `branch_rescue` first)

Tips ahead if the collision class recurs before the !1759 preflight fix lands (a third manual rescue would prove the recipe deserves automation immediately), or once the Health Plane exists to watch overseer verbs — at which point this becomes the obvious second program.

### Open question

**What may page?** Proposed sink split: page severity → HUD mobile push; everything else → the flightdeck Factory lane as digest. The remaining taste call is the page list — is it exactly `main_red > 90m` and `north-star starvation > 36h`, or does anything else (new code-class escalation above $N; image-lag > 12h) earn an interrupt, and with what quiet hours? Secondary: does F5 (one admission door) get its own refactor window, or ride as a standing constraint ("no new start paths; every new preflight lands in the shared admission step")?

## Riskiest assumption + kill-test

**Load-bearing assumption**: Main-red and image-drift are crisply detectable from GitLab CE and Flux APIs — specifically, "latest non-skipped pipeline on `main` of services/loom-core" and "Flux ImagePolicy latestImage vs the deployment's pinned tag" — with effectively zero false positives, despite path-gated jobs and skipped pipelines (the documented MR head-pipeline skipped wedge is the known trap).

**Kill test**: Offline replay — one script pulls the last 30 days of main pipelines and image-bump commits and runs the detector logic over them. It must flag exactly the known epochs (the 2026-08-29 04:29→13:0x red run; the ≥11h pin lag behind !1748) and fire zero alerts anywhere else. ≤30 minutes against the GitLab/Flux APIs.

**Failure mode if wrong**: The alert channel cries wolf and gets muted, and the `base_red` hold freezes a healthy factory on phantom signals — the Health Plane would be net-negative.

**Status**: not run

> The downstream slice plan is BLOCKED until this kill-test passes. Pair with a
> disconfirming search: GitLab CE pipeline-status API behavior for skipped /
> path-filtered pipelines before trusting "latest pipeline" semantics.

## Handoff

- If chosen → next step is: `plan-loom-core` (draft plan in the Plan Store, namespace `mills/factory-reliability`, phase `draft` — deliberately outside the emitter namespace so nothing auto-enqueues before the kill-test runs)
- Linked spec/plan doc (fill in once it exists): `<.loom/NNN-...md>`
