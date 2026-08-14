<script lang="ts">
  /**
   * FleetPanel — composition shell for the Operations → Fleet view. The
   * heavy lifting lives in `fleet/*.svelte` subcomponents and
   * `lib/utils/fleetRows.ts`; the panel itself just wires polling, the
   * cross-store row joins, and the detail drawer drill.
   *
   * See `docs/HUD_PANEL_DECOMP.md` for the pattern this panel canaries
   * (Slice B1 of the HUD UX overhaul).
   */
  import type { UnifiedAgent } from '../utils/agents.ts';
  import type { FleetRow } from '../utils/fleetRows.ts';
  import { fleetStore } from '../stores/fleet.svelte.ts';
  import { spawnStore } from '../stores/spawn.svelte.ts';
  import { taskStore } from '../stores/tasks.svelte.ts';
  import { workflowStore } from '../stores/workflows.svelte.ts';
  import { memoryStore } from '../stores/memory.svelte.ts';
  import { graphStore } from '../stores/graph.svelte.ts';
  import { streamStore } from '../stores/stream.svelte.ts';
  import { traceStore } from '../stores/traces.svelte.ts';
  import { router } from '../stores/router.svelte.ts';
  import { buildFleetRows, buildSpawnByAgentId, buildExpiringClaims } from '../utils/fleetRows.ts';
  import ClaimConflictChip from './fleet/ClaimConflictChip.svelte';
  import PanelHeader from './shared/PanelHeader.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';
  import EconomicsPanel from './fleet/EconomicsPanel.svelte';
  import FleetTable from './fleet/FleetTable.svelte';
  import FleetStatsGrid from './fleet/FleetStatsGrid.svelte';
  import ActivityCard from './fleet/ActivityCard.svelte';
  import MemoryTiersCard from './fleet/MemoryTiersCard.svelte';
  import SessionDetail from './fleet/SessionDetail.svelte';

  const fleetPollingOwner = Symbol('FleetPanel');
  const tracePollingOwner = Symbol('FleetPanelTraces');

  $effect(() => {
    // Decomposed stores (fleet, tasks, spawn) now use 60s watchdog polling
    // + SSE-first updates per Slice B3. Other stores retain their original
    // cadences until they're migrated.
    fleetStore.startPolling(60000, fleetPollingOwner);
    traceStore.startPolling(15000, tracePollingOwner);
    taskStore.startPolling(60000);
    workflowStore.startPolling(10000);
    memoryStore.startPolling(10000);
    graphStore.startPolling(15000);
    streamStore.startPolling(3000);
    spawnStore.startPolling(60000);

    return () => {
      fleetStore.stopPolling(fleetPollingOwner);
      traceStore.stopPolling(tracePollingOwner);
      taskStore.stopPolling();
      workflowStore.stopPolling();
      memoryStore.stopPolling();
      graphStore.stopPolling();
      streamStore.stopPolling();
      spawnStore.stopPolling();
    };
  });

  let detailSessionId = $derived(router.detail);

  let fleetAgents = $derived(fleetStore.liveAgents ?? []);
  let agentLookup = $derived.by(() => {
    const map = new Map<string, UnifiedAgent>();
    for (const a of fleetAgents) map.set(a.agent_id, a);
    return map;
  });
  let spawnByAgentId = $derived(buildSpawnByAgentId(spawnStore.spawns));
  let expiringClaims = $derived(buildExpiringClaims(fleetStore.fileClaims));

  let fleetRowsResult = $derived(buildFleetRows({
    agents: fleetAgents,
    sortKey: fleetStore.sortKey,
    sortDir: fleetStore.sortDir,
    groupByRootSession: fleetStore.groupByRootSession,
    sessionById: fleetStore.sessionById,
    sessionTree: fleetStore.sessionTree,
    parentSession: (id) => fleetStore.parentSession(id),
    rootSession: (id) => fleetStore.rootSession(id),
    childSessions: (id) => fleetStore.childSessions(id),
    sessionLineage: (id) => fleetStore.sessionLineage(id),
    agentLookup,
  }));

  function navigateToSession(sessionId: string) {
    router.navigate('agents', 'fleet', sessionId);
  }

  function navigateToTrace(agentId: string) {
    router.navigate('activity', 'traces', agentId);
  }

  function navigateToSpawn(e: Event, spawnId: string) {
    e.stopPropagation();
    router.navigate('sandbox', 'spawn', spawnId);
  }

  function openAgentDetail(row: FleetRow) {
    const agent = row.agent;
    if (agent.session_id) {
      navigateToSession(agent.session_id);
      return;
    }
    navigateToTrace(agent.agent_id);
  }

  function backToFleet() {
    router.back();
  }
</script>

<div class="panel fleet-panel">
  <PanelHeader title="Fleet" icon={'◈'} count={fleetAgents.length}>
    {#snippet actions()}
      <ClaimConflictChip />
    {/snippet}
  </PanelHeader>

  {#if fleetStore.error}
    <!-- Fleet REST poll failed: keep the (stale) table visible but say so,
         instead of silently degrading to an empty-looking roster. -->
    <ErrorBanner prefix="Fleet refresh failed" message={fleetStore.error} />
  {/if}

  <EconomicsPanel />

  <div class="fleet-grid">
    <FleetTable
      rows={fleetRowsResult.rows}
      loading={!fleetStore.lastUpdated}
      ungroupedStartIndex={fleetRowsResult.ungroupedStartIndex}
      ungroupedCount={fleetRowsResult.ungroupedCount}
      {spawnByAgentId}
      {expiringClaims}
      onRowClick={openAgentDetail}
      onSessionClick={navigateToSession}
      onTraceClick={navigateToTrace}
      onSpawnClick={navigateToSpawn}
    />

    <FleetStatsGrid
      rootGroupCount={fleetRowsResult.rootGroupCount}
      ungroupedCount={fleetRowsResult.ungroupedCount}
    />

    <ActivityCard />
    <MemoryTiersCard />
  </div>

  <SessionDetail
    sessionId={detailSessionId}
    onNavigateSession={navigateToSession}
    onNavigateTrace={navigateToTrace}
    onNavigateSpawn={navigateToSpawn}
    onClose={backToFleet}
  />
</div>

<style>
  .fleet-panel {
    width: 100%;
    max-width: 100%;
    min-width: 0;
    overflow-y: auto;
  }

  .fleet-grid {
    display: grid;
    grid-template-columns: minmax(0, 1.35fr) minmax(280px, 0.65fr);
    grid-template-rows: auto auto;
    gap: var(--space-4);
    width: 100%;
    max-width: 100%;
    min-width: 0;
    height: 100%;
  }

  @media (max-width: 1400px) {
    .fleet-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
