# Iteration Plan: Factory View — Bolt Archive / Tartan of the Week

**Date:** 2026-07-08
**Source research:** `.loom/174-research-factory-view-inspiration-2026-07-08.md` (concept 3, inspiration thread D — GitHub Skyline; recommended slice 2)
**Prior slice:** `.loom/175-iteration-plan-factory-honest-loom-2026-07-08.md` (merged — deterministic `seededPattern` rows this slice builds on)

## Goal

The cloth currently evaporates: rows fade off the loom and the week's work is
gone. Every woven row is a real merged MR — the cloth is literally the fabric
of the codebase's week. Make it a persistent, shareable artifact: a strip view
of the last 7 days of cloth, exportable as SVG/PNG for standups or the office
TV.

## Riskiest assumption + kill-test

**Load-bearing assumption**: `GET /api/mills/pipeline/runs?state=terminal`
serves a full week of terminal runs in one call — the operator's `since`
default is already `now-7d` and `limit` is client-controlled with no cap
(`cmd/loom-mills-operator/handlers_pipeline.go:48-78`), and list rows carry
`EndedAt` + `State` + `BacklogID` needed for day-grouping.

**Kill test**: covered by the slice-1 kill-test (passed 2026-07-08, live
against `hud.flexinfer.ai`) — the same endpoint, same row shape; the only new
usage is a larger `limit`. Verified in-code: handler parses `limit` with no
maximum (`handlers_pipeline.go:60-68`).

**Failure mode if wrong**: the archive shows a truncated week; degraded, not
broken. No backend work required either way.

**Status**: passed 2026-07-08 (see slice-1 evidence + handler source refs).

## Slices (one MR)

1. **Non-storing archive fetch.** `millsStore.fetchArchiveRuns(limit=500)`
   returns terminal runs WITHOUT touching `pipelineHistory` — the live loom
   diffs `pipelineHistory` into weave events, so overwriting it with 500
   older runs would flood the shuttle with phantom picks.
2. **Rune-free tartan helpers** (`utils/tartanHelpers.ts` + vitest):
   `archiveDays(runs, days, now)` groups terminal runs by local end-day
   (oldest day first); `tartanSVG(days, opts)` renders the week as one
   deterministic SVG string — each run woven with the SAME `seededPattern`
   as the live loom, day gutters + labels, bolt/spark tones, totals caption.
   Colors are passed in resolved (no CSS vars) so the exported file is
   self-contained.
3. **BoltArchive overlay** (`components/mills/BoltArchive.svelte`): toggled
   from the Factory panel stage; fetches once on open, renders the SVG
   inline, offers "download SVG" and "download PNG" (canvas rasterize).
   Loading/error/empty states; Escape/backdrop closes.

## Non-goals (later slices)

Andon fullscreen mode (concept 6), pattern books (8), sounds (9).

## Verification

- vitest: day-grouping boundaries (local midnight, `now` injected), SVG
  determinism (same runs → identical string), bolt/spark mapping, empty-day
  bands, totals line.
- `pnpm build` in `internal/hud/frontend` (dist stays gitignored).
- Local render check via Preview MCP with mocked Mills data.
