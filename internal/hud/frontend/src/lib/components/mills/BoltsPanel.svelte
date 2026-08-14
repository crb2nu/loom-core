<script lang="ts">
  /**
   * BoltsPanel — the take-up roll: merged runs woven into the week's cloth.
   *
   * Shows the merged output as a persistent, inspectable, exportable
   * artifact: an inline tartan strip (7 days, one pick per terminal run)
   * plus a merged-runs table where every row is a real MR. Drives off
   * millsStore.fetchArchiveRuns (NOT pipelineHistory — that feeds the
   * loom's weave diff, per §6 rule 9 and BoltArchive). Export ("tartan of
   * the week") and the shift-report launch reuse the existing modals so
   * the offline SVG export is never regressed.
   */
  import { millsStore, type PipelineRun } from '../../stores/mills.svelte.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import DataTable from '../shared/DataTable.svelte';
  import MetricCard from '../shared/MetricCard.svelte';
  import LineageRibbon from './shared/LineageRibbon.svelte';
  import PipelineRunDetail from './PipelineRunDetail.svelte';
  import BoltArchive from './BoltArchive.svelte';
  import ShiftReport from './ShiftReport.svelte';
  import { archiveDays, archiveTotals, tartanSVG } from '../../utils/tartanHelpers.ts';
  import { seededPattern } from '../../utils/factoryHelpers.ts';
  import { createPoller } from '../../utils/poller.ts';
  import { relativeTime } from '../../utils/format.ts';
  import { fmtCost } from './shared/format.ts';
  import { mrURL } from '../../utils/gitlabLinks.ts';

  // Local load/error state for the archive fetch. millsStore.refreshArchiveRuns
  // deliberately swallows errors to keep the last-good archive across ticks, so
  // Bolts owns its own error surface (§3.4) and drives the initial fetch itself.
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Feed the shared store cache (same field Sparks reads and millFloorSpine /
  // boltRuns derive from) so the spine and table can never drift, while
  // capturing loading/error the swallowing refresh helper can't expose.
  async function loadArchive(initial = false): Promise<void> {
    if (initial) loading = true;
    try {
      const runs = await millsStore.fetchArchiveRuns();
      millsStore.archiveRuns = runs ?? [];
      error = null;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  // KPIs share the 15s mills cadence; the archive changes slowly so it gets a
  // dedicated 60s poller with an explicit initial fetch (no initial tick).
  $effect(() => {
    millsStore.startPolling(15000);
    void loadArchive(true);
    const poller = createPoller(() => loadArchive(false), 60_000);
    poller.start();
    return () => {
      poller.stop();
      millsStore.stopPolling();
    };
  });

  // Bolts are the archived runs wound onto the take-up roll (done/merged).
  let bolts = $derived(millsStore.boltRuns);

  // The inline tartan strip re-weaves the last 7 days of ALL terminal runs
  // (bolts + sparks, matching BoltArchive) so the quiet days show too.
  let days = $derived(archiveDays(millsStore.archiveRuns ?? [], 7, new Date()));
  let totals = $derived(archiveTotals(days));

  // Resolve live theme tokens to concrete colors so the inline strip renders
  // identically to the exported file (fallbacks mirror FactoryPanel/BoltArchive).
  let strip = $derived.by(() => {
    if (typeof document === 'undefined') return '';
    const css = getComputedStyle(document.documentElement);
    const triplet = (name: string, fallback: string) => css.getPropertyValue(name).trim() || fallback;
    const bolt = triplet('--success-rgb', '34, 224, 118');
    const spark = triplet('--warning-rgb', '255, 184, 48');
    const fog = triplet('--fg-rgb', '212, 238, 244');
    return tartanSVG(days, {
      title: 'mills · tartan of the week',
      colors: {
        bg: css.getPropertyValue('--bg-primary').trim() || '#0b0f14',
        bolt: `rgb(${bolt})`,
        spark: `rgb(${spark})`,
        fog: `rgb(${fog})`,
        dim: `rgba(${fog}, 0.35)`,
      },
    });
  });

  // North-star KPI: autonomous merges in the last 24h. Conditional KPI keys are
  // absent (not null) → guard the read and render "—" until the operator emits it.
  let mergedRuns24h = $derived(millsStore.kpis?.metrics?.pipeline_merged_runs);
  let mergedTrend = $derived(millsStore.metricSeries('pipeline_merged_runs'));

  // Plan / pattern-book attribution: join a run's BacklogID → backlog item's
  // PlanID (stamped plans embed their pattern slug) or Title. Built once per
  // backlog change; every array read normalized `?? []`.
  let planByBacklog = $derived.by(() => {
    const m = new Map<string, string>();
    for (const item of millsStore.backlog ?? []) {
      if (!item?.ID) continue;
      m.set(item.ID, item.PlanID || item.Title || '');
    }
    return m;
  });
  function planLabel(run: PipelineRun): string {
    return planByBacklog.get(run.BacklogID) || run.BacklogID || '—';
  }

  // Merged-runs table sort. Default: most recently wound first.
  let sortKey = $state('merged');
  let sortDir = $state<'asc' | 'desc'>('desc');
  const columns = [
    { key: 'run', label: 'run' },
    { key: 'bolt', label: 'bolt' },
    { key: 'plan', label: 'plan / book', hideBelow: 620 },
    { key: 'cost', label: 'cost', sortable: true, align: 'right' as const, width: '5.5rem' },
    { key: 'merged', label: 'merged', sortable: true, width: '7rem' },
    { key: 'swatch', label: 'swatch', hideBelow: 520, width: '5.5rem' },
  ];

  function endedMs(run: PipelineRun): number {
    const raw = run.EndedAt ?? run.StartedAt;
    const t = raw ? Date.parse(raw) : NaN;
    return Number.isFinite(t) ? t : 0;
  }

  let sortedBolts = $derived.by(() => {
    const rows = [...bolts];
    const dir = sortDir === 'asc' ? 1 : -1;
    rows.sort((a, b) => {
      let va: number;
      let vb: number;
      if (sortKey === 'cost') {
        va = a.CostUSD ?? 0;
        vb = b.CostUSD ?? 0;
      } else {
        va = endedMs(a);
        vb = endedMs(b);
      }
      return (va - vb) * dir;
    });
    return rows;
  });

  function targetProject(run: PipelineRun): string | undefined {
    return millsStore.backlog.find((item) => item.ID === run.BacklogID)?.TargetProject;
  }

  let copiedID = $state<string | null>(null);
  async function copyMR(run: PipelineRun): Promise<void> {
    if (run.MRIID == null) return;
    try {
      await navigator.clipboard.writeText(`!${run.MRIID}`);
      copiedID = run.ID;
      setTimeout(() => {
        if (copiedID === run.ID) copiedID = null;
      }, 1400);
    } catch {
      // Clipboard can be unavailable in odd embeds; the chip still shows the IID.
    }
  }

  function openRun(id: string): void {
    millsStore.openRunDetail(id);
  }

  // Tiny per-run swatch — the SAME seededPattern the live loom weaves, so a
  // bolt's swatch matches its cloth. Rendered as a fixed 20-cell strip.
  const SWATCH_N = 20;
  function swatchCells(id: string): boolean[] {
    return seededPattern(id, SWATCH_N);
  }

  let showArchive = $state(false);
  let showShift = $state(false);

  let isEmpty = $derived(!loading && !error && bolts.length === 0);
</script>

<PanelShell
  title="Bolts"
  icon="▤"
  count={loading ? null : bolts.length}
  {loading}
  {error}
  empty={isEmpty}
  emptyIcon="▤"
  emptyMessage="no cloth yet this week — the beam is still threading"
  emptyHint="Runs wind onto the take-up roll here once they merge."
  emptyTone="idle"
  errorHeading="couldn't unroll the archive"
>
  {#snippet actions()}
    <button
      type="button"
      class="btn btn-sm"
      onclick={() => (showArchive = true)}
      disabled={loading || error != null}
    >↓ export tartan</button>
    <button
      type="button"
      class="btn btn-sm"
      onclick={() => (showShift = true)}
      disabled={loading || error != null}
    >shift report ↗</button>
  {/snippet}

  <LineageRibbon mode="spine" segments={millsStore.millFloorSpine} current="bolts" />

  <div class="bolts-totals" aria-label="Week totals">
    <MetricCard
      label="bolts this week"
      value={totals.bolts}
      color="var(--success)"
    />
    <MetricCard
      label="cloth spend"
      value={fmtCost(totals.costUSD)}
    />
    <MetricCard
      label="merged (24h)"
      value={mergedRuns24h != null ? mergedRuns24h : '—'}
      color="var(--success)"
      badge="north star"
      badgeVariant="success"
      trend={mergedTrend.length > 1 ? mergedTrend : undefined}
      trendColor="var(--success)"
      hint="Autonomous merges in the last 24h — the mill-floor north-star metric."
    />
  </div>

  {#if strip}
    <div class="bolts-strip" role="img" aria-label={`tartan of the week — ${totals.bolts} bolts, ${totals.sparks} sparks`}>
      {@html strip}
    </div>
  {/if}

  <DataTable
    {columns}
    rows={sortedBolts}
    {sortKey}
    {sortDir}
    idKey="ID"
    stableLayout={true}
    rowLabel="bolt"
    onSort={(key, dir) => { sortKey = key; sortDir = dir; }}
    onRowClick={(run) => openRun(run.ID)}
  >
    {#snippet row({ row: run, hiddenColumns })}
      <td class="text-mono bolts-run">{run.BacklogID || run.ID}</td>
      <td>
        {#if run.MRIID != null}
          <span class="mr-affordances">
            <a
              class="bolt-chip"
              href={mrURL(targetProject(run), run.MRIID)}
              target="_blank"
              rel="noreferrer noopener"
              title={`Open merge request !${run.MRIID}`}
              onclick={(e) => e.stopPropagation()}
            >!{run.MRIID}</a>
            <button
              type="button"
              class="mr-copy"
              aria-label={`Copy merge request !${run.MRIID}`}
              title="Copy MR reference"
              onclick={(e) => { e.stopPropagation(); void copyMR(run); }}
            >{copiedID === run.ID ? '✓' : '⎘'}</button>
          </span>
        {:else}
          <span class="bolt-chip bolt-chip-none">—</span>
        {/if}
      </td>
      {#if !hiddenColumns.has('plan')}
        <td class="text-mono text-muted bolts-plan" title={planLabel(run)}>{planLabel(run)}</td>
      {/if}
      <td class="text-mono bolts-cost">{fmtCost(run.CostUSD)}</td>
      <td class="text-mono text-muted">{relativeTime(run.EndedAt ?? run.StartedAt)}</td>
      {#if !hiddenColumns.has('swatch')}
        <td class="bolts-swatch-cell">
          <svg class="bolts-swatch" viewBox="0 0 {SWATCH_N * 3} 10" width={SWATCH_N * 3} height="10" aria-hidden="true">
            {#each swatchCells(run.ID) as on, i (i)}
              <rect x={i * 3} y="1" width="2.4" height="8" fill="var(--success)" fill-opacity={on ? 0.85 : 0.28} />
            {/each}
          </svg>
        </td>
      {/if}
    {/snippet}
  </DataTable>
</PanelShell>

<PipelineRunDetail />

{#if showArchive}
  <BoltArchive onclose={() => (showArchive = false)} />
{/if}

{#if showShift}
  <ShiftReport onclose={() => (showShift = false)} />
{/if}

<style>
  .bolts-totals {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(9rem, 1fr));
    gap: var(--space-2);
    margin: var(--space-3) 0;
  }

  .bolts-strip {
    margin: 0 0 var(--space-3);
    padding: var(--space-2);
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    overflow-x: auto;
  }
  .bolts-strip :global(svg) {
    display: block;
    max-width: 100%;
    height: auto;
    border-radius: var(--radius-sm);
  }

  .bolts-run {
    font-weight: 600;
    color: var(--fg-primary);
    word-break: break-all;
  }

  .bolts-plan {
    max-width: 20ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .bolts-cost { text-align: right; }

  .mr-affordances { display: inline-flex; align-items: center; gap: 3px; }

  .bolt-chip {
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    color: var(--success);
    background: color-mix(in srgb, var(--success) 8%, transparent);
    border: 1px solid color-mix(in srgb, var(--success) 45%, transparent);
    border-radius: var(--radius-full);
    padding: 2px var(--space-2);
    cursor: pointer;
    white-space: nowrap;
    text-decoration: none;
  }
  .bolt-chip:hover { background: color-mix(in srgb, var(--success) 16%, transparent); }
  .bolt-chip-none {
    color: var(--fg-dim);
    background: none;
    border-color: var(--border-subtle);
    cursor: default;
  }
  .mr-copy {
    padding: 1px 3px;
    color: var(--fg-muted);
    background: transparent;
    border: 0;
    cursor: pointer;
  }
  .mr-copy:hover { color: var(--success); }

  .bolts-swatch-cell { width: 5.5rem; }
  .bolts-swatch {
    display: block;
    border-radius: var(--radius-xs);
  }
</style>
