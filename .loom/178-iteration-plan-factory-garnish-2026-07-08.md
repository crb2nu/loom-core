# Iteration Plan: Factory View — Garnish Slices (4/5/7/9)

**Date:** 2026-07-08
**Source research:** `.loom/174-research-factory-view-inspiration-2026-07-08.md` (concepts 4, 5, 7, 9 — the "independent garnish" tier)
**Prior slices:** `.loom/175` honest loom (!1008), `.loom/176` bolt archive (!1013), `.loom/177` andon mode (!1014) — all merged 2026-07-08

## Goal

The four small polish concepts, shipped together as one MR (they all
touch FactoryPanel; parallel worktrees would conflict): the floor log
becomes a split-flap departure board (E), fleet agents appear as
bobbins on a creel (5), the gauges gain mission-control motion (F/7),
and the loom gets opt-in synthesized sounds (9). Every addition obeys
the panel's honesty contract: motion and audio fire only for observed
events, and a stale feed freezes everything.

## Riskiest assumption + kill-test

Internal-only frontend polish over data surfaces already proven live by
slices 1–3 — no new external-behavior claims. Skipped per the rule's
scope note ("internal-only specs usually don't"). One in-code check:
`fleetStore.liveAgents`/`rootAgentId` exist and power the same count the
gauge shows (`fleet.svelte.ts:283`, `utils/agents.ts:143`).

## Slices (one MR, one commit each)

0. **Paused-detection fix** (found while scoping): FactoryPanel/AndonMode
   read `status.Enabled`, but the operator sends snake_case
   `policy_enabled` (`handlers_status.go:38`) — the paused badge and
   PAUSED lamp could never fire. Use OverviewPanel's status→policy
   fallback.
1. **Departure board (4)**: `departureHelpers.ts` + `DepartureBoard.svelte`
   replace `floorLogLines` + the CSS marquee. One row per run; DELAYED =
   *observed* in the same stage past a fuse (12 m default / 25 m
   ci_watch) via a stage-entry observation map (`nextStageSince`, read
   untracked in the panel effect to avoid a self-retriggering loop);
   history rows read ARRIVED / DIVERTED / HELD. Cells flap only on value
   change (`{#key}` remount); static under reduced-motion.
2. **The creel (5)**: logical fleet agents (per-conversation rows
   collapsed via `rootAgentId` — gauge switched to `liveAgentCount` for
   the same honest number) drawn as bobbins at the top of the stage;
   active agents' bobbins wind (orbiting pirn dot), idle sit dim, stale
   feed freezes the lot. Cap 8 + `+N` overflow.
3. **Gauge motion (7)**: shared `widgets/RollingNumber.svelte` (digit
   reels, em dash for unknown, reduced-motion static) on the four count
   gauges; pass-rate gains a needle dial with spring-overshoot easing.
   Budget fuel gauge DEFERRED — no rolling-window budget on the HUD
   status payload yet (needs operator work).
4. **Sounds (9)**: `factorySounds.ts` — synthesized Web Audio (clack =
   filtered noise burst per laid pick, chime = two partials per bolt,
   low sawtooth horn per spark), default OFF, 🔇/🔊 stage toggle
   persisted in localStorage, AudioContext created inside the toggle
   gesture (autoplay-safe), silent no-op without Web Audio. The reed
   beating air makes no sound — audio obeys the honesty rule.

## Verification

- vitest: departure fuse/observation semantics, sounds no-op guard;
  109+ suite green.
- `pnpm build`; Preview MCP live check: board rows + flap, creel pixels,
  needle at 66.6° for 0.87, rolling reels, sound toggle persistence,
  paused badge + PAUSED lamp with `policy_enabled: false` (the fix,
  proven end-to-end).
