# Iteration Plan: Factory View — Pattern Books

**Date:** 2026-07-08
**Source research:** `.loom/174-research-factory-view-inspiration-2026-07-08.md` (concept 8 — "M (needs catalog on HUD API — verify)"; inspiration thread C: Jacquard card chains were interchangeable programs and works of art)
**Prior slices:** `.loom/175`–`.loom/178` (all merged: !1008, !1013, !1014, !1015)

## Riskiest assumption + kill-test

**Load-bearing assumption**: the Pattern Loom catalog is already on the
HUD API, and a pipeline run's pattern attribution is derivable
client-side with no backend work.

**Kill test** (ran in-code, 2026-07-08): (1) `GET /api/patterns` serves
`PatternInfo{id, slug, name, makes, status}` — route exists
(`internal/hud/routes.go`), `patternsStore` + PatternsPanel already
consume it. (2) Attribution chain: `svc_pattern_stamp.go:134` mints
plan id `plan-stamp-<patternSlug(slug+"-"+primary)>`; the operator's
backlog LIST serializes full untagged `store.BacklogItem` structs, so
`PlanID` is on the wire for every row (`handlers_backlog.go:44`,
`types.go:77`); runs carry `BacklogID`. Since `pattern.Slug` is already
kebab, `plan-stamp-<slug>-…` prefix-matches the catalog (longest-slug
first to disambiguate `go-rest` vs `go-rest-service`).

**Failure mode if wrong**: shelf shows books but can never light one up
— decorative theater, the exact anti-goal. (It isn't wrong; both legs
verified in source above.)

**Status**: passed 2026-07-08 (code refs above; live render check in
this slice's verification).

## Slices (one MR)

1. **Rune-free attribution helpers** (`utils/patternBooks.ts` + vitest):
   `stampedPatternSlug(planID, slugs)` — longest-prefix match;
   `patternBooks(patterns, backlog, activeRuns, history)` — one book per
   approved pattern with {active, merged, escalated} counts derived
   run→backlog→PlanID→slug. Non-stamp plans and unknown slugs attribute
   to nothing (no guessing).
2. **PatternShelf** (`components/mills/PatternShelf.svelte`): the
   catalog as a shelf of labeled books between the loom stage and the
   gauges — spine with a mini punch-card chain, name + makes, woven
   counts. A book whose pattern has an active run "feeds": its chain
   animates (static under reduced-motion). Click → the Patterns panel.
   Empty catalog → shelf absent, no placeholder theater.
3. **Wiring**: `BacklogItem.PlanID?` added to the TS interface (already
   on the wire); one-shot `patternsStore.fetch()` on Factory mount (the
   patterns poller has no owner refcount, and the catalog is near-static
   — no new pollers per the guardrail); shuttle pick labels append
   `· book «name»` when the advancing run is pattern-stamped.

## Non-goals

Weaving the pattern chain into the punch tape (the tape IS the policy —
concept C's honesty stands); shift report (10); budget fuel gauge
(needs operator payload).

## Verification

- vitest: longest-slug disambiguation, non-stamp plan ids ignored,
  counts across active+history, approved-only shelf, sort order.
- `pnpm build`; Preview MCP: shelf renders from mocked catalog +
  stamped backlog, attributed book lights while its run is active,
  click navigates to Patterns.
