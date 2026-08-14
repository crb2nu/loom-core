<script lang="ts">
  /**
   * ServersHeader — panel identity row with running/idle/degraded/down +
   * tools counts. Reads healthStore directly. The status counts double as
   * clickable filter pills (toggle: clicking the active one clears the
   * filter), matching the TasksPanel header-pill pattern.
   */
  import { healthStore } from '../../stores/health.svelte.ts';
  import PanelHeader from '../shared/PanelHeader.svelte';

  let servers = $derived(healthStore.servers ?? []);
  let healthyCt = $derived(healthStore.healthyCount);
  let idleCt = $derived(healthStore.idleCount);
  let degradedCt = $derived(healthStore.degradedCount);
  let downCt = $derived(healthStore.downCount);
  let totalTools = $derived(servers.reduce((sum, s) => sum + (s.tool_count ?? 0), 0));

  function toggleStatusFilter(status: string) {
    healthStore.setStatusFilter(healthStore.statusFilter === status ? '' : status);
  }
</script>

<PanelHeader title="Servers" icon={'♥'} count={servers.length}>
  {#snippet stats()}
    <button
      class="pill-btn"
      class:pill-active={healthStore.statusFilter === 'healthy'}
      aria-pressed={healthStore.statusFilter === 'healthy'}
      onclick={() => toggleStatusFilter('healthy')}
      title="Filter to running"
    >
      <span class="header-stat healthy-stat">
        <span class="dot dot-healthy"></span>
        {healthyCt} running
      </span>
    </button>
    <button
      class="pill-btn"
      class:pill-active={healthStore.statusFilter === 'idle'}
      aria-pressed={healthStore.statusFilter === 'idle'}
      onclick={() => toggleStatusFilter('idle')}
      title="Filter to idle"
    >
      <span class="header-stat idle-stat">
        <span class="dot dot-idle"></span>
        {idleCt} idle
      </span>
    </button>
    {#if degradedCt > 0 || healthStore.statusFilter === 'degraded'}
      <button
        class="pill-btn"
        class:pill-active={healthStore.statusFilter === 'degraded'}
        aria-pressed={healthStore.statusFilter === 'degraded'}
        onclick={() => toggleStatusFilter('degraded')}
        title="Filter to degraded"
      >
        <span class="header-stat degraded-stat">
          <span class="dot dot-degraded"></span>
          {degradedCt} degraded
        </span>
      </button>
    {/if}
    {#if downCt > 0 || healthStore.statusFilter === 'down'}
      <button
        class="pill-btn"
        class:pill-active={healthStore.statusFilter === 'down'}
        aria-pressed={healthStore.statusFilter === 'down'}
        onclick={() => toggleStatusFilter('down')}
        title="Filter to down"
      >
        <span class="header-stat down-stat">
          <span class="dot dot-down"></span>
          {downCt} down
        </span>
      </button>
    {/if}
    <span class="header-stat tools-stat">
      <span class="tools-icon">{'⚙'}</span>
      {totalTools} tools
    </span>
  {/snippet}
</PanelHeader>

<style>
  .pill-btn {
    background: none;
    border: none;
    padding: 0;
    cursor: pointer;
    border-radius: var(--radius-full);
    transition: filter var(--transition-fast);
  }

  .pill-btn:hover {
    filter: brightness(1.25);
  }

  .pill-btn:focus-visible {
    outline: 2px solid color-mix(in srgb, var(--info) 55%, transparent);
    outline-offset: 2px;
  }

  .pill-btn.pill-active {
    outline: 2px solid var(--accent);
    outline-offset: 1px;
    border-radius: var(--radius-sm);
  }

  .header-stat {
    display: flex;
    align-items: center;
    gap: 4px;
    color: var(--fg-secondary);
    font-size: var(--text-sm);
  }

  .dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
  }

  .dot-healthy { background: var(--success); box-shadow: var(--glow-shadow-sm) var(--glow-success); }
  .dot-idle { background: var(--fg-muted); }
  .dot-degraded { background: var(--warning); box-shadow: var(--glow-shadow-sm) var(--glow-warning); }
  .dot-down { background: var(--error); box-shadow: var(--glow-shadow-sm) var(--glow-error); }

  .tools-icon {
    font-size: 11px;
    opacity: 0.7;
  }
</style>
