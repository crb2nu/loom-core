<script lang="ts">
  import { millsStore } from '../../stores/mills.svelte.ts';
  import { createPoller } from '../../utils/poller.ts';
  import {
    boltCosts,
    escalatedWaste,
    firstPassYield,
    mendingPile,
  } from '../../utils/millEfficiencyHelpers.ts';
  import MetricCard from '../shared/MetricCard.svelte';
  import { fmtCost, fmtPct } from './shared/format.ts';

  const archivePoller = createPoller(() => millsStore.refreshArchiveRuns(), 60_000);

  $effect(() => {
    void millsStore.refreshArchiveRuns();
    archivePoller.start();
    return () => archivePoller.stop();
  });

  let yieldReading = $derived(firstPassYield(millsStore.archiveRuns ?? []));
  let costs = $derived(boltCosts(millsStore.archiveRuns ?? []));
  let waste = $derived(escalatedWaste(millsStore.archiveRuns ?? []));
  let mendable = $derived(mendingPile(millsStore.archiveRuns ?? []));
</script>

<section class="efficiency-strip" aria-label="Mill efficiency, rolling seven days">
  <div class="strip-heading">
    <span>mill efficiency</span>
    <span class="window">seven-day cloth · today at the needle</span>
  </div>
  <div class="efficiency-grid">
    <MetricCard
      label="first-pass yield"
      value={fmtPct(yieldReading.today)}
      trend={yieldReading.daily}
      trendColor="var(--success)"
      compact
      sub="bolts ÷ bolts + sparks · today"
      hint="Daily successful bolts divided by successful bolts plus escalated runs."
    />
    <MetricCard
      label="true cost / bolt"
      value={fmtCost(costs.trueCostPerBolt)}
      color="var(--accent)"
      compact
      sub={`${fmtCost(costs.rawCostPerBolt)} raw · ${costs.bolts} bolts`}
      hint="All successful and escalated spend divided by successful bolts; raw excludes escalated spend."
    />
    <MetricCard
      label="waste · 7d"
      value={fmtCost(waste)}
      color={waste > 0 ? 'var(--warning)' : 'var(--success)'}
      compact
      sub="spend caught in escalated cloth"
    />
    <div class="mending-card" class:has-mending={mendable > 0}>
      <MetricCard
        label="mending pile"
        value={mendable}
        color={mendable > 0 ? 'var(--warning)' : 'var(--fg-primary)'}
        compact
        sub={mendable === 1 ? 'retryable piece ready to mend' : 'retryable pieces ready to mend'}
        hint="Escalated runs explicitly marked retryable in the seven-day window."
      />
    </div>
  </div>
</section>

<style>
  .efficiency-strip {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .strip-heading {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-3);
    color: var(--fg-secondary);
    font-size: var(--text-2xs);
    font-weight: 700;
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
  }

  .window {
    color: var(--fg-muted);
    font-size: var(--text-xs);
    font-weight: 400;
    letter-spacing: normal;
    text-transform: none;
  }

  .efficiency-grid {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: var(--space-2);
  }

  .mending-card {
    position: relative;
    min-width: 0;
  }

  .mending-card :global(.metric-card) {
    height: 100%;
    background-image:
      repeating-linear-gradient(0deg, transparent 0 5px, color-mix(in srgb, var(--warning) 5%, transparent) 5px 6px),
      repeating-linear-gradient(90deg, transparent 0 7px, color-mix(in srgb, var(--warning) 5%, transparent) 7px 8px);
  }

  .mending-card.has-mending :global(.metric-card) {
    border-style: dashed;
    border-color: color-mix(in srgb, var(--warning) 55%, var(--border));
  }

  @media (max-width: 900px) {
    .efficiency-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  }

  @media (max-width: 520px) {
    .strip-heading { align-items: flex-start; flex-direction: column; gap: 2px; }
    .efficiency-grid { grid-template-columns: 1fr; }
  }
</style>
