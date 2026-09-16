<script lang="ts">
  /**
   * One operational home for the three services that carry a Mills run from
   * supervision to merge. Each feed retains ownership of its polling and
   * honest loading/error/empty state; collapsing only hides this subtree.
   */
  import PanelShell from '../shared/PanelShell.svelte';
  import OverseersPanel from './OverseersPanel.svelte';
  import MRWatchPanel from '../MRWatchPanel.svelte';
  import { mergeQueueStore } from '../../stores/mergeQueue.svelte.ts';

  let collapsed = $state(false);

  $effect(() => {
    mergeQueueStore.startPolling(60000);
    return () => mergeQueueStore.stopPolling();
  });
</script>

<PanelShell
  title="Mill Staff"
  icon="⚑"
  grouped
  collapsible
  {collapsed}
  contentId="mill-staff-group-content"
  oncollapsedchange={(next) => (collapsed = next)}
>
  <div class="mill-staff-panels" aria-label="Mill Staff status panels">
    <section class="staff-panel" aria-label="Overseer status">
      <OverseersPanel />
    </section>

    <section class="staff-panel" aria-label="MR watch status">
      <MRWatchPanel />
    </section>

    <section class="staff-panel queue-panel" aria-label="Merge queue status">
      <header class="queue-header">
        <div>
          <p class="queue-kicker">Merge queue</p>
          <h3>Ready work and blockers</h3>
        </div>
        <span class="queue-count">{mergeQueueStore.totalCount}</span>
      </header>

      {#if mergeQueueStore.loading && mergeQueueStore.lastUpdated == null}
        <p class="queue-state" role="status">Loading merge queue…</p>
      {:else if mergeQueueStore.error && mergeQueueStore.lastUpdated == null}
        <p class="queue-state queue-error" role="alert">Merge queue unavailable — {mergeQueueStore.error}</p>
      {:else if mergeQueueStore.totalCount === 0}
        <p class="queue-state" role="status">Merge queue is empty.</p>
      {:else}
        {#if mergeQueueStore.error}
          <p class="queue-state queue-error" role="status">Refresh failed; showing the last queue snapshot.</p>
        {/if}
        <dl class="queue-summary">
          <div><dt>ready</dt><dd>{mergeQueueStore.summary.ready_to_merge}</dd></div>
          <div><dt>blocked</dt><dd>{mergeQueueStore.summary.blocked}</dd></div>
          <div><dt>conflicts</dt><dd>{mergeQueueStore.summary.conflict_pairs}</dd></div>
        </dl>
      {/if}
    </section>
  </div>
</PanelShell>

<style>
  .mill-staff-panels { display: flex; flex-direction: column; gap: var(--space-5); min-width: 0; }
  .staff-panel { min-width: 0; }
  .queue-panel { padding: var(--panel-padding); border: 1px solid var(--mills-color-border); border-radius: var(--mills-radius-surface); }
  .queue-header { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
  .queue-kicker { margin: 0; color: var(--mills-color-text-muted); font-size: var(--mills-text-label); text-transform: uppercase; letter-spacing: var(--tracking-wide); }
  h3 { margin: var(--space-1) 0 0; color: var(--mills-color-text); font-size: var(--mills-text-heading); }
  .queue-count { font-family: var(--font-mono); color: var(--mills-color-text-secondary); }
  .queue-state { margin: var(--space-4) 0 0; color: var(--mills-color-text-muted); }
  .queue-error { color: var(--mills-color-error); }
  .queue-summary { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: var(--space-3); margin: var(--space-4) 0 0; }
  .queue-summary div { padding: var(--space-3); background: var(--mills-color-surface-raised); border-radius: var(--mills-radius-state); }
  .queue-summary dt { color: var(--mills-color-text-muted); font-size: var(--mills-text-label); }
  .queue-summary dd { margin: var(--space-1) 0 0; color: var(--mills-color-text); font: 700 var(--mills-text-title)/1 var(--font-mono); }
</style>
