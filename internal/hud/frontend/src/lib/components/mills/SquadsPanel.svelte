<script lang="ts">
  import { millsSquadsStore } from '../../stores/mills_squads.svelte.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import MetricCard from '../shared/MetricCard.svelte';
  import { fmtCost, fmtPct } from './shared/format.ts';

  $effect(() => {
    millsSquadsStore.startPolling(15000);
    return () => { millsSquadsStore.stopPolling(); };
  });

  let entries = $derived(millsSquadsStore.state);
  let loading = $derived(millsSquadsStore.loading && entries.length === 0);
  let disabled = $derived(millsSquadsStore.disabled);
  let error = $derived(millsSquadsStore.error);
  let details = $derived(millsSquadsStore.details);

  // expanded squad name → fetch detail (memory + outcomes) on demand. The
  // detail call is fired once per expand, not on every poll, so the table
  // doesn't churn while a card is open.
  let expanded = $state<string | null>(null);

  function toggle(name: string): void {
    if (expanded === name) {
      expanded = null;
      return;
    }
    expanded = name;
    if (!details[name]) {
      void millsSquadsStore.fetchDetail(name);
    }
  }

  function successColor(rate: number, total: number): string {
    if (total === 0) return 'var(--fg-muted)';
    if (rate >= 0.75) return 'var(--success)';
    if (rate >= 0.5) return 'var(--warning)';
    return 'var(--error)';
  }
</script>

<PanelShell
  title="Drawing-in — Squads"
  icon="◈"
  count={entries.length}
  loading={loading}
  error={!disabled && error && entries.length === 0 ? error : null}
  errorHeading="Couldn't load squads"
  empty={!error && entries.length === 0}
  emptyIcon={disabled ? '◯' : '□'}
  emptyMessage={disabled ? 'Mills operator not configured' : 'No squads loaded yet'}
  emptyHint={disabled
    ? 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.'
    : 'Squads load from platform/gitops/k3s/mills/squads/*.yaml on operator boot.'}
  emptyTone={disabled ? 'disabled' : 'idle'}
>
  {#if error && entries.length > 0}
    <ErrorBanner prefix="Squads refresh failed" message={error} />
  {/if}

  <div class="squad-grid">
    {#each entries as entry (entry.squad.Name)}
      {@const stats = entry.outcome_stats}
      {@const detail = details[entry.squad.Name]}
      <article
        class="squad-card"
        class:expanded={expanded === entry.squad.Name}
        class:disabled={entry.squad.Enabled === false}
      >
        <header class="squad-header">
          <button
            type="button"
            class="squad-name-btn"
            onclick={() => toggle(entry.squad.Name)}
            aria-expanded={expanded === entry.squad.Name}
          >
            <span class="squad-toggle">{expanded === entry.squad.Name ? '▾' : '▸'}</span>
            <span class="squad-name">{entry.squad.Name}</span>
          </button>
          {#if entry.squad.Enabled === false}
            <span class="squad-badge badge-disabled">disabled</span>
          {/if}
          {#if entry.squad.RecursionEnabled}
            <span class="squad-badge badge-recursion">recursion</span>
          {/if}
        </header>

        {#if stats.total === 0 && stats.in_flight === 0}
          <div class="squad-empty" role="note">
            <span class="empty-kicker">Not yet routed</span>
            <span class="empty-copy">
              The squad router has not picked this squad for any backlog item yet.
              {#if entry.squad.Paths && entry.squad.Paths.length > 0}
                Routing fires when an item's slices touch
                <code>{entry.squad.Paths[0]}</code>{entry.squad.Paths.length > 1 ? ' (+others)' : ''}
                with confidence ≥ <code>policy.squads.routing.min_confidence</code>.
              {:else}
                Add path globs in <code>platform/gitops/k3s/mills/squads/{entry.squad.Name}.yaml</code> so the router can match items.
              {/if}
            </span>
          </div>
        {:else}
          <div class="squad-metrics">
            <MetricCard label="success rate" value={fmtPct(stats.success_rate)} color={successColor(stats.success_rate, stats.total)} sub={`window of ${stats.window}`} compact />
            <MetricCard label="total" value={stats.total} sub={`${stats.merged_clean} clean / ${stats.failed} failed`} compact />
            <MetricCard label="in flight" value={stats.in_flight} sub={`${fmtCost(stats.total_cost_usd)} window cost`} compact />
          </div>
        {/if}

        {#if expanded === entry.squad.Name}
          <div class="squad-detail">
            {#if detail && detail.recent_memory && detail.recent_memory.length > 0}
              <h3 class="detail-heading">Top memory</h3>
              <ul class="memory-list">
                {#each detail.recent_memory.slice(0, 5) as mem (mem.ID)}
                  <li class="memory-item">
                    <span class="memory-kind kind-{mem.Kind}">{mem.Kind}</span>
                    <span class="memory-title">{mem.Title}</span>
                    <span class="memory-importance">imp {mem.Importance.toFixed(2)}</span>
                  </li>
                {/each}
              </ul>
            {:else if detail}
              <p class="detail-empty">No memory entries yet.</p>
            {:else}
              <p class="detail-empty">Loading detail…</p>
            {/if}
          </div>
        {/if}
      </article>
    {/each}
  </div>
</PanelShell>

<style>
  .squad-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(20rem, 1fr));
    gap: 0.75rem;
    padding: 0.5rem 0.25rem;
  }
  .squad-card {
    background: var(--bg-subtle);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    padding: 0.75rem 0.9rem;
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
  }
  .squad-card.expanded {
    border-color: var(--border-strong);
  }
  .squad-card.disabled {
    opacity: 0.65;
  }
  .squad-header {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .squad-name-btn {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    background: transparent;
    border: none;
    padding: 0;
    color: var(--text-default);
    cursor: pointer;
    font-size: var(--text-sm);
    font-weight: 600;
  }
  .squad-name-btn:hover .squad-name {
    color: var(--text-link);
  }
  .squad-name-btn:focus-visible {
    outline: 2px solid var(--info);
    outline-offset: 2px;
    border-radius: var(--radius-sm);
  }
  .squad-toggle {
    color: var(--text-muted);
    font-size: var(--text-xs);
    width: 0.9rem;
    text-align: center;
  }
  .squad-badge {
    font-size: var(--text-2xs);
    padding: 0.05rem 0.4rem;
    border-radius: var(--radius-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .badge-disabled {
    background: rgba(var(--error-rgb), 0.15);
    color: var(--error);
  }
  .badge-recursion {
    background: rgba(var(--info-rgb), 0.15);
    color: var(--info);
  }
  .squad-metrics {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 0.5rem;
  }
  .squad-empty {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    padding: 0.5rem 0.6rem;
    border: 1px dashed var(--border-subtle);
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--bg-default) 60%, transparent);
  }
  .squad-empty .empty-kicker {
    font-size: var(--text-2xs);
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }
  .squad-empty .empty-copy {
    color: var(--fg-secondary);
    font-size: var(--text-xs);
    line-height: 1.4;
  }
  .squad-empty code {
    padding: 0 0.25rem;
    border-radius: var(--radius-xs);
    background: color-mix(in srgb, var(--bg-subtle) 80%, transparent);
    color: var(--fg-secondary);
    font-size: var(--text-2xs);
  }
  .squad-detail {
    border-top: 1px solid var(--border-subtle);
    padding-top: 0.5rem;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }
  .detail-heading {
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
    margin: 0;
  }
  .detail-empty {
    font-size: var(--text-xs);
    color: var(--text-muted);
    margin: 0;
  }
  .memory-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }
  .memory-item {
    display: grid;
    grid-template-columns: auto 1fr auto;
    gap: 0.5rem;
    align-items: baseline;
    font-size: var(--text-xs);
  }
  .memory-kind {
    font-size: var(--text-2xs);
    padding: 0.05rem 0.35rem;
    border-radius: var(--radius-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    background: var(--bg-default);
    color: var(--text-muted);
  }
  .kind-merge      { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .kind-tech_debt  { background: rgba(var(--warning-rgb), 0.15); color: var(--warning); }
  .kind-convention { background: color-mix(in srgb, var(--tier-short) 15%, transparent); color: var(--tier-short); }
  .kind-followup   { background: rgba(var(--info-rgb), 0.15); color: var(--info); }
  .memory-title {
    color: var(--text-default);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .memory-importance {
    color: var(--text-muted);
    font-size: var(--text-2xs);
    font-variant-numeric: tabular-nums;
  }
</style>
