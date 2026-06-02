# Design: HUD Live-Session activity channel (tool-call telemetry)

**Status**: Draft — prototype landed on a branch, gated on deployed kill-test
**Author**: Claude (Opus 4.8), 2026-06-01
**Related**: MR !583 (audit `agent_id` fix), !578 (roster-blank fix),
`project_hud_no_agents_session_list_timeout` memory,
worktrees `feat-presence-session-telemetry`, `fix/hud-session-list-truncation`

---

## Problem

The HUD "Live Sessions" panel lists active sessions but each reads
**"No captured activity for this session yet / 0 calls."** `buildSessionTrace`
([internal/hud/app_routes_operations.go:181](internal/hud/app_routes_operations.go))
assembles per-session activity from three sources, all empty for interactive
CLI agents:

1. **Context entries** (`agent_context_add`) — interactive agents don't write
   context entries; `entry_count=0`. Expected-empty.
2. **eventLog events** — see gap analysis below.
3. **Audit traces** (`loom/audit-traces`) — filtered by exact
   `trace.AgentID == session.AgentID` + time bounds.

MR !583 fixed source (3): audit traces now carry the resolved proxy-session
`agent_id` instead of an empty string, so the exact-match filter works for any
call routed through the HUD's **own** daemon. This doc addresses source (2) —
the eventLog "calls" channel — and the distributed-agent case that neither fix
covers.

## Riskiest assumption + kill-test

**Load-bearing assumption**: A `tool.call` event published on the daemon
EventBus with a `session_id` field, when appended to the HUD `EventLog`, is
matched to the right session by `eventMatchesSessionTrace`
([app_routes_operations.go:281](internal/hud/app_routes_operations.go)) and
renders as a "call" in the deployed Live Sessions panel — i.e. the panel reads
`EventLog`, not some other store, and the session_id correlation holds on the
real host.

**Kill test** (≤30 min, on the **deployed** mobile-hud, not unit-only):
1. Build the prototype image, deploy to the in-cluster `mobile-hud`.
2. Drive one real tool call from an agent whose proxy session has a known
   `session_id` (capture it from `/api/hud/sessions`).
3. `GET /api/mobile/v1/sessions/<session_id>/trace` and assert the `events`
   array contains a `tool.call` entry for that call.
4. Open the Live Sessions panel in the browser and confirm the call renders
   (count > 0, not "No captured activity").
   - **Positive check**: the trace `events` array is non-empty AND the UI shows it.
   - **Negative check**: confirm a session with *no* calls still shows 0 (no
     cross-session leakage from the broadened eventLog).

**Failure mode if wrong**: We wire daemon-emit + eventLog-append, unit tests
go green, but the deployed panel still shows 0 — because the panel reads a
different field/store, or the session_id on the event doesn't match the
session_id the UI keys on, or SSE vs. ring-buffer divergence. This is the exact
"green tests, wrong host" trap the spec-riskiest-assumption rule exists for.

**Status**: **FAILED 2026-06-02** on the deployed `mobile-hud`. Slices 1+2 (sink
+ source) are merged to main (`runtime.go:117-128`, `daemon_call.go:220`) and the
deployed image (`:20260602-133612`) post-dates the merge, but every active
session's trace still shows **0 `tool.call` events**. Root cause is the same
identity gap the correlation spec hit: `emitAudit` only publishes when
`params.SessionID != ""`, and proxy/hub-routed interactive calls reach the central
daemon with empty `session_id` AND empty `agent_id` (audit entries observed with
`agent_id:""`). Full evidence in
`.loom/spec-hud-agent-id-correlation-2026-06-01.md` → "Deployed kill-test
2026-06-02". Slice 3 (distribution) and Slice 4 (hygiene) remain BLOCKED until the
identity-propagation keystone lands and this kill-test passes.

---

## Architecture trace (as-is)

```
  agent tool call
        │
   ┌────┴─────────────────────────────────────────────┐
   │ Path A: CLI hooks (claude-code, gemini, codex)    │
   │   PreToolUse/PostToolUse hook                      │
   │     → `loom agent event emit` (cmd_agent_event_emit.go)
   │     → pkg/eventpub.HTTPPublisher                   │
   │     → daemon POST /events/publish (api_events.go:58)
   │     → d.eventBus.Publish(tool.call.start/end)      │
   │                                                    │
   │ Path B: hookless / proxied agents (kilocode, …)    │
   │   loom proxy handleProxyToolsCall                  │
   │     → proxyHeartbeat() POST /api/agent/heartbeat   │  (presence only)
   │     → loom/call → daemon → emitAudit               │  (audit only, NO event)
   └────┬───────────────────────────────────────────────┘
        │ daemon EventBus
        ▼
   HUD event consumer (internal/hud/runtime.go:105-123)
     ec.On("session.start"/"session.end"/"agent.status.change") → eventLog.Append
     ec.OnAny(...) → sseHub.Broadcast   ← tool.call* reaches browsers LIVE here
        │                                  but is NEVER appended to eventLog
        ▼
   EventLog ring buffer (internal/hud/eventlog.go)
        │
        ▼
   buildSessionTrace → sessionTraceEvents → eventLog.All(1000)
     eventMatchesSessionTrace: matches on Data.session_id OR (agentID + time bounds)
```

## The three gaps

1. **Sink gap (HUD)** — `runtime.go` appends only `session.start`,
   `session.end`, `agent.status.change`, `decomp.hint` to the `EventLog`.
   `tool.call`/`tool.call.start`/`tool.call.end` are fanned out to browsers via
   `OnAny`→SSE but **never appended**, so `buildSessionTrace` (which reads
   `eventLog.All`) never sees them. Even Path-A hook events don't render in the
   per-session trace today.

2. **Source gap (daemon)** — the daemon **defines** `EventToolCall`,
   `EventToolCallStart`, `EventToolCallEnd` ([events.go:29,68,72](internal/daemon/events.go))
   but **never publishes** them. Only Path-A hooks inject tool-call events (via
   `/events/publish`). Hookless/proxied agents (Path B) emit presence heartbeats
   and an audit entry, but **no event** — so there is nothing for the panel to
   show even after the sink gap is closed.

3. **Distribution gap** — each agent's **local** `loomd` has its own EventBus.
   The central in-cluster `mobile-hud` subscribes to **one** daemon's event
   stream (its `MetricsAddr`). A distributed agent's tool calls live on its
   local daemon's bus and never reach the central HUD unless explicitly routed
   there (eventpub → central daemon `/events/publish`, or proxy → HUD REST).

## Decision

**Primary channel = daemon-emit from the audit chokepoint, not a new
proxy→HUD REST endpoint.**

Rationale: `emitAudit` ([daemon_call.go](internal/daemon/daemon_call.go)) is the
single point every executed tool call passes through, and after MR !583 it
already holds the resolved `agent_id` *and* the `session_id`. Publishing one
`EventToolCall` there gives same-daemon visibility for **every** agent — hook or
hookless — with zero proxy changes and reusing the existing daemon→HUD event
bridge. A new proxy→HUD REST endpoint would duplicate identity resolution, add a
second code path, and still miss daemon-internal calls (synthetic bulk, weaver).

The proxy→HUD-REST idea from the original analysis is the right tool for the
**distribution gap only** (remote daemon → central HUD), where there is no
shared bus. There, prefer reusing the existing `eventpub` publisher pointed at
the **central** daemon's `/events/publish` (already the Path-A mechanism) over a
bespoke REST endpoint.

### Slices

- **Slice 1 — sink (HUD):** append `tool.call`/`tool.call.start`/`tool.call.end`
  events to the `EventLog` in `runtime.go`. Small, safe, unit-testable. *(prototyped)*
- **Slice 2 — source (daemon):** publish `EventToolCall` from `emitAudit` when
  `params.SessionID != ""` (session-scoped; avoids flooding the bus with
  hookless ambient calls), payload `{session_id, agent_id, server, tool, status,
  duration_ms, cached}`. *(prototyped)*
- **Slice 3 — distribution:** point distributed agents' `eventpub`
  (`LOOM_DAEMON_HTTP_URL`) at the central daemon, or have the proxy forward
  session-scoped tool.call events to the central `/events/publish`. Config +
  deployment; **this is where the deployed kill-test must pass before shipping.**
- **Slice 4 — hygiene:** rate-limit / sample high-frequency tool calls so the
  1000-entry ring buffer isn't dominated by one chatty session; confirm SSE
  back-pressure (`bus.backpressure`) is unaffected.

### Negative check (disconfirming search)

- Does the panel read something *other* than `EventLog`? `buildSessionTrace`
  also merges `Entries` (context) and `Traces` (audit). If the deployed UI
  prioritizes audit `Traces` (already fixed by !583) the eventLog channel may be
  redundant for same-daemon agents — verify which source the UI surfaces as
  "calls" before investing in Slices 2-3. **This is part of the kill-test.**
- `OnAny`→SSE already pushes `tool.call*` to browsers live. If the panel renders
  from the **live SSE stream** rather than the trace endpoint, the sink gap may
  only affect the *historical* trace, not the live count — changing which slice
  matters. Confirm on the deployed host.

## Secondary: stale "active" sessions

~14 sessions show "active" but are flagged "14 stale sessions" (codex keepalive
wrappers, federation-mirror sessions). Options (separate change):
- Don't register keepalive/mirror sessions as `status=active`, or
- Tighten the live-panel cutoff (`fleetLivePresenceStaleAfter=90s`) so they drop
  from the live panel before the reaper (`fleetStaleSessionReapAfter=10m`).
Tracked with the `feat-presence-session-telemetry` / session-reaper work.
