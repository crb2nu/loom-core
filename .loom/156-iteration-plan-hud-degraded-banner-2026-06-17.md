# Iteration Plan: degraded-state banner in the HUD (Slice 5b of .loom/149)

**Date**: 2026-06-17
**Branch**: `feat/hud-degraded-banner`
**Parent plan**: `.loom/149-plan-backend-hub-transport-stability-2026-06-12.md` (Slice 5)

## Context

Slice 5a (`.loom/155`, merged !721) added explicit `degraded` /
`degraded_reason` / `degraded_since` fields to the `FleetSnapshot`, carried to
the frontend on the existing fleet API + SSE payload. This iteration lands the
frontend half: the HUD reads those fields and renders an honest
"Degraded since HH:MM — <reason>" banner instead of waiting ~90s for the generic
staleness pill (which, because `UpdatedAt` keeps advancing on every carry-over,
often never fires at all).

## Scope

**In (5b)**:
- `fleet.svelte.ts`: `degraded` / `degradedReason` / `degradedSince` `$state`
  fields, parsed in `applySnapshot` (healthy snapshot clears them since
  `degraded` is serialized without omitempty; reason/since are omitempty).
- `ConnectionBanner.svelte`: a `degraded` banner state that takes priority over
  the generic `stale` pill (degraded is the more specific signal). Text:
  "Degraded since HH:MM — <reason>"; icon ◐; `aria-live="polite"` (inherited).
  Hidden while a connection banner (reconnecting/disconnected/circuit-open) is
  showing, since those are higher severity.
- `theme.css`: `.connection-degraded` rule (amber, medium prominence — above the
  faint `.connection-stale`, since it is a real backend degradation).
- Rebuilt `go:embed`'d HUD dist (`make hud-frontend`); committed (CI does not
  rebuild it).

**Out**: circuit-open state surfacing on the monitor side (Slice 5 stretch,
unrelated to the carry-over path).

## Riskiest assumption + kill-test

**Load-bearing assumption**: A connected HUD whose backend emits a `FleetSnapshot`
with `degraded:true` renders the amber "Degraded since HH:MM — <reason>" banner
(not masked by the connection/stale states, and not silently dropped).

**Kill test** (≤30 min, live): On a running HUD, induce a sessions/presence
sub-fetch failure on the daemon (e.g. transient agent_context lock-timeout, which
is the storm's own signature) and confirm the banner appears with the correct
reason + onset time, then clears on recovery. Could not run in the local vite
preview: the banner requires a *connected* daemon in a *degraded* sub-fetch state,
which the preview (no live degraded backend) cannot produce — a disconnected
preview shows the higher-priority "Disconnected" banner and masks it.

**Failure mode if wrong**: the banner never shows (priority/visibility bug) or
shows stale onset — but the 5a backend fields are still correct and
machine-readable, so the regression is cosmetic.

**Status**: not run (deferred to a live HUD with an induced degraded fleet).
Build-level verification done: `make hud-frontend` (vite) compiled the Svelte
component + TS store clean; `go build ./internal/hud/` embedded the new dist OK.

## Acceptance criteria
1. `applySnapshot({degraded:true, degraded_reason, degraded_since})` sets the
   three store fields; a `{degraded:false}` snapshot clears them.
2. Banner shows "Degraded since HH:MM — <reason>" when SSE connected + degraded.
3. Connection states (reconnecting/disconnected/circuit-open) take priority;
   degraded takes priority over generic stale.
4. `make hud-frontend` builds clean; `go build ./internal/hud/` embeds OK.

## Risks
- Conditional banner whose live rendering was not exercised (see kill-test).
  Mitigated: simple type-checked binding; vite build compiles the template;
  backend contract proven by `TestFleetMonitor_DegradedStateSurfacing` (5a).
- dist churn: 37 generated bundle files change; collapsed in MR diff via
  `.gitattributes` (`linguist-generated -diff`).

## Status
- [x] implemented
- [x] build green (vite + go embed)
- [ ] live banner kill-test (deferred — needs induced degraded fleet)
- [ ] MR merged
