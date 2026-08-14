<script lang="ts">
  // BlockedSessionsCard — "Waiting on you" on the Overview ("Now") view.
  //
  // Reads GET /api/blocked: the sessions the flightdeck bridge classified as
  // stalled on a human (a permission prompt), longest wait first. This is the
  // one HUD signal whose subject is the operator themselves, and it had no
  // desktop surface at all — the route shipped, the mobile dashboard consumed
  // it, and the HUD rendered nothing.
  //
  // Card chrome mirrors RecoverySLOCard so it sits in the Overview stack
  // without introducing a second card idiom.

  import { blockedStore } from '../../stores/blocked.svelte.ts';
  import { router } from '../../stores/router.svelte.ts';
  import { fleetStore } from '../../stores/fleet.svelte.ts';
  import Badge from '../../widgets/Badge.svelte';
  import EmptyState from '../shared/EmptyState.svelte';
  import { truncateId, truncatePath } from '../../utils/format.ts';

  $effect(() => {
    blockedStore.startPolling(15000);
    return () => blockedStore.stopPolling();
  });

  let sessions = $derived(blockedStore.sessions);
  let unavailable = $derived(blockedStore.unavailable);
  let error = $derived(blockedStore.error);
  let longest = $derived(blockedStore.longestWaitSeconds);

  // A wait of a few seconds is a prompt in flight; minutes mean an agent has
  // been parked. The badge escalates so the card reads at a glance.
  function waitLabel(seconds: number): string {
    const s = Math.max(0, Math.floor(seconds ?? 0));
    if (s < 60) return `${s}s`;
    if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
    return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  }

  function waitSeverity(seconds: number): 'info' | 'warning' | 'error' {
    if (seconds >= 600) return 'error';
    if (seconds >= 120) return 'warning';
    return 'info';
  }

  // Jump to the blocked agent's live session if the fleet knows one, else the
  // Fleet roster — the same drill-down the attention lanes use.
  function openSession(agentID: string): void {
    const session = fleetStore.sessionForAgent(agentID);
    router.navigate('agents', 'fleet', session?.id ?? null);
  }
</script>

<section class="blocked-card" class:has-blocked={sessions.length > 0}>
  <header class="card-header">
    <h3>Waiting on you</h3>
    {#if sessions.length > 0}
      <Badge
        text={`${sessions.length} blocked · ${waitLabel(longest)} longest`}
        variant={waitSeverity(longest)}
      />
    {/if}
  </header>

  {#if unavailable}
    <div class="status-line">
      Blocked-session tracking isn't available on this HUD build
      (<code>GET /api/blocked</code>).
    </div>
  {:else if error && sessions.length === 0}
    <div class="error-banner" role="alert">Couldn't load blocked sessions: {error}</div>
  {:else if sessions.length === 0}
    <EmptyState
      icon="✓"
      message="No agent is waiting on you"
      description="Sessions stalled on a permission prompt show up here, longest wait first."
      compact={true}
    />
  {:else}
    {#if error}
      <div class="error-banner" role="alert">Refresh failed: {error}</div>
    {/if}
    <ul class="blocked-list">
      {#each sessions as s (s.session_id)}
        <li class="blocked-row">
          <button
            type="button"
            class="row-main"
            onclick={() => openSession(s.agent_id)}
            title={`Open ${s.agent_id}`}
          >
            <span class="agent">{s.agent_id || 'unknown agent'}</span>
            <span class="reason">
              <!-- The separator lives inside .tool with a CSS margin: Svelte
                   trims leading whitespace in element content, so a literal
                   " · " prefix renders as "permission· Edit". -->
              {s.reason}{#if s.tool_name}<span class="tool">· {s.tool_name}</span>{/if}
            </span>
            {#if s.cwd}
              <span class="cwd">{truncatePath(s.cwd, 44)}</span>
            {/if}
          </button>
          <span class="meta">
            <span class="wait wait-{waitSeverity(s.waited_seconds)}">{waitLabel(s.waited_seconds)}</span>
            <span class="sid">{truncateId(s.session_id)}</span>
          </span>
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .blocked-card {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    position: relative;
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  /* A card that is quiet most of the time must be unmissable when it isn't. */
  .blocked-card.has-blocked {
    border-color: color-mix(in srgb, var(--warning) 45%, var(--border));
  }

  .blocked-card::before {
    content: '';
    position: absolute;
    inset: 0;
    border-radius: inherit;
    background: var(--surface-highlight);
    pointer-events: none;
  }

  .card-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .card-header h3 {
    margin: 0;
    font-size: var(--text-sm);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }

  .status-line {
    color: var(--fg-muted);
    font-size: var(--text-sm);
    padding: var(--space-2) 0;
  }

  .status-line code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    background: var(--bg-tertiary);
    padding: 0 4px;
    border-radius: var(--radius-xs);
  }

  .error-banner {
    background: var(--error-dim);
    color: var(--error);
    border: 1px solid color-mix(in srgb, var(--error) 25%, transparent);
    border-radius: var(--radius-md);
    padding: var(--space-2);
    font-size: var(--text-sm);
  }

  .blocked-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .blocked-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-1) 0;
    border-top: 1px solid var(--border-subtle);
  }

  .blocked-row:first-child {
    border-top: none;
  }

  .row-main {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 1px;
    flex: 1;
    min-width: 0;
    background: transparent;
    border: none;
    padding: 0;
    font: inherit;
    text-align: left;
    cursor: pointer;
    color: inherit;
  }

  .row-main:hover .agent {
    color: var(--accent);
  }

  .row-main:focus-visible {
    outline: 2px solid var(--info);
    outline-offset: 2px;
    border-radius: var(--radius-xs);
  }

  .agent {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--fg-primary);
    max-width: 100%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .reason {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
  }

  .tool {
    font-family: var(--font-mono);
    color: var(--fg-dim);
    margin-left: 0.35em;
  }

  .cwd {
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--fg-dim);
  }

  .meta {
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 1px;
    flex-shrink: 0;
  }

  .wait {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    font-weight: 600;
  }

  .wait-info { color: var(--info); }
  .wait-warning { color: var(--warning); }
  .wait-error { color: var(--error); }

  .sid {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-dim);
  }
</style>
