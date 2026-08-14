<script lang="ts">
  // OverviewPanel - triage-first composed shell (Slice A2 of HUD UX overhaul).
  //
  // The 1504-line monolith was decomposed into:
  //   - InstrumentStrip   - 4 signal-ring gauges + today chip
  //   - HeroSummary       - command headline + attention lanes
  //   - InboxDeck         - 7-kind operator inbox with action cards
  //   - inbox.ts          - typed selectors that derive cards from stores
  //   - existing MillsKPIRow + LiveSessionsCard for non-triage surfaces
  //
  // This file keeps store polling bootstrap, derived state, and layout shell.
  // Logic that varies per card kind lives in inbox.ts; theme/CSS for extracted
  // sections lives in their respective components.

  import { router } from '../stores/router.svelte.ts';
  import { fleetStore } from '../stores/fleet.svelte.ts';
  import { healthStore } from '../stores/health.svelte.ts';
  import { taskStore } from '../stores/tasks.svelte.ts';
  import { workflowStore } from '../stores/workflows.svelte.ts';
  import { memoryStore } from '../stores/memory.svelte.ts';
  import { streamStore } from '../stores/stream.svelte.ts';
  import { sandboxStore } from '../stores/sandbox.svelte.ts';
  import { graphStore } from '../stores/graph.svelte.ts';
  import { costStore } from '../stores/cost.svelte.ts';
  import { rbacStore } from '../stores/rbac.svelte.ts';
  import { coordinationStore } from '../stores/coordination.svelte.ts';
  import { mergeQueueStore } from '../stores/mergeQueue.svelte.ts';
  import { shuttleStore } from '../stores/shuttle.svelte.ts';
  import { millsStore } from '../stores/mills.svelte.ts';
  import { otelStore } from '../stores/otel.svelte.ts';
  import { liveSessionsStore } from '../stores/liveSessions.svelte.ts';
  import { alertsStore } from '../stores/alerts.svelte.ts';
  import MillsKPIRow from './mills/MillsKPIRow.svelte';
  import LiveSessionsCard from './LiveSessionsCard.svelte';
  import RecoverySLOCard from './RecoverySLOCard.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';
  import InstrumentStrip from './overview/InstrumentStrip.svelte';
  import HeroSummary from './overview/HeroSummary.svelte';
  import InboxDeck from './overview/InboxDeck.svelte';
  import BlockedSessionsCard from './overview/BlockedSessionsCard.svelte';
  import SupportingStrip from './overview/SupportingStrip.svelte';
  import type { CoordinationAgent } from '../stores/coordination.svelte.ts';
  import { relativeTime } from '../utils/format.ts';
  import { selectInboxCards } from '../utils/inbox.ts';
  import { navigateToAgentSessionOrTraces } from '../utils/drilldown.ts';
  import { createPoller } from '../utils/poller.ts';

  interface KpiConflict {
    path: string;
    agents: string[];
  }
  interface Kpis {
    sessions_today: number;
    tokens_today: number;
    tasks_completed_today: number;
    active_agents: number;
    pending_approvals: number;
    file_conflicts: number;
    conflict_details: KpiConflict[];
  }

  // Mirrors HeroSummary's HeroSpec/Lane prop contract (structural), plus the
  // extra consumesKind the inbox reads off the hero.
  interface HeroSpec {
    eyebrow: string;
    headline: string;
    detail: string;
    tone: 'alert' | 'calm';
    action: { label: string; route: string } | null;
    consumesKind: string | null;
  }
  interface Lane {
    route: string;
    label: string;
    action: string;
    value: string;
    detail: string;
    severity: 'error' | 'warning' | 'info' | 'success';
    kind?: string;
    agent?: unknown;
  }

  const fleetPollingOwner = Symbol('OverviewPanel');
  const otelPollingOwner = Symbol('OverviewPanel');

  let initialLoad = $state(true);
  let kpis = $state<Kpis>({
    sessions_today: 0,
    tokens_today: 0,
    tasks_completed_today: 0,
    active_agents: 0,
    pending_approvals: 0,
    file_conflicts: 0,
    conflict_details: [],
  });

  async function fetchKPIs() {
    try {
      const res = await globalThis.fetch('/api/kpis');
      if (res.ok) kpis = (await res.json()) as Kpis;
    } catch {
      // Non-critical: live stores still drive the dashboard.
    } finally {
      initialLoad = false;
    }
  }

  // createPoller fires no initial tick by design, hence the explicit first call.
  $effect(() => {
    void fetchKPIs();
    const p = createPoller(() => fetchKPIs(), 15000);
    p.start();
    return () => p.stop();
  });

  // Polling bootstrap: this is currently the landing view, so eagerly start
  // every store the dashboard reads. Stores with SSE-backed snapshots use
  // 60s watchdog polling — SSE pushes drive the data, polling is a safety
  // net for SSE disconnects (Slice B3 + B3 follow-up). Stores without SSE
  // (mills, otel) keep their 30s cadence until they get SSE coverage.
  $effect(() => {
    fleetStore.startPolling(60000, fleetPollingOwner);
    healthStore.startPolling(60000);
    taskStore.startPolling(60000);
    streamStore.startPolling(60000);
    memoryStore.startPolling(60000);
    costStore.startPolling(60000);
    rbacStore.startPolling(60000);
    coordinationStore.startPolling(60000);
    mergeQueueStore.startPolling(60000);
    shuttleStore.startPolling(60000);
    // mills + otel have no SSE backing — bumping them to 60s would add a
    // 30s data lag. Leave at 30s until they get hud.* event subscriptions.
    millsStore.startPolling(30000);
    otelStore.startPolling(30000, otelPollingOwner);
    // Alerts feed one attention lane only, so 60s is plenty — the engine
    // itself fires off the pipeline monitor's cycle, not off this poll.
    alertsStore.startPolling(60000);
    liveSessionsStore.connect();
    return () => {
      fleetStore.stopPolling(fleetPollingOwner);
      healthStore.stopPolling();
      taskStore.stopPolling();
      memoryStore.stopPolling();
      streamStore.stopPolling();
      costStore.stopPolling();
      rbacStore.stopPolling();
      coordinationStore.stopPolling();
      mergeQueueStore.stopPolling();
      shuttleStore.stopPolling();
      millsStore.stopPolling();
      otelStore.stopPolling(otelPollingOwner);
      alertsStore.stopPolling();
    };
  });

  let _tick = $state(0);
  $effect(() => {
    const t = setInterval(() => { _tick++; }, 10000);
    return () => clearInterval(t);
  });

  // Age of the last refresh. The math is relativeTime's — this wrapper only
  // reads _tick so the label re-renders on the interval above, and keeps the
  // empty string for "no timestamp" (relativeTime's '---' would render a
  // dash where this panel wants nothing at all). The local copy this
  // replaced capped at hours, so a 3-day-old lastRefreshed read '72h ago'.
  function agoText(ts: number | null | undefined) {
    void _tick;
    if (!ts) return '';
    return relativeTime(ts);
  }

  function navigate(panel: string) { router.navigate(panel); }

  /* ── Derived counts ── */
  let sessionCount = $derived(fleetStore.activeSessions.length);
  let agentSummary = $derived(fleetStore.unifiedSummary);
  let agentCount = $derived(agentSummary.live_agents);

  let healthyCount = $derived(healthStore.healthyCount);
  let serverCount = $derived(healthStore.availableCount);
  let downCount = $derived(healthStore.downCount);

  let pendingTasks = $derived(taskStore.pendingCount);
  let activeTasks = $derived(taskStore.inProgressCount);
  let blockedTasks = $derived(taskStore.blockedCount);
  let coordinationSummary = $derived(coordinationStore.summary);
  let activeAlerts = $derived(alertsStore.activeAlerts);
  let criticalAlerts = $derived(alertsStore.criticalCount);
  let activeBlockers = $derived(coordinationStore.activeBlockers);
  let topAttentionAgents = $derived(coordinationStore.topAttentionAgents);

  let workingItems = $derived(memoryStore.stats.working_memory?.items ?? 0);
  let shortItems = $derived(memoryStore.stats.short_term_memory?.items ?? 0);
  let longItems = $derived(memoryStore.stats.long_term_memory?.items ?? 0);
  let totalTokens = $derived(memoryStore.stats.total_tokens ?? 0);
  let daemonRunning = $derived(fleetStore.status?.running ?? false);
  let processCount = $derived(fleetStore.status?.processes?.length ?? 0);
  let graphEntities = $derived(graphStore.stats?.total_entities ?? 0);
  let graphTopTypes = $derived.by(() => {
    const types = graphStore.stats?.entity_types ?? {};
    return Object.entries(types)
      .sort((a, b) => b[1] - a[1])
      .slice(0, 1)
      .map(([n, c]) => `${n}:${c}`)
      .join('') || 'empty';
  });
  let streamCount = $derived(streamStore.entries.length);
  let lastStreamAge = $derived.by(() => {
    if (streamStore.entries.length === 0) return null;
    try {
      const t = new Date(streamStore.entries[0].timestamp);
      const diff = Math.floor((Date.now() - t.getTime()) / 1000);
      if (diff < 60) return `${diff}s ago`;
      if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
      return `${Math.floor(diff / 3600)}h ago`;
    } catch { return null; }
  });

  let pendingApprovals = $derived(
    workflowStore.activeWorkflows.filter(w => w.status === 'waiting_approval').length
  );

  let completedTaskCount = $derived.by(() => {
    const storeCount = taskStore.tasks.filter(t => t.status === 'completed').length;
    return storeCount > 0 ? storeCount : kpis.tasks_completed_today;
  });

  let lastRefreshed = $derived.by(() => {
    const candidates = [
      fleetStore.lastUpdated,
      healthStore.lastUpdated,
      taskStore.lastUpdated,
    ].filter((d): d is Date => d !== null);
    if (candidates.length === 0) return null;
    return new Date(Math.max(...candidates.map(d => d.getTime())));
  });

  /* ── Instruments (signal strip) ── */
  let instruments = $derived.by(() => [
    {
      label: 'Active Agents',
      value: agentCount,
      max: Math.max(agentCount, sessionCount, 4),
      color: 'var(--info)',
      route: 'fleet',
    },
    {
      label: 'Tasks Done',
      value: completedTaskCount,
      max: Math.max(completedTaskCount, pendingTasks + activeTasks, 8),
      color: 'var(--success)',
      route: 'tasks',
    },
    {
      label: 'System Load',
      value: parseInt(shuttleStore.systemLoadPct) || 0,
      max: 100,
      suffix: '%',
      color: (parseInt(shuttleStore.systemLoadPct) || 0) > 80
        ? 'var(--error)'
        : (parseInt(shuttleStore.systemLoadPct) || 0) > 60
          ? 'var(--warning)'
          : 'var(--info)',
      route: 'dispatch',
    },
    {
      label: 'Running',
      value: healthyCount,
      max: Math.max(serverCount, 1),
      suffix: `/${serverCount}`,
      color: downCount > 0 ? 'var(--error)' : 'var(--success)',
      route: 'servers',
    },
  ]);

  /* ── Store outages ── */
  // Every store the dashboard composes exposes a public `error`. Reading none
  // of them is how a dead daemon renders as "System nominal / No active
  // pressure" — the gauges and lanes below are all zero-valued on failure.
  let outages = $derived.by((): Array<[string, string]> => {
    const out: Array<[string, string]> = [];
    if (fleetStore.error) out.push(['Fleet', fleetStore.error]);
    if (healthStore.error) out.push(['Servers', healthStore.error]);
    if (taskStore.error) out.push(['Tasks', taskStore.error]);
    if (millsStore.error) out.push(['Mills', millsStore.error]);
    if (coordinationStore.error) out.push(['Coordination', coordinationStore.error]);
    return out;
  });
  let outageMessage = $derived(
    outages.length === 0
      ? null
      : `${outages.map(([name]) => name).join(', ')} unreachable — ${outages[0][1]}`,
  );

  /* ── Hero summary ── */
  let heroSummary = $derived.by((): HeroSpec => {
    const storeConflicts = coordinationSummary.conflict_files ?? 0;
    const kpiConflicts = kpis.file_conflicts ?? 0;
    const conflictCount = storeConflicts > 0 ? storeConflicts : kpiConflicts;

    // Each alert branch also names the inbox-card kind it represents
    // (consumesKind) so the inbox can drop that card — otherwise the
    // page's two most prominent elements state the same fact twice with
    // two differently-labelled CTAs (hero "Check servers" + inbox card
    // "Open Servers" for the same 4-down incident).
    //
    // Signal loss outranks every pressure branch below: with a store dark we
    // cannot know whether there is pressure, so claiming either way is wrong.
    if (outageMessage !== null) {
      return {
        eyebrow: 'Signal lost',
        headline: 'HUD data is incomplete',
        detail: outageMessage,
        tone: 'alert',
        action: null,
        consumesKind: null,
      };
    }
    if (conflictCount > 0) {
      const conflict = kpis.conflict_details?.[0];
      return {
        eyebrow: 'Coordination pressure',
        headline: 'File conflicts need attention',
        detail: conflict
          ? `${conflict.path} is shared by ${conflict.agents.join(', ')}`
          : `${conflictCount} file conflict${conflictCount === 1 ? '' : 's'} detected`,
        tone: 'alert',
        action: { label: 'Resolve conflicts', route: 'dispatch' },
        consumesKind: 'file_conflict',
      };
    }
    if (downCount > 0) {
      return {
        eyebrow: 'Infrastructure watch',
        headline: 'Server health needs attention',
        detail: `${downCount} server${downCount === 1 ? '' : 's'} down · ${serverCount} monitored`,
        tone: 'alert',
        action: { label: 'Check servers', route: 'servers' },
        consumesKind: 'server_down',
      };
    }
    if (blockedTasks > 0 || coordinationSummary.cross_agent_blockers > 0) {
      return {
        eyebrow: 'Work queue',
        headline: 'Blocked work needs attention',
        detail: `${blockedTasks} blocked task${blockedTasks === 1 ? '' : 's'} · ${coordinationSummary.cross_agent_blockers} cross-agent blocker${coordinationSummary.cross_agent_blockers === 1 ? '' : 's'}`,
        tone: 'alert',
        action: { label: 'Unblock tasks', route: 'dispatch' },
        consumesKind: 'blocked_task',
      };
    }
    if (pendingApprovals > 0) {
      return {
        eyebrow: 'Approvals pending',
        headline: 'Workflow approvals are waiting',
        detail: `${pendingApprovals} workflow decision${pendingApprovals === 1 ? '' : 's'} ready for review`,
        tone: 'alert',
        action: { label: 'Review approvals', route: 'workflows' },
        consumesKind: 'pending_approval',
      };
    }
    return {
      eyebrow: 'System nominal',
      headline: 'No active pressure',
      detail: 'All systems operating within normal parameters.',
      tone: 'calm',
      action: null,
      consumesKind: null,
    };
  });

  /* ── Attention lanes (compact right column) ── */
  let attentionLanes = $derived.by((): Lane[] => {
    const lanes: Lane[] = [];
    if (downCount > 0 || !daemonRunning) {
      lanes.push({
        route: 'servers',
        label: 'Runtime',
        action: 'Investigate',
        value: downCount > 0 ? `${downCount} down` : 'Daemon offline',
        detail: daemonRunning
          ? `${healthyCount}/${serverCount} healthy · ${processCount} proc`
          : 'Daemon needs restart',
        severity: 'error',
      });
    }
    // Un-acked pipeline alerts from the alert engine (Operations ▸ Alerts).
    // Compact by design: one lane, count + the newest title, no fan-out.
    if (activeAlerts.length > 0) {
      lanes.push({
        route: 'alerts',
        label: 'Alerts',
        action: 'Triage',
        value: `${activeAlerts.length} active`,
        detail: criticalAlerts > 0
          ? `${criticalAlerts} critical · ${activeAlerts[0].title}`
          : activeAlerts[0].title,
        severity: criticalAlerts > 0 ? 'error' : 'warning',
      });
    }
    if (blockedTasks > 0 || coordinationSummary.cross_agent_blockers > 0) {
      lanes.push({
        route: 'dispatch',
        label: 'Blocked',
        action: 'Unblock',
        value: `${blockedTasks} task${blockedTasks === 1 ? '' : 's'}`,
        detail: activeBlockers.length > 0
          ? activeBlockers[0].task_title
          : `${coordinationSummary.cross_agent_blockers} cross-agent`,
        severity: 'warning',
      });
    }
    if (pendingApprovals > 0) {
      lanes.push({
        route: 'workflows',
        label: 'Approvals',
        action: 'Review',
        value: `${pendingApprovals} waiting`,
        detail: 'Workflow decisions ready',
        severity: 'warning',
      });
    }
    if (coordinationSummary.agents_needing_attention > 0) {
      const leadAgent = topAttentionAgents[0];
      lanes.push({
        route: 'fleet',
        label: 'Attention',
        action: leadAgent?.session_id ? 'Session' : 'Traces',
        // "flagged", not "agents": this is a needs-review backlog from the
        // coordination engine that can exceed the live-agent count — labeling
        // it "N agents" right next to the "2 live agents" status reads as a
        // contradiction (e.g. "22 agents" when only 2 are live).
        value: `${coordinationSummary.agents_needing_attention} flagged`,
        detail: leadAgent
          ? `${leadAgent.agent_id} · ${leadAgent.attention_reasons?.[0] || 'needs review'}`
          : 'Needs review',
        severity: 'info',
        kind: 'agent',
        agent: leadAgent,
      });
    }
    if (shuttleStore.hasRecommendations) {
      lanes.push({
        route: 'dispatch',
        label: 'Dispatch',
        action: 'Route',
        value: `${shuttleStore.recommendations.length} suggestion${shuttleStore.recommendations.length === 1 ? '' : 's'}`,
        detail: shuttleStore.recommendations[0]?.task_title || 'Ready',
        severity: 'info',
      });
    }
    return lanes.slice(0, 5);
  });

  function onLaneClick(lane: Lane) {
    if (lane.kind === 'agent' && lane.agent) {
      navigateToAgentSessionOrTraces(router, lane.agent as CoordinationAgent, (id) => fleetStore.sessionForAgent(id));
      return;
    }
    router.navigate(lane.route);
  }

  /* ── Inbox cards ── */
  // The hero already presents the top pressure point with its own CTA, so
  // drop the inbox card of the same kind — the inbox is "everything else".
  let inboxCards = $derived.by(() => selectInboxCards({
    router,
    coordination: coordinationStore,
    tasks: taskStore,
    workflows: workflowStore,
    health: healthStore,
    fleet: fleetStore,
    rbac: rbacStore,
    liveSessions: liveSessionsStore,
  }).filter((card) => card.kind !== heroSummary.consumesKind));

  /* ── Supporting surfaces ── */
  let supportSurfaces = $derived.by(() => [
    { route: 'memory',  label: 'Memory',  value: `${workingItems + shortItems + longItems}`, detail: `${totalTokens.toLocaleString()} tok` },
    { route: 'stream',  label: 'Stream',  value: `${streamCount}`,                          detail: lastStreamAge ?? 'idle' },
    { route: 'sandbox', label: 'Sandbox', value: `${sandboxStore.runningCount}`,            detail: sandboxStore.available ? 'online' : 'offline' },
    { route: 'graph',   label: 'Graph',   value: `${graphEntities}`,                        detail: graphTopTypes },
  ]);

  let todayLabel = $derived(`${kpis.sessions_today} sessions · ${completedTaskCount} tasks`);
  let refreshedLabel = $derived(lastRefreshed ? agoText(lastRefreshed.getTime()) : null);
</script>

<div class="panel overview">
  {#if initialLoad}
    <div class="skeleton-block" aria-hidden="true">
      <div class="skeleton skeleton-bar" style="height: 64px;"></div>
      <div class="skeleton skeleton-bar" style="height: 120px;"></div>
      <div class="skeleton skeleton-bar" style="height: 200px;"></div>
    </div>
  {:else}
    <InstrumentStrip
      instruments={instruments}
      todayLabel={todayLabel}
      refreshedLabel={refreshedLabel}
      onSelect={navigate}
    />

    <!-- Directly under the gauges: every ring above reads 0 on a failed fetch,
         so the banner is what stops them being read as authoritative. -->
    {#if outageMessage}
      <ErrorBanner prefix="Overview data incomplete" message={outageMessage} />
    {/if}

    <HeroSummary
      hero={heroSummary}
      lanes={attentionLanes}
      onAction={navigate}
      onLaneClick={onLaneClick}
    />

    <InboxDeck cards={inboxCards} />

    <!-- "Waiting on you" sits directly under the inbox: every other card on
         this page reports on the fleet, this one reports on the operator, so
         it belongs above the ambient status surfaces rather than among them.
         It owns its own /api/blocked poll (BlockedSessionsCard). -->
    <BlockedSessionsCard />

    <section class="live-sessions-section">
      <LiveSessionsCard agentCount={agentCount} sessionCount={sessionCount} />
    </section>

    <MillsKPIRow />

    <RecoverySLOCard />

    <SupportingStrip surfaces={supportSurfaces} onSelect={navigate} />
  {/if}
</div>

<style>
  .overview {
    display: flex;
    flex-direction: column;
    flex: 1;
    width: 100%;
    min-height: 0;
    min-width: 0;
    padding: 0 var(--space-5) var(--space-4);
    gap: var(--space-5);
    overflow-y: auto;
  }

  .skeleton-block {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-4) 0;
  }

  @media (max-width: 480px) {
    .overview {
      padding: var(--space-3);
      gap: var(--space-3);
    }
  }
</style>
