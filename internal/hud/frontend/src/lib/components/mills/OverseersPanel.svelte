<script lang="ts">
  /**
   * OverseersPanel — "Overseers" (Mills family). Surfaces the supervisory
   * agents (groomer / sentinel / foreman) that ride the shared guarded-auto-act
   * harness: for each agent, its enable/dry-run/pause state, last tick time and
   * result counts, and any live admission-suppression lease. Below the agent
   * cards, a compact per-agent recent-actions log (from the events table) shows
   * what each overseer actually did in the last 24h.
   *
   * Sourced from GET /api/mills/overseers via the HUD's domain/mills proxy.
   * Read-only — pause/resume/tick are ops endpoints, not exposed here.
   */
  import { millsOverseersStore } from '../../stores/mills_overseers.svelte.ts';
  import type {
    OverseerAgent,
    OverseerEvent,
  } from '../../stores/mills_overseers.svelte.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import Badge from '../../widgets/Badge.svelte';
  import type { BadgeVariant } from '../../utils/tokens.ts';
  import { relativeTime } from '../../utils/format.ts';

  $effect(() => {
    millsOverseersStore.startPolling(15000);
    return () => {
      millsOverseersStore.stopPolling();
    };
  });

  let status = $derived(millsOverseersStore.status);
  let agents = $derived<OverseerAgent[]>(status?.agents ?? []);
  let recentActions = $derived(status?.recent_actions ?? {});
  let masterEnabled = $derived(status?.enabled === true);
  let loading = $derived(millsOverseersStore.loading && agents.length === 0);
  let disabled = $derived(millsOverseersStore.disabled);
  let error = $derived(millsOverseersStore.error);

  // Agents whose suppression lease is still live (until in the future). The
  // warning strip is the panel's loudest signal: a suppressed overseer means
  // automated work admission is being held back right now.
  let suppressed = $derived(
    agents.filter(
      (a) => a.suppression != null && new Date(a.suppression.until).getTime() > Date.now(),
    ),
  );

  // expanded agent name → show its recent-actions log. Collapsed by default so
  // the panel opens compact; the log can be long.
  let expanded = $state<Record<string, boolean>>({});

  function toggle(name: string): void {
    expanded = { ...expanded, [name]: !expanded[name] };
  }

  function stateBadge(a: OverseerAgent): { text: string; variant: BadgeVariant } {
    if (!a.enabled) return { text: 'disabled', variant: 'muted' };
    if (a.paused) return { text: 'paused', variant: 'warning' };
    return { text: 'active', variant: 'success' };
  }

  // Pull the few payload fields worth showing inline on a recent-action row.
  // The full payload lives in the events table; this is the at-a-glance subset.
  const PAYLOAD_KEYS = [
    'canonical_id',
    'jaccard',
    'reason',
    'basis',
    'allowed',
    'priority',
    'incident',
  ] as const;

  function payloadSummary(ev: OverseerEvent): string {
    const p = ev.Payload;
    if (!p) return '';
    const parts: string[] = [];
    for (const k of PAYLOAD_KEYS) {
      if (p[k] == null) continue;
      const v = p[k];
      parts.push(`${k}=${typeof v === 'number' ? v : String(v)}`);
    }
    return parts.join('  ');
  }

  // Short display kind: drop the leading "overseer.<agent>." namespace so the
  // row reads "dedup_close.dryrun" not "overseer.groomer.dedup_close.dryrun".
  function shortKind(kind: string): string {
    const parts = kind.split('.');
    return parts.length > 2 ? parts.slice(2).join('.') : kind;
  }
</script>

<PanelShell
  title="The Alley — Overseers"
  icon="❖"
  count={agents.length}
  loading={loading}
  error={!disabled && error && agents.length === 0 ? error : null}
  errorHeading="Couldn't reach the overseers"
  empty={!error && agents.length === 0}
  emptyIcon={disabled ? '◯' : '□'}
  emptyMessage={disabled ? 'Mills operator not configured' : 'No overseers registered'}
  emptyHint={disabled
    ? 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.'
    : 'Supervisory agents (groomer / sentinel / foreman) register on operator boot when the overseers policy is present.'}
  emptyTone={disabled ? 'disabled' : 'idle'}
>
  {#if error && agents.length > 0}
    <ErrorBanner prefix="Overseers refresh failed" message={error} />
  {/if}

  <!-- Master-gate + live-suppression banner. Loudest signal in the panel. -->
  <div class="overseers-status-strip">
    <span class="master-gate" class:on={masterEnabled} class:off={!masterEnabled}>
      <span class="gate-dot" aria-hidden="true"></span>
      overseers {masterEnabled ? 'enabled' : 'disabled'}
    </span>
  </div>

  {#if suppressed.length > 0}
    <div class="suppression-warning" role="alert">
      <span class="warn-icon" aria-hidden="true">⚠</span>
      <div class="warn-body">
        <strong>Work admission suppressed</strong>
        <ul class="warn-list">
          {#each suppressed as a (a.name)}
            <li>
              <span class="warn-agent">{a.name}</span>
              <span class="warn-reason">{a.suppression?.reason}</span>
              <!-- "expires in 10m", not "until just now": relativeTime reads
                   forward for unlapsed deadlines. -->
              <span class="warn-until">expires {relativeTime(a.suppression?.until)}</span>
            </li>
          {/each}
        </ul>
      </div>
    </div>
  {/if}

  <div class="agent-grid">
    {#each agents as agent (agent.name)}
      {@const badge = stateBadge(agent)}
      {@const actions = recentActions[agent.name] ?? []}
      {@const res = agent.last_result}
      <article class="agent-card" class:disabled={!agent.enabled}>
        <header class="agent-header">
          <span class="agent-name">{agent.name}</span>
          <div class="agent-badges">
            <Badge text={badge.text} variant={badge.variant} />
            {#if agent.dry_run}
              <Badge text="dry-run" variant="info" />
            {/if}
            {#if agent.suppression && new Date(agent.suppression.until).getTime() > Date.now()}
              <Badge text="suppressing" variant="error" />
            {/if}
          </div>
        </header>

        <div class="agent-meta">
          <span class="meta-tick">
            last tick {agent.last_tick_at ? relativeTime(agent.last_tick_at) : 'never'}
          </span>
          {#if res.note}
            <span class="meta-note" title="tick note">{res.note}</span>
          {/if}
        </div>

        {#if agent.last_error}
          <div class="agent-error" title={agent.last_error}>{agent.last_error}</div>
        {/if}

        <dl class="result-counts">
          <div class="count"><dt>inspected</dt><dd>{res.inspected}</dd></div>
          <div class="count count-acted" class:hot={res.acted > 0}>
            <dt>acted</dt><dd>{res.acted}</dd>
          </div>
          <div class="count"><dt>planned</dt><dd>{res.planned}</dd></div>
          <div class="count"><dt>skipped</dt><dd>{res.skipped}</dd></div>
          <div class="count count-errored" class:hot={res.errored > 0}>
            <dt>errored</dt><dd>{res.errored}</dd>
          </div>
        </dl>

        <button
          type="button"
          class="actions-toggle"
          onclick={() => toggle(agent.name)}
          aria-expanded={!!expanded[agent.name]}
          disabled={actions.length === 0}
        >
          <span class="toggle-caret">{expanded[agent.name] ? '▾' : '▸'}</span>
          recent actions
          <span class="toggle-count">{actions.length}</span>
        </button>

        {#if expanded[agent.name] && actions.length > 0}
          <ul class="action-log">
            {#each actions as ev (ev.ID)}
              <li class="action-row">
                <span class="action-kind">{shortKind(ev.Kind)}</span>
                <span class="action-subject" title="{ev.SubjectKind}:{ev.SubjectID}">
                  {ev.SubjectID}
                </span>
                {#if payloadSummary(ev)}
                  <span class="action-payload">{payloadSummary(ev)}</span>
                {/if}
                <span class="action-time">{relativeTime(ev.OccurredAt)}</span>
              </li>
            {/each}
          </ul>
        {/if}
      </article>
    {/each}
  </div>
</PanelShell>

<style>
  .overseers-status-strip {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.4rem 0.25rem 0.6rem;
  }
  .master-gate {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    font-weight: 600;
  }
  .master-gate.on {
    color: var(--success);
  }
  .master-gate.off {
    color: var(--text-muted);
  }
  .gate-dot {
    width: 0.5rem;
    height: 0.5rem;
    border-radius: var(--radius-full);
    background: currentColor;
  }

  .suppression-warning {
    display: flex;
    gap: 0.6rem;
    align-items: flex-start;
    margin: 0 0.25rem 0.75rem;
    padding: 0.6rem 0.75rem;
    border: 1px solid var(--error);
    border-radius: var(--radius-sm);
    background: rgba(var(--error-rgb), 0.1);
  }
  .warn-icon {
    color: var(--error);
    font-size: var(--text-base);
    line-height: 1.2;
  }
  .warn-body {
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
  }
  .warn-body strong {
    color: var(--error);
    font-size: var(--text-12);
  }
  .warn-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
  }
  .warn-list li {
    display: flex;
    gap: 0.5rem;
    flex-wrap: wrap;
    align-items: baseline;
    font-size: var(--text-xs);
  }
  .warn-agent {
    font-weight: 600;
    color: var(--text-default);
  }
  .warn-reason {
    color: var(--fg-secondary);
  }
  .warn-until {
    color: var(--text-muted);
    font-size: var(--text-2xs);
    font-variant-numeric: tabular-nums;
  }

  .agent-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(22rem, 1fr));
    gap: 0.75rem;
    padding: 0.25rem;
  }
  .agent-card {
    background: var(--bg-subtle);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    padding: 0.75rem 0.9rem;
    display: flex;
    flex-direction: column;
    gap: 0.55rem;
  }
  .agent-card.disabled {
    opacity: 0.7;
  }
  .agent-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .agent-name {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--text-default);
    text-transform: capitalize;
  }
  .agent-badges {
    display: inline-flex;
    gap: 0.3rem;
    flex-wrap: wrap;
  }
  .agent-meta {
    display: flex;
    align-items: baseline;
    gap: 0.6rem;
    flex-wrap: wrap;
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .meta-note {
    padding: 0.05rem 0.4rem;
    border-radius: var(--radius-xs);
    background: var(--bg-default);
    color: var(--fg-secondary);
    font-variant-numeric: tabular-nums;
  }
  .agent-error {
    font-size: var(--text-xs);
    color: var(--error);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .result-counts {
    display: grid;
    grid-template-columns: repeat(5, 1fr);
    gap: 0.4rem;
    margin: 0;
  }
  .count {
    display: flex;
    flex-direction: column;
    gap: 0.1rem;
    align-items: center;
    text-align: center;
  }
  .count dt {
    font-size: var(--text-2xs);
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  .count dd {
    margin: 0;
    font-size: var(--text-base);
    font-weight: 600;
    font-variant-numeric: tabular-nums;
    color: var(--text-default);
  }
  .count-acted.hot dd {
    color: var(--success);
  }
  .count-errored.hot dd {
    color: var(--error);
  }
  .actions-toggle {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    align-self: flex-start;
    background: transparent;
    border: none;
    padding: 0.15rem 0;
    color: var(--text-muted);
    cursor: pointer;
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .actions-toggle:hover:not(:disabled) {
    color: var(--text-link);
  }
  .actions-toggle:disabled {
    cursor: default;
    opacity: 0.5;
  }
  .actions-toggle:focus-visible {
    outline: 2px solid var(--info);
    outline-offset: 2px;
    border-radius: var(--radius-sm);
  }
  .toggle-caret {
    width: 0.8rem;
    text-align: center;
  }
  .toggle-count {
    padding: 0 0.35rem;
    border-radius: var(--radius-full);
    background: var(--bg-default);
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }
  .action-log {
    list-style: none;
    margin: 0;
    padding: 0.25rem 0 0;
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    max-height: 14rem;
    overflow-y: auto;
    border-top: 1px solid var(--border-subtle);
  }
  .action-row {
    display: grid;
    grid-template-columns: auto auto 1fr auto;
    gap: 0.5rem;
    align-items: baseline;
    font-size: var(--text-xs);
    padding-top: 0.25rem;
  }
  .action-kind {
    font-family: var(--font-mono, monospace);
    color: var(--fg-secondary);
    white-space: nowrap;
  }
  .action-subject {
    color: var(--text-muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 8rem;
  }
  .action-payload {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .action-time {
    color: var(--text-muted);
    font-size: var(--text-2xs);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
</style>
