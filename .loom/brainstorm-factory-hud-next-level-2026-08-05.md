# Factory HUD — next level (2026-08-05)

**Question**: after the hierarchy pass (instrument rail, board clock, andon
polish — MR !1449), what takes the Mills factory HUD experience to the next
level?

**Grounding**: live review 2026-08-05 against the cluster HUD (real cloth,
real KPIs, real departures); the wave 1–4 self-improving-factory substrate
(judge.verdict / run.provenance / regression.attributed events, five report
endpoints, learning-signal gauges, council grounding + exhaust sourcing);
standing constraints — the honesty rule (never render fiction; the loom
halts on a stale feed), single 15 s poll substrate, panel-registry + poller
conventions, code names stay / theme lives in labels.

## Phase 1 — Diverge

### F1 · The Inspector's Bench (drill-down depth)
The cloth is clickable but inspection is shallow. Make every pixel
answerable: click a woven row → the run's stage timeline with judge scores
(judge.verdict), per-stage cost, the provenance stamp (policy checksum,
stage models), and any regression back-link. The factory stops being a
picture of work and becomes the forensic surface over the ground truth
shipped this week.
**Bet**: the new event substrate is the differentiator no dashboard has;
click-depth converts observability into trust.
**Risk**: drawer complexity creep; duplicates Mill Staff / Governance
surfaces if routing is sloppy.

### F2 · The Time Machine (shift scrubbing)
The loom shows only now + recent diffs; the bolt archive is a static
export. Add a shift scrubber: drag across 24 h / 7 d and the cloth re-weaves
from history (the deterministic seeded weave already guarantees the same
history renders the same cloth). Andon gains a "last shift" ghost.
**Bet**: temporal navigation turns charm into an analytical tool —
postmortems happen ON the floor.
**Risk**: stage-level history is partial; a scrubber that papers over gaps
violates the honesty rule; the build cost is real.

### F3 · The Steam Whistle (push, not pull)
The factory is a pull surface. Invert it: lamp transitions (weaving→storm),
fuel-low, and judge-drift alerts push — browser/PWA notification, iOS
attention lane (the merge lane contract already exists), lock-screen live
activity. The TV board becomes the fallback, not the primary.
**Bet**: for a lights-out factory the operator's real UI is their pocket;
attention routing beats pixels.
**Risk**: recreates escalation noise in a new channel; LoomCompanion is a
separate codebase; must unify with the existing mobile attention contract
or it forks.

### F4 · The Order Book (demand-side visibility)
Everything on the floor shows execution. The demand side — intents arriving
from ROADMAP.md, proposals born, proposals SUPPRESSED by merged-work
grounding (!1442), exhaust items surfaced (!1445) — is completely dark.
Give the floor a receiving dock: what the factory decided to make, and what
it declined to make, with reasons.
**Bet**: the council's judgment is now the most interesting part of the
system; showing suppressions is how autonomy earns trust.
**Risk**: plan-time data is slow and textual — may read as a boring list
next to the loom; overlaps the Drawing Office tab's mandate.

### F5 · Gauge Calibration (pro density mode)
The design optimizes for narrative charm; a floor manager wants data ink:
sparklines on every instrument, deltas vs the prior shift, p50/p90 stage
durations on the board, threshold markers on fuel and yield. One toggle,
same layout, denser ink.
**Bet**: the users are experts; Tufte beats theming for daily driving.
**Risk**: two modes = two surfaces to keep honest; density can kill the
glanceability that makes the page distinctive.

### F6 · The Foreman's Log (grounded narration)
The shift report is a modal you must open. Instead: every few hours the
system writes two grounded sentences on floor-state change ("two sparks
this hour share one litellm signature — miner filed a candidate; groomer
proposed nothing") as a quiet ticker on the floor and a section of the
shift report. Every sentence must cite the event IDs it narrates — no
citation, no sentence.
**Bet**: narrative compression is what humans actually read; the factory
explaining itself closes the trust loop.
**Risk**: LLM narration fabricates unless the cite-or-silence rule is
enforced mechanically; cadence and cost need a policy.

### F7 · Cloth as Navigation (the loom IS the app)
Sixteen tabs orbit a decorative centerpiece. Invert: the cloth becomes
primary navigation — warp thread → backlog item, bobbin → agent session,
tank → budget policy — and tabs collapse into it.
**Bet**: the metaphor is strong enough to carry the whole surface; one deep
surface beats sixteen shallow ones.
**Risk**: canvas-as-navigation is genuinely hard for discoverability and
a11y; large refactor; hidden affordances are a support burden.

## Phase 2 — Cross-Pollinate

**C1 = F1 + F2 (the postmortem bench)**: scrub to any point in the window,
then click any woven row for its full forensic record. The natural end
state — but the combined cost is a quarter, not a slice. Sequence F1 first;
F2 joins later without rework because inspection is already event-keyed.

**C2 = F6 × F4 (the log narrates the dock)**: the foreman's log is at its
best narrating DEMAND decisions — suppressions, groundings, exhaust intake
— which are exactly the events too dry to read as a list. Citation-locked
narration sidesteps F6's fabrication risk because every sentence must
resolve to a real event. This combo makes the invisible council visible
without building a new panel.

**T1 (the real axis)**: F3 (push) versus everything else (pull). If
attention routing wins, the HUD's remaining job is forensics (F1) and
everything else is garnish. The decision axis is: who is this surface for
next quarter — the operator debugging autonomy, or the audience learning to
trust it?

## Phase 3 — Converge

**Recommended — F1 + F4-lite ("the answerable floor")**: ship the
Inspector's Bench over the new ground-truth events (judge scores, per-stage
cost, provenance, regression back-links in the run drawer), and surface
demand decisions as departure-board row types (a `suppressed` row with its
grounding reason; an `intake` row for exhaust items) rather than a new
panel. Why it wins now: it monetizes this week's data work immediately, it
is incremental (drawer + board rows, no new navigation), it obeys the
honesty rule for free (events only), and it makes the factory the single
trust surface for autonomy right as the 08-08 promotion review asks
exactly those questions.

**Runner-up — C2 (the grounded foreman's log)**: if the page's primary
audience is people rather than the operator — if the factory gets
screenshotted into updates more than it gets debugged from — the narrated
log delivers more per token. Tips the other way the moment a local model
(weaver) can run the citation-locked summarizer for free.

**Open question**: who is the factory page's primary user next quarter —
the operator-of-record doing forensics (→ bench), or stakeholders learning
to trust the autonomy (→ log)? The answer picks between the recommendation
and the runner-up; both are staged so neither forecloses the other.

## Provenance

Live session 2026-08-05: factory + andon polish (MR !1449), cluster-HUD
review at desktop/375px. Related: `.loom/174-research-factory-view-inspiration-2026-07-08.md`
(the original factory round), `docs/FACTORY_MODEL.md`,
`project_self_improving_factory_waves` (memory). Deferred small fixes from
the polish pass, independent of direction: store error-string copy
("failed:status:"), departure history feed-vs-clock ordering, pattern-book
chip affordance.
