<script lang="ts">
  /**
   * OperatorPanel — the Operator Deck, the HUD's unified operator surface and
   * landing view (#operator/deck).
   *
   * One page, three zones under a signal strip:
   *   Queue (left)    — plans/tasks/projects waiting on a decision, with
   *                     spin + dispatch controls (QueueColumn).
   *   In flight (mid) — Mills pipeline runs, tracked MRs/CI, live agent
   *                     sessions as one unified board (InflightBoard).
   *   Inspect (right) — selection-aware detail dock: run stages/gates, live
   *                     tool-call feed + context health, MR history, or the
   *                     ambient context stream (InspectDock).
   *
   * The deck composes the existing domain stores (each already owns its
   * endpoint contract + polling) rather than introducing a new aggregate
   * endpoint; the polling bootstrap below mirrors OverviewPanel's.
   * Dispatch actions reuse the exact flows PlansPanel ships (spawn / handoff
   * / Mills backlog + start), so the deck is a new surface over proven paths.
   */
  import { router } from '../stores/router.svelte.ts';
  import { millsStore } from '../stores/mills.svelte.ts';
  import { mrwatchStore } from '../stores/mrwatch.svelte.ts';
  import { fleetStore } from '../stores/fleet.svelte.ts';
  import { taskStore } from '../stores/tasks.svelte.ts';
  import { projectsStore } from '../stores/projects.svelte.ts';
  import { contextHealthStore } from '../stores/contextHealth.svelte.ts';
  import { streamStore } from '../stores/stream.svelte.ts';
  import { alertsStore } from '../stores/alerts.svelte.ts';
  import { liveSessionsStore } from '../stores/liveSessions.svelte.ts';
  import { spawnStore } from '../stores/spawn.svelte.ts';
  import { toastStore } from '../stores/toasts.svelte.ts';
  import { createHandoff } from '../clients/presenceActions.ts';
  import OperatorStrip from './operator/OperatorStrip.svelte';
  import QueueColumn from './operator/QueueColumn.svelte';
  import InflightBoard from './operator/InflightBoard.svelte';
  import InspectDock from './operator/InspectDock.svelte';
  import PlanDispatchDialog from './shared/PlanDispatchDialog.svelte';
  import SpinPlanDialog from './shared/SpinPlanDialog.svelte';
  import ConfirmDialog from './shared/ConfirmDialog.svelte';
  import { spinRunsStore } from '../stores/spinRuns.svelte.ts';
  import type { SpinRun } from '../utils/spinRunsHelpers.ts';
  import { vendorSessionsStore } from '../stores/vendorSessions.svelte.ts';
  import { linkLiveAgents } from '../utils/sessionsUnify.ts';
  import {
    agentRows,
    findRow,
    millsRunRows,
    mrRows,
    sortRowsPinned,
    sortRowsStable,
    type InflightRow,
  } from '../utils/operatorHelpers.ts';
  import {
    type Plan,
    buildMillsBacklogItem,
    dispatchInstructions,
  } from '../utils/plansHelpers';

  const fleetPollingOwner = Symbol('OperatorPanel');

  // Polling bootstrap — every store the deck reads. Cadences follow the
  // owning panels' precedents (mills/mrwatch 30s, fleet-family 60s with SSE
  // as the fast path, context health 20s; vendor transcripts change slowly
  // so their titles ride a lazy 120s).
  $effect(() => {
    millsStore.startPolling(30000);
    mrwatchStore.startPolling(30000);
    fleetStore.startPolling(60000, fleetPollingOwner);
    taskStore.startPolling(60000);
    projectsStore.startPolling(30000);
    contextHealthStore.startPolling(20000);
    streamStore.startPolling(60000);
    alertsStore.startPolling(60000);
    vendorSessionsStore.startPolling(120000);
    liveSessionsStore.connect();
    return () => {
      millsStore.stopPolling();
      mrwatchStore.stopPolling();
      fleetStore.stopPolling(fleetPollingOwner);
      taskStore.stopPolling();
      projectsStore.stopPolling();
      contextHealthStore.stopPolling();
      streamStore.stopPolling();
      alertsStore.stopPolling();
      vendorSessionsStore.stopPolling();
    };
  });

  // ---- In-flight rows ------------------------------------------------------
  // Mills + MR lanes: severity buckets with a stable within-bucket key (their
  // states are sticky transitions, so a move means something). Agent lane:
  // fully pinned alphabetical — the fleet's orphan flag flaps with heartbeat
  // timing, and severity-ranking it made rows hop buckets on every poll.

  let millsLaneRows = $derived(
    sortRowsStable(millsRunRows(millsStore.pipelineRuns, millsStore.backlog)),
  );
  let mrLaneRows = $derived(sortRowsStable(mrRows(mrwatchStore.liveMergeRequests)));
  // Full unified set (not liveAgents): agentRows applies its own conversation
  // grouping + heartbeat linger, which needs to see members whose status
  // momentarily flapped to offline. Vendor transcripts (cksum-linked in
  // sessionsUnify) upgrade hash-named rows to human titles; ordering stays
  // pinned on the conversation id, so enrichment can't make rows jump.
  let conversationContext = $derived(
    linkLiveAgents(fleetStore.unifiedAgents, vendorSessionsStore.sessions).byConversation,
  );
  let agentLaneRows = $derived(
    sortRowsPinned(agentRows(fleetStore.unifiedAgents, conversationContext)),
  );

  let lanes = $derived([
    {
      kind: 'mills' as const,
      label: 'Mills pipelines',
      rows: millsLaneRows,
      viewTarget: ['mills', 'shuttles'] as [string, string],
      empty: millsStore.disabled
        ? 'Mills operator not configured.'
        : 'No pipelines in flight — dispatch a plan to Mills.',
      error: millsStore.error,
      // Routine operator-redeploy window / partial refresh: keep the lane
      // honest without redding it — "no pipelines" and "couldn't ask" are
      // different claims.
      notice: millsStore.reconnecting
        ? 'Reconnecting to the Mills operator — showing last known state.'
        : millsStore.degraded,
    },
    {
      kind: 'mr' as const,
      label: 'Merge requests · CI',
      rows: mrLaneRows,
      viewTarget: ['agents', 'mrwatch'] as [string, string],
      empty: 'No tracked MRs.',
      error: mrwatchStore.error,
    },
    {
      kind: 'agent' as const,
      label: 'Agent sessions',
      rows: agentLaneRows,
      viewTarget: ['agents', 'fleet'] as [string, string],
      empty: 'No live agents — spawn one from a plan in the queue.',
      error: fleetStore.error,
    },
  ]);

  // Selection survives poll ticks via the stable row key; a row that leaves
  // the board (run finished, MR merged, agent ended) folds the dock back to
  // the ambient stream automatically via findRow's null.
  let selectedKey = $state<string | null>(null);
  let allRows = $derived([...millsLaneRows, ...mrLaneRows, ...agentLaneRows]);
  let selected = $derived(findRow(allRows, selectedKey));

  function onSelect(row: InflightRow): void {
    selectedKey = selectedKey === row.key ? null : row.key;
  }

  // ---- Dispatch (mirrors PlansPanel's proven flows) ------------------------

  let busy = $state(false);
  let dispatchOpen = $state(false);
  let dispatchTarget = $state<{ plan: Plan } | null>(null);
  let confirmMillsTarget = $state<{ plan: Plan } | null>(null);

  function openDispatch(plan: Plan): void {
    dispatchTarget = { plan };
    dispatchOpen = true;
    void fleetStore.fetch(); // freshen the "hand to live agent" picker
  }
  function closeDispatch(): void {
    dispatchOpen = false;
    dispatchTarget = null;
  }

  async function dispatchSpawn(): Promise<void> {
    const t = dispatchTarget;
    if (!t || busy) return;
    closeDispatch();
    busy = true;
    const plan = t.plan;
    const branch = plan.slices?.[0]?.branch_name;
    const res = await spawnStore.spawn({
      agent_type: 'claude-code',
      project: plan.project || 'loom-core',
      task_description: `Work on plan: ${plan.title}`,
      ...(branch ? { branch } : {}),
      metadata: { plan_id: plan.id, source: 'hud-operator-deck' },
    });
    busy = false;
    if (res?.spawn_id) {
      toastStore.success(`Spawned ${res.spawn_id}`);
      router.navigate('sandbox', 'spawn', res.spawn_id);
    } else {
      toastStore.error('Spawn failed (admin token required?)');
    }
  }

  async function dispatchHandoff(targetAgentId: string): Promise<void> {
    const t = dispatchTarget;
    if (!t || busy) return;
    busy = true;
    try {
      await createHandoff({
        target_agent_id: targetAgentId,
        instructions: dispatchInstructions(t.plan),
        handoff_type: 'summary_only',
      });
      toastStore.success(`Handed off to ${targetAgentId}`);
      closeDispatch();
    } catch (e) {
      toastStore.error(`Handoff failed: ${e instanceof Error ? e.message : e}`);
    } finally {
      busy = false;
    }
  }

  function dispatchMills(): void {
    if (!dispatchTarget) return;
    confirmMillsTarget = dispatchTarget;
    closeDispatch();
  }

  // Queue the plan into Mills and start its pipeline. The confirm dialog
  // above IS the human gate — requeue:true so a previously-escalated
  // born-linked item flips back to queued instead of 409ing.
  async function runInMills(): Promise<void> {
    const t = confirmMillsTarget;
    confirmMillsTarget = null;
    if (!t || busy) return;
    busy = true;
    try {
      let backlogId = t.plan.mills_backlog_id ?? '';
      if (!backlogId) {
        const item = buildMillsBacklogItem(t.plan);
        const created = await millsStore.createBacklog(item);
        backlogId = created?.id || (item.ID as string);
      }
      await millsStore.startPipeline(backlogId, { requeue: true });
      toastStore.success(`Running in Mills: ${backlogId}`);
    } catch (e) {
      toastStore.error(`Mills run failed: ${e instanceof Error ? e.message : e}`);
    } finally {
      busy = false;
    }
  }

  // ---- Spin ----------------------------------------------------------------

  interface SpinSeed {
    brief: string;
    project: string;
    namespace: string;
    priority: string;
    label: string;
    respunFrom: string;
    frames?: string[];
  }
  let showSpin = $state(false);
  let spinSeed = $state<SpinSeed | null>(null);

  function openFreshSpin(): void {
    spinSeed = null;
    showSpin = true;
  }
  function retrySpin(run: SpinRun): void {
    spinSeed = {
      brief: run.brief,
      project: run.project ?? '',
      namespace: run.namespace ?? '',
      priority: run.priority ?? '',
      label: 'Retry spin',
      respunFrom: '',
      frames: run.frames,
    };
    showSpin = true;
  }
  function onSpinQueued(spin: { spinId: string; frames: string[] }): void {
    spinRunsStore.track(spin.spinId);
    toastStore.success('Spin queued — watch the ⟳ tray');
  }
</script>

<div class="deck panel" aria-label="Operator Deck">
  <OperatorStrip />

  <div class="deck-grid">
    <div class="deck-queue">
      <QueueColumn
        onDispatch={openDispatch}
        onSpin={openFreshSpin}
        onRetrySpin={retrySpin}
      />
    </div>

    <div class="deck-board">
      <InflightBoard
        {lanes}
        selectedKey={selected?.key ?? null}
        {onSelect}
        onOpenView={(view, subView) => router.navigate(view, subView)}
      />
    </div>

    <div class="deck-dock">
      <InspectDock {selected} onClose={() => (selectedKey = null)} />
    </div>
  </div>
</div>

<PlanDispatchDialog
  open={dispatchOpen}
  target={dispatchTarget}
  agents={fleetStore.liveAgents}
  {busy}
  onSpawn={dispatchSpawn}
  onHandoff={dispatchHandoff}
  onMills={dispatchMills}
  onClose={closeDispatch}
/>

<SpinPlanDialog
  open={showSpin}
  onClose={() => { showSpin = false; spinSeed = null; }}
  onQueued={onSpinQueued}
  seed={spinSeed}
/>

<ConfirmDialog
  open={!!confirmMillsTarget}
  title="Run in Mills?"
  message={`Create/queue a Mills backlog item for "${confirmMillsTarget?.plan.title ?? ''}" and start an autonomous pipeline. This spends budget and may open and merge a merge request.`}
  confirmLabel="Run in Mills"
  onConfirm={runInMills}
  onCancel={() => (confirmMillsTarget = null)}
/>

<style>
  .deck {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
    min-width: 0;
    gap: var(--space-3);
    padding: 0 var(--space-2);
    overflow: hidden;
  }

  .deck-grid {
    display: grid;
    grid-template-columns: minmax(260px, 320px) minmax(0, 1fr) minmax(280px, 360px);
    gap: var(--space-4);
    flex: 1;
    min-height: 0;
  }

  .deck-queue,
  .deck-board,
  .deck-dock {
    min-width: 0;
    min-height: 0;
    overflow-y: auto;
    scrollbar-width: thin;
  }

  .deck-dock {
    display: flex;
    flex-direction: column;
  }

  /* ≤1100px: the dock folds under the board; queue keeps its column. */
  @media (max-width: 1100px) {
    .deck-grid {
      grid-template-columns: minmax(240px, 300px) minmax(0, 1fr);
      grid-template-rows: auto auto;
    }
    .deck-queue { grid-row: 1 / span 2; }
    .deck-dock { grid-column: 2; }
  }

  /* ≤800px (phone): single column, in-flight first — the deck leads with
     "what's happening", then the inspector, then the queue. The page itself
     scrolls; every zone must take its NATURAL height (flex:none +
     min-height:auto), or the desktop min-height:0 shrink rules squash the
     zones into each other and their translucent sections visibly overlap. */
  @media (max-width: 800px) {
    .deck { overflow-y: auto; }
    .deck-grid {
      display: flex;
      flex-direction: column;
      flex: none;
      min-height: auto;
      overflow: visible;
    }
    .deck-queue { grid-row: auto; order: 3; }
    .deck-board { order: 1; }
    .deck-dock { order: 2; }
    .deck-queue,
    .deck-board,
    .deck-dock {
      flex: none;
      min-height: auto;
      overflow: visible;
    }
  }
</style>
