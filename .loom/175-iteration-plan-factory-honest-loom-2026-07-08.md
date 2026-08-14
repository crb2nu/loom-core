# Iteration Plan: Factory View — The Honest Loom

**Date:** 2026-07-08
**Source research:** `.loom/174-research-factory-view-inspiration-2026-07-08.md` (concepts 1 + 2 + B's staleness honesty; kill-test passed 2026-07-08)
**Files:** `internal/hud/frontend/src/lib/components/mills/FactoryPanel.svelte`, `internal/hud/frontend/src/lib/utils/factoryHelpers.ts` (+ test)

## Goal

Convert the Factory panel's three theater elements into instruments, per
inspiration threads A (factory games: the animation must BE the mechanism),
B (andon/digital-twin: never imply activity on a stale feed), and C (the
Jacquard tape IS the program).

## Slices (one MR)

1. **Event-driven weaving (A).** A shuttle pass lays cloth only for a real
   event: terminal run (bolt/spark row) or a stage transition ("pick") of an
   active run. No event → the reed beats air (motion without fabrication).
   New rune-free helper `diffStagePicks(prevMap, activeRuns)` diffs
   `CurrentStage` across polls. Floating labels become 100% real (drop the
   30%-random stage label).
2. **Deterministic cloth + inspectable rows (A/D).** Row thread patterns
   seeded from run IDs (`seededPattern`, mulberry32 over a string hash) —
   same history always weaves the same fabric. Canvas hit-testing on the
   cloth region: hover → tooltip (backlog ID, kind), click →
   `millsStore.openRunDetail(runID)` + mount `<PipelineRunDetail />`
   (drawer already exists, store-driven).
3. **The tape is the program (C).** Punch-card holes become a pure function
   of live policy (`policyTapeSeed(version, enabled)` + `tapeHole`). A
   policy version bump visibly re-punches the chain and floats a
   "policy spliced · vN" label (suppressed on first policy load).
4. **Staleness honesty (B).** `millsStore.isStale || error` halts the loom
   mid-pick with an explicit caption and freezes the tape scroll — the loom
   may never weave on a dead feed.

## Non-goals (later slices)

Bolt archive/export (concept 3), andon fullscreen mode (concept 6), pattern
books (concept 8), sounds (concept 9).

## Verification

- vitest: `diffStagePicks`, `seededPattern` determinism, `policyTapeSeed`/
  `tapeHole` stability + version-bump divergence.
- `pnpm build` in `internal/hud/frontend` (dist stays gitignored).
- Local render check via Preview MCP where feasible.
