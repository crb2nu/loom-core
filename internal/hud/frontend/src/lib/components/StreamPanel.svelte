<script>
  import { streamStore } from '../stores/stream.svelte.ts';
  import { formatTime, entryVariant } from '../utils/format.ts';
  import Badge from '../widgets/Badge.svelte';
  import SparkLine from '../widgets/SparkLine.svelte';
  import EmptyState from './shared/EmptyState.svelte';
  import VirtualList from '../widgets/VirtualList.svelte';

  // Stream rows are bounded at 500 entries server-side but still poll every
  // 2s and accumulate over the session; virtualize the render path so the
  // long-tail of older entries doesn't sit in the DOM. Single-line rows fit
  // a fixed height cleanly; 40px is enough for the padded line on desktop
  // and stays close to the prior mobile touch-target minimum (44px).
  const STREAM_ROW_HEIGHT = 40;

  $effect(() => {
    streamStore.startPolling(2000);
    return () => { streamStore.stopPolling(); };
  });

  let entries = $derived(streamStore.entries ?? []);
  let paused = $derived(streamStore.paused ?? false);

  let typeFilter = $state('all');
  let agentFilter = $state('all');
  let streamEl = $state(null);

  // Scroll-aware auto-pause: snap-to-top should only fire when the user is
  // already near the top. If they've scrolled down to read history, the
  // 2s poll cadence would otherwise yank them back to the newest entry on
  // every prepend (the previous behavior, escapable only via the explicit
  // pause toggle). Tolerance is half a row so brief overshoot still counts
  // as "at top".
  const STREAM_SCROLL_TOP_TOLERANCE_PX = STREAM_ROW_HEIGHT / 2;
  let isAtTop = $state(true);

  $effect(() => {
    if (!streamEl) return;
    const handler = () => {
      isAtTop = streamEl.scrollTop < STREAM_SCROLL_TOP_TOLERANCE_PX;
    };
    streamEl.addEventListener('scroll', handler, { passive: true });
    return () => streamEl.removeEventListener('scroll', handler);
  });

  // Unseen-prepend indicator: when the user is scrolled down and new
  // entries arrive, !478 anchors the visible rows in place — which is
  // correct but leaves the user with no signal that newer entries have
  // accumulated above. Track that count here so the panel can offer a
  // single-click "scroll to top" affordance, and reset it whenever they
  // return to the top.
  let unseenCount = $state(0);
  $effect(() => {
    if (isAtTop) unseenCount = 0;
  });

  function jumpToNewest() {
    if (streamEl) streamEl.scrollTop = 0;
  }

  const entryTypes = ['all', 'decision', 'finding', 'error', 'task', 'file_read', 'note'];

  let agents = $derived.by(() => {
    const set = new Set();
    entries.forEach(e => { if (e.agent) set.add(e.agent); });
    return ['all', ...Array.from(set).sort()];
  });

  let filtered = $derived.by(() => {
    let result = entries;

    if (typeFilter !== 'all') {
      result = result.filter(e => e.entry_type === typeFilter);
    }

    if (agentFilter !== 'all') {
      result = result.filter(e => e.agent === agentFilter);
    }

    return result;
  });

  // React to new entries arriving from the poll. Gate on entries.length
  // (not filtered.length) so changes to typeFilter/agentFilter don't get
  // misread as a prepend. When entries actually grow:
  //   - if the user is near the top and not paused, snap to top so the
  //     newest entry stays visible (the previous behavior);
  //   - if the user has scrolled down to read history, anchor their
  //     visible items by compensating scrollTop by the number of newly
  //     visible prepended rows. Without this, prepends shift the items
  //     under the user's viewport and they re-read content they were
  //     already past.
  let prevEntriesLen = 0;
  let prevFilteredLen = 0;
  $effect(() => {
    const entriesLen = entries.length;
    const filteredLen = filtered.length;
    const entriesDelta = entriesLen - prevEntriesLen;
    const filteredDelta = filteredLen - prevFilteredLen;
    prevEntriesLen = entriesLen;
    prevFilteredLen = filteredLen;

    if (entriesDelta <= 0 || paused || !streamEl) return;

    if (isAtTop) {
      streamEl.scrollTop = 0;
    } else if (filteredDelta > 0) {
      streamEl.scrollTop += filteredDelta * STREAM_ROW_HEIGHT;
      unseenCount += filteredDelta;
    }
  });

  function setTypeFilter(type) {
    typeFilter = type;
    streamStore.filterType = type;
  }

  function setAgentFilter(agent) {
    agentFilter = agent;
    streamStore.filterAgent = agent;
  }

  // Event density: bucket entries into 12 time slices for sparkline
  let densityData = $derived.by(() => {
    if (entries.length < 2) return [];
    const times = entries.map(e => new Date(e.timestamp).getTime()).filter(t => !isNaN(t));
    if (times.length < 2) return [];
    const min = Math.min(...times);
    const max = Math.max(...times);
    const span = max - min || 1;
    const buckets = new Array(12).fill(0);
    for (const t of times) {
      const idx = Math.min(11, Math.floor(((t - min) / span) * 12));
      buckets[idx]++;
    }
    return buckets;
  });

  // Entry type distribution counts
  let typeCounts = $derived.by(() => {
    const counts = {};
    for (const e of entries) {
      const t = e.entry_type ?? 'note';
      counts[t] = (counts[t] || 0) + 1;
    }
    return counts;
  });

  function typeBorderColor(type) {
    const map = {
      decision: 'var(--accent)',
      finding: 'var(--info)',
      error: 'var(--error)',
      task: 'var(--warning)',
      file_read: 'var(--info)',
      note: 'var(--success)',
    };
    return map[type] ?? 'var(--border)';
  }
</script>

<div class="panel stream-panel">
  <!-- Header: Filters -->
  <div class="stream-header">
    <div class="filter-pills">
      {#each entryTypes as type}
        <button
          class="filter-chip"
          class:active={typeFilter === type}
          onclick={() => setTypeFilter(type)}
        >
          {type === 'all' ? 'All' : type.replace('_', ' ')}
        </button>
      {/each}
    </div>
    <div class="header-controls">
      <select
        class="agent-select"
        value={agentFilter}
        onchange={(e) => setAgentFilter(e.target.value)}
      >
        {#each agents as agent}
          <option value={agent}>{agent === 'all' ? 'All Agents' : agent}</option>
        {/each}
      </select>
      <button
        class="btn pause-btn"
        class:paused-btn={paused}
        onclick={() => streamStore.togglePause()}
      >
        {#if paused}
          <span class="pause-icon">&#9654;</span> Resume
        {:else}
          <span class="pause-icon">&#9208;</span> Pause
        {/if}
      </button>
    </div>
  </div>

  <!-- Density strip -->
  {#if densityData.length > 0 || Object.keys(typeCounts).length > 0}
    <div class="density-strip">
      {#if densityData.length > 0}
        <SparkLine data={densityData} width={140} height={20} color="var(--accent)" />
      {/if}
      {#if Object.keys(typeCounts).length > 0}
        <div class="type-dist">
          {#each Object.entries(typeCounts).sort((a, b) => b[1] - a[1]) as [type, count]}
            <span class="type-dist-item" style:border-color={typeBorderColor(type)}>
              {type.replace('_', ' ')}: {count}
            </span>
          {/each}
        </div>
      {/if}
    </div>
  {/if}

  <!-- Stream area -->
  <div class="stream-container">
    {#if paused}
      <div class="paused-overlay">
        <span class="paused-text">PAUSED</span>
      </div>
    {/if}

    {#if unseenCount > 0 && !isAtTop}
      <button type="button" class="unseen-indicator" onclick={jumpToNewest}>
        ↑ {unseenCount} new {unseenCount === 1 ? 'entry' : 'entries'}
      </button>
    {/if}

    {#if filtered.length === 0}
      <EmptyState icon={'\u25C9'} heading="No activity yet" description="Context entries will appear here in real-time" />
    {:else}
      <VirtualList items={filtered} itemHeight={STREAM_ROW_HEIGHT} bind:containerEl={streamEl}>
        {#snippet children({ item: entry, index })}
          <div
            class="stream-row"
            class:alt-row={index % 2 === 1}
            style="border-left: 3px solid {typeBorderColor(entry.entry_type)}"
          >
            <span class="stream-time text-mono">{formatTime(entry.timestamp)}</span>
            <Badge text={entry.entry_type ?? 'note'} variant={entryVariant(entry.entry_type)} />
            <span class="stream-agent">
              <Badge text={entry.agent ?? '---'} variant="info" />
            </span>
            <span class="stream-ns text-mono text-muted">{entry.namespace ?? ''}</span>
            <span class="stream-title truncate">{entry.title ?? entry.content?.slice(0, 80) ?? '---'}</span>
          </div>
        {/snippet}
      </VirtualList>
    {/if}
  </div>
</div>

<style>
  .stream-panel {
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  .stream-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: var(--space-2) 0;
    border-bottom: 1px solid var(--border);
    gap: var(--space-3);
    flex-wrap: wrap;
  }

  .density-strip {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    padding: var(--space-1) 0;
    border-bottom: 1px solid var(--border-subtle);
    flex-wrap: wrap;
  }

  .type-dist {
    display: flex;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .type-dist-item {
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--fg-muted);
    border-left: 2px solid;
    padding-left: 6px;
    letter-spacing: var(--tracking-normal);
  }

  .filter-pills {
    display: flex;
    gap: 4px;
    flex-wrap: wrap;
  }

  .header-controls {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .agent-select {
    min-width: 120px;
  }

  .pause-btn {
    padding: 4px var(--space-3);
    background: var(--bg-tertiary);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    font-weight: 500;
    display: flex;
    align-items: center;
    gap: 4px;
    transition: background var(--transition-fast), color var(--transition-fast), box-shadow var(--transition-fast);
    letter-spacing: var(--tracking-normal);
  }

  .pause-btn:hover {
    background: var(--bg-elevated);
    color: var(--fg-primary);
  }

  .paused-btn {
    background: var(--warning-dim);
    color: var(--warning);
    border: 1px solid rgba(255, 184, 48, 0.3);
    box-shadow: 0 0 8px var(--glow-warning);
  }

  .pause-icon {
    font-size: 10px;
  }

  /* Stream container — bounded flex column. VirtualList owns the scroll
     viewport; .stream-container just provides a fixed slot underneath the
     paused overlay so VirtualList can compute its own client height. */
  .stream-container {
    flex: 1;
    min-height: 0;
    overflow: hidden;
    position: relative;
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    margin-top: var(--space-2);
    display: flex;
    flex-direction: column;
  }

  .paused-overlay {
    position: sticky;
    top: 0;
    z-index: 10;
    display: flex;
    justify-content: center;
    padding: 4px;
    background: var(--warning-dim);
    border-bottom: 1px solid rgba(255, 184, 48, 0.3);
    backdrop-filter: blur(4px);
  }

  .paused-text {
    font-family: var(--font-mono);
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 3px;
    color: var(--warning);
    animation: glowPulse 2s ease-in-out infinite;
  }

  /* Floating "N new entries above" chip that appears when the user is
     scrolled down and prepends have accumulated since they left the top.
     Positioned absolute so it doesn't reflow the VirtualList beneath. */
  .unseen-indicator {
    position: absolute;
    top: var(--space-2);
    left: 50%;
    transform: translateX(-50%);
    z-index: 20;
    padding: 4px var(--space-3);
    background: color-mix(in srgb, var(--info) 22%, var(--bg-secondary));
    border: 1px solid var(--info);
    border-radius: var(--radius-full);
    color: var(--fg-primary);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    font-weight: 600;
    cursor: pointer;
    box-shadow: 0 2px 8px rgba(0, 0, 0, 0.4);
    transition: background var(--transition-fast), transform var(--transition-fast);
  }

  .unseen-indicator:hover {
    background: color-mix(in srgb, var(--info) 40%, var(--bg-secondary));
    transform: translateX(-50%) translateY(-1px);
  }

  .unseen-indicator:focus-visible {
    outline: 2px solid var(--border-focus);
    outline-offset: 2px;
  }

  .stream-row {
    box-sizing: border-box;
    height: 40px;
    overflow: hidden;
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: 6px var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
    font-size: var(--text-sm);
    transition: background var(--transition-fast);
    position: relative;
  }

  .stream-row:hover {
    background: var(--bg-tertiary);
  }

  .stream-row:last-child {
    border-bottom: none;
  }

  .alt-row {
    background: rgba(6, 12, 16, 0.4);
  }

  .stream-time {
    color: var(--fg-dim);
    font-size: var(--text-xs);
    flex-shrink: 0;
    width: 65px;
    letter-spacing: var(--tracking-normal);
  }

  .stream-agent {
    flex-shrink: 0;
  }

  .stream-ns {
    font-size: var(--text-xs);
    flex-shrink: 0;
    max-width: 120px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .stream-title {
    color: var(--fg-primary);
    flex: 1;
    min-width: 0;
  }

  @media (max-width: 768px) {
    .stream-header {
      flex-direction: column;
      align-items: flex-start;
    }
    .stream-ns {
      display: none;
    }
    /* .stream-row uses an explicit 40px height for VirtualList; no mobile
       min-height override — virtualization requires a single row height. */
  }
</style>
