# RALPH Iteration Plan — HUD Live Events Listener Drift (2026-06-10)

## Review

- Source backlog: `.loom/hud-audit-plan-2026-06-06.md` and current HUD/live-events reports.
- Prior context: `loom-core/ralph-loop` notes show Live Sessions previously blocked on deployed tool-call correlation and HUD a11y Slice 7 completed through 7c.
- Code finding: `liveSessionsStore` subscribes to canonical daemon events (`session.start`, `session.end`, `agent.status.change`, `tool.call.start`, `tool.call.end`), but `eventStore.connect()` only attached browser `EventSource` listeners for a hardcoded allowlist that omitted those names. Named SSE events do not flow through `onmessage`, so the UI silently missed them.

## Align

- Slice name: **dynamic named-SSE listener registration for HUD stores**.
- Scope in:
  - `internal/hud/frontend/src/lib/stores/events.svelte.ts` registers a named EventSource listener whenever a store subscribes with `eventStore.on(...)`.
  - Reconnects attach both the documented core event set and all currently registered store event names.
  - Keep the existing core event list as eager registration for events needed by wildcard/history surfaces.
  - Rebuild embedded frontend dist.
- Scope out:
  - No backend event-shape changes.
  - No visual redesign of Live Sessions.
  - No new frontend test harness; this repo still has build-only frontend validation.
- Acceptance criteria:
  1. `session.*`, `tool.call*`, and `agent.status.change` subscriptions can receive named SSE events.
  2. Store-specific events not present in the old allowlist (for example `hud.cost`, `hud.otel`, `access.denied`) attach when subscribed.
  3. EventSource reconnects preserve listener coverage.
  4. `make hud-frontend` succeeds and regenerated `dist/` includes the listener change.

## Risk

- Load-bearing assumption: store subscriptions are created before or shortly after the shared EventSource connects, so dynamic listener attachment covers both startup and reconnect.
- Kill test: inspect `EventStore.on(...)` and reconnect path; build the frontend. A named event subscribed after connection must call `ensureSourceListener`, while a reconnect must iterate `this.listeners.keys()`.
- Failure mode if wrong: a named event subscribed only through wildcard listeners remains invisible. Mitigation: keep core event eager registration for known daemon and HUD event families.

## Prove

- `make hud-frontend`.
- Bundle/source inspection for `ensureSourceListener` and canonical `tool.call.start` event registration.

## Handoff

- Next slices: if live HUD still shows stale Live Sessions after this lands, run a deployed kill-test that emits one canonical `tool.call` event for a known active session and checks `/api/events`, `/api/sessions/{id}/trace`, and the Live Sessions card in the same browser session.
