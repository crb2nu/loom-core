<script>
  import { streamStore } from '../stores/stream.svelte.ts';
  import { formatTime, entryVariant } from '../utils/format.ts';
  import Badge from '../widgets/Badge.svelte';
  import SparkLine from '../widgets/SparkLine.svelte';
  import EmptyState from './shared/EmptyState.svelte';
  import VirtualList from '../widgets/VirtualList.svelte';
  import { createStreamScroll } from '../widgets/useStreamScroll.svelte.ts';
  import UnseenAboveChip from '../widgets/UnseenAboveChip.svelte';

  // Stream rows are bounded at 500 entries server-side but accumulate over
  // the session via SSE pushes (`hud.stream` every 5s); virtualize the render
  // path so the long-tail of older entries doesn't sit in the DOM. Single-line
  // rows fit a fixed height cleanly; 40px is enough for the padded line on
  // desktop and stays close to the prior mobile touch-target minimum (44px).
  const STREAM_ROW_HEIGHT = 40;

  // Slice B3 — polling is just a safety-net fallback for SSE disconnects.
  $effect(() => {
    streamStore.startPolling(60000);
    return () => { streamStore.stopPolling(); };
  });

  let entries = $derived(streamStore.entries ?? []);
  let paused = $derived(streamStore.paused ?? false);

  let typeFilter = $state('all');
  let agentFilter = $state('all');

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

  // Scroll behavior: snap-to-top on real prepend (when at top + not
  // paused), anchor scrollTop on prepend when scrolled down, accumulate
  // unseen count for the "↑ N new entries" chip. See createStreamScroll
  // for the full pattern — TimelinePanel and TracesPanel share it.
  const scroll = createStreamScroll({
    rowHeight: STREAM_ROW_HEIGHT,
    source: () => entries.length,
    visible: () => filtered.length,
    paused: () => paused,
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

    {#if scroll.unseenCount > 0 && !scroll.isAtTop}
      <UnseenAboveChip count={scroll.unseenCount} onClick={scroll.jumpToNewest} />
    {/if}

    {#if filtered.length === 0}
      {#if streamStore.error}
        <!-- A failed /stream/recent fetch previously rendered the same
             "No activity yet" copy as an empty cold-start, hiding the
             reason from the operator. -->
        <EmptyState icon={'\u26A0'} heading="Stream unavailable" description={streamStore.error} />
      {:else}
        <EmptyState icon={'\u25C9'} heading="No activity yet" description="Context entries will appear here in real-time" />
      {/if}
    {:else}
      <VirtualList items={filtered} itemHeight={STREAM_ROW_HEIGHT} bind:containerEl={scroll.containerEl}>
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

  .filter-chip:focus-visible {
    outline: 2px solid var(--info);
    outline-offset: 2px;
    border-radius: var(--radius-sm);
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
