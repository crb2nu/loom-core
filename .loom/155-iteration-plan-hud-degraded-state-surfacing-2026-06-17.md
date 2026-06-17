# Iteration Plan: explicit degraded-state on fleet snapshots (Slice 5a of .loom/149)

**Date**: 2026-06-17
**Branch**: `feat/hud-degraded-state-surfacing`
**Parent plan**: `.loom/149-plan-backend-hub-transport-stability-2026-06-12.md` (Slice 5)

## Context

`.loom/149` Slice 5 ("honest degraded-state surfacing"): on a partial
sessions/presence sub-fetch failure the fleet monitor carries over the previous
snapshot's roster (`internal/hud/monitor/fleet.go`, the `else` branch at the old
~line 692) so the HUD shows live data instead of blank rows. But `UpdatedAt`
still advances on every carry-over, so the frontend's `staleAfter` banner never
fires — the degradation is **silent**. The only signal today is a server-side
`m.Logger.Info("...carried over...")` line.

This iteration lands the backend half (5a): make the degraded state explicit and
machine-readable in the snapshot/API/SSE payload. Frontend rendering of a
"degraded since HH:MM (reason)" banner is deferred to 5b (needs the
`go:embed`'d HUD dist rebuild; CI does not rebuild it).

## Scope

**In (5a)**:
- `FleetSnapshot.{Degraded bool, DegradedReason string, DegradedSince time.Time}`
  (json `degraded` / `degraded_reason,omitempty` / `degraded_since,omitempty`).
- Populate in `refresh()`'s partial-failure branch: `Degraded=true`,
  `DegradedReason` from which side failed, `DegradedSince` = onset of the current
  degraded streak (persisted across consecutive degraded refreshes via `prev`).
- Recovery (the `sessionsOK && presenceOK` branch) leaves all three at zero
  values automatically (fresh `FleetSnapshot{}` each refresh).
- `degradedReason(sessionsOK, presenceOK)` pure helper.
- Carry-over log line gains `degraded_reason` / `degraded_since`.
- The fields flow to the frontend through the existing JSON serialization on the
  fleet API + SSE snapshot — additive and backward-compatible (older clients
  ignore unknown fields).

**Out (5b, follow-up)**:
- Frontend `fleet.svelte.ts` / `ConnectionBanner` reading `degraded` to render
  "degraded since HH:MM (sessions fetch failing)" instead of the generic
  staleness pill; TS type mirror; `make hud-frontend` dist rebuild + commit.
- Circuit-open state surfacing.

## Acceptance criteria
1. Healthy refresh → `Degraded=false`, `DegradedReason==""`, `DegradedSince` zero.
2. Sessions/presence partial failure → `Degraded=true`, reason names the failing
   side(s), `DegradedSince` stamped.
3. Consecutive degraded refreshes preserve the original `DegradedSince`.
4. Recovery clears all three.
5. `degradedReason` returns the correct phrase for each (sessionsOK, presenceOK)
   combination reachable in the branch.
6. `go test ./internal/hud/monitor/` green; build/vet/gofmt/golangci-lint clean.

## Risks
- Additive JSON fields only; no behavior change to existing carry-over logic, so
  the live HUD is unaffected until 5b consumes the fields. Low risk.
- `DegradedSince` persistence depends on `prev` (the prior snapshot); the
  existing `fleetSnapshotLooksEmpty` carry-over (`snap = prev`) preserves prev's
  degraded fields, which is consistent (still degraded).

## Test plan
- `TestDegradedReason` (pure helper, all reachable combinations).
- `TestFleetMonitor_DegradedStateSurfacing` — healthy → degraded(onset) →
  still-degraded(onset persists) → recovered, driven by a toggled failing
  `agent_presence_list` handler on the mock daemon.

## Status
- [x] implemented
- [x] tests green (`go test ./internal/hud/monitor/` full suite ok 2.4s; build + vet + gofmt clean 2026-06-17). New tests: `TestDegradedReason`, `TestFleetMonitor_DegradedStateSurfacing`.
- [ ] MR merged
