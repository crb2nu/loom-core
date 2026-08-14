<script lang="ts">
  // MillsKPIRow renders the six canonical mills KPIs as a horizontal card
  // row for the Overview panel. Source data is the operator's
  // /api/mills/kpis snapshot; trend sparklines are computed from the
  // store's in-memory polling history. The row hides itself when the
  // mills operator is not configured so the Overview stays calm for users
  // who aren't running the mills.
  //
  // KPI selection follows .loom/89-research-agent-swarm-council-pipeline-2026-04-25.md §8.
  import { millsStore } from '../../stores/mills.svelte.ts';
  import { router } from '../../stores/router.svelte.ts';
  import MetricCard from '../shared/MetricCard.svelte';
  import { fmtCost, fmtDuration, fmtPct } from './shared/format.ts';

  let metrics = $derived(millsStore.kpis?.metrics ?? null);
  let disabled = $derived(millsStore.disabled);


  // Integer count: shows "0" (a real, meaningful value) rather than "—" so the
  // north-star merge count reads as data even when the derived ratios are idle.
  function fmtCount(v: number | undefined): string {
    if (v === undefined || !Number.isFinite(v)) return '—';
    return `${Math.round(v)}`;
  }

  // Trend direction relative to the first sample in history. Used to
  // pick the sparkline color so an "↓ trend" KPI like cost-per-merge
  // colors green on a downward trend, red on upward.
  type Direction = 'lower-better' | 'higher-better' | 'neutral';
  function trendColor(series: number[], direction: Direction): string {
    if (series.length < 2) return 'var(--info)';
    const delta = series[series.length - 1] - series[0];
    if (Math.abs(delta) < 1e-9) return 'var(--info)';
    const improving = (direction === 'lower-better' && delta < 0) ||
                      (direction === 'higher-better' && delta > 0);
    if (direction === 'neutral') return 'var(--info)';
    return improving ? 'var(--success)' : 'var(--warning)';
  }

  // Threshold-driven badge. Targets pulled from research doc §8 KPI
  // table. We deliberately keep these tight — the badge surfaces "needs
  // attention", not noise.
  type BadgeVariant = 'success' | 'warning' | 'error' | 'muted';
  function badge(value: number | undefined, opts: {
    target: number;
    direction: Direction;
    softMargin: number;
  }): { label: string; variant: BadgeVariant } | null {
    if (value === undefined || !Number.isFinite(value)) return null;
    const { target, direction, softMargin } = opts;
    const meets = direction === 'lower-better' ? value <= target : value >= target;
    if (meets) return { label: 'on target', variant: 'success' };
    const soft = direction === 'lower-better'
      ? value <= target * (1 + softMargin)
      : value >= target * (1 - softMargin);
    return soft
      ? { label: 'watch', variant: 'warning' }
      : { label: 'off target', variant: 'error' };
  }

  let cards = $derived.by(() => {
    const m = metrics ?? {};
    const costPerMerged = m.cost_per_merged_change_usd ?? m.cost_per_merged_pipeline_usd;
    const costSeries = millsStore.metricSeries('cost_per_merged_change_usd');
    const costFallbackSeries = millsStore.metricSeries('cost_per_merged_pipeline_usd');
    const cards: Array<{
      label: string;
      value: string;
      trend: number[];
      trendColor: string;
      badge?: string;
      badgeVariant?: BadgeVariant;
      proxy?: boolean;
    }> = [
      {
        // North-star: autonomous merges in the window. Always a real number
        // (incl. 0), so the row leads with signal even when the derived ratios
        // are idle/"—".
        label: 'Merges 24h',
        value: fmtCount(m.pipeline_merged_runs),
        trend: millsStore.metricSeries('pipeline_merged_runs'),
        trendColor: trendColor(millsStore.metricSeries('pipeline_merged_runs'), 'higher-better'),
      },
      {
        label: 'Cost / merged',
        value: fmtCost(costPerMerged),
        trend: costSeries.length > 0 ? costSeries : costFallbackSeries,
        trendColor: trendColor(costSeries.length > 0 ? costSeries : costFallbackSeries, 'lower-better'),
      },
      {
        label: 'Slice→merge p50',
        value: fmtDuration(m.slice_to_merge_p50_seconds == null ? undefined : m.slice_to_merge_p50_seconds * 1_000),
        trend: millsStore.metricSeries('slice_to_merge_p50_seconds'),
        trendColor: trendColor(millsStore.metricSeries('slice_to_merge_p50_seconds'), 'lower-better'),
      },
      {
        label: 'Gate pass rate',
        value: fmtPct(m.gate_pass_rate),
        trend: millsStore.metricSeries('gate_pass_rate'),
        trendColor: trendColor(millsStore.metricSeries('gate_pass_rate'), 'higher-better'),
        ...(badge(m.gate_pass_rate, { target: 0.85, direction: 'higher-better', softMargin: 0.10 }) ?? {}),
      },
      {
        label: 'Auto-merge rate',
        value: fmtPct(m.auto_merge_rate),
        trend: millsStore.metricSeries('auto_merge_rate'),
        trendColor: trendColor(millsStore.metricSeries('auto_merge_rate'), 'neutral'),
      },
      {
        label: 'Regression rate',
        value: fmtPct(m.regression_rate),
        trend: millsStore.metricSeries('regression_rate'),
        trendColor: trendColor(millsStore.metricSeries('regression_rate'), 'lower-better'),
        // Backend now derives this from the canonical regression-fix
        // label on merged backlog items. The signal is honest only to
        // the degree the label is being emitted; until file-overlap
        // detection lands the metric stays flagged as a proxy so the
        // operator interprets short-term moves (especially 0%)
        // accordingly.
        proxy: true,
        ...(badge(m.regression_rate, { target: 0.02, direction: 'lower-better', softMargin: 0.5 }) ?? {}),
      },
    ];
    return cards;
  });

  function gotoMills() {
    router.navigate('mills');
  }
</script>

{#if !disabled && metrics}
  <section class="mills-kpi-section">
    <div class="mills-kpi-header">
      <span class="mills-kpi-label">Mills</span>
      <span class="mills-kpi-note">Rolling 24h</span>
    </div>
    <div class="mills-kpi-grid">
      {#each cards as card (card.label)}
        <MetricCard
          label={card.label}
          value={card.value}
          trend={card.trend.length > 1 ? card.trend : undefined}
          trendColor={card.trendColor}
          badge={card.badge}
          badgeVariant={card.badgeVariant ?? 'muted'}
          proxy={card.proxy ?? false}
          compact
          onclick={gotoMills}
        />
      {/each}
    </div>
  </section>
{/if}

<style>
  .mills-kpi-section {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .mills-kpi-header {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .mills-kpi-label {
    font-size: var(--text-2xs);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-secondary);
  }

  .mills-kpi-note {
    font-size: var(--text-xs);
    color: var(--fg-muted);
    font-family: var(--font-mono);
  }

  /* auto-fit rather than a hard track count: the row builds six cards, and a
     fixed repeat(5) stranded the sixth alone on its own line. */
  .mills-kpi-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
    gap: var(--space-2);
  }

  @media (max-width: 900px) {
    .mills-kpi-grid {
      grid-template-columns: repeat(2, 1fr);
    }
  }

  @media (max-width: 480px) {
    .mills-kpi-grid {
      grid-template-columns: 1fr 1fr;
    }
  }
</style>
