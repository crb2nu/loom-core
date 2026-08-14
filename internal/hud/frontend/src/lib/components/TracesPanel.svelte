<script lang="ts">
  import { router } from '../stores/router.svelte.ts';
  import { traceStore } from '../stores/traces.svelte.ts';
  import { formatTraceDuration, traceBreakdown, traceStatusVariant } from '../utils/traces.ts';
  import EmptyState from './shared/EmptyState.svelte';
  import PanelHeader from './shared/PanelHeader.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';
  import Badge from '../widgets/Badge.svelte';
  import VirtualList from '../widgets/VirtualList.svelte';
  import { createStreamScroll } from '../widgets/useStreamScroll.svelte.ts';
  import UnseenAboveChip from '../widgets/UnseenAboveChip.svelte';
  import { formatTime } from '../utils/format.ts';

  // Audit-driven trace lists are unbounded; virtualize the render path so a
  // backlog of thousands of entries doesn't pin the renderer. Fixed height
  // covers the common two-line trace; long error messages clip with an
  // ellipsis inside the row (the full error is preserved in the title
  // tooltip and remains available in the audit stream).
  const TRACE_ROW_HEIGHT = 104;

  const tracePollingOwner = Symbol('TracesPanel');

  $effect(() => {
    traceStore.startPolling(15000, tracePollingOwner);
    return () => traceStore.stopPolling(tracePollingOwner);
  });

  let query = $state('');
  let statusFilter = $state('all');
  let agentFilter = $derived((router.detail ?? '').trim());

  let entries = $derived(traceStore.entries ?? []);
  let summary = $derived(traceStore.summary ?? {});

  // A filtered agent_id that matches no audit records is often a subagent
  // whose tool calls were logged under the root agent's id (Claude Code's
  // Task tool shares the parent MCP session). We can't infer the parent
  // from the trace stream alone, but we can show a clearer empty-state
  // hint so the user knows the filter, not the data, is the issue.
  let agentHasAnyTrace = $derived(
    !agentFilter || entries.some((entry) => (entry.agent_id ?? '') === agentFilter),
  );

  let filtered = $derived.by(() => {
    let rows = entries;
    if (agentFilter) {
      rows = rows.filter((entry) => (entry.agent_id ?? '') === agentFilter);
    }
    if (statusFilter !== 'all') {
      rows = rows.filter((entry) => {
        if (statusFilter === 'cached') return !!entry.cached;
        return entry.status === statusFilter;
      });
    }
    if (!query.trim()) return rows;
    const q = query.trim().toLowerCase();
    return rows.filter((entry) =>
      entry.server.toLowerCase().includes(q) ||
      entry.tool.toLowerCase().includes(q) ||
      (entry.agent_id ?? '').toLowerCase().includes(q) ||
      (entry.error ?? '').toLowerCase().includes(q),
    );
  });

  const statusChips = [
    { value: 'all', label: 'All' },
    { value: 'error', label: 'Errors' },
    { value: 'denied', label: 'Denied' },
    { value: 'cached', label: 'Cached' },
    { value: 'success', label: 'Success' },
  ];

  // Shared scroll behavior — snap-to-top on prepend when at top, anchor
  // scrollTop when scrolled down, unseen-count for the "↑ N new traces"
  // chip. Traces have no pause concept; the composable defaults paused
  // to false.
  const scroll = createStreamScroll({
    rowHeight: TRACE_ROW_HEIGHT,
    source: () => entries.length,
    visible: () => filtered.length,
  });
</script>

<div class="panel traces-panel">
  <PanelHeader title="Traces" icon={'≡'} count={filtered.length}>
    {#snippet stats()}
      <div class="summary-card">
        <span class="summary-label">P50</span>
        <strong>{formatTraceDuration(summary.p50_ms ?? 0)}</strong>
      </div>
      <div class="summary-card">
        <span class="summary-label">P95</span>
        <strong>{formatTraceDuration(summary.p95_ms ?? 0)}</strong>
      </div>
      <div class="summary-card">
        <span class="summary-label">Errors</span>
        <strong>{summary.errors ?? 0}</strong>
      </div>
      <div class="summary-card">
        <span class="summary-label">Slowest</span>
        <strong>{formatTraceDuration(summary.slowest_ms ?? 0)}</strong>
      </div>
    {/snippet}
    {#snippet actions()}
      {#if agentFilter}
        <button class="filter-chip active" onclick={() => router.navigate('activity', 'traces')}>
          Agent: {agentFilter} ×
        </button>
      {/if}
      <input
        type="text"
        class="panel-search-input"
        placeholder="Filter traces..."
        aria-label="Search traces"
        bind:value={query}
        data-panel-search="primary"
      />
    {/snippet}
  </PanelHeader>

  <div class="traces-toolbar">
    <div class="chip-row" role="group" aria-label="Status filter">
      {#each statusChips as chip}
        <button
          class="filter-chip"
          class:active={statusFilter === chip.value}
          aria-pressed={statusFilter === chip.value}
          onclick={() => { statusFilter = chip.value; }}
        >
          {chip.label}
        </button>
      {/each}
    </div>
  </div>

  {#if !traceStore.enabled}
    <EmptyState icon={'\u25A6'} heading="Trace stream unavailable" description="Enable daemon audit logging to populate recent tool-call traces." />
  {:else if traceStore.error && entries.length === 0}
    <!-- A failed /api/traces poll with no data yet previously rendered the
         same "No traces matched" copy as an empty cold-start. -->
    <EmptyState icon={'\u26A0'} heading="Traces unavailable" description={traceStore.error} compact />
  {:else if filtered.length === 0}
    {#if traceStore.error}
      <!-- Poll failure with stale (filtered-out) rows still cached. -->
      <ErrorBanner prefix="Trace refresh failed" message={traceStore.error} />
    {/if}
    {#if agentFilter && !agentHasAnyTrace}
      <EmptyState
        icon={'\u25A6'}
        heading="No traces matched for this agent"
        description="Subagents (e.g. Claude Code Task-tool children) share their parent's MCP session, so their tool calls are recorded under the root agent's id. Try the Traces button on the parent row, or remove the filter to see the full stream."
      />
    {:else}
      <EmptyState icon={'\u25A6'} heading="No traces matched" compact />
    {/if}
  {:else}
    {#if traceStore.error}
      <!-- Poll failure with stale rows still on screen. -->
      <ErrorBanner prefix="Trace refresh failed" message={traceStore.error} />
    {/if}
    <div class="trace-list">
      {#if scroll.unseenCount > 0 && !scroll.isAtTop}
        <UnseenAboveChip count={scroll.unseenCount} onClick={scroll.jumpToNewest} singular="trace" plural="traces" />
      {/if}
      <VirtualList items={filtered} itemHeight={TRACE_ROW_HEIGHT} label="Tool traces" bind:containerEl={scroll.containerEl}>
        {#snippet children({ item: entry })}
          <div class="trace-row">
            <div class="trace-row-top">
              <div class="trace-id">
                <span class="trace-time">{formatTime(entry.timestamp)}</span>
                <span class="trace-server">{entry.server}</span>
                <span class="trace-tool">{entry.tool}</span>
              </div>
              <div class="trace-badges">
                <span class="trace-duration">{formatTraceDuration(entry.duration_ms)}</span>
                <Badge text={entry.status} variant={traceStatusVariant(entry.status)} />
                {#if entry.cached}
                  <Badge text="cached" variant="info" />
                {/if}
                {#if entry.target}
                  <Badge text={entry.target} variant="accent" />
                {/if}
              </div>
            </div>
            <div class="trace-row-meta">
              {#if entry.agent_id}
                <span class="meta-chip">{entry.agent_id}</span>
              {/if}
              {#if entry.pipeline_stage}
                <span class="meta-chip">{entry.pipeline_stage}</span>
              {/if}
              {#if traceBreakdown(entry)}
                <span class="meta-chip breakdown">{traceBreakdown(entry)}</span>
              {/if}
            </div>
            {#if entry.error}
              <div class="trace-error" title={entry.error}>{entry.error}</div>
            {/if}
          </div>
        {/snippet}
      </VirtualList>
    </div>
  {/if}

  {#if traceStore.path}
    <div class="trace-footer">Source: {traceStore.path}</div>
  {/if}
</div>

<style>
  .traces-panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-height: 0;
  }

  .summary-card {
    min-width: 88px;
    padding: 10px 12px;
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--bg-secondary) 86%, transparent);
  }

  .summary-label {
    display: block;
    color: var(--fg-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    margin-bottom: 4px;
  }

  .traces-toolbar {
    display: flex;
    gap: var(--space-2);
    align-items: center;
    flex-wrap: wrap;
  }

  .chip-row {
    display: flex;
    gap: var(--space-1);
    flex-wrap: wrap;
  }

  .filter-chip {
    padding: 6px 10px;
    border-radius: var(--radius-full);
    border: 1px solid var(--border-subtle);
    background: var(--bg-secondary);
    color: var(--fg-muted);
    font-size: var(--text-xs);
  }

  .filter-chip.active {
    color: var(--fg-primary);
    border-color: color-mix(in srgb, var(--info) 30%, var(--border));
    background: color-mix(in srgb, var(--info) 10%, var(--bg-tertiary));
  }

  .filter-chip:focus-visible {
    outline: 2px solid var(--info);
    outline-offset: 2px;
    border-radius: var(--radius-sm);
  }

  /* VirtualList owns the scroll viewport; .trace-list just provides a
     bounded flex slot inside .traces-panel so VirtualList can compute its
     own client height. The inter-row gap is baked into TRACE_ROW_HEIGHT
     (96px row + 8px gap) since absolute-positioned children can't use
     flex gap. position: relative anchors the UnseenAboveChip overlay. */
  .trace-list {
    flex: 1;
    min-height: 0;
    overflow: hidden;
    position: relative;
  }

  .trace-row {
    box-sizing: border-box;
    height: 96px;
    overflow: hidden;
    padding: 12px;
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--bg-secondary) 88%, transparent);
  }

  .trace-row-top {
    display: flex;
    justify-content: space-between;
    gap: var(--space-2);
    flex-wrap: wrap;
    align-items: center;
  }

  .trace-id,
  .trace-badges,
  .trace-row-meta {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
    align-items: center;
  }

  .trace-time,
  .trace-duration,
  .trace-server,
  .meta-chip,
  .trace-footer {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .trace-tool {
    font-size: var(--text-sm);
    font-weight: 600;
  }

  .trace-server {
    color: var(--fg-secondary);
  }

  .trace-row-meta {
    margin-top: 8px;
  }

  .meta-chip {
    padding: 2px 6px;
    border-radius: var(--radius-sm);
    background: var(--bg-tertiary);
    border: 1px solid var(--border-subtle);
    color: var(--fg-secondary);
  }

  .meta-chip.breakdown {
    white-space: normal;
  }

  .trace-error {
    margin-top: 8px;
    color: var(--error);
    font-size: var(--text-sm);
    /* Single-line clamp so a long error doesn't blow past the fixed row
       height; full text is kept in the title attribute and the audit log. */
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .trace-footer {
    color: var(--fg-dim);
  }
</style>
