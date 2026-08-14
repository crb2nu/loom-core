<!--
  LiveSessionsCard — Phase 3 of the spectator plan
  (`.loom/99-implementation-plan-agent-telemetry-spectator-2026-05-04.md`).

  Shows every active agent session and the trailing handful of tool calls
  per session. Subscribes to `liveSessionsStore`, which itself listens to
  the SSE event stream for session.start/end, agent.status.change, and
  tool.call.start/end.

  Renders a compact collapsed view by default; clicking a row expands to a
  scrollable modal-ish detail panel showing the session's full ring buffer.
-->

<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import {
    liveSessionsStore,
    type LiveSession,
    type ToolCall,
  } from '../stores/liveSessions.svelte.ts';
  import { relativeTime } from '../utils/format.ts';
  import EmptyState from './shared/EmptyState.svelte';

  // agentCount lets the empty state distinguish "no agents connected at all"
  // from "agents connected but idle between turns". Without this, operators
  // who saw N live agents in the footer + zero sessions assumed something
  // was wrong with the spectator pipeline (it was just idle).
  //
  // sessionCount is the canonical active-session count from the fleet snapshot
  // (`fleetStore.activeSessions.length`) — the SAME source the status bar uses.
  // The headline reads it directly so this card and the status bar can never
  // disagree (previously the headline counted SSE-store entries, which drift
  // from the REST snapshot by a session or two — the "18 active vs 17 active
  // sessions" jank). The row list below still streams from liveSessionsStore.
  let { agentCount = 0, sessionCount }: { agentCount?: number; sessionCount?: number } = $props();

  let expandedSessionID: string | null = $state(null);

  onMount(() => {
    liveSessionsStore.connect();
  });

  onDestroy(() => {
    // We intentionally don't disconnect — the store is process-wide and other
    // panels may also subscribe. Disconnect happens on App teardown.
  });

  // Sessions are grouped by conversation so one chat that hopped across
  // repos/worktrees renders under a single header instead of as flat, unrelated
  // rows (the lifecycle hooks mint a distinct agent_id per workspace+conversation),
  // while distinct chats sharing a repo stay separate. This matches the Fleet
  // "Live Agents" table's conversation-first grouping. `groupCount` below is the
  // distinct-conversation count derived from the visible groups; `agentCount`
  // (a prop) stays the workspace-scoped connected-agent count for the empty state.
  let groups = $derived(liveSessionsStore.groupedSessions);
  // Prefer the canonical fleet-snapshot count; fall back to the SSE store only
  // when this card is mounted standalone without the prop.
  let activeCount = $derived(sessionCount ?? liveSessionsStore.activeSessionCount);
  let groupCount = $derived(groups.length);
  let inFlightCount = $derived(liveSessionsStore.inFlightCallCount);

  function toggleExpand(sid: string) {
    expandedSessionID = expandedSessionID === sid ? null : sid;
  }

  function statusClass(status: string): string {
    switch (status) {
      case 'active':
        return 'status-active';
      case 'idle':
        return 'status-idle';
      case 'offline':
      case 'expired':
        return 'status-offline';
      default:
        return 'status-unknown';
    }
  }

  function formatToolName(call: ToolCall): string {
    if (call.source === 'context') return `context.${call.tool_name}`;
    if (call.source === 'event') return call.tool_name.replace(/^agent\./, '');
    if (call.server_name) return `${call.server_name}.${call.tool_name}`;
    return call.tool_name;
  }

  function activityLabel(session: LiveSession): string {
    const calls = session.recent_calls.filter((call) => call.source === 'tool' || !call.source).length;
    if (calls === session.recent_calls.length) {
      return `${calls} call${calls === 1 ? '' : 's'}`;
    }
    const n = session.recent_calls.length;
    return `${n} event${n === 1 ? '' : 's'}`;
  }

  // Freshness from the last presence heartbeat. Empty when unknown. The age
  // arrives in seconds, so it is turned back into a timestamp for the shared
  // formatter rather than re-deriving the tiers here.
  function heartbeatLabel(session: LiveSession): string {
    const s = session.heartbeat_age_seconds;
    if (s === undefined || !Number.isFinite(s)) return '';
    return relativeTime(Date.now() - s * 1_000);
  }

  function formatDuration(ms: number | undefined): string {
    if (ms === undefined) return '—';
    if (ms < 1000) return `${Math.round(ms)}ms`;
    return `${(ms / 1000).toFixed(1)}s`;
  }

  function callBadgeClass(call: ToolCall): string {
    if (call.in_flight) return 'badge-pending';
    if (call.error) return 'badge-error';
    if (call.exit_code !== undefined && call.exit_code !== 0) return 'badge-error';
    return 'badge-ok';
  }
</script>

<section class="live-sessions-card" data-testid="live-sessions-card">
  <header class="card-header">
    <h3>Live Sessions</h3>
    <div class="header-meta">
      {#if groupCount > 0 && groupCount !== activeCount}
        <span class="meta-pill" data-testid="conversation-count">
          <span class="meta-num">{groupCount}</span> conversation{groupCount === 1 ? '' : 's'}
        </span>
      {/if}
      <span class="meta-pill" data-testid="active-count">
        <span class="meta-num">{activeCount}</span> active
      </span>
      {#if inFlightCount > 0}
        <span class="meta-pill meta-pill-emphasis" data-testid="in-flight-count">
          <span class="meta-num">{inFlightCount}</span> in flight
        </span>
      {/if}
    </div>
  </header>

  {#if groups.length === 0}
    {#if agentCount > 0}
      <EmptyState
        compact
        heading="{agentCount} agent{agentCount === 1 ? '' : 's'} connected, no active sessions"
        description="Agents are online but idle between turns. Sessions appear when a CLI emits a SessionStart hook."
      />
    {:else}
      <EmptyState
        compact
        heading="No active sessions yet"
        description="Sessions appear when a CLI emits a SessionStart hook."
      />
    {/if}
  {:else}
    <ul class="agent-group-list">
      {#each groups as group (group.root)}
        <li class="agent-group" data-testid="agent-group" data-root={group.root}>
          <div class="group-header">
            <span class={`status-dot ${statusClass(group.status)}`} aria-hidden="true"></span>
            <span class="agent-id">{group.root || '(unknown agent)'}</span>
            {#if group.sessions.length > 1}
              <span class="group-count" data-testid="group-session-count"
                >{group.sessions.length} sessions</span
              >
            {/if}
          </div>

          <ul class="session-list">
            {#each group.sessions as session (session.session_id)}
              {@const expanded = expandedSessionID === session.session_id}
              {@const collapsed = expanded ? session.recent_calls : session.recent_calls.slice(0, 4)}
              <li class="session-row" data-testid="session-row" data-session-id={session.session_id}>
                <button
                  class="session-row-button"
                  type="button"
                  aria-expanded={expanded}
                  onclick={() => toggleExpand(session.session_id)}
                >
                  <span class="session-desc session-id-primary" title={session.description || session.session_id}>
                    {session.description || session.session_id.slice(0, 8)}
                  </span>
                  {#if session.ended_at}
                    <span class="ended-pill">ended</span>
                  {/if}
                  <span class="call-count">
                    {activityLabel(session)}
                  </span>
                </button>

                <div class="session-meta">
                  {#if session.current_task}
                    <span class="meta-task" title={session.current_task}>▶ {session.current_task}</span>
                  {/if}
                  {#if session.branch}
                    <span class="meta-chip" title="branch">⎇ {session.branch}</span>
                  {/if}
                  {#if session.active_files}
                    <span class="meta-chip">{session.active_files} file{session.active_files === 1 ? '' : 's'}</span>
                  {/if}
                  <span class="meta-hash">{session.session_id.slice(0, 8)}</span>
                  {#if heartbeatLabel(session)}
                    <span class="meta-age">{heartbeatLabel(session)}</span>
                  {/if}
                </div>

                {#if collapsed.length === 0}
                  {#if !session.current_task}
                    <p class="row-empty">
                      {session.telemetry_status && session.telemetry_status !== 'real'
                        ? `No tool calls — telemetry: ${session.telemetry_status}`
                        : 'Idle — no tool calls this turn.'}
                    </p>
                  {/if}
                {:else}
                  <ul class="call-list" class:expanded>
                    {#each collapsed as call (call.call_id)}
                      <li class="call-row">
                        <span class={`badge ${callBadgeClass(call)}`} aria-hidden="true"></span>
                        <span class="tool-name" title={formatToolName(call)}>{formatToolName(call)}</span>
                        <span class="dur">{formatDuration(call.duration_ms)}</span>
                        {#if call.error}
                          <span class="err" title={call.error}>{call.error}</span>
                        {:else if call.result_summary}
                          <span class="summary" title={call.result_summary}>{call.result_summary}</span>
                        {:else if call.in_flight}
                          <span class="summary muted">running…</span>
                        {/if}
                      </li>
                    {/each}
                  </ul>
                  {#if !expanded && session.recent_calls.length > 4}
                    <p class="more-hint">…and {session.recent_calls.length - 4} more — click to expand</p>
                  {/if}
                {/if}
              </li>
            {/each}
          </ul>
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .live-sessions-card {
    background: var(--surface-1);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    padding: var(--space-md) var(--space-lg);
  }

  .card-header {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    margin-bottom: var(--space-sm);
  }

  .card-header h3 {
    margin: 0;
    font-size: var(--font-size-lg);
    font-weight: 600;
  }

  .header-meta {
    display: flex;
    gap: var(--space-xs);
  }

  .meta-pill {
    background: var(--surface-2);
    border: 1px solid var(--border-subtle);
    border-radius: 999px;
    padding: 1px 8px;
    font-size: 11px;
    color: var(--text-muted);
  }

  .meta-pill-emphasis {
    border-color: var(--accent-amber);
    color: var(--accent-amber);
  }

  .meta-num {
    color: var(--text-default);
    font-weight: 600;
  }

  .agent-group-list,
  .session-list,
  .call-list {
    list-style: none;
    padding: 0;
    margin: 0;
  }

  /* Strong separator between logical agents; sessions within an agent are
     separated by the lighter .session-row rule below. */
  .agent-group + .agent-group {
    border-top: 1px solid var(--border-subtle);
    padding-top: var(--space-sm);
    margin-top: var(--space-sm);
  }

  .group-header {
    display: flex;
    align-items: center;
    gap: var(--space-xs);
    padding: 2px 0;
  }

  .group-count {
    background: var(--surface-2);
    border: 1px solid var(--border-subtle);
    border-radius: 999px;
    padding: 0 6px;
    font-size: 10px;
    color: var(--text-muted);
  }

  /* Sessions nest under their agent header. */
  .session-list {
    padding-left: 14px;
  }

  .session-row {
    border-top: 1px solid var(--border-subtle);
    padding-top: var(--space-sm);
    margin-top: var(--space-sm);
  }

  .session-row:first-child {
    border-top: none;
    margin-top: 0;
    padding-top: 0;
  }

  .session-id-primary {
    flex: 1 1 auto;
  }

  .session-row-button {
    display: flex;
    gap: var(--space-sm);
    align-items: center;
    width: 100%;
    background: none;
    border: none;
    padding: 4px 0;
    color: inherit;
    font: inherit;
    text-align: left;
    cursor: pointer;
  }

  .session-row-button:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 2px;
  }

  .agent-id {
    display: inline-flex;
    align-items: center;
    gap: var(--space-xs);
    font-weight: 600;
    flex: 1 1 auto;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .session-id {
    font-family: var(--font-mono);
    color: var(--text-muted);
    font-size: 11px;
  }

  .session-desc {
    color: var(--text-default);
    font-size: 12px;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .session-meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-xs);
    padding-left: 0;
    margin-top: 1px;
    font-size: 11px;
    color: var(--text-muted);
    min-width: 0;
  }

  .meta-task {
    color: var(--accent-green);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 60%;
  }

  .meta-chip {
    font-family: var(--font-mono);
    background: var(--surface-2);
    border: 1px solid var(--border-subtle);
    border-radius: 4px;
    padding: 0 5px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 40%;
  }

  .meta-hash {
    font-family: var(--font-mono);
    opacity: 0.7;
  }

  .meta-age {
    margin-left: auto;
    font-variant-numeric: tabular-nums;
  }

  .ended-pill {
    background: var(--surface-2);
    border-radius: 4px;
    padding: 1px 6px;
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
  }

  .call-count {
    color: var(--text-muted);
    font-size: 11px;
  }

  .status-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    display: inline-block;
    background: var(--text-muted);
  }

  .status-dot.status-active {
    background: var(--accent-green);
    box-shadow: 0 0 6px rgba(63, 185, 80, 0.5);
  }

  .status-dot.status-idle {
    background: var(--accent-amber);
  }

  .status-dot.status-offline {
    background: var(--text-muted);
  }

  .status-dot.status-unknown {
    background: var(--surface-3);
  }

  .call-list {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding-left: 16px;
    margin-top: 4px;
    font-size: 12px;
  }

  .call-list.expanded {
    max-height: 320px;
    overflow-y: auto;
  }

  .call-row {
    display: grid;
    grid-template-columns: 8px minmax(0, 1fr) 56px minmax(0, 2fr);
    align-items: center;
    gap: var(--space-xs);
  }

  .badge {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    display: inline-block;
  }

  .badge-ok {
    background: var(--accent-green);
  }

  .badge-error {
    background: var(--accent-red);
  }

  .badge-pending {
    background: var(--accent-blue);
    animation: pulse 1.5s ease-in-out infinite;
  }

  @keyframes pulse {
    0%,
    100% {
      opacity: 0.4;
    }
    50% {
      opacity: 1;
    }
  }

  .tool-name {
    font-family: var(--font-mono);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .dur {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
    text-align: right;
  }

  .summary,
  .err {
    color: var(--text-muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .summary.muted {
    font-style: italic;
  }

  .err {
    color: var(--accent-red);
  }

  .row-empty,
  .more-hint {
    margin: 4px 0 0 0;
    font-size: 11px;
    color: var(--text-muted);
    padding-left: 16px;
  }
</style>
