<script lang="ts">
  /**
   * ShuttlesPanel (mill-floor S2) — every active pipeline run as a shuttle
   * carrying a plan across the weave stages. The split-flap DepartureBoard
   * shows real stage motion (a flap-flip = news, not ambience) with subruns
   * grouped under their parent; a row opens the shared PipelineRunDetail
   * drawer. Below the board, a visually + textually separated "weaver
   * capacity" strip reports the agent fleet — it is honestly labelled as
   * orthogonal to run lineage and hides itself when /api/shuttle/status
   * errors, so a fleet outage never blocks the run board.
   */
  import { untrack } from 'svelte';
  import { millsStore, type PipelineRun } from '../../stores/mills.svelte.ts';
  import { shuttleStore } from '../../stores/shuttle.svelte.ts';
  import {
    departureRows,
    nextStageSince,
    type StageObservation,
  } from '../../utils/departureHelpers.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import LineageRibbon from './shared/LineageRibbon.svelte';
  import DepartureBoard from './DepartureBoard.svelte';
  import PipelineRunDetail from './PipelineRunDetail.svelte';

  // Polling: both stores fetch explicitly on start() then hand off to their
  // own createPoller (visibility-pause, overlap-guard, no initial tick) —
  // mills on the 15s hud.mills cadence, shuttle on the 60s hud.fleet
  // watchdog. Mounting drives them; unmount stops both (house rule #5).
  $effect(() => {
    millsStore.startPolling(15000);
    shuttleStore.startPolling(60000);
    return () => {
      millsStore.stopPolling();
      shuttleStore.stopPolling();
    };
  });

  // Stage-entry observations advance per poll so "DELAYED" means "observed
  // sitting in this stage past the fuse", not a guess. Read the previous map
  // untracked — the effect keys off activeRuns only, or writing stageSinceMap
  // would retrigger it in a loop (house rule #4).
  let activeRuns = $derived(millsStore.pipelineRuns ?? []);
  let stageSinceMap = $state<Map<string, StageObservation>>(new Map());
  $effect(() => {
    const runs = activeRuns;
    stageSinceMap = nextStageSince(untrack(() => stageSinceMap), runs, Date.now());
  });

  // Order active runs parent→child so subruns render directly under their
  // parent as a tree (mirrors the retired PipelinesPanel grouping). Orphans
  // whose parent isn't on the current page are appended so nothing is dropped.
  let treeRuns = $derived.by<PipelineRun[]>(() => {
    const runs = activeRuns;
    const byParent = new Map<string, PipelineRun[]>();
    const tops: PipelineRun[] = [];
    for (const r of runs) {
      const parent = r.ParentRunID ?? null;
      if (parent) {
        if (!byParent.has(parent)) byParent.set(parent, []);
        byParent.get(parent)!.push(r);
      } else {
        tops.push(r);
      }
    }
    const out: PipelineRun[] = [];
    const visit = (r: PipelineRun) => {
      out.push(r);
      for (const k of byParent.get(r.ID) ?? []) visit(k);
    };
    for (const r of tops) visit(r);
    for (const r of runs) if (!out.includes(r)) out.push(r);
    return out;
  });

  // Board model: only genuinely in-flight runs ride the "en route" path —
  // escalated/paused runs on the same page go through the helper's history
  // path so they board as "diverted"/"held" instead of masquerading as
  // motion (and so the header's in-flight count can be smaller than the
  // board's row count without lying). maxRows is lifted to show every row
  // rather than the Factory glance-board's default cap of 7.
  let inFlightRuns = $derived(
    treeRuns.filter((r) => {
      const s = (r.State ?? '').toLowerCase();
      return s !== 'escalated' && s !== 'paused' && s !== 'done' && s !== 'merged';
    }),
  );
  let heldRuns = $derived(
    treeRuns.filter((r) => {
      const s = (r.State ?? '').toLowerCase();
      return s === 'escalated' || s === 'paused';
    }),
  );
  let boardRows = $derived(
    departureRows(inFlightRuns, heldRuns, stageSinceMap, Date.now(), {
      maxRows: Math.max(treeRuns.length, 1),
    }),
  );

  let hasRuns = $derived(activeRuns.length > 0);
  // Header count = the spine's shuttle definition (in flight, not held or
  // struck), not the raw run page. The spine node sits on this same screen,
  // so the two numbers must come from the one store getter.
  let shuttleCount = $derived(millsStore.activeShuttleCount);
  let disabled = $derived(millsStore.disabled);
  let refreshError = $derived(millsStore.error);

  // PanelShell gates loading/error so a spinner can never co-render with an
  // error (house rule #8). A populated board is never replaced by an error
  // card — a stale-refresh failure surfaces as an inline banner instead.
  let panelError = $derived.by<string | null>(() => {
    if (hasRuns) return null;
    if (disabled) return 'Mills operator not configured — set LOOM_MILLS_OPERATOR_URL on the HUD.';
    return refreshError;
  });
  let loading = $derived(millsStore.loading && !hasRuns && !panelError);

  let selectedID = $derived(millsStore.selectedRunID);
  function openRun(key: string): void {
    millsStore.openRunDetail(key);
  }

  // Weaver-capacity strip (orthogonal to lineage). Absent entirely when the
  // shuttle endpoint errors so a fleet outage never blocks the board.
  let capacities = $derived(shuttleStore.capacities ?? []);
  let recommendations = $derived(shuttleStore.recommendations ?? []);
  let capacityError = $derived(shuttleStore.error);
  let showCapacity = $derived(!capacityError);
  let capacityCollapsed = $state(false);

  function fmtSlots(c: { active_tasks: number; max_tasks: number }): string {
    return `${c.active_tasks}/${c.max_tasks}`;
  }
</script>

<PanelShell
  title="Shuttles"
  icon="⇢"
  count={hasRuns ? shuttleCount : null}
  {loading}
  error={panelError}
  errorHeading={disabled ? 'Operator offline' : 'Refresh failed'}
>
  <LineageRibbon mode="spine" segments={millsStore.millFloorSpine} current="shuttles" />

  {#if hasRuns && refreshError}
    <!-- Poll failed while runs are still on screen — flag it rather than
         present stale rows as fresh, but keep the board visible. -->
    <ErrorBanner prefix="Shuttles refresh failed" message={refreshError} />
  {/if}

  <div class="board-wrap">
    <DepartureBoard rows={boardRows} onSelect={openRun} selectedKey={selectedID} />
  </div>

  {#if showCapacity}
    <section class="capacity" aria-label="Weaver capacity">
      <header class="capacity-head">
        <button
          type="button"
          class="capacity-toggle"
          aria-expanded={!capacityCollapsed}
          onclick={() => (capacityCollapsed = !capacityCollapsed)}
        >
          <span class="capacity-caret" class:open={!capacityCollapsed} aria-hidden="true">▸</span>
          <span class="capacity-title">weaver capacity</span>
        </button>
        <span class="capacity-note">agent fleet · not run lineage (no shared id)</span>
        <span class="capacity-load" title="system load across the weaver fleet">
          load {shuttleStore.systemLoadPct}
        </span>
      </header>

      {#if !capacityCollapsed}
        {#if capacities.length > 0}
          <ul class="capacity-list">
            {#each capacities as c (c.agent_id)}
              <li class="weaver" class:idle={c.available_slots >= c.max_tasks}>
                <span class="weaver-id">{c.agent_id}</span>
                <span class="weaver-slots" title="active / max concurrent tasks">
                  {fmtSlots(c)}
                </span>
                <span class="weaver-status">{c.status}</span>
              </li>
            {/each}
          </ul>
          {#if recommendations.length > 0}
            <p class="capacity-reco">
              {recommendations.length} dispatch suggestion{recommendations.length === 1 ? '' : 's'} available
            </p>
          {/if}
        {:else}
          <p class="capacity-empty">no weavers reporting capacity</p>
        {/if}
      {/if}
    </section>
  {/if}
</PanelShell>

<PipelineRunDetail />

<style>
  .board-wrap {
    display: flex;
    flex-direction: column;
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-lg);
    background:
      linear-gradient(180deg, color-mix(in srgb, var(--fg-primary) 2%, transparent), transparent),
      color-mix(in srgb, var(--bg-secondary) 92%, transparent);
    overflow: hidden;
  }

  /* The capacity strip is deliberately set apart from the board: a full gap,
     a dashed top rule, and a muted surface, so it never reads as part of the
     shuttle roster it sits beneath. */
  .capacity {
    margin-top: var(--space-5);
    padding: var(--space-3) var(--space-4);
    border: 1px dashed var(--border-subtle);
    border-radius: var(--radius-lg);
    background: color-mix(in srgb, var(--bg-tertiary) 55%, transparent);
  }

  .capacity-head {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
  }

  .capacity-toggle {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: 0;
    border: none;
    background: transparent;
    color: var(--fg-secondary);
    font-size: var(--text-sm);
    font-weight: 600;
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
    cursor: pointer;
  }
  .capacity-toggle:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
    border-radius: var(--radius-sm);
  }

  .capacity-caret {
    display: inline-block;
    color: var(--fg-dim);
    transition: transform 0.12s ease-out;
  }
  .capacity-caret.open { transform: rotate(90deg); }

  .capacity-note {
    font-size: var(--text-2xs);
    color: var(--fg-muted);
    font-style: italic;
  }
  .capacity-load {
    margin-left: auto;
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    font-variant-numeric: tabular-nums;
  }

  .capacity-list {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2) var(--space-4);
    margin: var(--space-3) 0 0;
    padding: 0;
    list-style: none;
  }
  .weaver {
    display: inline-flex;
    align-items: baseline;
    gap: var(--space-2);
    font-size: var(--text-xs);
  }
  .weaver-id { color: var(--fg-primary); font-weight: 600; }
  .weaver-slots {
    font-family: var(--font-mono);
    color: var(--fg-secondary);
    font-variant-numeric: tabular-nums;
  }
  .weaver-status { color: var(--fg-muted); }
  .weaver.idle .weaver-id { color: var(--fg-muted); }

  .capacity-reco {
    margin: var(--space-2) 0 0;
    font-size: var(--text-2xs);
    color: var(--fg-muted);
  }
  .capacity-empty {
    margin: var(--space-3) 0 0;
    font-size: var(--text-xs);
    color: var(--fg-muted);
  }

  @media (prefers-reduced-motion: reduce) {
    .capacity-caret { transition: none; }
  }
</style>
