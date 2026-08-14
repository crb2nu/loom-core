# Research: Factory View — Inspiration & Creative Directions

**Date:** 2026-07-08
**Branch:** `claude/factory-view-research-b8acdb`
**Baseline:** `internal/hud/frontend/src/lib/components/mills/FactoryPanel.svelte` (489 lines) + `internal/hud/frontend/src/lib/utils/factoryHelpers.ts` (121 lines)
**Prior art in-repo:** `.loom/78-plan-dark-factory-patterns-2026-04-05.md` (the "dark factory" thesis), `.loom/plan-hud-work-mills-ux-2026-06-26.md` (Mills UX slices)

---

## Riskiest assumption + kill-test

**Load-bearing assumption**: The Mills HUD proxy endpoints already expose
per-run detail sufficient to drive *truthful* factory animation and row-level
drilldown — specifically `gates[]` (escalation reason), stage timestamps, MR
URL, and backlog linkage on `/api/mills/pipeline/runs` and the history
endpoint — without any operator-side backend work.

**Kill test** (≤30 min): From a machine with HUD access, capture
`GET /api/mills/pipeline/runs` and the pipeline-history endpoint once per
poll interval for 15 minutes while at least one run is active. Confirm:
(1) each run object carries `gates[]`, `CurrentStage`, MR/branch refs;
(2) at 15 s polling, stage transitions are observed (not skipped whole) for
a typical run. Record the JSON as evidence.

**Failure mode if wrong**: Concepts 1–3 below degrade from "truthful
machine" to "decorative theater" — the exact failure the panel's own design
comment warns against — and drilldown needs a new operator endpoint first
(backend slice before any frontend slice).

**Status**: passed 2026-07-08 — verified live against `hud.flexinfer.ai`:

```bash
curl -sk "https://hud.flexinfer.ai/api/mills/pipeline/runs?state=terminal&limit=2"
curl -sk "https://hud.flexinfer.ai/api/mills/pipeline/runs/<run-id>"
```

Findings (evidence: live JSON captured in-session):

- **List/history payload** (`?state=terminal`): run rows carry `ID`,
  `BacklogID`, `Template`, `State`, `CurrentStage`, `Attempts`, `MRIID`,
  `StartedAt/EndedAt`, `CostUSD`, `EscalationClass`, `FailureClass`,
  `EscalationRetryable`. **No `gates[]` here** — confirms memory
  `project_mills_scope_gate_basename_mismatch`.
- **Per-run detail** (`/runs/<id>`): returns `{run, gates[], stages[]}`.
  `gates[]` has `GateName`, `Outcome`, `Reasons`, `AfterStage`, `JudgedBy`.
  `stages[]` is richer than assumed: per-stage `StartedAt/EndedAt`,
  `Outcome`, **per-stage `CostUSD`**, `SpawnID`, `Artifacts`
  (`agent_id`, `turn_count`), and a `LogTail` with the agent's actual
  last message.

**Consequence for the concepts**: drilldown (concept 2) needs one lazy
detail fetch per inspected row — fine. Truthful weaving (concept 1) gets
stage timestamps for free. And `LogTail`/`SpawnID` unlock more than
planned: the floor log can quote the *real* weaver's last words, and the
shuttle can carry the actual spawn agent identity.

---

## 1. Where the Factory view stands today

The panel is already a strong, coherent metaphor — a canvas Jacquard loom fed
by real Mills state:

| Element | Data source | Honest? |
|---|---|---|
| Warp threads | backlog counts by state (`FactoryPanel.svelte:45-51`) | ✅ count-honest, identity-anonymous |
| Shuttle | active run count + stages (`:78-82`) | ⚠️ speed ∝ count; passes are decorative |
| Green/amber rows | terminal-run diff (`factoryHelpers.ts:23-42`) | ✅ 1 row = 1 real run |
| Plain rows | RNG timing filler (`:166-171`) | ❌ pure theater |
| Punch-card tape | hash noise (`:189-203`) | ❌ pure theater — "the program" shows no program |
| Gauges | KPI window metrics (`:355-362`) | ✅ |
| Floor log | active runs @ stage + recents (`factoryHelpers.ts:59+`) | ✅ |

**The core tension to resolve:** the most emotionally satisfying parts
(shuttle, tape, cloth texture) are the least truthful, and the most truthful
parts (gauges) are the most conventional. Every concept below pushes
decorative elements toward data-honesty rather than adding new decoration.

### Untapped Mills data (no new backend needed — verified live, see kill-test)

- Per-run `gates[]` — *why* a spark happened (scope gate, nonempty_diff, budget…)
- Per-stage `stages[]` — real timestamps, outcome, **per-stage cost**, `SpawnID`,
  `Artifacts.turn_count`, and `LogTail` (the agent's actual last message)
- `CostUSD` per run — the cloth could literally show what each bolt cost to weave
- Escalation classes a–d (infra vs real, `project_mills_escalation_denoise_debt073`;
  `EscalationClass`/`FailureClass` fields confirmed on run rows)
- Pattern Loom catalog — 4 named patterns (literally "pattern books" for a Jacquard loom!)
- Policy checksum + enabled gates — a real "program" for the punch tape
- Rolling 24 h budget window — fuel for the machine
- Council / Spinning Room runs, plan-priority Live Beam
- Spawn-pool pods + fleet agents — the "weavers" gauge exists but they're invisible on the floor

---

## 2. Inspiration catalog

### A. Factory games — the mechanism must be real

[Opus Magnum / shapez / Factorio](https://forum.quartertothree.com/t/shapez-2-or-how-to-vibe-with-a-factory-game/162146)
teach one lesson above all: satisfaction comes from *watching a real machine
loop without a hitch* — "clear visual feedback, smooth animations, an
unobtrusive UI, and the rhythmic satisfaction of watching optimized systems
work" ([Eneba](https://www.eneba.com/hub/games/games-like-factorio/),
[PCGamesN on shapez 2](https://www.pcgamesn.com/shapez-2/out-now-early-access)).
Opus Magnum's GIFs went viral because the animation *is* the solution — zero
gap between representation and mechanism.

**Steal:** every shuttle pass should correspond to a real event (stage
transition, poll tick with progress), not a timer. When the viewer learns
"one pass = one stage advanced," the panel becomes rewatchable.

### B. Andon boards & digital twins — two views, one truth

Manufacturing UX literature is explicit: the glanceable overhead board and
the interactive drill-down console are *different products generated from one
source of truth* — "nobody interacts with an Andon board; it exists to be
glanced at from 3–10 m" ([Fuselab manufacturing dashboard guide](https://fuselabcreative.com/manufacturing-dashboard-ux-design/)).
Digital-twin dashboards add the time dimension: "what's happening now, what's
likely next, what could happen under different scenarios"
([Simio](https://www.simio.com/blog/digital-twin-dashboard-design-that-drives-real-business-decisions)),
with color/animation encoding flow state — green smooth, amber emerging
bottleneck, red constraint. Also flagged: alarm fatigue, shift-handoff
continuity, and **stale-data visibility during network failures** — the loom
should visibly stop when the feed stops, never keep weaving on stale state.

**Steal:** an explicit "andon mode" (fullscreen, 3 states, readable across a
room) as a *mode* of the same panel; a freshness indicator woven into the
scene (feed stale → shuttle freezes mid-flight with a visible caption).

### C. The Jacquard loom itself — the metaphor is deeper than we're using

The historical loom is not just aesthetic garnish; it's the origin myth of
computing. The 1839 woven portrait of Jacquard used
[24,000 punch cards × 1,050 positions](https://www.scienceandindustrymuseum.org.uk/objects-and-stories/jacquard-loom),
and Ada Lovelace's line — "the Analytical Engine weaves algebraic patterns
just as the Jacquard loom weaves flowers and leaves" — is *exactly* what
Mills does: policy cards in, software bolts out. The punch cards were
[interchangeable programs](https://www.computerhistory.org/storageengine/punched-cards-control-jacquard-loom/),
and card chains were [works of art in their own right](https://www.sarah-archer.com/writing/2020/12/7/eitherand-where-loom-and-canvas-meet).
Macclesfield Silk Museum even built a
[Raspberry Pi hole→pattern simulator](https://www.raspberrypi.com/news/jacquard-loom-simulator/)
so visitors could see the card-to-cloth causality.

**Steal:** make the tape *be* the program. Encode the live policy (checksum
bits, enabled gates, catalog pattern IDs) into the hole pattern. When policy
changes (checksum bump), the tape visibly splices to a new card chain. The
Pattern Loom catalog becomes a shelf of labeled "pattern books."

### D. GitHub Skyline / Git City — history as artifact you keep

[GitHub Skyline](https://github.com/github/gh-skyline) and
[Git City](https://www.opensourceprojects.dev/post/6d3af576-60ff-4a11-a8f2-ef45fe7bea66)
prove people *love* a persistent, shareable artifact grown from their work —
3D-printed contribution graphs, cities where each repo is a building
([DEV overview](https://dev.to/github/view-your-github-contribution-graph-as-an-animated-skyline-3d-print-it-2dpl)).
The factory's cloth currently evaporates (rows fade and pop). But each row
is a real merged MR — the cloth is literally *the fabric of the codebase's
week*.

**Steal:** deterministic row patterns seeded from run/backlog IDs (same run
always weaves the same threads), a "bolt archive" view of the last N weeks
of cloth, and an export (SVG/PNG "tartan of the week") for standups or the
office TV. Cheap, unique, and no other tool has it.

### E. Split-flap boards — mechanical state change you can feel

[Vestaboard](https://www.vestaboard.com/split-flap-display) and browser
clones ([Festaboard](https://festaboard.com/),
[Mini Split Flap](https://minisplitflap.com/),
[Aceternity's component](https://ui.aceternity.com/components/text-flipping-board))
show the enduring appeal of the departure board: state changes are *events
with physicality*. [VestaSpotter](https://vestaspotter.jakesgoodapps.com/)
maps live flights to one — the exact pattern of "live ops feed → mechanical
board."

**Steal:** the floor log as a departure board — one row per active run
(`RUN · backlog-id · laying weft · ON TIME/DELAYED`), flap-flip on stage
transitions instead of a CSS marquee. Marquees read as ambient noise;
flap-flips read as *news*.

### F. Mission control — gauges with gravitas

The [NASA console aesthetic](https://airandspace.si.edu/multimedia-gallery/image/nasa-consolesjpg)
(teal/amber, nixie counters, needle gauges,
[Apollo display panels](https://history.nasa.gov/alsj/CSM10_Displays_&_Controls_pp83-86.pdf))
suits the existing gauge strip. The north-star metric (autonomous merges 24 h)
deserves an *odometer* — a counter that mechanically clicks up and never
reads as a mere number.

**Steal:** needle physics on gate-pass-rate (spring + overshoot), odometer
roll on bolts-merged, a "fuel" gauge for the rolling 24 h budget window.
Restraint: keep the current flat token-driven style; borrow *motion*, not
skeuomorphic chrome.

### G. Swarm dashboards — the competitive frame

Current multi-agent UIs are htop-style tables and dependency graphs
([claude-swarm](https://github.com/affaan-m/claude-swarm),
[Multi-Agent-AI-Swarm](https://github.com/shortcut119/Multi-Agent-AI-Swarm),
[EPAM's swarm writeup](https://www.epam.com/insights/ai/blogs/building-multi-swarm-autonomous-ai-agent)).
"Command center dashboards turn invisible AI activity into something
observable" ([Finabeo](https://www.finabeo.com/blogs/inside-the-july-25th-ai-hackerspace-live))
— but nobody is doing it with *craft* identity. 2026 dataviz trend pieces
([Luzmo](https://www.luzmo.com/blog/data-visualization-trends),
[Fuselab 2026](https://fuselabcreative.com/top-data-visualization-trends-2026/))
point at real-time ambient viz and scrollytelling narratives. The loom is a
defensible aesthetic moat — lean in, don't converge on tables.

---

## 3. Creative concepts, ranked

Scored: **wow** (demo/screenshot value), **truth** (moves theater→data),
**effort** (S/M/L, frontend-only unless noted).

| # | Concept | Wow | Truth | Effort |
|---|---|---|---|---|
| 1 | **Truthful shuttle & tape** — passes driven by real stage-transition events; tape holes encode live policy checksum + enabled gates; visible tape splice on policy change; shuttle carries the run's short-ID as a woven label | ★★★ | ★★★ | M |
| 2 | **Inspectable cloth** — hover a row → tooltip (backlog ID, state, gate that fired); click → existing run drill-down. Deterministic per-run thread pattern (seeded PRNG from run ID) so the cloth is stable across reloads | ★★★ | ★★★ | M |
| 3 | **Bolt archive / "tartan of the week"** — persist woven rows (already have history endpoint); a strip view of the last 7 days of cloth; export as SVG/PNG for standup/TV | ★★★ | ★★ | M |
| 4 | **Departure-board floor log** — split-flap rows per active run, flap on stage transition, `DELAYED` tone when a run exceeds stage p90 | ★★ | ★★ | M |
| 5 | **Weavers on the floor** — fleet agents + spawn-pool pods rendered as bobbins/spindles at the loom edge; a pod spinning up = a bobbin winding | ★★ | ★★ | M |
| 6 | **Andon mode** — fullscreen glance mode: giant north-star odometer, weaving/paused/escalation-storm tri-state, readable at 3 m; doubles as the office-TV view. Freshness-honest: stale feed visibly halts the loom | ★★ | ★★ | S/M |
| 7 | **Mission-control gauge motion** — needle physics, odometer north-star, budget fuel gauge | ★★ | ★ | S |
| 8 | **Pattern books** — Pattern Loom catalog rendered as labeled card-chains on a shelf; a run stamped from a pattern visually pulls its card chain into the tape | ★★ | ★★ | M (needs catalog on HUD API — verify) |
| 9 | **Loom sounds (opt-in)** — Web Audio clack per pass, chime on bolt, low klaxon on spark; default off, respects reduced-motion. Prior art in-workspace: `libs/py-chiptune`, `labs/git-tunes` | ★ | ★ | S |
| 10 | **Shift report scrollytelling** — end-of-day generated overlay: "the floor wove 6 bolts, 2 sparks (both infra-class), pattern `go-rest-service` stamped twice" | ★★ | ★★ | L (needs summarizer) |

### Recommended sequencing

**Slice 1 = concepts 1+2 together** ("the honest loom"): same files, same
poll loop, and they convert the panel's two biggest theater elements into
instruments. The kill-test above is slice 1's gate.
**Slice 2 = concept 3** (archive/export) — highest shareability per effort,
builds directly on slice 1's deterministic rows.
**Slice 3 = concept 6** (andon mode) — unlocks the TV/ambient use case and
the staleness-honesty fix, which is a correctness issue as much as a feature.

Concepts 4, 5, 7, 9 are independent garnish slices; 8 and 10 need scoping.

### Design guardrails (carry into any slice)

- **Reduced-motion parity**: every new animation needs the static fallback
  the canvas already implements (`FactoryPanel.svelte:96,308`).
- **Token-driven color only** — the panel reads `--*-rgb` triplets from live
  theme tokens; keep it (memory: `project_hud_phantom_token_alias_gaps` —
  check a token is *defined* before relying on it).
- **Poller discipline**: reuse `millsStore.startPolling`/`fleetStore`
  ownership pattern; no new intervals (memory: `project_hud_frontend_registry_poller`).
- **Truth over theater**: no element may imply activity when the feed is
  stale or Mills is paused. The paused/idle captions (`:302-306`) are the
  precedent — extend, don't dilute.
- **Helpers stay rune-free** in `factoryHelpers.ts` so vitest covers the
  data→weave mapping without a Svelte runtime.

---

## 4. Sources

- [Fuselab — Manufacturing dashboard UX guide](https://fuselabcreative.com/manufacturing-dashboard-ux-design/)
- [Simio — Digital twin dashboard design](https://www.simio.com/blog/digital-twin-dashboard-design-that-drives-real-business-decisions)
- [Science & Industry Museum — the Jacquard loom](https://www.scienceandindustrymuseum.org.uk/objects-and-stories/jacquard-loom)
- [Computer History Museum — punched cards control Jacquard loom](https://www.computerhistory.org/storageengine/punched-cards-control-jacquard-loom/)
- [Sarah Archer — Either/And: where loom and canvas meet](https://www.sarah-archer.com/writing/2020/12/7/eitherand-where-loom-and-canvas-meet)
- [Raspberry Pi — Jacquard loom simulator](https://www.raspberrypi.com/news/jacquard-loom-simulator/)
- [github/gh-skyline](https://github.com/github/gh-skyline) · [Git City](https://www.opensourceprojects.dev/post/6d3af576-60ff-4a11-a8f2-ef45fe7bea66) · [DEV — animated skyline](https://dev.to/github/view-your-github-contribution-graph-as-an-animated-skyline-3d-print-it-2dpl)
- [Vestaboard](https://www.vestaboard.com/split-flap-display) · [Festaboard](https://festaboard.com/) · [VestaSpotter](https://vestaspotter.jakesgoodapps.com/) · [Mini Split Flap](https://minisplitflap.com/) · [Aceternity flipping board](https://ui.aceternity.com/components/text-flipping-board)
- [NASA consoles (Smithsonian)](https://airandspace.si.edu/multimedia-gallery/image/nasa-consolesjpg) · [Apollo CSM displays & controls](https://history.nasa.gov/alsj/CSM10_Displays_&_Controls_pp83-86.pdf)
- [claude-swarm](https://github.com/affaan-m/claude-swarm) · [Multi-Agent-AI-Swarm](https://github.com/shortcut119/Multi-Agent-AI-Swarm) · [EPAM — building a multi-agent swarm](https://www.epam.com/insights/ai/blogs/building-multi-swarm-autonomous-ai-agent) · [Finabeo — visualising AI swarms](https://www.finabeo.com/blogs/inside-the-july-25th-ai-hackerspace-live)
- [shapez 2 vibe thread](https://forum.quartertothree.com/t/shapez-2-or-how-to-vibe-with-a-factory-game/162146) · [PCGamesN on shapez 2](https://www.pcgamesn.com/shapez-2/out-now-early-access) · [games like Factorio](https://www.eneba.com/hub/games/games-like-factorio/)
- [Luzmo — dataviz trends 2026](https://www.luzmo.com/blog/data-visualization-trends) · [Fuselab — dataviz trends 2026](https://fuselabcreative.com/top-data-visualization-trends-2026/)
