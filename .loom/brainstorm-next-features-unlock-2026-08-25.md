# Brainstorm: next features to unlock our potential

**Date**: 2026-08-25
**Triggered by**: Operator's open-ended `/brainstorm the next features to unlock our potential`. Third epic-selection brainstorm in the series (2026-08-12 → taste-gated demand, `.loom/brainstorm-next-feature-push-2026-08-12.md`; 2026-08-13 → factory immune system, `.loom/brainstorm-prevention-immune-system-2026-08-13.md`). Asked while Cloth Hall (grading context, `.loom/99-product-spec-cloth-hall-2026-08-22.md`) is mid-flight — S1 bolt-card merged, S2 shift-ledger queued on dependency hold — and the shepherd's 168h dry-run soak concludes ~2026-08-27, which opens the next epic slot.
**Constraints noted**: none stated by the operator. Inferred from live state:

- Single-operator attention is the scarce resource; every framing is ultimately judged in minutes-of-operator per unit of shipped value.
- Don't duplicate in-flight lanes: Cloth Hall covers grading context/UX/calibration; the shepherd program is merged and soaking; immune S4/S5 drafts (!1632/!1631) sit in widen-vs-close limbo awaiting an operator decision.
- Taste S5/S6 remain deliberately HELD: the grade-coverage gate wants *habitual* 48h grading over a 14d soak; the one-time batch catch-up (85.6% coverage) does not satisfy it. Cloth Hall's product bet is making the habit real.
- The plan-slice emitter still has no dependency awareness — any sequenced epic stays `draft` with manual staged enqueue.
- MCP 2026-07-28 is a breaking, stateless spec revision; 12-month deprecation clocks started 2026-07-28. Loom leaves pin 2024-11-05. Roadmap exists: `.loom/195-research-mcp-2026-07-28-gap-roadmap-2026-08-15.md`.
- Mills cannot yet spawn against `libs/` repos (SPAWN_PROJECTS gap + SPAWN_GIT_BASE_URL pins `/services`); enablement item `bl-mills-enable-libs-mcp-go-20260815` filed, unlanded.

## Phase 1 — Framings

### F1 — Let taste steer (finish demand autonomy: S5/S6 + ranker enable)

The grading *loop* is being finished by Cloth Hall, but taste still changes nothing: the deterministic ranker shipped long ago behind `RankerEnabled` default-off, S5 (ranked-dispatch extend) and S6 (outcome-grade writeback, whose structurally-NULL-grade defect Cloth Hall S4 fixes) are held, and dispatch order is still taste-blind. This framing says the next epic is simply the *promised second half of the last one*: once Cloth Hall lands and the coverage gate reads true organically, flip the ranker on, extend dispatch with taste features, and let the factory start picking its own winners.

- **Bet**: stored grades convert into materially different dispatch choices — the factory stops burning runs on regret-shaped work without operator grooming.
- **Risk**: the existing grade corpus is anti-churn-shaped (best-instance keep, duplicate re-proposals regret) — a ranker trained on it mostly re-learns dedup the groomer already does, and the behavior delta is marginal.

### F2 — Founder loop: from backlog-worker to product-proposer

Demand today comes from council briefs over signals plus the operator's ~1 brainstorm/week. The factory never proposes multi-slice product bets itself — epics are hand-made artifacts (this document included). Build the founder loop: periodic opportunity scans over usage evidence, dead surfaces, the portfolio tier table, and dark endpoints; drafted product specs that carry the operator's own methodology (riskiest assumption + kill-test, per the `spec-riskiest-assumption` contract); one-tap adopt/decline. The factory starts filling its own epic slot.

- **Bet**: the binding constraint has moved up a level — epic-ideation bandwidth, not slice throughput — and spec-drafting is mechanizable because the methodology is already written down.
- **Risk**: plausible-but-hollow epics are the fabricated-slice failure mode one level up, and no taste substrate exists yet for grading *specs* (bolt grades only).

### F3 — Pay the MCP debt (execute .loom/195 Phases 0–1)

Loom's product surface *is* MCP, and the spec left us behind: 2026-07-28 is stateless and breaking, leaves hardcode 2024-11-05, structuredContent/outputSchema (which retires the TOON array-column data-loss class that has bitten verdicts repeatedly) is absent, and deprecation clocks run. The saving grace is architectural: `pkg/mcpscaffold/scaffold.go` is a single choke point shared by ~30 servers, so SDK fixes land fleet-wide. Execute the written roadmap: Phase 0 Mills-enablement of libs/mcp-go, then leaf version negotiation + structuredContent.

- **Bet**: interop with client SDKs' upgrade cycles is existential, and the choke-point architecture makes the migration far cheaper than a 30-server rewrite looks.
- **Risk**: weeks of protocol work with zero user-visible capability; until Phase 0 lands, Mills can't carry it, so it burns the one resource that doesn't scale — operator attention.

### F4 — Point the factory at the fleet (multi-repo Mills)

The cross-repo keystone is proven (group token, plan→repo bootstrap), yet Mills works exactly one repo while 13 tier-1 repos accumulate backlogs handled by ad-hoc operator sessions. Generalize the spawn substrate (SPAWN_PROJECTS, git base URL, spawn git identity, per-repo devbox/CI adapters) and run one satellite repo end-to-end as the pilot. The portfolio table stops being a filing system and becomes addressable inventory.

- **Bet**: the factory's marginal cost per additional repo is now low; the untapped asset is the other 90% of the portfolio.
- **Risk**: the immune-system wedge catalog (base images, devbox quirks, CI variance) gets re-litigated per repo — support surface multiplies against a single operator.

### F5 — Operator cockpit: decisions, not dashboards

Strip the operator's remaining jobs to their essence and they are *dispositions*: grade this bolt, flip this soak-gated allow flag, widen-or-close this scope draft, adopt this proposal, accept this promotion report. Each currently lives in a different surface (REST calls, gitops MRs, HUD panels, sqlite cat-outs). Build the unified decision queue — HUD + iOS one-tap with context cards, generalizing Cloth Hall S3's grade-in-context walkthrough to every decision class — and retire dark endpoints into it.

- **Bet**: minutes-per-decision is the true bottleneck; batching decisions with rich context makes each one several times cheaper and un-wedges every soak/allow gate faster.
- **Risk**: another derived surface to keep honest (the shift-ledger double-derivation drift is the cautionary tale); a summarizing card can hide exactly the anomaly that mattered.

### F6 — Externalize one slice as a forcing function

Package one genuinely novel slice — the self-healing merge queue + shepherd, devbox, or mcp-go + scaffold — as installable OSS with docs and a demo behind the flexinfer.ai front door. "Potential" in this framing means impact beyond one homelab, and external users force the generality, docs, and hardening that internal use never will.

- **Bet**: the factory story (serial merge queue, taste gating, immune system) is novel enough to earn an audience, and that audience compounds back into the platform.
- **Risk**: audience-of-zero — packaging and support burn operator attention with no adoption; interfaces freeze exactly when F3 needs to break them.

### F7 — Graduate the shepherd: hands-off first-line ops

The shepherd is live in dry-run with the 168h promotion report due ~08-27; the allow flags (`allow.relaunch`, `allow.scope_widen`) are the first rung. Climb the rest of the ladder: auto-disposition for the remaining escalation classes (supersession detection was proved manually derivable on Day-6; spec-capability mismatches are typed since M2), deferred M6 playbooks as executable runbooks, and a morning report of what the night shift fixed. The operator stops being the factory's pager.

- **Bet**: the wedge catalog plus B2 fingerprint/cohort machinery is now mature enough to delegate first-line ops safely.
- **Risk**: the !1671 incident proved novel failure shapes still appear; an auto-remediator acting on a mis-classified signal does real damage, and promoting before the soak data reads is faith, not evidence.

### F8 — Inoculation economy: vaccines as compounding knowledge

The immune epic's most differentiating mechanisms — the M1 inoculation loop and capability-typed admission, the pieces the operator called "a true differentiator" — are stalled in scope-escalated drafts (!1632/!1631). Land them and make the loop measurable: every escalation review mints a vaccine (lint rule, admission check, prompt fragment, engram) under proof contracts, and B2's failure fingerprints give the metric for free (repeat-fingerprint rate should trend to zero).

- **Bet**: the compounding asset is prevented-failure-classes, and for the first time it's directly measurable.
- **Risk**: vaccine bureaucracy — each mint costs review attention, and stale vaccines become permanent friction the way noctx lint did.

## Phase 2 — Cross-Pollinations & Tensions

### Combinations

- **C1 = F2 + F1 — taste-steered demand generation.** The founder loop is only safe if epic proposals flow through the same taste system that grades bolts: specs get graded, adoption/regret feeds calibration, and ranked dispatch extends one level up from slices to epics. Neither alone gets there — F1 merely re-orders an existing backlog, and F2 without grading reproduces the fabricated-work failure mode at epic scale. Together they are the actual "demand autonomy" promised in the 08-12 brainstorm.
- **C2 = F3 + F4 — the MCP debt as the first fleet campaign.** Phase 0 of `.loom/195` is *literally* "enable libs/mcp-go for Mills" — the same enablement F4 needs for any `libs/` repo. Run the MCP migration as the multi-repo pilot: one satellite repo (libs/mcp-go, a low-CI-variance Go library — the gentlest possible second repo), one mandatory campaign, choke-point leverage across ~30 servers. Mandatory debt gets paid *and* the fleet door gets opened, each de-risking the other.

### Tensions

- **T1 = F5/F7 vs F1/F2 — where does saved attention come from?** Cockpit and night-shift *recover* operator attention by making decisions cheaper; taste-steering and the founder loop *reduce demand* for operator attention by needing fewer picks. The axis: is the operator the bottleneck as decision-maker or as ideator? The evidence cuts both ways — escalated-shelf starvations recurred because triage attention lapsed (favors F5/F7), yet the batch-grading catch-up showed decisions do get made when surfaced well, while epic cadence has been stuck at operator-speed all along (favors F2). Whichever way this resolves determines the *next-next* epic.

## Phase 3 — Convergence

### Recommended: C2 — MCP modernization as the first fleet campaign

C2 wins on four grounds. It is the only *mandatory* item on the board — the 12-month deprecation clocks started 2026-07-28 and client SDKs will stop handshaking with a pinned 2024-11-05 fleet within their own upgrade cycles; everything else here is optional improvement. It is a double unlock — Phase 0 is simultaneously the enablement F4 needs, so one investment both pays debt and opens the fleet, with libs/mcp-go as the gentlest possible second repo rather than N heterogeneous ones. Its leverage is structural — structuredContent through the scaffold choke point retires the TOON data-loss class that has repeatedly produced zeroed/misparsed verdicts, a reliability payoff Mills itself feels. And the timing is right — Cloth Hall and the shepherd soak occupy the current lanes, the epic slot opens ~08-27, and this is the one candidate with a written, phased roadmap ready to slice into the plan store (staged enqueue, per the emitter no-dep rule).

### Runner-up: C1 — taste-steered demand generation (the founder loop)

C1 is the more visionary unlock and the truer continuation of the autonomy arc — but it has an evidence gate C2 doesn't: it needs Cloth Hall's bet to pay off first. If the post-Cloth-Hall soak shows habitual 48h grading actually happening organically (the ≥60% gate met without batch catch-ups, calibration meaningful after S4's fix), then the taste substrate exists, S5/S6 unlock, and mechanized epic proposal becomes the highest-leverage move — at which point C2 can wait a cycle, since its clocks are 12-month, not 12-week. If Cloth Hall's habit bet *fails*, C1 is dead on arrival regardless, which is exactly why it shouldn't be the primary pick today.

### Open question

Where does the C2 campaign **stop** and declare the fleet migrated — Phase 1 (leaves negotiate 2026-07-28 + structuredContent through the scaffold, hub fabric untouched), or Phase 3 (stateless Streamable HTTP as hub fabric, official-Go-SDK-at-scaffold decision)? The slicing, budget, and how much of the campaign Mills can self-serve all hang on that stop-line, and it's an ambition call only the operator can make.

## Riskiest assumption + kill-test

> Every brainstorm-derived plan must surface its riskiest load-bearing
> assumption explicitly. See the `spec-riskiest-assumption` skill.

**Load-bearing assumption**: Mills can work `libs/mcp-go` end-to-end — spawn clone, devbox test execution, MR creation on the libs project, and merge-queue handling — once `bl-mills-enable-libs-mcp-go-20260815` lands. Known wedge candidates it must clear: SPAWN_PROJECTS membership, SPAWN_GIT_BASE_URL's `/services` pin, spawn-pod git identity, group-token scope covering the `libs/` group (empty allow-lists fail closed *silently*), and devbox base-image fit for the mcp-go toolchain.

**Kill test**: land the enablement item, then run one deliberately trivial slice against libs/mcp-go (doc comment + existing-test touch, lint-clean) through the full pipeline via `POST /api/mills/pipeline/runs/{backlog_id}/start`. Observable outcome within one run: either a green MR on the libs project reaches the merge stage, or the run wedges at a specific stage whose failure names the missing enablement. Operator time ≤30 minutes; the pipeline does the waiting. Pair with the disconfirming check *before* the run: inspect SPAWN_PROJECTS and the cross-repo token's group allow-list for `libs/` (the plan→repo bootstrap needs all THREE keys present — silence is failure, not success).

**Failure mode if wrong**: the campaign degrades into weeks of operator-driven SDK surgery while the factory idles on loom-core-only demand — the exact inverse of "unlock our potential," and it would falsify the fleet framing (F4) at the same time.

**Status**: not run

> The downstream slice plan is BLOCKED until this kill-test passes.
> Pair it with at least one disconfirming-search query (look for
> "feature NOT supported in product" / "product limitations feature")
> before declaring the assumption verified.

## Handoff

- If chosen → next step is: `plan-loom-core` — slice `.loom/195` Phases 0–1 into a plan-store plan (draft + manual staged enqueue), with the kill-test as the S0 gate; link this doc and `.loom/195-research-mcp-2026-07-28-gap-roadmap-2026-08-15.md`.
- Linked spec/plan doc (fill in once it exists): `<.loom/NNN-product-spec-mcp-fleet-campaign-...md>`
