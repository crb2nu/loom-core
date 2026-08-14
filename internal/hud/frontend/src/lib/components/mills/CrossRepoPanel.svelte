<script lang="ts">
  /**
   * CrossRepoPanel — the Mills ▸ Cross-Repo tab: atomic multi-repo runs, their
   * per-repo branch/MR/CI state, and the abort control.
   *
   * Named CrossRepoCard until it was promoted to a first-class panel. It is
   * registered as a top-level Mills tab, so it carries the sibling panel
   * conventions: PanelShell header + count + disabled/empty/error states, the
   * shared ConfirmDialog for the destructive action, and runAdminAction for
   * outcome toasts.
   */
  import { millsCrossRepoStore } from '../../stores/mills_crossrepo.svelte.ts';
  import { inFlightStates, terminalStates, type CrossRepoRun, type CrossRepoState } from '../../stores/mills_crossrepo_types.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import MetricCard from '../shared/MetricCard.svelte';
  import { runAdminAction } from './shared/millsActions.ts';
  import { fmtPct, shortRunID } from './shared/format.ts';
  import { relativeTime } from '../../utils/format.ts';

  $effect(() => {
    millsCrossRepoStore.startPolling(15000);
    return () => { millsCrossRepoStore.stopPolling(); };
  });

  let runs = $derived(millsCrossRepoStore.runs);
  let loading = $derived(millsCrossRepoStore.loading && runs.length === 0);
  let disabled = $derived(millsCrossRepoStore.disabled);
  let storeError = $derived(millsCrossRepoStore.error);
  let atomicityRate = $derived(millsCrossRepoStore.atomicityRate);
  let inFlightCount = $derived(millsCrossRepoStore.inFlightCount);
  let mergedToday = $derived(millsCrossRepoStore.mergedTodayCount);
  let revertedToday = $derived(millsCrossRepoStore.revertedTodayCount);

  let expanded = $state<string | null>(null);
  let confirmRun = $state<CrossRepoRun | null>(null);
  let aborting = $state<string | null>(null);

  function toggle(id: string): void {
    expanded = expanded === id ? null : id;
  }

  function rateColor(rate: number | null): string {
    if (rate == null) return 'var(--fg-muted)';
    if (rate >= 0.9) return 'var(--success)';
    if (rate >= 0.7) return 'var(--warning)';
    return 'var(--error)';
  }

  function reposChips(run: CrossRepoRun): string[] {
    return run.repos.map((r) => r.repo_name || `p${r.project_id}`);
  }

  function isInFlight(state: CrossRepoState): boolean {
    return inFlightStates.has(state);
  }

  function isTerminal(state: CrossRepoState): boolean {
    return terminalStates.has(state);
  }

  function stateBadgeClass(state: CrossRepoState): string {
    return `state-badge state-${state}`;
  }

  function requestAbort(run: CrossRepoRun): void {
    if (isTerminal(run.state)) return;
    confirmRun = run;
  }

  // Abort goes through the same admin path as every other Mills mutation:
  // the store's abort() routes through adminFetch (Labs access-bar token, or
  // Cloudflare Access SSO with no local token — this card used to be the one
  // surface that popped a globalThis.prompt for it), the confirmation is the
  // shared ConfirmDialog, and runAdminAction turns the outcome into a toast.
  async function confirmAbort(): Promise<void> {
    const run = confirmRun;
    confirmRun = null;
    if (!run) return;
    aborting = run.id;
    const ok = await runAdminAction(() => millsCrossRepoStore.abort(run.id), {
      success: `Cross-repo run ${run.id} aborted`,
      failurePrefix: 'Abort failed',
    });
    aborting = null;
    if (ok) await millsCrossRepoStore.refresh();
  }
</script>

<PanelShell
  title="Cross-Repo"
  icon="⇄"
  count={runs.length}
  loading={loading}
  error={!disabled && storeError && runs.length === 0 ? storeError : null}
  errorHeading="Couldn't load cross-repo runs"
  empty={!storeError && runs.length === 0}
  emptyIcon={disabled ? '◯' : '□'}
  emptyMessage={disabled ? 'Mills operator not configured' : 'No cross-repo runs yet'}
  emptyHint={disabled
    ? 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.'
    : 'Cross-repo coordination is gated by policy.cross_repo.enabled in operator config.'}
  emptyTone={disabled ? 'disabled' : 'idle'}
>
  {#if storeError && runs.length > 0}
    <ErrorBanner prefix="Cross-Repo refresh failed" message={storeError} />
  {/if}

  <div class="kpi-row">
    <MetricCard label="atomicity rate" value={fmtPct(atomicityRate)} color={rateColor(atomicityRate)} sub="last 30 terminal runs" />
    <MetricCard label="in flight" value={inFlightCount} sub="open / gates_green / merging" />
    <MetricCard label="merged today" value={mergedToday} color="var(--success)" sub="since 00:00 local" />
    <MetricCard label="reverted today" value={revertedToday} color={revertedToday > 0 ? 'var(--error)' : 'var(--fg-primary)'} sub="since 00:00 local" />
  </div>

  <div class="runs-table-wrap">
  <table class="runs-table">
    <thead>
      <tr>
        <th>state</th>
        <th>id</th>
        <th>backlog</th>
        <th>repos</th>
        <th>created</th>
        <th class="col-actions">actions</th>
      </tr>
    </thead>
    <tbody>
      {#each runs as run (run.id)}
        <tr
          class="run-row"
          class:expanded={expanded === run.id}
          class:terminal={isTerminal(run.state)}
        >
          <td><span class={stateBadgeClass(run.state)}>{run.state}</span></td>
          <td class="mono">
            <button
              type="button"
              class="row-toggle"
              onclick={() => toggle(run.id)}
              aria-expanded={expanded === run.id}
            >
              <span class="toggle-arrow">{expanded === run.id ? '▾' : '▸'}</span>
              {shortRunID(run.id)}
            </button>
          </td>
          <td class="mono">{shortRunID(run.backlog_item_id)}</td>
          <td>
            <span class="repos-chips">
              {#each reposChips(run) as name (name)}
                <span class="repo-chip">{name}</span>
              {/each}
            </span>
          </td>
          <td class="dim">{relativeTime(run.created_at)}</td>
          <td class="col-actions">
            {#if !isTerminal(run.state)}
              <button
                type="button"
                class="btn-quiet"
                disabled={aborting === run.id}
                onclick={() => requestAbort(run)}
              >
                {aborting === run.id ? 'aborting…' : 'abort'}
              </button>
            {:else}
              <span class="dim">—</span>
            {/if}
          </td>
        </tr>
        {#if expanded === run.id}
          <tr class="run-detail-row">
            <td colspan="6">
              <div class="run-detail">
                <div class="detail-meta">
                  <span><strong>atomicity</strong>: {run.atomicity_strategy || '—'}</span>
                  <span><strong>updated</strong>: {relativeTime(run.updated_at)}</span>
                </div>
                <table class="detail-table">
                  <thead>
                    <tr><th>repo</th><th>project</th><th>branch</th><th>mr</th><th>ci</th><th>gate</th></tr>
                  </thead>
                  <tbody>
                    {#each run.repos as repo (repo.project_id + ':' + repo.branch)}
                      <tr>
                        <td>{repo.repo_name || '—'}</td>
                        <td class="mono">{repo.project_id}</td>
                        <td class="mono">{repo.branch}</td>
                        <td class="mono">{repo.mr_iid != null ? `!${repo.mr_iid}` : '—'}</td>
                        <td>{repo.ci_status || '—'}</td>
                        <td>{repo.gate_status || '—'}</td>
                      </tr>
                    {/each}
                  </tbody>
                </table>
              </div>
            </td>
          </tr>
        {/if}
      {/each}
    </tbody>
  </table>
  </div>
</PanelShell>

<ConfirmDialog
  open={confirmRun !== null}
  title="Abort cross-repo run?"
  message={confirmRun
    ? `Aborts ${confirmRun.id}. This marks the run failed; per-repo MRs are NOT closed.`
    : ''}
  confirmLabel="Abort"
  variant="danger"
  onConfirm={confirmAbort}
  onCancel={() => (confirmRun = null)}
/>

<style>
  .kpi-row {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
    gap: 0.6rem;
    padding: 0.5rem 0.25rem 0.75rem;
  }
  .runs-table-wrap {
    overflow-x: auto;
  }

  .runs-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-12);
  }
  .runs-table thead th {
    text-align: left;
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
    border-bottom: 1px solid var(--border-subtle);
    padding: 0.35rem 0.5rem;
    font-weight: 500;
  }
  .runs-table tbody td {
    padding: 0.4rem 0.5rem;
    border-bottom: 1px solid var(--border-subtle);
    vertical-align: middle;
  }
  .run-row.terminal {
    opacity: 0.75;
  }
  .run-row.expanded {
    background: var(--bg-subtle);
  }
  .col-actions {
    text-align: right;
    width: 8rem;
  }
  .row-toggle {
    background: transparent;
    border: none;
    color: var(--text-default);
    cursor: pointer;
    padding: 0;
    font: inherit;
    font-family: ui-monospace, SFMono-Regular, monospace;
    display: inline-flex;
    align-items: center;
    gap: 0.35rem;
  }
  .row-toggle:hover { color: var(--text-link); }
  .toggle-arrow { color: var(--text-muted); width: 0.9rem; text-align: center; }
  .mono { font-family: ui-monospace, SFMono-Regular, monospace; }
  .dim  { color: var(--text-muted); font-size: var(--text-xs); }

  .state-badge {
    display: inline-block;
    font-size: var(--text-2xs);
    padding: 0.1rem 0.45rem;
    border-radius: var(--radius-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    font-weight: 600;
    background: var(--bg-default);
    color: var(--text-muted);
  }
  .state-planning    { background: color-mix(in srgb, var(--tier-short) 15%, transparent); color: var(--tier-short); }
  .state-open        { background: rgba(var(--info-rgb), 0.15); color: var(--info); }
  .state-gates_green { background: rgba(var(--success-rgb), 0.18); color: var(--success); }
  .state-merging     { background: rgba(var(--warning-rgb), 0.18);  color: var(--warning); }
  .state-merged      { background: rgba(var(--success-rgb), 0.15);  color: var(--success); }
  .state-reverted    { background: rgba(var(--warning-rgb), 0.18);  color: var(--warning); }
  .state-failed      { background: rgba(var(--error-rgb), 0.18);   color: var(--error); }

  .repos-chips { display: inline-flex; flex-wrap: wrap; gap: 0.25rem; }
  .repo-chip {
    font-size: var(--text-2xs);
    padding: 0.05rem 0.4rem;
    border-radius: var(--radius-xs);
    background: var(--bg-default);
    color: var(--text-default);
    font-family: ui-monospace, SFMono-Regular, monospace;
  }

  .btn-quiet {
    font-size: var(--text-xs);
    padding: 0.2rem 0.6rem;
    border-radius: var(--radius-xs);
    border: 1px solid var(--border-subtle);
    background: transparent;
    color: var(--text-default);
    cursor: pointer;
  }
  .btn-quiet:hover:not(:disabled) { border-color: var(--border-strong); }
  .btn-quiet:disabled { opacity: 0.6; cursor: progress; }

  .run-detail-row td {
    background: var(--bg-default);
    padding: 0.5rem 1rem 0.75rem;
  }
  .run-detail {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .detail-meta {
    display: flex;
    gap: 1.5rem;
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .detail-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-xs);
  }
  .detail-table th {
    text-align: left;
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
    border-bottom: 1px solid var(--border-subtle);
    padding: 0.25rem 0.5rem;
    font-weight: 500;
  }
  .detail-table td {
    padding: 0.25rem 0.5rem;
    border-bottom: 1px solid var(--border-subtle);
  }
</style>
