# Plan — HUD chapter ingestion + display (2026-06-14)

Surface Claude Code session **chapters** in the HUD, keyed to the same
conversation identity introduced by MR !706 (conversation grouping).

## Riskiest assumption + kill-test

**Load-bearing assumption**: When Claude calls the MCP tool
`mcp__ccd_session__mark_chapter` (args `title`, optional `summary`), Claude
Code fires its **PostToolUse** hook for that MCP tool, and the hook stdin
includes `tool_input` (the title/summary) — so loom's already-installed
matcher-less PostToolUse `telemetry_eventEmit` hook
(`pkg/generator/configs_hooks.go` `appendTelemetryEventEmitHooks`) pipes it to
`loom agent event-emit --hook post-tool-use --platform claude-code`, where we
can mint a `chapter.marked` event.

**Kill test**: From a live Claude Code session, call `mark_chapter` with a
unique marker, then inspect what the installed hooks captured.

**Status**: **passed 2026-06-14.** Live evidence — flightdeck spool
`~/.loom/flightdeck/spool/transcript-20260614T195827Z-0000.jsonl` recorded the
call under BOTH `PreToolUse` and `PostToolUse`, each with
`tool_name=mcp__ccd_session__mark_chapter` and
`tool_input={title,summary}` present (`has_tool_input=True`). The installed
PostToolUse block [3] in `~/.claude/settings.json` is matcher-less (matches all
tools incl. MCP). `internal/daemon/api_events.go` `HandleEventsPublish` accepts
any non-empty event type → eventBus → HUD `broadcastAgentEvent` → eventLog +
`/api/events` SSE. So the only code needed downstream is (a) mint the event,
(b) allowlist it, (c) render it.

**Failure mode if wrong**: if PostToolUse didn't fire for MCP tools (or dropped
`tool_input`), chapters could never be ingested via hooks and we'd need a
different capture path (transcript tailing / a dedicated ccd_session bridge).
Retired by the kill-test above.

## Slices

- **S1 — ingestion (this MR)**: `nativeHookToEvent` mints `chapter.marked`
  `{session_id, agent_id, title, summary, marked_at, tool_use_id}` for the
  chapter tool (title/summary verbatim — user-authored labels, not secrets, no
  redaction); add `chapter.marked` to `isAllowedEmittedType`. Unit test.
- **S2 — display (this MR)**: frontend `chapters` store subscribes to
  `chapter.marked` SSE, keyed by `conversationId(agent_id)`; FleetTable shows a
  chapter pill (latest title + count) on the conversation lead row.
- **S3 — follow-up (deferred)**: durable per-session chapter history + a
  chapter timeline drawer; ingest pre-existing chapters on HUD load.
