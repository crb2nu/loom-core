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
  import {
    millsStore,
    type BacklogItem,
    type MergeQueueEntry,
    type PipelineRun,
  } from '../../stores/mills.svelte.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import DataTable from '../shared/DataTable.svelte';
  import MetricCard from '../shared/MetricCard.svelte';
  import LineageRibbon from './shared/LineageRibbon.svelte';
  import PipelineRunDetail from './PipelineRunDetail.svelte';
  import BoltArchive from './BoltArchive.svelte';
  import ShiftReport from './ShiftReport.svelte';
  import { archiveDays, archiveTotals, tartanSVG } from '../../utils/tartanHelpers.ts';
  import { createPoller } from '../../utils/poller.ts';
  import { relativeTime } from '../../utils/format.ts';
  import { fmtCost } from './shared/format.ts';
  import { mrURL } from '../../utils/gitlabLinks.ts';
  import OriginChip from './shared/OriginChip.svelte';
  import RepoChip from './shared/RepoChip.svelte';
  import {
    originOf,
    repoLabel,
    repoFacet,
    originFacet,
    whyLine,
    type OriginKind,
  } from './shared/provenance.ts';

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
  // The press rides the shared tick via mergeQueueActive (opt-in, like
  // historyActive) with one mount-time fetch so the lane shows immediately.
  $effect(() => {
    millsStore.startPolling(15000);
    millsStore.mergeQueueActive = true;
    void millsStore.fetchMergeQueue();
    void loadArchive(true);
    const poller = createPoller(() => loadArchive(false), 60_000);
    poller.start();
    return () => {
      poller.stop();
      millsStore.mergeQueueActive = false;
      millsStore.stopPolling();
    };
  });

  // The press (serial merge queue): entries FIFO as the operator returns
  // them; position is per-lane (project→target), matching the slot each
  // entry is actually waiting on.
  let pressEntries = $derived(millsStore.mergeQueue?.active ?? []);
  let settledEntries = $derived(millsStore.mergeQueue?.recent_settled ?? []);
  let pressEnabled = $derived(millsStore.mergeQueue?.summary?.enabled ?? null);
  let pressPositions = $derived.by(() => {
    const counts = new Map<string, number>();
    const out = new Map<number, number>();
    for (const e of pressEntries) {
      const lane = `${e.project}→${e.target_branch}`;
      const pos = (counts.get(lane) ?? 0) + 1;
      counts.set(lane, pos);
      out.set(e.id, pos);
    }
    return out;
  });
  const PRESS_STATE_LABELS: Record<string, string> = {
    queued: 'queued',
    rebasing: 'rebasing',
    awaiting_pipeline: 're-proving',
    merging: 'merging',
  };
  function pressStateLabel(e: MergeQueueEntry): string {
    return PRESS_STATE_LABELS[e.state] ?? e.state;
  }

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

  // Join a run's BacklogID → its backlog item once, and read repo / origin /
  // why off the result. The item carries TargetProject, Labels and CreatedBy on
  // every poll; before this the join existed only to feed mrURL(), so the repo
  // was resolved and then discarded.
  let itemByID = $derived.by(() => {
    const m = new Map<string, BacklogItem>();
    for (const item of millsStore.backlog ?? []) {
      if (item?.ID) m.set(item.ID, item);
    }
    return m;
  });
  function itemFor(run: PipelineRun): BacklogItem | undefined {
    return itemByID.get(run.BacklogID);
  }

  // The why-line: what this bolt actually was. Title first — the old plan/book
  // column preferred PlanID, which suppressed the readable reason on exactly
  // the plan-linked items that dominate the floor.
  function whyFor(run: PipelineRun): string {
    return whyLine(itemFor(run)) || run.BacklogID || '—';
  }

  // --- Facets -------------------------------------------------------------
  //
  // 159 items merged in the trailing 48h on the live floor, so "scroll and
  // read" is not a viable answer to "what landed in flexdeck this week".
  // Facets are computed off the UNFILTERED bolts so counts stay stable as the
  // operator narrows — a facet whose options vanish as you use it is a trap.
  let repoFilter = $state<string | null>(null);
  let originFilter = $state<OriginKind | null>(null);

  let boltItems = $derived(bolts.map((run) => itemFor(run) ?? {}) as BacklogItem[]);
  let repoOptions = $derived(repoFacet(boltItems));
  let originOptions = $derived(originFacet(boltItems));

  let filteredBolts = $derived(
    bolts.filter((run) => {
      const item = itemFor(run);
      if (repoFilter && repoLabel(item?.TargetProject) !== repoFilter) return false;
      if (originFilter && originOf(item).kind !== originFilter) return false;
      return true;
    }),
  );

  let filterActive = $derived(repoFilter != null || originFilter != null);
  function clearFilters(): void {
    repoFilter = null;
    originFilter = null;
  }

  // Merged-runs table sort. Default: most recently wound first.
  let sortKey = $state('merged');
  let sortDir = $state<'asc' | 'desc'>('desc');
  const columns = [
    { key: 'run', label: 'bolt / why' },
    { key: 'bolt', label: 'mr' },
    { key: 'repo', label: 'repo', hideBelow: 560, width: '8rem' },
    { key: 'origin', label: 'origin', hideBelow: 700, width: '7rem' },
    { key: 'cost', label: 'cost', sortable: true, align: 'right' as const, width: '5.5rem' },
    { key: 'merged', label: 'merged', sortable: true, width: '7rem' },
  ];

  function endedMs(run: PipelineRun): number {
    const raw = run.EndedAt ?? run.StartedAt;
    const t = raw ? Date.parse(raw) : NaN;
    return Number.isFinite(t) ? t : 0;
  }

  let sortedBolts = $derived.by(() => {
    const rows = [...filteredBolts];
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


  let showArchive = $state(false);
  let showShift = $state(false);

  // Entries waiting in the press keep the panel alive even on a quiet week:
  // PanelShell's empty state replaces the whole body, and a loaded lane
  // with zero merged bolts is exactly when the operator needs the press.
  let isEmpty = $derived(
    !loading && !error && bolts.length === 0 && pressEntries.length === 0 && settledEntries.length === 0,
  );
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

  <!-- The press: the serial merge lane proven cloth passes through on its
       way to the take-up roll. One entry per pipeline run holding or
       waiting on a lane's head slot (rebase → re-prove → merge). -->
  <div class="press" aria-label="Serial merge queue">
    <div class="press-head">
      <span class="press-tag">press · serial merge lane</span>
      {#if millsStore.mergeQueue}
        {#if pressEnabled === false}
          <span class="press-mode press-off">off — policy</span>
        {:else}
          <span class="press-mode">{pressEntries.length} in lane</span>
        {/if}
      {/if}
    </div>
    {#if millsStore.mergeQueueError}
      <div class="press-note press-error">press feed failed — {millsStore.mergeQueueError}</div>
    {:else if pressEnabled === false}
      <div class="press-note">serial lane disabled by policy — proven MRs merge directly</div>
    {:else if pressEntries.length === 0 && settledEntries.length === 0}
      <div class="press-note">press idle — lane clear, nothing waiting to merge</div>
    {:else if pressEntries.length > 0}
      <ol class="press-lane">
        {#each pressEntries as entry (entry.id)}
          <li class="press-entry">
            <span class="press-pos text-mono">#{pressPositions.get(entry.id)}</span>
            <span class="press-state press-state-{entry.state}">{pressStateLabel(entry)}</span>
            <a
              class="bolt-chip"
              href={mrURL(entry.project, entry.mr_iid)}
              target="_blank"
              rel="noreferrer noopener"
              title={`Open merge request !${entry.mr_iid}`}
            >!{entry.mr_iid}</a>
            <span class="press-id text-mono" title={entry.backlog_id || entry.pipeline_run_id}>
              {entry.backlog_id || entry.pipeline_run_id}
            </span>
            <span class="press-lane-label text-mono text-muted" title={`${entry.project} → ${entry.target_branch}`}>
              {entry.project}→{entry.target_branch}
            </span>
            {#if entry.attempts > 1}
              <span class="press-attempts">attempt {entry.attempts}</span>
            {/if}
            <span class="press-age text-muted">{relativeTime(entry.enqueued_at)}</span>
          </li>
        {/each}
      </ol>
    {/if}
    {#if !millsStore.mergeQueueError && pressEnabled !== false && settledEntries.length > 0}
      <div class="press-history-label">settled · last 24h</div>
      <ol class="press-history">
        {#each settledEntries as entry (entry.id)}
          <li class:press-evicted={entry.state === 'evicted'} class="press-settled-entry">
            <span class="press-state press-state-{entry.state}">{entry.state}</span>
            {#if entry.state === 'evicted' && entry.eviction_reason}
              <span class="press-reason">{entry.eviction_reason}</span>
            {/if}
            <a
              class="bolt-chip"
              href={mrURL(entry.project, entry.mr_iid)}
              target="_blank"
              rel="noreferrer noopener"
              title={`Open merge request !${entry.mr_iid}`}
            >!{entry.mr_iid}</a>
            <span class="press-id text-mono" title={entry.backlog_id || entry.pipeline_run_id}>
              {entry.backlog_id || entry.pipeline_run_id}
            </span>
            <span class="press-lane-label text-mono text-muted">{entry.project}→{entry.target_branch}</span>
            <span class="press-age text-muted">{relativeTime(entry.settled_at ?? entry.updated_at)}</span>
          </li>
        {/each}
      </ol>
    {/if}
  </div>

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

  <!-- Facet bar: narrow ~160 merges/48h down to the repo or author the
       operator is actually asking about. -->
  {#if repoOptions.length > 1 || originOptions.length > 1}
    <div class="facets" aria-label="Filter bolts">
      {#if repoOptions.length > 1}
        <div class="facet-row">
          <span class="facet-tag">repo</span>
          {#each repoOptions as opt (opt.value)}
            <button
              type="button"
              class="facet"
              class:on={repoFilter === opt.value}
              aria-pressed={repoFilter === opt.value}
              onclick={() => (repoFilter = repoFilter === opt.value ? null : opt.value)}
            >{opt.label}<span class="facet-n">{opt.count}</span></button>
          {/each}
        </div>
      {/if}
      {#if originOptions.length > 1}
        <div class="facet-row">
          <span class="facet-tag">origin</span>
          {#each originOptions as opt (opt.value)}
            <button
              type="button"
              class="facet"
              class:on={originFilter === opt.value}
              aria-pressed={originFilter === opt.value}
              onclick={() => (originFilter = originFilter === opt.value ? null : opt.value)}
            >{opt.label}<span class="facet-n">{opt.count}</span></button>
          {/each}
        </div>
      {/if}
      {#if filterActive}
        <button type="button" class="facet-clear" onclick={clearFilters}>
          clear · showing {filteredBolts.length} of {bolts.length}
        </button>
      {/if}
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
      <td class="bolts-run">
        <!-- Column flex lives on an inner wrapper so the td stays a real
             table-cell and keeps stable-layout's truncation rules. -->
        <div class="bolts-run-stack">
          <span class="bolts-why" title={whyFor(run)}>{whyFor(run)}</span>
          <span class="bolts-id text-mono text-muted">{run.BacklogID || run.ID}</span>
        </div>
      </td>
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
      {#if !hiddenColumns.has('repo')}
        <td><RepoChip targetProject={itemFor(run)?.TargetProject} /></td>
      {/if}
      {#if !hiddenColumns.has('origin')}
        <td><OriginChip item={itemFor(run)} /></td>
      {/if}
      <td class="text-mono bolts-cost">{fmtCost(run.CostUSD)}</td>
      <td class="text-mono text-muted">{relativeTime(run.EndedAt ?? run.StartedAt)}</td>
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
  .press {
    margin-top: var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
    overflow: hidden;
  }
  .press-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
    background: var(--bg-primary);
  }
  .press-tag {
    font-size: var(--text-2xs);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
    color: var(--accent);
    white-space: nowrap;
  }
  .press-mode {
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    color: var(--fg-muted);
    white-space: nowrap;
  }
  .press-off { color: var(--warning); }
  .press-note {
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--fg-dim);
  }
  .press-error { color: var(--warning); }
  .press-lane {
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .press-entry {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    border-bottom: 1px solid var(--border-subtle);
    flex-wrap: wrap;
  }
  .press-entry:last-child { border-bottom: 0; }
  .press-pos { color: var(--fg-dim); }
  .press-state {
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    padding: 1px var(--space-2);
    border-radius: var(--radius-full);
    border: 1px solid var(--border-subtle);
    color: var(--fg-muted);
    white-space: nowrap;
  }
  .press-state-rebasing { color: var(--info); border-color: color-mix(in srgb, var(--info) 45%, transparent); }
  .press-state-awaiting_pipeline { color: var(--accent); border-color: color-mix(in srgb, var(--accent) 45%, transparent); }
  .press-state-merging { color: var(--success); border-color: color-mix(in srgb, var(--success) 45%, transparent); }
  .press-state-merged { color: var(--success); border-color: transparent; }
  .press-state-evicted { color: var(--warning); border-color: color-mix(in srgb, var(--warning) 55%, transparent); }
  .press-id {
    color: var(--fg-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 26ch;
  }
  .press-lane-label {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 24ch;
  }
  .press-attempts { color: var(--warning); font-size: var(--text-2xs); white-space: nowrap; }
  .press-age { margin-left: auto; font-size: var(--text-2xs); white-space: nowrap; }
  .press-history-label {
    padding: var(--space-1) var(--space-3);
    border-top: 1px solid var(--border-subtle);
    font: var(--text-2xs) var(--font-mono);
    color: var(--fg-dim);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
  .press-history { margin: 0; padding: 0; list-style: none; }
  .press-settled-entry {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    color: var(--fg-muted);
    border-top: 1px solid var(--border-subtle);
    flex-wrap: wrap;
  }
  .press-settled-entry.press-evicted { background: color-mix(in srgb, var(--warning) 7%, transparent); }
  .press-reason {
    padding: 1px var(--space-2);
    border-radius: var(--radius-full);
    background: color-mix(in srgb, var(--warning) 14%, transparent);
    color: var(--warning);
    font: var(--text-2xs) var(--font-mono);
    white-space: nowrap;
  }

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

  /* Two lines: what it was, then the id. The title carries the "why it
     mattered" the operator previously had to open each row to read. */
  .bolts-run {
    min-width: 0;
    max-width: 42ch;
  }
  .bolts-run-stack {
    display: flex;
    flex-direction: column;
    gap: 1px;
    min-width: 0;
  }
  .bolts-why {
    font-weight: 600;
    color: var(--fg-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .bolts-id {
    font-size: var(--text-2xs);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .facets {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin: 0 0 var(--space-3);
    padding: var(--space-2);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
  }
  .facet-row {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-1);
  }
  .facet-tag {
    font-size: var(--text-2xs);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
    color: var(--fg-dim);
    min-width: 4rem;
  }
  .facet {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    padding: 1px var(--space-2);
    border-radius: var(--radius-full);
    border: 1px solid var(--border-subtle);
    background: transparent;
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    cursor: pointer;
  }
  .facet:hover { color: var(--fg-primary); border-color: var(--border); }
  .facet.on {
    color: var(--accent);
    background: color-mix(in srgb, var(--accent) 16%, transparent);
    border-color: color-mix(in srgb, var(--accent) 34%, transparent);
  }
  .facet-n { color: var(--fg-dim); }
  .facet.on .facet-n { color: inherit; }
  .facet-clear {
    align-self: flex-start;
    padding: 0;
    border: 0;
    background: transparent;
    color: var(--fg-dim);
    font-size: var(--text-2xs);
    cursor: pointer;
    text-decoration: underline;
  }
  .facet-clear:hover { color: var(--fg-primary); }

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
</style>
