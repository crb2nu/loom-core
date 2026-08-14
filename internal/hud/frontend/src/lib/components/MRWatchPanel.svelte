<script lang="ts">
  /**
   * MRWatchPanel — Operations ▸ MRs.
   *
   * The mrwatch registry (internal/hud/mrwatch) polls the watched GitLab
   * projects, classifies every open MR (healthy / flaky / blocked / …), and the
   * shepherd takes bounded auto-actions on the unhealthy ones — retrying a
   * flaky pipeline, arming auto-merge on a green head. Both the registry
   * snapshot and the shepherd's audit log were served over REST and rendered
   * nowhere, which meant an autonomous actor was mutating merge requests with
   * no operator-visible record.
   *
   * Read-only by design: the shepherd owns its own budget; this panel reports.
   */
  import { mrwatchStore, type MRWatchMergeRequest } from '../stores/mrwatch.svelte.ts';
  import PanelShell from './shared/PanelShell.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';
  import Badge from '../widgets/Badge.svelte';
  import type { BadgeVariant } from '../utils/tokens.ts';
  import { relativeTime } from '../utils/format.ts';
  import { MRWATCH_STATE_VARIANTS, isLiveMRWatchState } from '../utils/mrwatchStates.ts';

  $effect(() => {
    mrwatchStore.startPolling(30000);
    return () => mrwatchStore.stopPolling();
  });

  let mrs = $derived(mrwatchStore.liveMergeRequests);
  let landed = $derived(mrwatchStore.mergeRequests.filter((mr) => mr.state === 'merged'));
  let actions = $derived(mrwatchStore.recentActions);
  let unavailable = $derived(mrwatchStore.unavailable);
  let error = $derived(mrwatchStore.error);
  let loading = $derived(mrwatchStore.loading && mrs.length === 0);
  let countPairs = $derived(mrwatchStore.countPairs);
  let projects = $derived(mrwatchStore.projects);

  // Registry classifications. Anything unrecognised falls through to a neutral
  // chip rather than being dropped — the state vocabulary lives in Go and can
  // grow without a frontend release.
  function stateVariant(state: string): BadgeVariant {
    return MRWATCH_STATE_VARIANTS[state as keyof typeof MRWATCH_STATE_VARIANTS] ?? 'muted';
  }

  function outcomeVariant(outcome: string): BadgeVariant {
    if (outcome === 'ok' || outcome === 'success' || outcome === 'applied') return 'success';
    if (outcome === 'skipped' || outcome === 'noop' || outcome === 'dry_run') return 'muted';
    return 'error';
  }

  function mrLabel(mr: MRWatchMergeRequest): string {
    return `${mr.repo}!${mr.iid}`;
  }
</script>

<PanelShell
  title="Merge requests"
  icon="⑃"
  count={mrs.length}
  loading={loading}
  error={!unavailable && error && mrs.length === 0 ? error : null}
  errorHeading="Couldn't load the MR registry"
  empty={unavailable || (!error && mrs.length === 0 && landed.length === 0)}
  emptyIcon={unavailable ? '◯' : '✓'}
  emptyMessage={unavailable ? 'MR watch not available on this build' : 'No open merge requests tracked'}
  emptyHint={unavailable
    ? 'This HUD registers no /api/mrwatch routes. The branch→MR registry ships with the mrwatch poller enabled.'
    : 'The registry lists every open MR across the watched projects, with its classification and head-pipeline state.'}
  emptyTone={unavailable ? 'disabled' : 'ready'}
>
  {#snippet header()}
    <div class="summary-line">
      <span class="chips">
        {#each countPairs.filter(([state]) => isLiveMRWatchState(state)) as [state, n] (state)}
          <Badge text={`${state} ${n}`} variant={stateVariant(state)} />
        {/each}
        {#if countPairs.filter(([state]) => isLiveMRWatchState(state)).length === 0}
          <span class="dim">no classified MRs</span>
        {/if}
      </span>
      <span class="meta dim">
        {#if projects.length > 0}{projects.length} project{projects.length === 1 ? '' : 's'} · {/if}
        {#if mrwatchStore.lastPollAt}polled {relativeTime(mrwatchStore.lastPollAt)}{:else}never polled{/if}
        {#if mrwatchStore.stale} · <span class="stale">stale</span>{/if}
      </span>
    </div>
  {/snippet}

  {#if error && mrs.length > 0}
    <ErrorBanner prefix="MR registry refresh failed" message={error} />
  {/if}

  <div class="table-wrap">
    <table class="mr-table">
      <thead>
        <tr>
          <th>state</th>
          <th>mr</th>
          <th>title</th>
          <th>branch</th>
          <th>pipeline</th>
          <th>changed</th>
        </tr>
      </thead>
      <tbody>
        {#each mrs as mr (mr.repo + '!' + mr.iid)}
          <tr class:is-stale={mr.stale}>
            <td><Badge text={mr.state} variant={stateVariant(mr.state)} /></td>
            <td class="mono">
              {#if mr.web_url}
                <a href={mr.web_url} target="_blank" rel="noreferrer noopener">{mrLabel(mr)}</a>
              {:else}
                {mrLabel(mr)}
              {/if}
            </td>
            <td class="title" title={mr.title}>
              {mr.title}
              {#if mr.reason}<span class="reason">{mr.reason}</span>{/if}
            </td>
            <td class="mono dim" title={mr.source_branch}>
              {mr.source_branch}{#if mr.target_branch}<span class="dim"> → {mr.target_branch}</span>{/if}
            </td>
            <td class="mono">
              {#if mr.pipeline_url && mr.pipeline_status}
                <a href={mr.pipeline_url} target="_blank" rel="noreferrer noopener">{mr.pipeline_status}</a>
              {:else}
                <span class="dim">{mr.pipeline_status || '—'}</span>
              {/if}
            </td>
            <td class="dim">{relativeTime(mr.last_transition_at)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>

  {#if landed.length > 0}
    <details class="landed">
      <summary>landed (last 72h) · {landed.length}</summary>
      <ul>
        {#each landed as mr (mr.repo + '!' + mr.iid)}
          <li>
            <Badge text={mr.state} variant={stateVariant(mr.state)} />
            <span class="mono">{mrLabel(mr)}</span>
            <span class="dim">{mr.title}</span>
            {#if mr.merged_at}<span class="dim"> · merged {relativeTime(mr.merged_at)}</span>{/if}
          </li>
        {/each}
      </ul>
    </details>
  {/if}

  <section class="actions-feed">
    <h3>Shepherd actions</h3>
    {#if !mrwatchStore.shepherdEnabled}
      <p class="dim empty-note">Shepherd disabled (LOOM_MRWATCH_SHEPHERD).</p>
    {:else if actions.length === 0}
      <p class="dim empty-note">
        No auto-actions recorded. The shepherd retries flaky pipelines and arms
        auto-merge within a bounded per-cycle budget; it logs every attempt here,
        including the ones it declined.
      </p>
    {:else}
      <ul class="action-list">
        {#each actions as a, i (a.time + a.repo + a.mr_iid + i)}
          <li class="action-row">
            <span class="action-time dim">{relativeTime(a.time)}</span>
            <span class="action-verb mono">{a.action}</span>
            <span class="action-target mono">{a.repo}!{a.mr_iid}</span>
            <Badge text={a.state} variant={stateVariant(a.state)} />
            <Badge text={a.outcome} variant={outcomeVariant(a.outcome)} />
            {#if a.detail}<span class="action-detail dim">{a.detail}</span>{/if}
          </li>
        {/each}
      </ul>
    {/if}
  </section>
</PanelShell>

<style>
  .summary-line {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    flex-wrap: wrap;
    font-size: var(--text-xs);
  }

  .chips {
    display: inline-flex;
    gap: var(--space-1);
    flex-wrap: wrap;
  }

  .meta { font-family: var(--font-mono); }
  .stale { color: var(--warning); }
  .dim { color: var(--fg-muted); }
  .mono { font-family: var(--font-mono); }

  .table-wrap { overflow-x: auto; }

  .landed { margin-top: var(--space-3); color: var(--fg-muted); }
  .landed summary { cursor: pointer; }
  .landed ul { margin: var(--space-2) 0 0; padding-left: var(--space-4); }
  .landed li { display: flex; gap: var(--space-2); align-items: center; margin: var(--space-1) 0; }

  .mr-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.85rem;
  }

  .mr-table thead th {
    text-align: left;
    font-size: 0.7rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--fg-muted);
    border-bottom: 1px solid var(--border-subtle);
    padding: 0.35rem 0.5rem;
    font-weight: 500;
  }

  .mr-table tbody td {
    padding: 0.4rem 0.5rem;
    border-bottom: 1px solid var(--border-subtle);
    vertical-align: middle;
  }

  /* A stale row is retained data from a poll that failed for its project. */
  .mr-table tbody tr.is-stale td { opacity: 0.65; }

  .mr-table a {
    color: var(--accent);
    text-decoration: none;
  }
  .mr-table a:hover { text-decoration: underline; }

  .title {
    max-width: 28rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--fg-primary);
  }

  .reason {
    display: block;
    font-size: var(--text-xs);
    color: var(--fg-muted);
  }

  .actions-feed {
    margin-top: var(--space-5);
    border-top: 1px solid var(--border-subtle);
    padding-top: var(--space-3);
  }

  .actions-feed h3 {
    margin: 0 0 var(--space-2);
    font-size: var(--text-sm);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }

  .empty-note {
    margin: 0;
    font-size: var(--text-xs);
    line-height: 1.6;
    max-width: 60ch;
  }

  .action-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .action-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) 0;
    border-bottom: 1px solid var(--border-subtle);
    font-size: var(--text-xs);
    flex-wrap: wrap;
  }

  .action-time { min-width: 5rem; }
  .action-verb { color: var(--fg-primary); font-weight: 600; }
  .action-target { color: var(--fg-secondary); }
  .action-detail { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; }
</style>
