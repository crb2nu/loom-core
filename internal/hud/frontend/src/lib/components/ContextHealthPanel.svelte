<script lang="ts">
  /**
   * ContextHealthPanel — Context ▸ Health.
   *
   * Renders monitor.ContextHealthMonitor's snapshot: per-agent context-window
   * utilization, the health score behind it, and the compaction control. The
   * whole domain (five REST routes, a polling monitor, auto-compaction) shipped
   * with no HUD surface, so "which agent is about to blow its context" was only
   * answerable by curling the endpoint.
   *
   * Compaction is a real mutation, so it follows the sibling-panel path:
   * shared ConfirmDialog for the stray-click guard + runAdminAction for the
   * outcome toast (same as PolicyPanel's apply/reject).
   */
  import {
    contextHealthStore,
    utilizationTone,
    type AgentContextHealth,
  } from '../stores/contextHealth.svelte.ts';
  import PanelShell from './shared/PanelShell.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';
  import ConfirmDialog from './shared/ConfirmDialog.svelte';
  import MetricCard from './shared/MetricCard.svelte';
  import { runAdminAction } from './mills/shared/millsActions.ts';
  import { formatNumber, relativeTime, truncateId } from '../utils/format.ts';

  $effect(() => {
    contextHealthStore.startPolling(20000);
    return () => contextHealthStore.stopPolling();
  });

  let agents = $derived(contextHealthStore.agents);
  let unavailable = $derived(contextHealthStore.unavailable);
  let error = $derived(contextHealthStore.error);
  let loading = $derived(contextHealthStore.loading && agents.length === 0);
  let systemHealth = $derived(contextHealthStore.systemHealth);
  let totalBudget = $derived(contextHealthStore.totalBudget);
  let totalUsed = $derived(contextHealthStore.totalUsed);
  let compactionQueue = $derived(contextHealthStore.compactionQueue);
  let totalUtilization = $derived(contextHealthStore.totalUtilization);
  let needingCompaction = $derived(contextHealthStore.needingCompaction);

  let pendingCompact = $state<AgentContextHealth | null>(null);

  function pct(fraction: number): string {
    return `${Math.round((fraction ?? 0) * 100)}%`;
  }

  function healthColor(score: number): string {
    if (score >= 75) return 'var(--success)';
    if (score >= 50) return 'var(--warning)';
    return 'var(--error)';
  }

  function utilColor(fraction: number): string {
    const tone = utilizationTone(fraction);
    if (tone === 'crit') return 'var(--error)';
    if (tone === 'warn') return 'var(--warning)';
    return 'var(--success)';
  }

  // A row without a session_id has nothing to compact — the monitor keys
  // compaction by session, not agent.
  function canCompact(a: AgentContextHealth): boolean {
    return Boolean(a.session_id) && contextHealthStore.compacting === null;
  }

  async function confirmCompact(): Promise<void> {
    const agent = pendingCompact;
    pendingCompact = null;
    if (!agent) return;
    await runAdminAction(() => contextHealthStore.compact(agent.session_id), {
      success: `Compaction triggered for ${agent.agent_id}`,
      failurePrefix: 'Compaction failed',
    });
  }
</script>

<PanelShell
  title="Context health"
  icon="◷"
  count={agents.length}
  loading={loading}
  error={!unavailable && error && agents.length === 0 ? error : null}
  errorHeading="Couldn't load context health"
  empty={unavailable || (!error && agents.length === 0)}
  emptyIcon={unavailable ? '◯' : '✓'}
  emptyMessage={unavailable
    ? 'Context health monitor not available'
    : 'No agent sessions being tracked'}
  emptyHint={unavailable
    ? 'The HUD answers /api/context/health with 503 until the context health monitor is wired to the agent bridge.'
    : 'Agents appear here once they have an active session with recorded context entries.'}
  emptyTone={unavailable ? 'disabled' : 'ready'}
>
  {#snippet header()}
    <div class="summary-line">
      <span>
        Fleet context <strong style:color={utilColor(totalUtilization)}>{pct(totalUtilization)}</strong>
        used — {formatNumber(totalUsed)} / {formatNumber(totalBudget)} tokens
      </span>
      {#if contextHealthStore.updatedAt}
        <span class="dim">updated {relativeTime(contextHealthStore.updatedAt)}</span>
      {/if}
    </div>
  {/snippet}

  {#if error && agents.length > 0}
    <ErrorBanner prefix="Context health refresh failed" message={error} />
  {/if}

  <div class="kpi-row">
    <MetricCard
      label="system health"
      value={`${systemHealth}`}
      color={healthColor(systemHealth)}
      sub="0–100 composite"
    />
    <MetricCard
      label="fleet utilization"
      value={pct(totalUtilization)}
      color={utilColor(totalUtilization)}
      sub={`${formatNumber(totalUsed)} / ${formatNumber(totalBudget)} tok`}
    />
    <MetricCard
      label="needs compaction"
      value={needingCompaction.length}
      color={needingCompaction.length > 0 ? 'var(--warning)' : 'var(--fg-primary)'}
      sub="at or over 80% budget"
    />
    <MetricCard
      label="compaction queue"
      value={compactionQueue}
      sub="auto-compactions pending"
    />
  </div>

  <div class="agents-table-wrap">
    <table class="agents-table">
      <thead>
        <tr>
          <th>agent</th>
          <th>namespace</th>
          <th class="col-util">context used</th>
          <th class="col-num">health</th>
          <th class="col-num">stale</th>
          <th class="col-num">recall</th>
          <th>last entry</th>
          <th class="col-actions">actions</th>
        </tr>
      </thead>
      <tbody>
        {#each agents as a (a.agent_id + ':' + a.session_id)}
          <tr class:needs-compaction={a.compaction_needed}>
            <td>
              <span class="agent-id mono">{a.agent_id}</span>
              <span class="session mono dim">{truncateId(a.session_id)}</span>
            </td>
            <td class="mono dim">{a.namespace || '—'}</td>
            <td class="col-util">
              <div class="util">
                <div class="util-bar" role="img" aria-label={`${pct(a.budget_utilization)} of budget used`}>
                  <div
                    class="util-fill tone-{utilizationTone(a.budget_utilization)}"
                    style:width={`${Math.min(100, Math.max(0, (a.budget_utilization ?? 0) * 100))}%`}
                  ></div>
                </div>
                <span class="util-text mono" style:color={utilColor(a.budget_utilization)}>
                  {pct(a.budget_utilization)}
                </span>
              </div>
              <span class="util-sub mono dim">
                {formatNumber(a.tokens_used)} / {formatNumber(a.token_budget)}
              </span>
            </td>
            <td class="col-num mono" style:color={healthColor(a.health_score)}>{a.health_score}</td>
            <td class="col-num mono" class:warn={a.stale_entries > 0}>{a.stale_entries}</td>
            <td class="col-num mono">{pct(a.recall_hit_rate)}</td>
            <td class="mono dim">{a.last_entry_age || '—'}</td>
            <td class="col-actions">
              <button
                type="button"
                class="btn-quiet"
                class:urgent={a.compaction_needed}
                disabled={!canCompact(a)}
                title={a.session_id ? 'Trigger compaction for this session' : 'No session to compact'}
                onclick={() => (pendingCompact = a)}
              >
                {contextHealthStore.compacting === a.session_id ? 'compacting…' : 'compact'}
              </button>
            </td>
          </tr>
          {#if a.recommendation}
            <tr class="rec-row">
              <td colspan="8"><span class="rec">▸ {a.recommendation}</span></td>
            </tr>
          {/if}
        {/each}
      </tbody>
    </table>
  </div>
</PanelShell>

<ConfirmDialog
  open={pendingCompact !== null}
  title="Compact this session's context?"
  message={pendingCompact
    ? `Runs compression over ${pendingCompact.agent_id} (session ${truncateId(pendingCompact.session_id)}). Older entries are summarised; the operation is not reversible.`
    : ''}
  confirmLabel="Compact"
  variant="warn"
  onConfirm={confirmCompact}
  onCancel={() => (pendingCompact = null)}
/>

<style>
  .summary-line {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-3);
    font-size: var(--text-sm);
    color: var(--fg-secondary);
    flex-wrap: wrap;
  }

  .kpi-row {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
    gap: 0.6rem;
    padding: 0.25rem 0.25rem 0.75rem;
  }

  .agents-table-wrap {
    overflow-x: auto;
  }

  .agents-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.85rem;
  }

  .agents-table thead th {
    text-align: left;
    font-size: 0.7rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--fg-muted);
    border-bottom: 1px solid var(--border-subtle);
    padding: 0.35rem 0.5rem;
    font-weight: 500;
  }

  .agents-table tbody td {
    padding: 0.4rem 0.5rem;
    border-bottom: 1px solid var(--border-subtle);
    vertical-align: middle;
  }

  tr.needs-compaction td {
    background: color-mix(in srgb, var(--warning) 7%, transparent);
  }

  .rec-row td {
    border-bottom: 1px solid var(--border-subtle);
    padding-top: 0;
  }

  .rec {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
  }

  .col-num { text-align: right; width: 4.5rem; }
  .col-util { width: 13rem; }
  .col-actions { text-align: right; width: 7rem; }

  .mono { font-family: var(--font-mono); }
  .dim { color: var(--fg-muted); }
  .warn { color: var(--warning); }

  .agent-id {
    display: block;
    color: var(--fg-primary);
    font-weight: 600;
  }

  .session {
    display: block;
    font-size: var(--text-xs);
  }

  .util {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .util-bar {
    flex: 1;
    height: 6px;
    background: var(--bg-elevated);
    border-radius: var(--radius-xs);
    overflow: hidden;
    min-width: 60px;
  }

  .util-fill {
    height: 100%;
    border-radius: var(--radius-xs);
    transition: width var(--transition-slow);
  }

  .util-fill.tone-ok { background: var(--success); }
  .util-fill.tone-warn { background: var(--warning); }
  .util-fill.tone-crit { background: var(--error); }

  .util-text {
    font-size: var(--text-xs);
    font-weight: 600;
    min-width: 2.5rem;
    text-align: right;
  }

  .util-sub {
    display: block;
    font-size: var(--text-xs);
    margin-top: 2px;
  }

  .btn-quiet {
    font-size: 0.75rem;
    padding: 0.2rem 0.6rem;
    border-radius: var(--radius-xs);
    border: 1px solid var(--border-subtle);
    background: transparent;
    color: var(--fg-secondary);
    cursor: pointer;
  }

  .btn-quiet:hover:not(:disabled) {
    border-color: var(--border-focus);
    color: var(--fg-primary);
  }

  .btn-quiet.urgent {
    border-color: color-mix(in srgb, var(--warning) 45%, var(--border));
    color: var(--warning);
  }

  .btn-quiet:disabled {
    opacity: 0.45;
    cursor: default;
  }
</style>
