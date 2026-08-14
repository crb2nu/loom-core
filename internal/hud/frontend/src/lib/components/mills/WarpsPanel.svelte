<script lang="ts">
  /**
   * WarpsPanel — the beam. Every plan strung and waiting, bucketed by
   * priority P0–P3 (+ an "other" catch-all), so an operator sees what is
   * queued to be woven and can start or inspect it. Replaces the retired
   * generic BacklogPanel table (spec .loom/product-spec-mill-floor-views,
   * §3.1 / slice S1).
   *
   * All state gating flows through PanelShell (loading/error/empty are
   * mutually exclusive — a failed fetch never co-renders with a spinner).
   * The list is a shared DataTable per band, never a hand-rolled <table>.
   * The floor-nav spine (LineageRibbon) is the first child; drill-in reuses
   * the shared BacklogDetail drawer, keyed on the third hash segment.
   */
  import { untrack } from 'svelte';
  import type { BadgeVariant } from '../../utils/tokens.ts';
  import type { BacklogItem, CostEstimate } from '../../stores/mills.svelte.ts';
  import { millsStore } from '../../stores/mills.svelte.ts';
  import { router } from '../../stores/router.svelte.ts';
  import { STATUS_VARIANTS } from '../../utils/tokens.ts';
  import { warpCountFor } from '../../utils/factoryHelpers.ts';
  import { relativeTime } from '../../utils/format.ts';
  import { fmtCost } from './shared/format.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import DataTable from '../shared/DataTable.svelte';
  import Badge from '../../widgets/Badge.svelte';
  import SpinPlanDialog from '../shared/SpinPlanDialog.svelte';
  import LineageRibbon from './shared/LineageRibbon.svelte';
  import { priorityTone } from './shared/lineage.ts';
  import BacklogDetail from './BacklogDetail.svelte';

  // Shared mills poll (SSE hud.mills ≈15s + 60s watchdog). Only one mills
  // sub-view mounts at a time, so startPolling's reset contract is safe.
  $effect(() => {
    millsStore.startPolling(15000);
    return () => { millsStore.stopPolling(); };
  });

  // ── State reads ────────────────────────────────────────────────────────
  let buckets = $derived(millsStore.backlogByPriority);
  let backlogCount = $derived((millsStore.backlog ?? []).length);
  // strungCount is what the bands below actually render (queued/paused only).
  // backlogCount stays the "did any fetch land" signal for loading/refresh.
  let strungCount = $derived(millsStore.strungCount);
  let loading = $derived(millsStore.loading && backlogCount === 0);
  let disabled = $derived(millsStore.disabled);
  let error = $derived(millsStore.error);
  let previews = $derived(millsStore.costPreviews);
  let autonomyBlocked = $derived(millsStore.autonomyBlocked);
  let blockers = $derived(millsStore.autonomyBlockers);
  let selectedID = $derived(millsStore.selectedBacklogID);

  // One priority band per bucket, warmest tone first. `other` only renders
  // when it actually holds unbucketed items so the beam stays tidy. Tone is
  // never declared here: it comes from priorityTone, the same ramp the spine
  // ribbon directly above these bands paints its warp nodes with, so a band
  // and its warp node can never disagree on what P1 looks like.
  interface Band {
    key: 'P0' | 'P1' | 'P2' | 'P3' | 'other';
    label: string;
  }
  const BANDS: Band[] = [
    { key: 'P0', label: 'P0 · critical' },
    { key: 'P1', label: 'P1 · high' },
    { key: 'P2', label: 'P2 · normal' },
    { key: 'P3', label: 'P3 · low' },
    { key: 'other', label: 'unbucketed' },
  ];
  function bandTone(key: Band['key']): BadgeVariant {
    return priorityTone(key);
  }
  let visibleBands = $derived(
    BANDS.filter((b) => b.key !== 'other' || (buckets[b.key] ?? []).length > 0),
  );

  // Collapsed bands (by key). Default: all expanded.
  let collapsed = $state<Set<string>>(new Set());
  function toggleBand(key: string): void {
    const next = new Set(collapsed);
    if (next.has(key)) next.delete(key); else next.add(key);
    collapsed = next;
  }

  // ── Cost previews: one-shot per new backlog id (stable per policy), so a
  // poll tick never restarts a fetch storm. Mirrors the retired BacklogPanel.
  let fetchedIDs = $state<Set<string>>(new Set());
  $effect(() => {
    if (disabled) return;
    const missing = (millsStore.backlog ?? [])
      .map((i) => i.ID)
      .filter((id) => id && !fetchedIDs.has(id));
    if (missing.length === 0) return;
    const next = new Set(fetchedIDs);
    for (const id of missing) next.add(id);
    // Write inside untrack: this effect reads costPreviews indirectly via the
    // store and must not re-trigger on its own bookkeeping write.
    untrack(() => { fetchedIDs = next; });
    void Promise.all(missing.map((id) => millsStore.fetchCostPreview(id)));
  });

  // ── Drawer route-sync. The BacklogDetail drawer is driven by
  // millsStore.selectedBacklogID; the mill-floor hash carries the open item
  // as the third segment (#mills/warps/<backlogId>). Two one-directional
  // effects reconcile them without a reopen loop: router.detail is the source
  // of truth for opening (row click / deep link / back), and a null selection
  // (drawer's own X → closeBacklogDetail) clears a stale hash. Each effect
  // tracks exactly one signal and writes under untrack, so neither re-enters.
  $effect(() => {
    const want = router.detail;
    untrack(() => {
      const have = millsStore.selectedBacklogID;
      if (want && want !== have) millsStore.openBacklogDetail(want);
      else if (!want && have) millsStore.closeBacklogDetail();
    });
  });
  $effect(() => {
    const sel = millsStore.selectedBacklogID;
    untrack(() => {
      if (!sel && router.detail) router.navigateDetail(null);
    });
  });

  function openItem(item: BacklogItem): void {
    router.navigateDetail(item.ID);
  }

  // ── Spin dialog (reused Spinning Room action). Drafts land in the Plan
  // Store (phase=draft) for review before the beam picks them up.
  let showSpin = $state(false);

  // ── Column model. Budget lives only on the item detail, so the list shows
  // the co-fetched cost estimate ($ est) instead — the truthful list-level
  // signal. hideBelow drops secondary columns as the table narrows.
  const columns = [
    { key: 'id', label: 'ID', width: '120px' },
    { key: 'title', label: 'Title' },
    { key: 'plan', label: 'Plan', width: '150px', hideBelow: 760 },
    { key: 'est', label: 'Est.', width: '96px', align: 'right' as const },
    { key: 'state', label: 'State', width: '96px' },
    { key: 'age', label: 'Age', width: '80px', hideBelow: 620 },
  ];

  function confLabel(c: CostEstimate['confidence']): string {
    return c === 'medium' ? 'med' : c;
  }
  function planLabel(planID: string | undefined): string {
    if (!planID) return '—';
    return planID.replace(/^plan-stamp-/, '');
  }
  function stateVariant(state: string): BadgeVariant {
    return STATUS_VARIANTS[(state ?? '').toLowerCase()] ?? 'muted';
  }

  // Left-rail thread motif: bar count scales with how much is strung on the
  // band (never a fixed ornament). Bounded so a busy band stays a rail, not
  // a wall. Zero items → no threads (the band renders a "none strung" rail).
  function threadBars(count: number): number {
    if (count <= 0) return 0;
    return warpCountFor(count, 12, { min: 2, max: 12 });
  }

  // ── Empty / error copy (spec §3.1). emptyTone stays "idle" (a bare beam is
  // a healthy standing-by state) unless the operator is unconfigured or a
  // fetch failed / intake is blocked.
  function emptyMessage(): string {
    if (disabled) return 'Mills operator not configured';
    if (error) return 'Failed to load the beam';
    if (autonomyBlocked) return 'Mills cannot string the beam yet';
    return 'beam is bare — nothing strung to weave';
  }
  function emptyHint(): string {
    if (disabled) return 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.';
    if (error) return error ?? '';
    if (autonomyBlocked) return blockers.slice(0, 2).join(' · ');
    return 'Spin a plan or wait for the council to string the next item.';
  }
  function emptyTone(): 'idle' | 'ready' | 'error' | 'disabled' {
    if (disabled) return 'disabled';
    if (error || autonomyBlocked) return 'error';
    return 'idle';
  }
</script>

<!-- Declared outside PanelShell so it can be passed conditionally: the
     header-extra chrome (border + surface) renders whenever the snippet
     prop is present, so an always-passed snippet that renders nothing
     leaves an empty strip above the ribbon. -->
{#snippet blockedBanner()}
  <div class="blocked-banner" role="status">
    <span class="blocked-kicker">Blocked</span>
    <span class="blocked-main">Council intake unavailable — the beam can't be strung</span>
    <ul>
      {#each blockers.slice(0, 3) as blocker}
        <li>{blocker}</li>
      {/each}
    </ul>
  </div>
{/snippet}

<PanelShell
  title="Warps"
  icon="▟"
  header={autonomyBlocked ? blockedBanner : undefined}
  count={strungCount}
  loading={loading}
  empty={strungCount === 0}
  emptyIcon={disabled ? '◯' : '▟'}
  emptyMessage={emptyMessage()}
  emptyHint={emptyHint()}
  emptyTone={emptyTone()}
>
  {#snippet actions()}
    <button class="spin-btn" onclick={() => { showSpin = true; }} title="Spin a draft plan onto the beam">
      ⟳ Spin a plan
    </button>
  {/snippet}

  <LineageRibbon mode="spine" segments={millsStore.millFloorSpine} current="warps" />

  {#if error && backlogCount > 0}
    <div class="refresh-warn" role="alert">Beam refresh failed — showing the last-good strings. ({error})</div>
  {/if}

  <div class="bands">
    {#each visibleBands as band (band.key)}
      {@const items = buckets[band.key] ?? []}
      {@const isCollapsed = collapsed.has(band.key)}
      {@const bars = threadBars(items.length)}
      {@const tone = bandTone(band.key)}
      <section class="band tone-{tone}">
        <button
          class="band-head"
          aria-expanded={!isCollapsed}
          onclick={() => toggleBand(band.key)}
        >
          <span class="band-caret" class:collapsed={isCollapsed} aria-hidden="true">▾</span>
          <Badge text={band.label} variant={tone} />
          <span class="band-count">{items.length}</span>
          {#if bars > 0}
            <span class="thread-rail" aria-hidden="true">
              {#each Array(bars) as _}<span class="thread"></span>{/each}
            </span>
          {:else}
            <span class="none-strung">none strung</span>
          {/if}
        </button>

        {#if !isCollapsed && items.length > 0}
          <DataTable
            columns={columns}
            rows={items}
            rowLabel="warp"
            idKey="ID"
            onRowClick={openItem}
          >
            {#snippet row({ row: item, hiddenColumns })}
              {@const est = previews[item.ID]}
              <td
                class="mono clip"
                title={item.ID}
                class:selected={selectedID === item.ID}
              >{item.ID}</td>
              <td class="title" title={item.Title}>{item.Title}</td>
              {#if !hiddenColumns.has('plan')}
                <td class="mono muted clip" title={item.PlanID ?? ''}>{planLabel(item.PlanID)}</td>
              {/if}
              <td class="est">
                {#if est}
                  <span class="mono">{fmtCost(est.estimate_usd)}</span>
                  <span class="conf conf-{est.confidence}">{confLabel(est.confidence)}</span>
                {:else}
                  <span class="muted mono">—</span>
                {/if}
              </td>
              <td><Badge text={item.State} variant={stateVariant(item.State)} /></td>
              {#if !hiddenColumns.has('age')}
                <td class="muted mono age">{relativeTime(item.CreatedAt)}</td>
              {/if}
            {/snippet}
          </DataTable>
        {/if}
      </section>
    {/each}
  </div>
</PanelShell>

<!-- Shared drill-in drawer. Self-driven off millsStore.selectedBacklogID;
     the route-sync effects above keep it in step with the hash. -->
<BacklogDetail />

<SpinPlanDialog open={showSpin} onClose={() => { showSpin = false; }} />

<style>
  .spin-btn {
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    font-family: var(--font-mono);
    background: transparent;
    border: 1px solid var(--border-focus);
    color: var(--accent);
    cursor: pointer;
    white-space: nowrap;
    transition: background var(--transition-fast), border-color var(--transition-fast);
  }
  .spin-btn:hover { background: var(--accent-dim); }

  .blocked-banner {
    display: grid;
    gap: 0.3rem 0.75rem;
    color: var(--fg-secondary);
  }
  .blocked-kicker {
    width: max-content;
    padding: 0.08rem 0.45rem;
    border-radius: var(--radius-full);
    border: 1px solid rgba(var(--accent-rgb), 0.35);
    color: var(--accent);
    font-size: var(--text-xs);
    font-weight: 700;
    text-transform: uppercase;
  }
  .blocked-main { color: var(--fg-primary); font-weight: 700; }
  .blocked-banner ul { margin: 0; padding-left: 1rem; font-size: var(--text-sm); }
  .blocked-banner li + li { margin-top: 0.15rem; }

  .refresh-warn {
    margin: var(--space-3) 0 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--error);
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--error) 8%, transparent);
    color: var(--fg-secondary);
    font-size: var(--text-sm);
  }

  .bands {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    margin-top: var(--space-4);
  }

  .band {
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-lg);
    border-left: 3px solid var(--band-tone);
    overflow: hidden;
    --band-tone: var(--border);
  }
  /* Priority ramp (priorityTone): P0 error · P1 accent · P2 warning ·
     P3 info · unbucketed muted. Same tones the spine ribbon uses. */
  .band.tone-error   { --band-tone: var(--error); }
  .band.tone-accent  { --band-tone: var(--accent); }
  .band.tone-warning { --band-tone: var(--warning); }
  .band.tone-info    { --band-tone: var(--info); }
  .band.tone-muted   { --band-tone: var(--border); }

  .band-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    width: 100%;
    padding: var(--space-2) var(--space-3);
    background: color-mix(in srgb, var(--bg-secondary) 92%, transparent);
    border: none;
    cursor: pointer;
    text-align: left;
    color: var(--fg-secondary);
  }
  .band-head:hover { background: var(--bg-tertiary); }
  .band-head:focus-visible {
    outline: 2px solid var(--focus-ring);
    outline-offset: -2px;
  }

  .band-caret {
    font-size: var(--text-xs);
    transition: transform var(--transition-fast);
    color: var(--fg-muted);
  }
  .band-caret.collapsed { transform: rotate(-90deg); }
  @media (prefers-reduced-motion: reduce) {
    .band-caret { transition: none; }
  }

  .band-count {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--fg-secondary);
  }

  /* Thread motif: a rail of thin warp strings whose count encodes how much
     work is strung on the band (structure, not decoration). */
  .thread-rail {
    display: inline-flex;
    align-items: stretch;
    gap: 2px;
    height: 14px;
    margin-left: auto;
    opacity: 0.8;
  }
  .thread {
    width: 2px;
    border-radius: 1px;
    background: var(--band-tone);
  }
  .none-strung {
    margin-left: auto;
    font-size: var(--text-xs);
    font-style: italic;
    color: var(--fg-muted);
  }

  .mono { font-family: var(--font-mono); }
  .muted { color: var(--fg-muted); }
  .clip {
    max-width: 100%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .title { color: var(--fg-primary); }
  .mono.selected { box-shadow: inset 3px 0 0 var(--accent); }

  .est { white-space: nowrap; }
  .conf {
    display: inline-block;
    margin-left: 0.35rem;
    padding: 0.02rem 0.35rem;
    border-radius: 3px;
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    vertical-align: middle;
  }
  .conf-low    { background: rgba(var(--error-rgb), 0.15);   color: var(--error); }
  .conf-medium { background: rgba(var(--warning-rgb), 0.18); color: var(--warning); }
  .conf-high   { background: rgba(var(--success-rgb), 0.15); color: var(--success); }

  .age { white-space: nowrap; }
</style>
