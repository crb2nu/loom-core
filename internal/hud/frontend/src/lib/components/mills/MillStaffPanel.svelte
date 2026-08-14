<script lang="ts">
  /**
   * MillStaffPanel — "Mill Staff" (Mills family). One surface for the three
   * judgment lanes of docs/FACTORY_MODEL.md §3 (Drawing Office / Drawing-in /
   * The Alley) and the evidence the operator keeps about them.
   *
   * Departments read from the stores the individual lane panels already poll
   * (squads, overseers) plus the promotion report scoped to the council actor
   * family. The evidence tiles below give daylight to the five report
   * endpoints — promotion, judge calibration, regressions, config outcomes,
   * signature candidates — each of which was previously reachable only by
   * curling the operator.
   *
   * Read-only. Every report is window-bounded and may legitimately return no
   * evidence, so an empty tile states that rather than rendering blank.
   */
  import { millsStaffStore, STAFF_WINDOWS } from '../../stores/mills_staff.svelte.ts';
  import type {
    JudgeGate,
    PromotionActor,
    ReportSlot,
    SignatureCandidate,
    StaffWindow,
  } from '../../stores/mills_staff.svelte.ts';
  import { millsOverseersStore } from '../../stores/mills_overseers.svelte.ts';
  import type { OverseerAgent, OverseerEvent } from '../../stores/mills_overseers.svelte.ts';
  import { millsSquadsStore } from '../../stores/mills_squads.svelte.ts';
  import type { SquadsListEntry } from '../../stores/mills_squads.svelte.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import MetricCard from '../shared/MetricCard.svelte';
  import Badge from '../../widgets/Badge.svelte';
  import { relativeTime } from '../../utils/format.ts';

  // Three cadences on purpose: the lane snapshots are cheap reads that move
  // every tick, the reports are window-bounded scans that move over hours.
  $effect(() => {
    millsStaffStore.startPolling(60000);
    millsOverseersStore.startPolling(15000);
    millsSquadsStore.startPolling(15000);
    return () => {
      millsStaffStore.stopPolling();
      millsOverseersStore.stopPolling();
      millsSquadsStore.stopPolling();
    };
  });

  let staffWindow = $derived(millsStaffStore.window);

  // ---- Departments ----

  let councilPromotion = $derived(millsStaffStore.councilPromotion);
  let councilActors = $derived<PromotionActor[]>(councilPromotion.data?.per_actor ?? []);
  let councilExecuted = $derived(councilPromotion.data?.total_executed ?? 0);
  let councilDryRun = $derived(councilPromotion.data?.total_dry_run ?? 0);

  let squads = $derived<SquadsListEntry[]>(millsSquadsStore.state ?? []);
  let squadsDisabled = $derived(millsSquadsStore.disabled);

  let overseers = $derived<OverseerAgent[]>(millsOverseersStore.status?.agents ?? []);
  let overseersEnabled = $derived(millsOverseersStore.status?.enabled === true);
  let overseersDisabled = $derived(millsOverseersStore.disabled);

  // ---- Recent-actions strip ----

  // The overseers status keys recent_actions by actor; today those are the
  // three Alley agents, and any council.mutator / reconciler rows the operator
  // adds later flow through unchanged. Flattened newest-first across staff so
  // the strip reads as one floor log, not three.
  let recentActions = $derived.by<Array<{ actor: string; ev: OverseerEvent }>>(() => {
    const byActor = millsOverseersStore.status?.recent_actions ?? {};
    const rows: Array<{ actor: string; ev: OverseerEvent }> = [];
    for (const [actor, events] of Object.entries(byActor)) {
      for (const ev of events ?? []) {
        if (ev) rows.push({ actor, ev });
      }
    }
    rows.sort((a, b) => Date.parse(b.ev.OccurredAt ?? '') - Date.parse(a.ev.OccurredAt ?? ''));
    return rows.slice(0, 24);
  });

  // ---- Evidence tiles ----

  let promotion = $derived(millsStaffStore.promotion);
  let judge = $derived(millsStaffStore.judge);
  let regressions = $derived(millsStaffStore.regressions);
  let configOutcomes = $derived(millsStaffStore.configOutcomes);
  let signatures = $derived(millsStaffStore.signatures);

  // Gates with enough of both outcomes to read a discrimination from. A gate
  // that only ever graded merged work has no escalated mean to compare.
  let judgeGates = $derived<JudgeGate[]>(judge.data?.per_gate ?? []);

  let newestRegression = $derived(regressions.data?.regressions?.[0] ?? null);

  let topCandidates = $derived<SignatureCandidate[]>(
    [...(signatures.data?.candidates ?? [])]
      .sort((a, b) => (b.member_count ?? 0) - (a.member_count ?? 0))
      .slice(0, 3),
  );

  // Tile render states, derived rather than declared inline: {@const} is only
  // legal as the immediate child of a block, and the tiles sit in a plain grid.
  let promotionState = $derived(slotState(promotion, promotion.data?.zero_evidence === true));
  let judgeState = $derived(slotState(judge, judge.data?.zero_evidence === true));
  let regressionsState = $derived(slotState(regressions, (regressions.data?.count ?? 0) === 0));
  let configState = $derived(slotState(configOutcomes, configOutcomes.data?.zero_evidence === true));
  let signaturesState = $derived(slotState(signatures, (signatures.data?.count ?? 0) === 0));

  let expanded = $state<Record<string, boolean>>({});

  function toggle(id: string): void {
    expanded = { ...expanded, [id]: !expanded[id] };
  }

  // slotState collapses a report slot into the one word the tile renders when
  // it has nothing to show. `ok` means render the numbers.
  function slotState(slot: ReportSlot<unknown>, zero: boolean): 'disabled' | 'error' | 'zero' | 'ok' {
    if (slot.disabled) return 'disabled';
    if (slot.error && slot.data == null) return 'error';
    if (slot.data == null) return 'zero';
    return zero ? 'zero' : 'ok';
  }

  function pct(v: number | null | undefined): string {
    return `${Math.round((v ?? 0) * 100)}%`;
  }

  function usd(v: number | null | undefined): string {
    return `$${(v ?? 0).toFixed(2)}`;
  }

  function score(v: number | null | undefined): string {
    return (v ?? 0).toFixed(2);
  }

  function raw(value: unknown): string {
    try {
      return JSON.stringify(value, null, 2);
    } catch {
      return 'unserialisable payload';
    }
  }

  // Short display kind: drop the leading "<family>.<agent>." namespace so a row
  // reads "dedup_close.dryrun" not "overseer.groomer.dedup_close.dryrun".
  function shortKind(kind: string | null | undefined): string {
    const parts = (kind ?? '').split('.');
    return parts.length > 2 ? parts.slice(2).join('.') : (kind ?? '');
  }

  function overseerBadge(a: OverseerAgent): { text: string; variant: 'muted' | 'warning' | 'success' } {
    if (!a.enabled) return { text: 'disabled', variant: 'muted' };
    if (a.paused) return { text: 'paused', variant: 'warning' };
    return { text: 'active', variant: 'success' };
  }
</script>

<PanelShell
  title="Mill Staff"
  icon="⚑"
  loading={millsStaffStore.loading && promotion.lastUpdated == null}
>
  {#snippet toolbar()}
    <div class="staff-toolbar">
      <span class="toolbar-label">evidence window</span>
      <div class="window-buttons" role="group" aria-label="Evidence window">
        {#each STAFF_WINDOWS as w (w)}
          <button
            type="button"
            class="window-button"
            class:active={staffWindow === w}
            aria-pressed={staffWindow === w}
            onclick={() => millsStaffStore.setWindow(w as StaffWindow)}
          >
            {w}
          </button>
        {/each}
      </div>
    </div>
  {/snippet}

  <!-- ── Departments ─────────────────────────────────────────────────── -->
  <div class="dept-grid">
    <!-- Drawing Office (Council) -->
    <article class="dept-card">
      <header class="dept-header">
        <h3 class="dept-title">Drawing Office <span class="dept-code">(Council)</span></h3>
        <span class="dept-phase">plan-time</span>
      </header>
      <p class="dept-blurb">What enters the mill: briefs to proposals to backlog items.</p>
      {#if councilPromotion.disabled}
        <p class="dept-empty">Mills operator not configured.</p>
      {:else if councilPromotion.error && councilPromotion.data == null}
        <p class="dept-empty dept-error">{councilPromotion.error}</p>
      {:else if councilActors.length === 0}
        <p class="dept-empty">No council actions in window.</p>
      {:else}
        <dl class="dept-counts">
          <div class="count"><dt>executed</dt><dd>{councilExecuted}</dd></div>
          <div class="count"><dt>dry-run</dt><dd>{councilDryRun}</dd></div>
          <div class="count"><dt>actors</dt><dd>{councilActors.length}</dd></div>
        </dl>
        <ul class="dept-list">
          {#each councilActors as actor (actor.actor)}
            {@const actions = (actor.per_action ?? []).length}
            <li>
              <span class="dept-row-name">{actor.actor}</span>
              <span class="dept-row-meta">{actions} {actions === 1 ? 'action' : 'actions'}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </article>

    <!-- Drawing-in (Squads) -->
    <article class="dept-card">
      <header class="dept-header">
        <h3 class="dept-title">Drawing-in <span class="dept-code">(Squads)</span></h3>
        <span class="dept-phase">dispatch-time</span>
      </header>
      <p class="dept-blurb">Which crew a committed run is bound to; outcome memory feeds routing.</p>
      {#if squadsDisabled}
        <p class="dept-empty">Mills operator not configured.</p>
      {:else if squads.length === 0}
        <p class="dept-empty">No squads registered.</p>
      {:else}
        <ul class="dept-list">
          {#each squads as entry (entry.squad?.ID ?? entry.squad?.Name)}
            <li>
              <span class="dept-row-name">{entry.squad?.Name ?? 'unnamed'}</span>
              <span class="dept-row-meta">
                {entry.outcome_stats?.total ?? 0} runs · {pct(entry.outcome_stats?.success_rate)} clean
              </span>
            </li>
          {/each}
        </ul>
      {/if}
    </article>

    <!-- The Alley (Overseers) -->
    <article class="dept-card">
      <header class="dept-header">
        <h3 class="dept-title">The Alley <span class="dept-code">(Overseers)</span></h3>
        <span class="dept-phase">floor-time</span>
      </header>
      <p class="dept-blurb">Guarded interventions on the running floor: close, demote, suppress.</p>
      {#if overseersDisabled}
        <p class="dept-empty">Mills operator not configured.</p>
      {:else if overseers.length === 0}
        <p class="dept-empty">No overseers registered.</p>
      {:else}
        <div class="alley-gate" class:on={overseersEnabled}>
          overseers {overseersEnabled ? 'enabled' : 'disabled'}
        </div>
        <ul class="dept-list">
          {#each overseers as agent (agent.name)}
            {@const badge = overseerBadge(agent)}
            <li>
              <span class="dept-row-name">{agent.name}</span>
              <span class="dept-row-badges">
                <Badge text={badge.text} variant={badge.variant} />
                {#if agent.dry_run}<Badge text="dry-run" variant="info" />{/if}
              </span>
              <span class="dept-row-meta">
                {agent.last_tick_at ? relativeTime(agent.last_tick_at) : 'never ticked'}
              </span>
            </li>
          {/each}
        </ul>
      {/if}
    </article>
  </div>

  <!-- ── Recent actions across staff ─────────────────────────────────── -->
  <section class="strip" aria-label="Recent staff actions">
    <h3 class="section-title">Recent actions <span class="section-count">{recentActions.length}</span></h3>
    {#if recentActions.length === 0}
      <p class="dept-empty">
        {overseersDisabled ? 'Mills operator not configured.' : 'No staff actions in the last 24h.'}
      </p>
    {:else}
      <ul class="action-log">
        {#each recentActions as row (row.actor + ':' + row.ev.ID)}
          <li class="action-row">
            <span class="action-actor">{row.actor}</span>
            <span class="action-kind">{shortKind(row.ev.Kind)}</span>
            <span class="action-subject" title="{row.ev.SubjectKind}:{row.ev.SubjectID}">
              {row.ev.SubjectID}
            </span>
            <span class="action-time">{relativeTime(row.ev.OccurredAt)}</span>
          </li>
        {/each}
      </ul>
    {/if}
  </section>

  <!-- ── Evidence tiles ──────────────────────────────────────────────── -->
  <section class="evidence" aria-label="Staff evidence reports">
    <h3 class="section-title">Evidence <span class="section-window">over {staffWindow}</span></h3>
    <div class="tile-grid">
      <!-- Promotion report -->
      <MetricCard
        label="Promotion"
        badge={promotion.data?.zero_evidence ? 'zero evidence' : ''}
        badgeVariant="warning"
        element="article"
      >
        {#if promotionState === 'disabled'}
          <p class="tile-empty">Mills operator not configured.</p>
        {:else if promotionState === 'error'}
          <p class="tile-empty tile-error">{promotion.error}</p>
        {:else if promotionState === 'zero'}
          <p class="tile-empty">No evidence in window.</p>
        {:else}
          <div class="tile-figures">
            <div class="figure"><span class="fig-value">{promotion.data?.total_dry_run ?? 0}</span><span class="fig-label">dry-run</span></div>
            <div class="figure"><span class="fig-value">{promotion.data?.total_executed ?? 0}</span><span class="fig-label">executed</span></div>
          </div>
          <p class="tile-sub">
            {promotion.data?.total_actions ?? 0} audited actions across
            {(promotion.data?.per_actor ?? []).length} actors under
            <code>{promotion.data?.actor_prefix}</code>
          </p>
        {/if}
        {#if promotion.error && promotion.data != null}
          <p class="tile-stale">stale — {promotion.error}</p>
        {/if}
        <button type="button" class="tile-toggle" onclick={() => toggle('promotion')} aria-expanded={!!expanded.promotion}>
          {expanded.promotion ? '▾' : '▸'} raw JSON
        </button>
        {#if expanded.promotion}
          <pre class="tile-raw">{raw(promotion.data)}</pre>
        {/if}
      </MetricCard>

      <!-- Judge calibration -->
      <MetricCard
        label="Judge calibration"
        badge={judge.data?.zero_evidence ? 'zero evidence' : ''}
        badgeVariant="warning"
        element="article"
      >
        {#if judgeState === 'disabled'}
          <p class="tile-empty">Mills operator not configured.</p>
        {:else if judgeState === 'error'}
          <p class="tile-empty tile-error">{judge.error}</p>
        {:else if judgeState === 'zero' || judgeGates.length === 0}
          <p class="tile-empty">No evidence in window.</p>
        {:else}
          <p class="tile-sub">
            {judge.data?.joined_verdicts ?? 0} of {judge.data?.total_verdicts ?? 0} verdicts joined
            to a terminal run
          </p>
          <ul class="gate-list">
            {#each judgeGates as gate (gate.gate)}
              <li class="gate-row" class:inverted={gate.mean_score_escalated > gate.mean_score_merged && gate.merged_verdicts > 0 && gate.escalated_verdicts > 0}>
                <span class="gate-name">{gate.gate}</span>
                <span class="gate-scores">
                  <span class="gate-merged" title="{gate.merged_verdicts} merged verdicts">{score(gate.mean_score_merged)}</span>
                  <span class="gate-arrow" aria-hidden="true">/</span>
                  <span class="gate-escalated" title="{gate.escalated_verdicts} escalated verdicts">{score(gate.mean_score_escalated)}</span>
                </span>
              </li>
            {/each}
          </ul>
          <p class="tile-legend">merged mean / escalated mean — a gate that discriminates scores merged work higher</p>
        {/if}
        {#if judge.error && judge.data != null}
          <p class="tile-stale">stale — {judge.error}</p>
        {/if}
        <button type="button" class="tile-toggle" onclick={() => toggle('judge')} aria-expanded={!!expanded.judge}>
          {expanded.judge ? '▾' : '▸'} raw JSON
        </button>
        {#if expanded.judge}
          <pre class="tile-raw">{raw(judge.data)}</pre>
        {/if}
      </MetricCard>

      <!-- Regressions -->
      <MetricCard
        label="Regressions"
        badge={(regressions.data?.count ?? 0) > 0 ? `${regressions.data?.count} reverted` : ''}
        badgeVariant="error"
        element="article"
      >
        {#if regressionsState === 'disabled'}
          <p class="tile-empty">Mills operator not configured.</p>
        {:else if regressionsState === 'error'}
          <p class="tile-empty tile-error">{regressions.error}</p>
        {:else if regressionsState === 'zero'}
          <p class="tile-empty">No evidence in window.</p>
        {:else}
          <div class="tile-figures">
            <div class="figure"><span class="fig-value">{regressions.data?.count ?? 0}</span><span class="fig-label">in {regressions.data?.window}</span></div>
          </div>
          {#if newestRegression}
            <p class="tile-sub">
              newest: <code>!{newestRegression.regressed_mr_iid}</code>
              {newestRegression.revert_title || 'revert'}
              <span class="tile-when">{relativeTime(newestRegression.attributed_at)}</span>
            </p>
          {/if}
        {/if}
        {#if regressions.error && regressions.data != null}
          <p class="tile-stale">stale — {regressions.error}</p>
        {/if}
        <button type="button" class="tile-toggle" onclick={() => toggle('regressions')} aria-expanded={!!expanded.regressions}>
          {expanded.regressions ? '▾' : '▸'} raw JSON
        </button>
        {#if expanded.regressions}
          <pre class="tile-raw">{raw(regressions.data)}</pre>
        {/if}
      </MetricCard>

      <!-- Config outcomes -->
      <MetricCard
        label="Config outcomes"
        badge={configOutcomes.data?.zero_evidence ? 'zero evidence' : ''}
        badgeVariant="warning"
        element="article"
      >
        {#if configState === 'disabled'}
          <p class="tile-empty">Mills operator not configured.</p>
        {:else if configState === 'error'}
          <p class="tile-empty tile-error">{configOutcomes.error}</p>
        {:else if configState === 'zero'}
          <p class="tile-empty">No evidence in window.</p>
        {:else}
          <div class="tile-figures">
            <div class="figure"><span class="fig-value">{pct(configOutcomes.data?.totals?.merge_rate)}</span><span class="fig-label">merge rate</span></div>
            <div class="figure"><span class="fig-value">{configOutcomes.data?.totals?.runs ?? 0}</span><span class="fig-label">runs</span></div>
            <div class="figure"><span class="fig-value">{usd(configOutcomes.data?.totals?.mean_cost_usd)}</span><span class="fig-label">mean cost</span></div>
          </div>
          <p class="tile-sub">
            {configOutcomes.data?.stamped_runs ?? 0} stamped ·
            {configOutcomes.data?.uncovered_runs ?? 0} uncovered ·
            {(configOutcomes.data?.per_policy_checksum ?? []).length} policy revisions
          </p>
        {/if}
        {#if configOutcomes.error && configOutcomes.data != null}
          <p class="tile-stale">stale — {configOutcomes.error}</p>
        {/if}
        <button type="button" class="tile-toggle" onclick={() => toggle('config')} aria-expanded={!!expanded.config}>
          {expanded.config ? '▾' : '▸'} raw JSON
        </button>
        {#if expanded.config}
          <pre class="tile-raw">{raw(configOutcomes.data)}</pre>
        {/if}
      </MetricCard>

      <!-- Signature candidates -->
      <MetricCard
        label="Signature candidates"
        badge={(signatures.data?.count ?? 0) > 0 ? `${signatures.data?.count} proposed` : ''}
        badgeVariant="info"
        element="article"
      >
        {#if signaturesState === 'disabled'}
          <p class="tile-empty">Mills operator not configured.</p>
        {:else if signaturesState === 'error'}
          <p class="tile-empty tile-error">{signatures.error}</p>
        {:else if signaturesState === 'zero'}
          <p class="tile-empty">No evidence in window.</p>
        {:else}
          <div class="tile-figures">
            <div class="figure"><span class="fig-value">{signatures.data?.count ?? 0}</span><span class="fig-label">candidates</span></div>
          </div>
          <ul class="phrase-list">
            {#each topCandidates as c (c.fingerprint)}
              <li>
                <span class="phrase">{c.phrase || '(empty phrase)'}</span>
                <span class="phrase-count">{c.member_count}×</span>
              </li>
            {/each}
          </ul>
        {/if}
        {#if signatures.error && signatures.data != null}
          <p class="tile-stale">stale — {signatures.error}</p>
        {/if}
        <button type="button" class="tile-toggle" onclick={() => toggle('signatures')} aria-expanded={!!expanded.signatures}>
          {expanded.signatures ? '▾' : '▸'} raw JSON
        </button>
        {#if expanded.signatures}
          <pre class="tile-raw">{raw(signatures.data)}</pre>
        {/if}
      </MetricCard>
    </div>
  </section>
</PanelShell>

<style>
  .staff-toolbar {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .toolbar-label {
    font-size: var(--mills-text-caption);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--mills-color-text-muted);
  }
  .window-buttons {
    display: inline-flex;
    gap: 2px;
  }
  .window-button {
    padding: 2px 10px;
    background: transparent;
    border: 1px solid var(--mills-color-border-subtle);
    border-radius: var(--mills-radius-control);
    color: var(--mills-color-text-muted);
    font-family: var(--font-mono);
    font-size: var(--mills-text-caption);
    cursor: pointer;
  }
  .window-button:hover {
    color: var(--mills-color-text);
    border-color: var(--mills-color-border);
  }
  .window-button.active {
    color: var(--mills-color-accent);
    border-color: var(--mills-color-accent);
    background: color-mix(in srgb, var(--mills-color-accent) 12%, transparent);
  }
  .window-button:focus-visible {
    outline: 2px solid var(--mills-color-border-focus);
    outline-offset: 1px;
  }

  .dept-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(19rem, 1fr));
    gap: var(--space-3);
  }
  .dept-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
    background: var(--mills-color-surface);
    border: 1px solid var(--mills-color-border-subtle);
    border-radius: var(--mills-radius-surface);
  }
  .dept-header {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
  }
  .dept-title {
    margin: 0;
    font-size: var(--mills-text-body);
    font-weight: 700;
    color: var(--mills-color-text);
  }
  .dept-code {
    font-weight: 500;
    color: var(--mills-color-text-muted);
  }
  .dept-phase {
    font-size: var(--mills-text-caption);
    font-family: var(--font-mono);
    color: var(--mills-color-text-muted);
  }
  .dept-blurb {
    margin: 0;
    font-size: var(--mills-text-caption);
    line-height: 1.5;
    color: var(--mills-color-text-secondary);
  }
  .dept-empty {
    margin: 0;
    font-size: var(--mills-text-label);
    color: var(--mills-color-text-muted);
  }
  .dept-error {
    color: var(--mills-color-error);
    word-break: break-word;
  }
  .dept-counts {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: var(--space-2);
    margin: 0;
  }
  .count {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 2px;
  }
  .count dt {
    font-size: var(--mills-text-caption);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    color: var(--mills-color-text-muted);
  }
  .count dd {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--mills-text-title);
    font-weight: 700;
    color: var(--mills-color-text);
  }
  .dept-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
    max-height: 11rem;
    overflow-y: auto;
  }
  .dept-list li {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    font-size: var(--mills-text-label);
  }
  .dept-row-name {
    font-weight: 600;
    color: var(--mills-color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .dept-row-badges {
    display: inline-flex;
    gap: 3px;
  }
  .dept-row-meta {
    margin-left: auto;
    font-family: var(--font-mono);
    font-size: var(--mills-text-caption);
    color: var(--mills-color-text-muted);
    white-space: nowrap;
  }
  .alley-gate {
    font-size: var(--mills-text-caption);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    font-weight: 600;
    color: var(--mills-color-text-muted);
  }
  .alley-gate.on {
    color: var(--mills-color-success);
  }

  .section-title {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    margin: var(--space-4) 0 var(--space-2);
    font-size: var(--mills-text-label);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    color: var(--mills-color-text-secondary);
  }
  .section-count,
  .section-window {
    font-family: var(--font-mono);
    font-weight: 500;
    text-transform: none;
    letter-spacing: 0;
    color: var(--mills-color-text-muted);
  }

  .action-log {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
    max-height: 13rem;
    overflow-y: auto;
    border-top: 1px solid var(--mills-color-border-subtle);
  }
  .action-row {
    display: grid;
    grid-template-columns: 6rem auto minmax(0, 1fr) auto;
    gap: var(--space-2);
    align-items: baseline;
    padding-top: 3px;
    font-size: var(--mills-text-caption);
  }
  .action-actor {
    font-weight: 600;
    color: var(--mills-color-text-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .action-kind {
    font-family: var(--font-mono);
    color: var(--mills-color-text);
    white-space: nowrap;
  }
  .action-subject,
  .action-time {
    color: var(--mills-color-text-muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .action-time {
    font-family: var(--font-mono);
  }

  .tile-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(20rem, 1fr));
    gap: var(--space-3);
  }
  .tile-empty {
    margin: 0;
    font-size: var(--mills-text-label);
    color: var(--mills-color-text-muted);
  }
  .tile-error {
    color: var(--mills-color-error);
    word-break: break-word;
  }
  .tile-stale {
    margin: 0;
    font-size: var(--mills-text-caption);
    color: var(--mills-color-warning);
    word-break: break-word;
  }
  .tile-figures {
    display: flex;
    gap: var(--space-4);
    flex-wrap: wrap;
  }
  .figure {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .fig-value {
    font-family: var(--font-mono);
    font-size: var(--mills-text-display);
    font-weight: 700;
    line-height: 1;
    color: var(--mills-color-text);
  }
  .fig-label {
    font-size: var(--mills-text-caption);
    text-transform: uppercase;
    letter-spacing: 0.03em;
    color: var(--mills-color-text-muted);
  }
  .tile-sub {
    margin: 0;
    font-size: var(--mills-text-caption);
    line-height: 1.5;
    color: var(--mills-color-text-secondary);
  }
  .tile-sub code {
    font-family: var(--font-mono);
    color: var(--mills-color-text);
  }
  .tile-when {
    color: var(--mills-color-text-muted);
  }
  .tile-legend {
    margin: 0;
    font-size: var(--mills-text-caption);
    color: var(--mills-color-text-muted);
  }

  .gate-list,
  .phrase-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 3px;
    max-height: 10rem;
    overflow-y: auto;
  }
  .gate-row,
  .phrase-list li {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
    font-size: var(--mills-text-label);
  }
  .gate-name {
    color: var(--mills-color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .gate-scores {
    font-family: var(--font-mono);
    white-space: nowrap;
  }
  .gate-merged {
    color: var(--mills-color-success);
  }
  .gate-escalated {
    color: var(--mills-color-text-muted);
  }
  .gate-arrow {
    color: var(--mills-color-border);
    padding: 0 2px;
  }
  /* An escalated mean above the merged mean is the gate grading backwards —
     the one reading in this tile that is a finding, not a number. */
  .gate-row.inverted .gate-escalated {
    color: var(--mills-color-error);
    font-weight: 700;
  }
  .phrase {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--mills-color-text);
  }
  .phrase-count {
    font-family: var(--font-mono);
    font-size: var(--mills-text-caption);
    color: var(--mills-color-text-muted);
  }

  .tile-toggle {
    align-self: flex-start;
    margin-top: auto;
    padding: 2px 0;
    background: transparent;
    border: none;
    color: var(--mills-color-text-muted);
    font-size: var(--mills-text-caption);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    cursor: pointer;
  }
  .tile-toggle:hover {
    color: var(--mills-color-accent);
  }
  .tile-toggle:focus-visible {
    outline: 2px solid var(--mills-color-border-focus);
    outline-offset: 2px;
    border-radius: var(--mills-radius-control);
  }
  .tile-raw {
    margin: 0;
    padding: var(--space-2);
    max-height: 18rem;
    overflow: auto;
    background: var(--mills-color-surface-raised);
    border: 1px solid var(--mills-color-border-subtle);
    border-radius: var(--mills-radius-control);
    font-family: var(--font-mono);
    font-size: var(--mills-text-caption);
    color: var(--mills-color-text-secondary);
    white-space: pre;
  }
</style>
