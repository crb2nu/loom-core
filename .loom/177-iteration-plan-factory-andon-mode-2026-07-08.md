# Iteration Plan: Factory View — Andon Mode

**Date:** 2026-07-08
**Source research:** `.loom/174-research-factory-view-inspiration-2026-07-08.md` (concept 6, inspiration threads B — andon boards/digital twins — and F's north-star odometer; recommended slice 3)
**Prior slices:** `.loom/175` (honest loom, merged !1008), `.loom/176` (bolt archive, merged !1013)

## Goal

The glanceable overhead board and the interactive console are different
products generated from one source of truth — "nobody interacts with an
Andon board; it exists to be glanced at from 3–10 m". Give the Factory
panel an explicit fullscreen andon mode for the office TV: a tri-state
lamp (weaving / paused / escalation storm, plus idle), a giant
north-star odometer (autonomous merges · 24h), and unmissable
staleness honesty — the board may never glow green on a dead feed.

## Riskiest assumption + kill-test

**Load-bearing assumption**: everything the board needs is already on the
panel's existing 15 s poll — `millsStore.kpis.metrics`
(`pipeline_merged_runs`/`pipeline_escalated_runs`/`gate_pass_rate`),
`status.Enabled`, `lastUpdated`/`isStale`, and active-run counts — and the
router's three-segment hash (`#mills/factory/andon`, `router.detail` +
`navigateDetail`, `router.svelte.ts:191-211,294-297`) survives reload so a
TV can bookmark the mode.

**Kill test**: covered by slice-1's live kill-test (KPI + runs payloads
verified against `hud.flexinfer.ai` 2026-07-08) plus in-code verification
of the router detail segment (parse + `_syncHash` round-trip). Render
check via Preview MCP: load `#mills/factory/andon` cold and confirm the
board mounts without a click.

**Failure mode if wrong**: the mode needs a new endpoint or a router
change first — backend/router slice before the frontend slice.

**Status**: passed 2026-07-08 (slice-1 evidence + router source refs).

## Slices (one MR)

1. **Rune-free andon helpers** (`utils/andonHelpers.ts` + vitest):
   `andonState(...)` — priority stale > paused > storm > weaving > idle,
   with an explicit documented storm rule (≥3 sparks in 24 h AND sparks
   at least half of bolts); `odometerDigits(value, minDigits)` for the
   mechanical counter; `freshnessLabel(lastUpdated, now)` for the
   "updated Ns ago" line.
2. **AndonMode overlay** (`components/mills/AndonMode.svelte`): fixed
   full-viewport board — state lamp band, giant odometer on bolts
   merged · 24h (digit columns roll on change; static under
   reduced-motion), four secondary gauges (shuttles / sparks / threads /
   pass rate), footer with live freshness tick + wall clock. Stale feed
   flips the lamp to STALE and freezes nothing silently. Browser
   fullscreen toggle button; Escape / ✕ exits.
3. **Routing + wiring**: `⛶ andon` header action on the Factory panel;
   mode state IS `router.detail === 'andon'` so `#mills/factory/andon`
   deep-links from a TV bookmark and Escape restores `#mills/factory`.
   Overlay mounts inside FactoryPanel, so the existing 15 s poll keeps
   running — no new pollers.

## Non-goals (later slices)

Needle physics / budget fuel gauge (concept 7), departure-board floor log
(4), weaver bobbins (5), sounds (9).

## Verification

- vitest: state-priority matrix incl. storm boundary cases, odometer
  digit padding/rollover, freshness buckets.
- `pnpm build` (dist stays gitignored).
- Preview MCP: cold-load `#mills/factory/andon` with mocked Mills data;
  screenshot weaving + stale states; Escape returns to the loom.
