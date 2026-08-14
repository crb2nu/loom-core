<script lang="ts">
  /**
   * TelemetryPanel — "Mill Telemetry" (factory family). A window-scoped
   * roll-up of stage/gate telemetry across the whole run population, sourced
   * from GET /api/mills/telemetry/stages?window= (S5) plus the windowed KPI
   * snapshot. Where the Factory floor animates ONE loom live, this is the
   * plant-wide instrument panel: where time and money burn, which gates
   * false-fail, and what actually drives escalations.
   *
   * The endpoint is live on current builds. A deployment older than the route
   * answers 404, which the store flags as telemetryUnavailable — the panel
   * then says so explicitly. It used to render a committed production fixture
   * behind a "sample data" badge instead, which meant a 404 painted a full
   * dashboard of 2026-07-16 numbers that had nothing to do with this cluster.
   */
  import { millsStore } from '../../stores/mills.svelte.ts';
  import PanelHeader from '../shared/PanelHeader.svelte';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import EmptyState from '../shared/EmptyState.svelte';
  import MetricCard from '../shared/MetricCard.svelte';
  import { createPoller } from '../../utils/poller.ts';
  import {
    aggregateGatePassRate,
    escalationFunnel,
    escalationRate,
    failurePareto,
    fmtCount,
    fmtDurationSeconds,
    fmtMinutes,
    fmtPct,
    fmtUSD,
    gateHealth,
    modelEconomics,
    stageWaterfall,
    TELEMETRY_WINDOWS,
    windowLabel,
    type TelemetryWindow,
  } from '../../utils/telemetryHelpers.ts';

  // Named windowSel (not `window`) so it never shadows the global.
  let windowSel = $state<TelemetryWindow>('7d');

  // Status/disabled/error come off the shared mills poll like every other
  // mills panel. Only one mills sub-view is mounted at a time, so the
  // reset-on-start startPolling contract is safe here.
  $effect(() => {
    millsStore.startPolling(15000);
    return () => millsStore.stopPolling();
  });

  // A dedicated telemetry poller, re-created whenever the window changes so
  // both fetches always target the selected window. createPoller pauses on a
  // hidden tab and fires no initial tick, so we kick an explicit immediate
  // refresh on mount / window change.
  $effect(() => {
    const w = windowSel;
    void millsStore.refreshTelemetry(w);
    const poller = createPoller(() => millsStore.refreshTelemetry(w), 30_000);
    poller.start();
    return () => poller.stop();
  });

  let disabled = $derived(millsStore.disabled);
  let error = $derived(millsStore.telemetryError);
  // The route 404'd and no live report is cached — an operator/HUD version
  // fact, not a fetch failure and not "still loading".
  let unavailable = $derived(millsStore.telemetryUnavailable && millsStore.telemetryReport === null);
  let report = $derived(millsStore.telemetryReport);
  let metrics = $derived(millsStore.telemetryKpis?.metrics ?? null);

  // Stat tiles (a): escalation rate, retry burn ($ + minutes), cost/merged,
  // gate pass rate. KPI-sourced where the metric is canonical there; the
  // stage roll-up supplies retry burn and the sample-mode fallbacks.
  type Tile = { label: string; value: string; color: string; hint?: string };
  const COLOR: Record<'success' | 'warning' | 'error' | 'plain', string> = {
    success: 'var(--success)',
    warning: 'var(--warning)',
    error: 'var(--error)',
    plain: 'var(--fg-primary)',
  };
  let tiles = $derived.by<Tile[]>(() => {
    const r = report;
    if (!r) return [];
    const m = metrics ?? {};
    const escRate = m.escalation_rate ?? escalationRate(r.runs);
    const gateRate = m.gate_pass_rate ?? aggregateGatePassRate(r.gates);
    const costPerMerged = m.cost_per_merged_change_usd ?? m.cost_per_merged_pipeline_usd;
    return [
      {
        label: 'Escalation rate',
        value: fmtPct(escRate),
        color: escRate > 0.4 ? COLOR.error : escRate > 0.2 ? COLOR.warning : COLOR.success,
        hint: `${r.runs.escalated} escalated of ${r.runs.total} runs`,
      },
      {
        label: 'Retry burn',
        value: `${fmtUSD(r.runs.retry_burn_cost_usd)} · ${fmtMinutes(r.runs.retry_burn_seconds)}`,
        color: COLOR.plain,
        hint: 'Spend and wall-clock lost to stage attempts beyond the first',
      },
      {
        label: 'Cost / merged',
        value: fmtUSD(costPerMerged),
        color: COLOR.plain,
        hint: 'Pipeline spend per merged change over the window',
      },
      {
        label: 'Gate pass rate',
        value: fmtPct(gateRate),
        color: gateRate >= 0.85 ? COLOR.success : gateRate >= 0.7 ? COLOR.warning : COLOR.error,
        hint: 'Share of gate evaluations that passed',
      },
    ];
  });

  let bars = $derived(report ? stageWaterfall(report.stages) : []);
  let gates = $derived(report ? gateHealth(report.gates) : []);
  let funnel = $derived(report ? escalationFunnel(report.escalation_funnel) : []);
  let pareto = $derived(report ? failurePareto(report.failure_classes) : []);
  // model_economics is null-normalized to [] at the fetch boundary
  // (mills.svelte.ts), and modelEconomics tolerates an empty array, so this is
  // safe even on an older operator that omits the field.
  let econ = $derived(report ? modelEconomics(report.model_economics) : []);

  function funnelTone(outcome: string): string {
    // success-then-escalate is a gate false-fail (worth flagging amber);
    // error is a genuine stage failure (red); anything else is neutral.
    if (outcome === 'error') return 'var(--error)';
    if (outcome === 'success') return 'var(--warning)';
    return 'var(--fg-muted)';
  }
</script>

<div class="panel telemetry-panel">
  <PanelHeader title="Mill Telemetry" icon={'❖'} count={report ? report.runs.total : null}>
    {#snippet stats()}
      <span class="text-muted text-xs">stage &amp; gate roll-up across the run population</span>
    {/snippet}
    {#snippet actions()}
      <div class="window-toggle" role="tablist" aria-label="Telemetry window">
        {#each TELEMETRY_WINDOWS as w (w)}
          <button
            type="button"
            role="tab"
            class="window-tab"
            class:active={windowSel === w}
            aria-selected={windowSel === w}
            onclick={() => (windowSel = w)}
          >{windowLabel(w)}</button>
        {/each}
      </div>
    {/snippet}
  </PanelHeader>

  {#if error}
    <ErrorBanner prefix="Telemetry feed failed" message={error} />
  {/if}

  {#if disabled}
    <EmptyState
      icon={'◯'}
      heading="Mills telemetry is dark"
      description="The Mills operator is not configured on this daemon, so there is no telemetry to roll up."
    />
  {:else if unavailable}
    <!-- 404 with nothing cached: the connected deployment predates the
         telemetry route. A version fact, so name it — this is where the panel
         used to silently substitute a committed production fixture. -->
    <EmptyState
      icon={'◯'}
      heading="Telemetry endpoint not available"
      description="This deployment does not serve GET /api/mills/telemetry/stages. Stage and gate roll-ups will appear once the HUD and Mills operator are upgraded."
    />
  {:else if !report && !error}
    <!-- Spinner only while we're genuinely waiting: no report yet AND no
         error. On a hard fetch failure (500/timeout) with nothing cached the
         top-level ErrorBanner carries the state, so the spinner must not also
         render — otherwise the panel shows "Telemetry feed failed" and a
         perpetual "Loading telemetry…" together. -->
    <div class="tele-loading" role="status" aria-live="polite">
      <span class="tele-spinner" aria-hidden="true"></span>
      <span>Loading telemetry…</span>
    </div>
  {:else if !report}
    <!-- Hard fetch failure with nothing cached. The ErrorBanner above is the
         whole story; rendering the section scaffold underneath it would fill
         the pane with "No escalations in this window" lines that describe the
         absent response, not the cluster. -->
  {:else}
    <div class="tele-body">
      <!-- (a) Stat tiles -->
      <section class="tele-tiles">
        {#each tiles as tile (tile.label)}
          <MetricCard label={tile.label} value={tile.value} color={tile.color} hint={tile.hint} />
        {/each}
      </section>

      <!-- (b) Stage duration waterfall -->
      <section class="tele-section">
        <h3 class="tele-title">
          Stage waterfall
          <span class="tele-sub">p50 bar · p90 overlay · sorted slowest-first</span>
        </h3>
        <div class="waterfall">
          {#each bars as bar (bar.stage)}
            <div class="wf-row" class:high-error={bar.highError}>
              <div class="wf-name mono">{bar.stage}</div>
              <div class="wf-track" title="p50 {fmtDurationSeconds(bar.p50_seconds)} · p90 {fmtDurationSeconds(bar.p90_seconds)} · max {fmtDurationSeconds(bar.max_seconds)}">
                <div class="wf-p90" style="width: {bar.p90Frac * 100}%"></div>
                <div class="wf-p50" style="width: {bar.p50Frac * 100}%"></div>
              </div>
              <div class="wf-nums mono">
                <span class="wf-p50-num">{fmtDurationSeconds(bar.p50_seconds)}</span>
                <span class="wf-p90-num">/ {fmtDurationSeconds(bar.p90_seconds)}</span>
              </div>
              <div class="wf-err">
                <span class="err-badge" class:hot={bar.highError} title="{bar.errors} errors of {bar.attempts} attempts">
                  {fmtPct(bar.error_rate)}
                </span>
              </div>
            </div>
          {/each}
        </div>
        <div class="wf-legend">
          <span><i class="key key-p50"></i>p50</span>
          <span><i class="key key-p90"></i>p90</span>
          <span><i class="key key-err"></i>error rate &gt; 25%</span>
        </div>
      </section>

      <!-- (c) Gate health -->
      <section class="tele-section">
        <h3 class="tele-title">
          Gate health
          <span class="tele-sub">pass / fail / unparseable per gate</span>
        </h3>
        <div class="gate-grid">
          {#each gates as g (g.gate)}
            <div class="gate-row">
              <div class="gate-name mono">{g.gate}</div>
              <div class="gate-bar" title="{g.passes} pass · {g.fails} fail · {g.unparseable} unparseable · {g.evaluations} evaluations">
                <div class="seg seg-pass" style="width: {g.passFrac * 100}%"></div>
                <div class="seg seg-fail" style="width: {g.failFrac * 100}%"></div>
                <div class="seg seg-unparse" style="width: {g.unparseableFrac * 100}%"></div>
                <div class="seg seg-skip" style="width: {g.skipFrac * 100}%"></div>
              </div>
              <div class="gate-nums mono">
                <span class="n-pass">{g.passes}</span>
                {#if g.fails > 0}<span class="n-fail">{g.fails}✗</span>{/if}
                {#if g.unparseable > 0}<span class="n-unparse">{g.unparseable}?</span>{/if}
              </div>
            </div>
          {/each}
        </div>
        <div class="gate-legend">
          <span><i class="key key-pass"></i>pass</span>
          <span><i class="key key-fail"></i>fail</span>
          <span><i class="key key-unparse"></i>unparseable — judge answer couldn't be parsed (harness defect, not a real fail)</span>
        </div>
      </section>

      <div class="tele-cols">
        <!-- (d) Escalation funnel -->
        <section class="tele-section col">
          <h3 class="tele-title">
            Escalation funnel
            <span class="tele-sub">last stage · outcome before escalate</span>
          </h3>
          {#if funnel.length === 0}
            <div class="tele-empty">No escalations in this window.</div>
          {:else}
            <div class="funnel">
              {#each funnel as f (f.last_stage + ':' + f.outcome)}
                <div class="fn-row">
                  <div class="fn-name mono">
                    {f.last_stage}<span class="fn-outcome" style:color={funnelTone(f.outcome)}>:{f.outcome}</span>
                  </div>
                  <div class="fn-track">
                    <div class="fn-fill" style="width: {f.frac * 100}%; background: {funnelTone(f.outcome)}"></div>
                  </div>
                  <div class="fn-count mono">{f.count}</div>
                </div>
              {/each}
            </div>
          {/if}
        </section>

        <!-- (e) Failure Pareto -->
        <section class="tele-section col">
          <h3 class="tele-title">
            Failure Pareto
            <span class="tele-sub">stage · class by count</span>
          </h3>
          {#if pareto.length === 0}
            <div class="tele-empty">No classified failures in this window.</div>
          {:else}
            <div class="pareto">
              {#each pareto as p (p.stage + ':' + p.class)}
                <div class="pt-row">
                  <div class="pt-name mono">
                    <span class="pt-stage">{p.stage}</span>
                    <span class="pt-class">{p.class}</span>
                  </div>
                  <div class="pt-track">
                    <div class="pt-fill" style="width: {p.frac * 100}%"></div>
                  </div>
                  <div class="pt-nums mono">
                    <span class="pt-count">{fmtCount(p.count)}</span>
                    <span class="pt-share">{fmtPct(p.share)}</span>
                  </div>
                </div>
              {/each}
            </div>
          {/if}
        </section>
      </div>

      <!-- (f) Model economics — per (model, backend) tier cost + reliability -->
      <section class="tele-section">
        <h3 class="tele-title">
          Model economics
          <span class="tele-sub">cost &amp; reliability per model · backend · sorted by spend</span>
        </h3>
        {#if econ.length === 0}
          <div class="tele-empty">No model-attributed stage cost in this window.</div>
        {:else}
          <div class="me-table" role="table" aria-label="Model economics by cost">
            <div class="me-row me-head" role="row">
              <span role="columnheader">Model</span>
              <span role="columnheader">Backend</span>
              <span class="me-num" role="columnheader">Calls</span>
              <span class="me-num" role="columnheader">Cost</span>
              <span class="me-num" role="columnheader">Err rate</span>
              <span class="me-num" role="columnheader">Avg</span>
            </div>
            {#each econ as m (m.model + ':' + m.backend)}
              <div class="me-row" class:high-error={m.highError} role="row">
                <span class="me-model mono" role="cell" title={m.model}>
                  <span class="me-cost-bar" style="width: {m.costFrac * 100}%" aria-hidden="true"></span>
                  <span class="me-model-label">{m.model}</span>
                </span>
                <span class="me-backend mono" role="cell">{m.backend}</span>
                <span class="me-num mono" role="cell">{fmtCount(m.calls)}</span>
                <span class="me-num mono me-cost" role="cell">{fmtUSD(m.cost_usd)}</span>
                <span class="me-num mono" role="cell">
                  <span class="me-err" class:hot={m.highError} title="{m.errors} errors of {m.calls} calls">{fmtPct(m.error_rate)}</span>
                </span>
                <span class="me-num mono" role="cell">{fmtDurationSeconds(m.avg_seconds)}</span>
              </div>
            {/each}
          </div>
        {/if}
      </section>
    </div>
  {/if}
</div>

<style>
  .telemetry-panel {
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  .window-toggle {
    display: inline-flex;
    gap: 0.15rem;
    padding: 0.15rem;
    border-radius: var(--radius-md);
    background: var(--bg-subtle);
    border: 1px solid var(--border-subtle);
  }
  .window-tab {
    padding: 0.2rem 0.7rem;
    border: none;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--fg-muted);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    cursor: pointer;
    transition: background 0.1s ease-out, color 0.1s ease-out;
  }
  .window-tab:hover { color: var(--fg-primary); }
  .window-tab.active {
    background: color-mix(in srgb, var(--accent) 22%, transparent);
    color: var(--fg-primary);
  }

  .tele-loading {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    padding: var(--space-4);
    color: var(--fg-muted);
    font-size: var(--text-sm);
  }
  .tele-spinner {
    width: 15px;
    height: 15px;
    border-radius: 50%;
    border: 2px solid color-mix(in srgb, var(--accent) 30%, transparent);
    border-top-color: var(--accent);
    animation: tele-spin 0.7s linear infinite;
  }
  @keyframes tele-spin { to { transform: rotate(360deg); } }
  @media (prefers-reduced-motion: reduce) { .tele-spinner { animation-duration: 1.6s; } }

  .tele-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    overflow-y: auto;
    padding-top: var(--space-2);
  }

  .tele-tiles {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: var(--space-2);
  }
  @media (max-width: 720px) {
    .tele-tiles { grid-template-columns: repeat(2, 1fr); }
  }

  .tele-section {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
    padding: var(--space-3);
  }
  .tele-title {
    margin: 0 0 var(--space-3);
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--fg-primary);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    display: flex;
    align-items: baseline;
    gap: 0.6rem;
    flex-wrap: wrap;
  }
  .tele-sub {
    font-size: var(--text-2xs);
    font-weight: 400;
    text-transform: none;
    letter-spacing: 0;
    color: var(--fg-muted);
    font-family: var(--font-mono);
  }
  .tele-empty {
    font-size: var(--text-sm);
    color: var(--fg-muted);
    font-style: italic;
    padding: var(--space-2) 0;
  }
  .mono { font-family: var(--font-mono); }

  /* (b) Waterfall */
  .waterfall {
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
  }
  .wf-row {
    display: grid;
    grid-template-columns: 110px minmax(0, 1fr) auto 52px;
    gap: 0.6rem;
    align-items: center;
  }
  .wf-name {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .wf-row.high-error .wf-name { color: var(--error); }
  .wf-track {
    position: relative;
    height: 16px;
    background: var(--bg-primary);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-xs);
    overflow: hidden;
  }
  .wf-p90 {
    position: absolute;
    inset: 0 auto 0 0;
    height: 100%;
    background: rgba(var(--info-rgb), 0.22);
  }
  .wf-p50 {
    position: absolute;
    inset: 0 auto 0 0;
    height: 100%;
    background: var(--info);
    opacity: 0.85;
    border-radius: 0 var(--radius-xs) var(--radius-xs) 0;
  }
  .wf-row.high-error .wf-p50 { background: var(--error); }
  .wf-row.high-error .wf-p90 { background: rgba(var(--error-rgb), 0.2); }
  .wf-nums {
    font-size: var(--text-2xs);
    color: var(--fg-secondary);
    white-space: nowrap;
    text-align: right;
  }
  .wf-p90-num { color: var(--fg-muted); }
  .wf-err { text-align: right; }
  .err-badge {
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    color: var(--fg-muted);
    padding: 0.02rem 0.3rem;
    border-radius: var(--radius-xs);
    background: var(--bg-tertiary);
  }
  .err-badge.hot {
    color: var(--error);
    background: rgba(var(--error-rgb), 0.14);
  }
  .wf-legend,
  .gate-legend {
    display: flex;
    flex-wrap: wrap;
    gap: 0.9rem;
    margin-top: var(--space-3);
    font-size: var(--text-2xs);
    color: var(--fg-muted);
  }
  .key {
    display: inline-block;
    width: 9px;
    height: 9px;
    border-radius: var(--radius-xs);
    margin-right: 4px;
    vertical-align: middle;
  }
  .key-p50 { background: var(--info); }
  .key-p90 { background: rgba(var(--info-rgb), 0.22); }
  .key-err { background: var(--error); }
  .key-pass { background: var(--success); }
  .key-fail { background: var(--error); }
  .key-unparse {
    background: repeating-linear-gradient(
      45deg,
      var(--warning),
      var(--warning) 3px,
      transparent 3px,
      transparent 6px
    );
    background-color: rgba(var(--warning-rgb), 0.3);
  }

  /* (c) Gate health */
  .gate-grid {
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
  }
  .gate-row {
    display: grid;
    grid-template-columns: 130px minmax(0, 1fr) auto;
    gap: 0.6rem;
    align-items: center;
  }
  .gate-name {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .gate-bar {
    display: flex;
    height: 16px;
    background: var(--bg-primary);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-xs);
    overflow: hidden;
  }
  .seg { height: 100%; }
  .seg-pass { background: var(--success); opacity: 0.8; }
  .seg-fail { background: var(--error); opacity: 0.85; }
  .seg-skip { background: var(--fg-muted); opacity: 0.4; }
  /* Unparseable is a judge-harness defect, not a real fail — a striped
     texture makes it read as "noise" and keeps it distinct from the solid
     pass/fail/skip segments regardless of hue. */
  .seg-unparse {
    background-color: rgba(var(--warning-rgb), 0.35);
    background-image: repeating-linear-gradient(
      45deg,
      rgba(var(--warning-rgb), 0.9),
      rgba(var(--warning-rgb), 0.9) 3px,
      transparent 3px,
      transparent 6px
    );
  }
  .gate-nums {
    display: flex;
    gap: 0.4rem;
    font-size: var(--text-2xs);
    white-space: nowrap;
  }
  .n-pass { color: var(--success); }
  .n-fail { color: var(--error); }
  .n-unparse { color: var(--warning); }

  /* (d)/(e) two-column analytics */
  .tele-cols {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-4);
  }
  @media (max-width: 720px) {
    .tele-cols { grid-template-columns: 1fr; }
  }
  .col { margin: 0; }

  .funnel,
  .pareto {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  .fn-row,
  .pt-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) 90px auto;
    gap: 0.5rem;
    align-items: center;
  }
  .fn-name,
  .pt-name {
    font-size: var(--text-2xs);
    color: var(--fg-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .fn-outcome { font-weight: 600; }
  .pt-name { display: flex; gap: 0.4rem; align-items: baseline; }
  .pt-stage { color: var(--fg-secondary); }
  .pt-class { color: var(--fg-muted); font-size: var(--text-2xs); }
  .fn-track,
  .pt-track {
    height: 12px;
    background: var(--bg-primary);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-xs);
    overflow: hidden;
  }
  .fn-fill { height: 100%; opacity: 0.8; }
  .pt-fill { height: 100%; background: var(--accent); opacity: 0.75; }
  .fn-count,
  .pt-nums {
    font-size: var(--text-2xs);
    color: var(--fg-secondary);
    white-space: nowrap;
    text-align: right;
  }
  .pt-nums { display: flex; gap: 0.4rem; justify-content: flex-end; }
  .pt-share { color: var(--fg-muted); }

  /* (f) Model economics table */
  .me-table {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
  }
  .me-row {
    display: grid;
    grid-template-columns: minmax(0, 1.6fr) minmax(0, 0.9fr) 56px 68px 66px 60px;
    gap: 0.5rem;
    align-items: center;
    padding: 0.15rem 0;
  }
  .me-head {
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--fg-muted);
    border-bottom: 1px solid var(--border-subtle);
    padding-bottom: 0.3rem;
    margin-bottom: 0.15rem;
  }
  .me-num { text-align: right; }
  .me-model {
    position: relative;
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    padding: 0.1rem 0.3rem;
    border-radius: var(--radius-xs);
  }
  /* A subtle spend bar behind the model name gives the cost column an at-a-
     glance magnitude read without a separate chart. */
  .me-cost-bar {
    position: absolute;
    inset: 0 auto 0 0;
    height: 100%;
    background: color-mix(in srgb, var(--accent) 18%, transparent);
    border-radius: var(--radius-xs);
    z-index: 0;
  }
  .me-model-label { position: relative; z-index: 1; }
  .me-row.high-error .me-model { color: var(--error); }
  .me-backend {
    font-size: var(--text-2xs);
    color: var(--fg-muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .me-cost { color: var(--fg-primary); font-weight: 600; }
  .me-err {
    font-size: var(--text-2xs);
    padding: 0.02rem 0.3rem;
    border-radius: var(--radius-xs);
    background: var(--bg-tertiary);
    color: var(--fg-muted);
  }
  .me-err.hot { color: var(--error); background: rgba(var(--error-rgb), 0.14); }

  .text-muted { color: var(--fg-muted); }
  .text-xs { font-size: var(--text-xs); }
</style>
