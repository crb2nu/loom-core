<script lang="ts">
  import type { BadgeVariant } from '../utils/tokens.ts';
  import { timelineStore } from '../stores/timeline.svelte.ts';
  import { formatTime, agentColor, eventIcon, statusVariant } from '../utils/format.ts';
  import Badge from '../widgets/Badge.svelte';
  import EmptyState from './shared/EmptyState.svelte';
  import PanelHeader from './shared/PanelHeader.svelte';
  import VirtualList from '../widgets/VirtualList.svelte';
  import { createStreamScroll } from '../widgets/useStreamScroll.svelte.ts';
  import UnseenAboveChip from '../widgets/UnseenAboveChip.svelte';

  // Timeline rows accumulate per-event from the audit stream; virtualize the
  // render path so a long backlog doesn't pin the renderer. Fixed height
  // covers the common header-row + one detail-chip line (60px row + 4px gap
  // baked in since absolute children can't use flex gap). Rare events with
  // many chips clip via overflow inside the row; the underlying data is
  // unaffected and still searchable through the filter input.
  const TIMELINE_ROW_HEIGHT = 64;

  $effect(() => {
    timelineStore.startPolling(30000);
    return () => {
      timelineStore.stopPolling();
    };
  });

  let entries = $derived(timelineStore.entries ?? []);
  let filter = $state('');

  let filtered = $derived.by(() => {
    if (!filter) return entries;
    const q = filter.toLowerCase();
    return entries.filter(e =>
      e.event_type.toLowerCase().includes(q) ||
      (e.agent_id ?? '').toLowerCase().includes(q) ||
      (e.agent_type ?? '').toLowerCase().includes(q)
    );
  });

  // Shared scroll behavior — snap-to-top on prepend when at top, anchor
  // scrollTop when scrolled down, unseen-count for the "↑ N new events"
  // chip. No pause concept on the timeline; the composable defaults
  // paused to false.
  const scroll = createStreamScroll({
    rowHeight: TIMELINE_ROW_HEIGHT,
    source: () => entries.length,
    visible: () => filtered.length,
  });

  function eventVariant(type: string): BadgeVariant {
    if (type.includes('session.start')) return 'success';
    if (type.includes('session.end') || type.includes('reaped')) return 'error';
    if (type.includes('heartbeat')) return 'info';
    if (type.includes('task')) return 'accent';
    if (type.includes('conflict')) return 'warning';
    if (type.includes('approval')) return 'warning';
    if (type.includes('dispatch')) return 'accent';
    return 'info';
  }

  function shortEventType(type: string) {
    // Remove common prefixes for compact display.
    return type.replace('agent.', '').replace('hud.', '');
  }
</script>

<div class="panel timeline-panel">
  <PanelHeader title="Timeline" icon={'≡'} count={filtered.length}>
    {#snippet actions()}
      <input
        type="text"
        class="panel-search-input"
        placeholder="Filter events..."
        aria-label="Filter events"
        bind:value={filter}
      />
    {/snippet}
  </PanelHeader>

  {#if filtered.length === 0}
    {#if timelineStore.error}
      <!-- A failed /api/timeline fetch previously rendered the same
           "No timeline events" copy as the empty cold-start. -->
      <EmptyState icon={'\u26A0'} heading="Timeline unavailable" description={timelineStore.error} compact />
    {:else}
      <EmptyState icon={'\u23F0'} heading="No timeline events" compact />
    {/if}
  {:else}
    <div class="timeline-list">
      {#if scroll.unseenCount > 0 && !scroll.isAtTop}
        <UnseenAboveChip count={scroll.unseenCount} onClick={scroll.jumpToNewest} singular="event" plural="events" />
      {/if}
      <VirtualList items={filtered} itemHeight={TIMELINE_ROW_HEIGHT} label="Event timeline" bind:containerEl={scroll.containerEl}>
        {#snippet children({ item: entry })}
          <div class="timeline-entry">
            <div class="timeline-time">{formatTime(entry.timestamp)}</div>
            <div class="timeline-icon" style:color={agentColor(entry.agent_type)}>
              {eventIcon(entry.event_type)}
            </div>
            <div class="timeline-body">
              <div class="timeline-header-row">
                <Badge text={shortEventType(entry.event_type)} variant={eventVariant(entry.event_type)} />
                {#if entry.agent_id}
                  <span class="agent-badge" style:color={agentColor(entry.agent_type)}>
                    {entry.agent_id}
                  </span>
                {/if}
              </div>
              {#if entry.data}
                <div class="timeline-detail">
                  {#if entry.data.namespace}
                    <span class="detail-chip">{entry.data.namespace}</span>
                  {/if}
                  {#if entry.data.session_id}
                    <span class="detail-chip text-muted">{String(entry.data.session_id).slice(0, 12)}...</span>
                  {/if}
                  {#if entry.data.title}
                    <span class="detail-chip">{entry.data.title}</span>
                  {/if}
                  {#if entry.data.status}
                    <span class="detail-chip">{entry.data.status}</span>
                  {/if}
                  {#if entry.data.reason}
                    <span class="detail-chip text-muted">{entry.data.reason}</span>
                  {/if}
                  {#if entry.data.branch}
                    <span class="detail-chip text-mono">{entry.data.branch}</span>
                  {/if}
                </div>
              {/if}
            </div>
          </div>
        {/snippet}
      </VirtualList>
    </div>
  {/if}
</div>

<style>
  .timeline-panel {
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  .panel-search-input {
    width: 200px;
    font-size: var(--text-sm);
  }

  /* VirtualList owns the scroll viewport; .timeline-list provides a bounded
     flex slot so VirtualList can compute its own client height. The 4px
     inter-row gap is baked into TIMELINE_ROW_HEIGHT since absolute-positioned
     children can't use flex gap. position: relative anchors the
     UnseenAboveChip overlay. */
  .timeline-list {
    flex: 1;
    min-height: 0;
    overflow: hidden;
    position: relative;
  }

  .timeline-entry {
    box-sizing: border-box;
    height: 60px;
    overflow: hidden;
    display: flex;
    align-items: flex-start;
    gap: var(--space-2);
    padding: 6px var(--space-2);
    border-radius: var(--radius-sm);
  }

  .timeline-time {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-dim);
    white-space: nowrap;
    min-width: 64px;
    padding-top: 1px;
    letter-spacing: var(--tracking-normal);
  }

  .timeline-icon {
    font-size: var(--text-sm);
    min-width: 16px;
    text-align: center;
    padding-top: 2px;
  }

  .timeline-body {
    flex: 1;
    min-width: 0;
  }

  .timeline-header-row {
    display: flex;
    align-items: center;
    gap: 6px;
  }

  .agent-badge {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    font-weight: 500;
  }

  .timeline-detail {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    margin-top: 2px;
  }

  .detail-chip {
    font-size: var(--text-xs);
    padding: 1px 5px;
    background: var(--bg-tertiary);
    border-radius: var(--radius-sm);
    color: var(--fg-secondary);
    border: 1px solid var(--border-subtle);
  }
</style>
