<script lang="ts">
  /**
   * AlertsPanel — Operations ▸ Alerts.
   *
   * The operator triage surface over internal/hud/domain/alerting: the pipeline
   * alert engine's fired alerts, its configured rules, and the auto-fix
   * proposal/execution queue. Eleven desktop REST routes shipped with no HUD at
   * all, so "what is currently alerting" was a curl away and nothing else.
   *
   * The two halves are NOT equally live, and the copy says so rather than
   * implying capability the backend does not have:
   *
   *   Alerts   — real. The engine is constructed in internal/hud/embed.go and
   *              evaluated against every pipeline-monitor snapshot. Ack writes
   *              through to the engine's ring.
   *   Auto-fix — inert. `App.autofixEngine` is declared (internal/hud/app.go:262)
   *              and never assigned outside tests, so the list routes answer
   *              200-with-[] forever and every mutation answers 503. Diagnose /
   *              approve / reject are therefore attempted, not promised: the
   *              first 503 flips the section to a named not-configured state.
   *
   * And even with the engine wired, `retry` proposals do nothing: ExecuteAutoFix
   * marks them "succeeded" with the result string "pipeline retry requested"
   * without re-running anything (internal/hud/autofix/autofix.go:278). The
   * strategy chip carries that caveat so an operator does not approve a retry
   * expecting a pipeline to move.
   *
   * Mutations follow the sibling-panel path: shared ConfirmDialog for the
   * stray-click guard + runAdminAction for the outcome toast (as PolicyPanel
   * and ContextHealthPanel do).
   */
  import {
    alertsStore,
    formatGoDuration,
    isZeroTime,
    severityTone,
    type Alert,
    type AutoFixProposal,
  } from '../stores/alerts.svelte.ts';
  import PanelShell from './shared/PanelShell.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';
  import ConfirmDialog from './shared/ConfirmDialog.svelte';
  import Badge from '../widgets/Badge.svelte';
  import type { BadgeVariant } from '../utils/tokens.ts';
  import { runAdminAction } from './mills/shared/millsActions.ts';
  import { relativeTime } from '../utils/format.ts';

  $effect(() => {
    alertsStore.startPolling(20000);
    return () => alertsStore.stopPolling();
  });

  let active = $derived(alertsStore.activeAlerts);
  let handled = $derived(alertsStore.handledAlerts);
  let rules = $derived(alertsStore.rules);
  let pendingProposals = $derived(alertsStore.pendingProposals);
  let executions = $derived(alertsStore.executions);
  let diagnoses = $derived(alertsStore.diagnoses);
  let unavailable = $derived(alertsStore.unavailable);
  let autofixUnavailable = $derived(alertsStore.autofixUnavailable);
  let error = $derived(alertsStore.error);
  let loading = $derived(alertsStore.loading && alertsStore.alerts.length === 0);
  let severityCounts = $derived(alertsStore.severityCounts);

  const SEVERITY_VARIANTS: Record<string, BadgeVariant> = {
    critical: 'error',
    warning: 'warning',
    info: 'info',
  };

  function severityVariant(severity: string): BadgeVariant {
    return SEVERITY_VARIANTS[severity] ?? 'muted';
  }

  function execVariant(status: string): BadgeVariant {
    if (status === 'succeeded') return 'success';
    if (status === 'running') return 'info';
    if (status === 'rejected') return 'muted';
    if (status === 'failed') return 'error';
    return 'muted';
  }

  function strategyVariant(strategy: string): BadgeVariant {
    if (strategy === 'agent_fix') return 'accent';
    if (strategy === 'retry') return 'warning';
    return 'muted';
  }

  // Only `agent_fix` actually does work on approval. `retry` is the documented
  // no-op and `manual` records an immediate failure, so both get a caveat
  // instead of a silently misleading button.
  function strategyCaveat(strategy: string): string {
    if (strategy === 'retry') {
      return 'Approving records a "pipeline retry requested" success without re-running the pipeline (autofix.go:278 is a placeholder).';
    }
    if (strategy === 'manual') {
      return 'Approving records an immediate failure — the engine has no automated path for this category.';
    }
    return 'Approving spawns a fixer agent against the diagnosed project.';
  }

  /** Diagnosis needs both a project and a pipeline id; the handler 400s without. */
  function canDiagnose(a: Alert): boolean {
    return Boolean(a.pipeline?.project) && (a.pipeline?.id ?? 0) > 0;
  }

  function confidencePct(c: number): string {
    return `${Math.round((c ?? 0) * 100)}%`;
  }

  // ---- Mutations ----

  type Pending =
    | { kind: 'diagnose'; alert: Alert }
    | { kind: 'approve'; proposal: AutoFixProposal }
    | { kind: 'reject'; proposal: AutoFixProposal };

  let pending = $state<Pending | null>(null);

  let confirmCopy = $derived.by(() => {
    if (!pending) return null;
    if (pending.kind === 'diagnose') {
      return {
        title: 'Run LLM diagnosis?',
        message: `Pulls pipeline ${pending.alert.pipeline.id} on ${pending.alert.pipeline.project}, reads every failed job's trace, and spends one model completion. It also mints an auto-fix proposal from the result.`,
        confirmLabel: 'Diagnose',
        variant: 'warn' as const,
      };
    }
    if (pending.kind === 'approve') {
      return {
        title: 'Approve this auto-fix?',
        message: strategyCaveat(pending.proposal.strategy),
        confirmLabel: 'Approve',
        variant: 'warn' as const,
      };
    }
    return {
      title: 'Reject this auto-fix?',
      message:
        'Records a rejected execution against the proposal. The proposal itself is kept in the engine’s history.',
      confirmLabel: 'Reject',
      variant: 'danger' as const,
    };
  });

  async function ack(a: Alert): Promise<void> {
    await runAdminAction(() => alertsStore.ack(a.id), {
      success: `Acknowledged ${a.rule_name}`,
      failurePrefix: 'Acknowledge failed',
    });
  }

  async function runPending(): Promise<void> {
    const req = pending;
    pending = null;
    if (!req) return;

    if (req.kind === 'diagnose') {
      await runAdminAction(() => alertsStore.diagnose(req.alert), {
        success: `Diagnosis complete for pipeline ${req.alert.pipeline.id}`,
        failurePrefix: 'Diagnosis failed',
      });
      return;
    }
    if (req.kind === 'approve') {
      await runAdminAction(() => alertsStore.approve(req.proposal.id), {
        success: `Proposal ${req.proposal.id} approved`,
        failurePrefix: 'Approve failed',
      });
      return;
    }
    await runAdminAction(() => alertsStore.reject(req.proposal.id), {
      success: `Proposal ${req.proposal.id} rejected`,
      failurePrefix: 'Reject failed',
    });
  }
</script>

<PanelShell
  title="Alerts"
  icon="⚑"
  count={active.length}
  loading={loading}
  error={!unavailable && error && alertsStore.alerts.length === 0 ? error : null}
  errorHeading="Couldn't load alerts"
  empty={unavailable}
  emptyIcon="◯"
  emptyMessage="Alerting not available on this build"
  emptyHint="This HUD registers no /api/alerts routes. The pipeline alert engine ships with the GitLab pipeline monitor enabled."
  emptyTone="disabled"
>
  {#snippet header()}
    <div class="summary-line">
      <span class="chips">
        {#each severityCounts as [sev, n] (sev)}
          <Badge text={`${sev} ${n}`} variant={severityVariant(sev)} />
        {/each}
        {#if severityCounts.length === 0}
          <span class="dim">no active alerts</span>
        {/if}
      </span>
      <span class="meta dim">
        {alertsStore.enabledRuleCount}/{rules.length} rule{rules.length === 1 ? '' : 's'} enabled ·
        {handled.length} acknowledged · {pendingProposals.length} proposal{pendingProposals.length === 1 ? '' : 's'} pending
      </span>
    </div>
  {/snippet}

  {#if error && alertsStore.alerts.length > 0}
    <ErrorBanner prefix="Alert refresh failed" message={error} />
  {/if}

  <!-- ── Active alerts ── -->
  <section class="block">
    <h3>Active</h3>
    {#if active.length === 0}
      <p class="dim empty-note">
        No active alerts. The engine evaluates every pipeline-monitor snapshot
        against its rules below and fires here; acknowledged alerts move to the
        history at the bottom of this panel.
      </p>
    {:else}
      <ul class="alert-list">
        {#each active as a (a.id)}
          <li class="alert tone-{severityTone(a.severity)}">
            <div class="alert-head">
              <Badge text={a.severity} variant={severityVariant(a.severity)} />
              <span class="alert-title">{a.title}</span>
              <span class="alert-age dim mono">{relativeTime(a.fired_at)}</span>
            </div>
            <p class="alert-message">{a.message}</p>
            <div class="alert-source dim mono">
              <span>{a.rule_name}</span>
              {#if a.pipeline?.project}
                <span aria-hidden="true">·</span>
                {#if a.pipeline.url}
                  <a href={a.pipeline.url} target="_blank" rel="noreferrer noopener"
                    >{a.pipeline.project}#{a.pipeline.id}</a
                  >
                {:else}
                  <span>{a.pipeline.project}#{a.pipeline.id}</span>
                {/if}
                {#if a.pipeline.ref}<span class="ref">{a.pipeline.ref}</span>{/if}
                {#if a.pipeline.status}<span>({a.pipeline.status})</span>{/if}
              {/if}
            </div>

            {#if diagnoses[a.id]}
              {@const d = diagnoses[a.id]}
              <div class="diagnosis">
                <div class="diagnosis-head">
                  <Badge text={d.diagnosis.category} variant="accent" />
                  <span class="mono dim">confidence {confidencePct(d.diagnosis.confidence)}</span>
                </div>
                <p class="diagnosis-line"><strong>Root cause</strong> — {d.diagnosis.root_cause}</p>
                <p class="diagnosis-line"><strong>Suggested fix</strong> — {d.diagnosis.suggested_fix}</p>
                {#if d.diagnosis.failed_jobs && d.diagnosis.failed_jobs.length > 0}
                  <p class="diagnosis-line dim mono">
                    failed jobs: {d.diagnosis.failed_jobs.join(', ')}
                  </p>
                {/if}
                {#if d.proposal}
                  <p class="diagnosis-line dim">
                    Proposed <Badge
                      text={d.proposal.strategy}
                      variant={strategyVariant(d.proposal.strategy)}
                    /> — see Auto-fix proposals below.
                  </p>
                {/if}
              </div>
            {/if}

            <div class="alert-actions">
              <button
                type="button"
                class="btn-quiet"
                disabled={alertsStore.acking === a.id}
                onclick={() => ack(a)}
              >
                {alertsStore.acking === a.id ? 'acking…' : 'acknowledge'}
              </button>
              <button
                type="button"
                class="btn-quiet"
                disabled={!canDiagnose(a) || autofixUnavailable || alertsStore.diagnosing !== null}
                title={autofixUnavailable
                  ? 'The auto-fix engine is not configured on this HUD (503 from /api/alerts/diagnose).'
                  : canDiagnose(a)
                    ? 'Runs an LLM diagnosis over the failed pipeline. Slow — it reads every failed job trace.'
                    : 'This alert carries no pipeline reference to diagnose.'}
                onclick={() => (pending = { kind: 'diagnose', alert: a })}
              >
                {alertsStore.diagnosing === a.id ? 'diagnosing…' : 'diagnose'}
              </button>
            </div>
          </li>
        {/each}
      </ul>
    {/if}
  </section>

  <!-- ── Auto-fix proposals ── -->
  <section class="block">
    <h3>Auto-fix proposals</h3>
    {#if autofixUnavailable}
      <p class="dim empty-note">
        The auto-fix engine is not configured on this HUD — it answered 503
        “auto-fix engine not configured”. Diagnose, approve, and reject are
        disabled until it is wired; the proposal and execution lists will keep
        answering with an empty array regardless.
      </p>
    {:else if pendingProposals.length === 0}
      <p class="dim empty-note">
        No pending proposals. Proposals are only minted by a diagnosis, and a
        proposal leaves this list as soon as it has an execution — approve and
        reject both record one.
      </p>
    {:else}
      <ul class="proposal-list">
        {#each pendingProposals as p (p.id)}
          <li class="proposal">
            <div class="proposal-head">
              <Badge text={p.strategy} variant={strategyVariant(p.strategy)} />
              <span class="mono dim">confidence {confidencePct(p.confidence)}</span>
              {#if !p.requires_approval}
                <Badge text="auto-approved" variant="muted" />
              {/if}
              <span class="proposal-age dim mono">{relativeTime(p.created_at)}</span>
            </div>
            <p class="proposal-desc">{p.description || 'No description supplied by the diagnosis.'}</p>
            <p class="proposal-caveat dim">{strategyCaveat(p.strategy)}</p>
            {#if p.estimated_files && p.estimated_files.length > 0}
              <p class="dim mono proposal-files">{p.estimated_files.join(', ')}</p>
            {/if}
            <div class="proposal-actions">
              <button
                type="button"
                class="btn-approve"
                disabled={alertsStore.busyProposal === p.id}
                onclick={() => (pending = { kind: 'approve', proposal: p })}
              >
                {alertsStore.busyProposal === p.id ? 'Working…' : 'Approve'}
              </button>
              <button
                type="button"
                class="btn-reject"
                disabled={alertsStore.busyProposal === p.id}
                onclick={() => (pending = { kind: 'reject', proposal: p })}
              >Reject</button>
            </div>
          </li>
        {/each}
      </ul>
    {/if}
  </section>

  <!-- ── Execution history ── -->
  <section class="block">
    <h3>Auto-fix executions</h3>
    {#if executions.length === 0}
      <p class="dim empty-note">
        No executions recorded. Every approve and every reject appends one entry
        here; the list is the only record of what an auto-fix actually did.
      </p>
    {:else}
      <ul class="exec-list">
        {#each executions as e (e.id)}
          <li class="exec-row">
            <span class="exec-time dim mono">{relativeTime(e.started_at)}</span>
            <Badge text={e.status} variant={execVariant(e.status)} />
            <span class="exec-proposal mono dim">{e.proposal_id}</span>
            {#if e.spawn_id}<span class="mono dim">spawn {e.spawn_id}</span>{/if}
            {#if e.result}<span class="exec-result dim">{e.result}</span>{/if}
          </li>
        {/each}
      </ul>
    {/if}
  </section>

  <!-- ── Rules (read-only) ── -->
  <section class="block">
    <h3>Rules</h3>
    {#if rules.length === 0}
      <p class="dim empty-note">
        No rules configured. The engine seeds three defaults (pipeline failed,
        consecutive failures, pipeline stuck) on construction, so an empty list
        means the rule set was replaced with an empty one.
      </p>
    {:else}
      <div class="table-wrap">
        <table class="rules-table">
          <thead>
            <tr>
              <th>rule</th>
              <th>severity</th>
              <th>condition</th>
              <th class="col-num">threshold</th>
              <th class="col-num">cooldown</th>
              <th>last fired</th>
              <th>state</th>
            </tr>
          </thead>
          <tbody>
            {#each rules as r (r.id)}
              <tr class:is-disabled={!r.enabled}>
                <td>
                  <span class="rule-name">{r.name}</span>
                  <span class="rule-id mono dim">{r.id}</span>
                </td>
                <td><Badge text={r.severity} variant={severityVariant(r.severity)} /></td>
                <td class="mono dim">
                  {r.condition?.type || '—'}
                  {#if r.condition?.duration}<span> ≥ {formatGoDuration(r.condition.duration)}</span>{/if}
                  {#if r.condition?.projects && r.condition.projects.length > 0}
                    <span class="rule-projects">{r.condition.projects.join(', ')}</span>
                  {/if}
                </td>
                <td class="col-num mono">{r.condition?.threshold || '—'}</td>
                <td class="col-num mono">{formatGoDuration(r.cooldown)}</td>
                <td class="mono dim">{isZeroTime(r.last_fired) ? 'never' : relativeTime(r.last_fired)}</td>
                <td>
                  <Badge text={r.enabled ? 'enabled' : 'disabled'} variant={r.enabled ? 'success' : 'muted'} />
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <p class="dim rules-note">
        Read-only. Rules are editable over PUT /api/alerts/rules, which replaces
        the whole set in one call — an inline editor would need to round-trip
        every rule, so it is deliberately not wired here.
      </p>
    {/if}
  </section>

  <!-- ── Acknowledged history ── -->
  {#if handled.length > 0}
    <section class="block">
      <h3>Acknowledged</h3>
      <ul class="exec-list">
        {#each handled as a (a.id)}
          <li class="exec-row">
            <span class="exec-time dim mono">{relativeTime(a.fired_at)}</span>
            <Badge text={a.severity} variant={severityVariant(a.severity)} />
            <span class="exec-result">{a.title}</span>
            {#if a.acked_by}<span class="dim mono">by {a.acked_by}</span>{/if}
          </li>
        {/each}
      </ul>
    </section>
  {/if}
</PanelShell>

<ConfirmDialog
  open={pending !== null}
  title={confirmCopy?.title ?? ''}
  message={confirmCopy?.message ?? ''}
  confirmLabel={confirmCopy?.confirmLabel ?? 'Confirm'}
  variant={confirmCopy?.variant ?? 'default'}
  onConfirm={runPending}
  onCancel={() => (pending = null)}
/>

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
  .dim { color: var(--fg-muted); }
  .mono { font-family: var(--font-mono); }

  .block { margin-bottom: var(--space-5); }
  .block + .block {
    border-top: 1px solid var(--border-subtle);
    padding-top: var(--space-3);
  }

  .block h3 {
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
    max-width: 62ch;
  }

  .alert-list,
  .proposal-list,
  .exec-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }

  .exec-list { gap: 2px; }

  .alert,
  .proposal {
    border: 1px solid var(--border-subtle);
    border-left-width: 3px;
    border-radius: var(--radius-xs);
    padding: 0.55rem 0.7rem;
    background: var(--bg-subtle);
  }

  /* Severity drives the left rail so the list scans by colour before text. */
  .alert.tone-crit  { border-left-color: var(--error); }
  .alert.tone-warn  { border-left-color: var(--warning); }
  .alert.tone-info  { border-left-color: var(--info); }
  .alert.tone-muted { border-left-color: var(--border); }

  .proposal { border-left-color: var(--accent); }

  .alert-head,
  .proposal-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .alert-title {
    font-weight: 600;
    color: var(--fg-primary);
    font-size: var(--text-sm);
    flex: 1;
    min-width: 0;
  }

  .alert-age,
  .proposal-age { font-size: var(--text-xs); margin-left: auto; }

  .alert-message,
  .proposal-desc {
    margin: 0.3rem 0 0.25rem;
    font-size: var(--text-xs);
    line-height: 1.5;
    color: var(--fg-secondary);
  }

  .alert-source {
    display: flex;
    align-items: center;
    gap: 0.35rem;
    flex-wrap: wrap;
    font-size: var(--text-xs);
  }

  .alert-source a { color: var(--accent); text-decoration: none; }
  .alert-source a:hover { text-decoration: underline; }
  .ref { color: var(--fg-secondary); }

  .diagnosis {
    margin-top: 0.5rem;
    padding: 0.45rem 0.6rem;
    border-radius: var(--radius-xs);
    background: color-mix(in srgb, var(--accent) 7%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent) 25%, var(--border-subtle));
  }

  .diagnosis-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin-bottom: 0.25rem;
  }

  .diagnosis-line {
    margin: 0 0 0.2rem;
    font-size: var(--text-xs);
    line-height: 1.5;
    color: var(--fg-secondary);
  }

  .proposal-caveat,
  .proposal-files,
  .rules-note {
    margin: 0 0 0.35rem;
    font-size: var(--text-xs);
    line-height: 1.5;
  }

  .rules-note { margin-top: var(--space-2); max-width: 62ch; }

  .alert-actions,
  .proposal-actions {
    display: flex;
    gap: 0.4rem;
    margin-top: 0.45rem;
  }

  .proposal-actions { justify-content: flex-end; }

  .exec-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) 0;
    border-bottom: 1px solid var(--border-subtle);
    font-size: var(--text-xs);
    flex-wrap: wrap;
  }

  .exec-time { min-width: 5rem; }
  .exec-proposal { color: var(--fg-secondary); }
  .exec-result { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; }

  .table-wrap { overflow-x: auto; }

  .rules-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.85rem;
  }

  .rules-table thead th {
    text-align: left;
    font-size: 0.7rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--fg-muted);
    border-bottom: 1px solid var(--border-subtle);
    padding: 0.35rem 0.5rem;
    font-weight: 500;
  }

  .rules-table tbody td {
    padding: 0.4rem 0.5rem;
    border-bottom: 1px solid var(--border-subtle);
    vertical-align: middle;
  }

  .rules-table tbody tr.is-disabled td { opacity: 0.55; }

  .rule-name { display: block; color: var(--fg-primary); font-weight: 600; }
  .rule-id { display: block; font-size: var(--text-xs); }
  .rule-projects { display: block; font-size: var(--text-xs); }

  .col-num { text-align: right; width: 6rem; }

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

  .btn-quiet:disabled { opacity: 0.45; cursor: default; }

  .btn-approve,
  .btn-reject {
    padding: 0.25rem 0.7rem;
    border-radius: var(--radius-xs);
    border: 1px solid var(--border-subtle);
    font-size: 0.78rem;
    cursor: pointer;
    background: var(--bg-subtle);
    color: var(--fg-primary);
  }

  .btn-approve {
    background: color-mix(in srgb, var(--success) 16%, transparent);
    color: var(--success);
    border-color: color-mix(in srgb, var(--success) 35%, var(--border));
  }
  .btn-approve:hover:not(:disabled) { background: color-mix(in srgb, var(--success) 26%, transparent); }

  .btn-reject {
    background: color-mix(in srgb, var(--error) 14%, transparent);
    color: var(--error);
    border-color: color-mix(in srgb, var(--error) 35%, var(--border));
  }
  .btn-reject:hover:not(:disabled) { background: color-mix(in srgb, var(--error) 24%, transparent); }

  .btn-approve:disabled,
  .btn-reject:disabled { opacity: 0.6; cursor: progress; }
</style>
