<script lang="ts">
  import type { UnifiedAgent } from '../utils/agents.ts';
  import { fleetStore } from '../stores/fleet.svelte.ts';
  import { presenceStore } from '../stores/presence.svelte.ts';
  import { presenceActionsStore } from '../stores/presenceActions.svelte.ts';
  import PresenceClaimsTab from './presence/PresenceClaimsTab.svelte';
  import PresenceWorktreesTab from './presence/PresenceWorktreesTab.svelte';
  import PresenceHandoffsTab from './presence/PresenceHandoffsTab.svelte';
  import PresenceDiagnosticsTab from './presence/PresenceDiagnosticsTab.svelte';
  import CreateHandoffModal from './presence/CreateHandoffModal.svelte';
  import PanelHeader from './shared/PanelHeader.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';

  const fleetPollingOwner = Symbol('PresencePanel');

  $effect(() => {
    // Slice B3 — 60s watchdog poll; presence updates arrive via agent.heartbeat
    // and agent.session.* SSE events, so the short 5s poll was redundant.
    fleetStore.startPolling(60000, fleetPollingOwner);
    // presenceStore supplies worktree assignments (GET /api/worktrees), which
    // the fleet store does not carry. Its poller self-gates on SSE health, so
    // this is an initial fetch + SSE subscription + disconnected-only fallback.
    presenceStore.startPolling();
    return () => {
      fleetStore.stopPolling(fleetPollingOwner);
      presenceStore.stopPolling();
    };
  });

  function isCodexKeepaliveWrapper(agent: UnifiedAgent) {
    return (
      agent?.agent_type === 'codex' &&
      /^codex-\d+$/.test(agent?.agent_id ?? '') &&
      (agent?.description ?? '').toLowerCase().includes('keepalive wrapper session') &&
      !!agent?.namespace
    );
  }

  function isLegacyCodexBootstrap(agent: UnifiedAgent) {
    return (
      agent?.agent_type === 'codex' &&
      /^codex-.+-\d+-[0-9a-f]{8}$/.test(agent?.agent_id ?? '') &&
      (agent?.description ?? '').toLowerCase().includes('heartbeat bootstrap session') &&
      !!agent?.namespace
    );
  }

  function suppressLegacyCodexBootstrapAgents(inputAgents: UnifiedAgent[]) {
    const stableWrapperNamespaces = new Set(
      inputAgents.filter(isCodexKeepaliveWrapper).map((agent) => agent.namespace)
    );
    if (stableWrapperNamespaces.size === 0) return inputAgents;
    return inputAgents.filter(
      (agent) => !(isLegacyCodexBootstrap(agent) && stableWrapperNamespaces.has(agent.namespace))
    );
  }

  // The roster itself lives on Fleet (the Agents tab was retired 2026-08-30;
  // its Task/Nudge actions moved to Fleet's rows). `agents` stays for the
  // Diagnostics tab and the header count.
  let agents = $derived(suppressLegacyCodexBootstrapAgents(fleetStore.unifiedAgents ?? []));
  let claims = $derived(fleetStore.fileClaims ?? []);
  let worktrees = $derived(presenceStore.worktrees ?? []);
  let fileConflicts = $derived.by(() => {
    const fileCounts: Record<string, string[]> = {};
    for (const claim of claims) {
      if (!fileCounts[claim.file_path]) fileCounts[claim.file_path] = [];
      fileCounts[claim.file_path].push(claim.agent_id);
    }
    return Object.entries(fileCounts)
      .filter(([, owners]) => owners.length > 1)
      .map(([path, owners]) => ({ path, agents: [...new Set(owners)] }));
  });
  // --- Tab management ---
  let activeTab = $state('claims');

  function setActiveTab(nextTab: string) {
    activeTab = nextTab;
  }

  $effect(() => {
    if (activeTab === 'handoffs') {
      presenceActionsStore.refreshHandoffs();
    }
  });
</script>

<div class="panel presence-panel">
  <PanelHeader title="Presence" icon={'◉'} count={agents.length} />

  <!-- Tab bar -->
  <div class="tab-bar">
    <button class="tab-btn" class:active={activeTab === 'claims'} onclick={() => setActiveTab('claims')}>
      Claims <span class="tab-count">{claims.length}</span>
    </button>
    <button class="tab-btn" class:active={activeTab === 'worktrees'} onclick={() => setActiveTab('worktrees')}>
      Worktrees <span class="tab-count">{worktrees.length}</span>
    </button>
    <button class="tab-btn" class:active={activeTab === 'handoffs'} onclick={() => setActiveTab('handoffs')}>
      Handoffs <span class="tab-count">{presenceActionsStore.handoffs.length}</span>
    </button>
    <button class="tab-btn" class:active={activeTab === 'diagnostics'} onclick={() => setActiveTab('diagnostics')}>
      Diagnostics
    </button>
    <div class="tab-spacer"></div>
    {#if fileConflicts.length > 0}
      <span class="conflict-badge" title="{fileConflicts.length} file(s) claimed by multiple agents">
        ⚠ {fileConflicts.length} conflicts
      </span>
    {/if}
  </div>

  {#if fleetStore.error}
    <!-- The fleet REST poll (watchdog behind the agent.* SSE stream) failed.
         Presence tabs otherwise render an indistinguishable "No registered
         agents" empty on a fetch failure, so surface the reason here. Clears
         on the next successful fetch/snapshot (the store resets error to null). -->
    <ErrorBanner prefix="Agent presence refresh failed" message={fleetStore.error} />
  {/if}

  {#if presenceStore.error}
    <!-- Worktree assignments (GET /api/worktrees) come from presenceStore, not
         the fleet poll, so its failure would otherwise render as an empty
         Worktrees tab and a zero count in the tab bar. -->
    <ErrorBanner prefix="Presence refresh failed" message={presenceStore.error} />
  {/if}

  <div class="tab-content">
    {#if activeTab === 'claims'}
      <PresenceClaimsTab
        {claims}
        {fileConflicts}
        onReleaseClaim={(agentId, filePath, opts) => presenceActionsStore.onReleaseClaim(agentId, filePath, opts)}
      />

    {:else if activeTab === 'worktrees'}
      <PresenceWorktreesTab {worktrees} />

    {:else if activeTab === 'handoffs'}
      <PresenceHandoffsTab
        handoffs={presenceActionsStore.handoffs}
        handoffLoading={presenceActionsStore.handoffLoading}
        handoffError={presenceActionsStore.handoffError}
        onOpenHandoffModal={() => presenceActionsStore.openHandoffModal()}
        onAcceptHandoff={(id, targetAgentID) => presenceActionsStore.onAcceptHandoff(id, targetAgentID)}
      />

    {:else if activeTab === 'diagnostics'}
      <PresenceDiagnosticsTab {agents} />
    {/if}
  </div>
</div>

<CreateHandoffModal />

<style>
  .presence-panel {
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  .tab-bar {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 2px;
    padding: var(--space-1) 0;
    border-bottom: 1px solid var(--border);
    margin-bottom: var(--space-2);
    position: relative;
  }

  .tab-bar::after {
    content: '';
    position: absolute;
    bottom: 0;
    left: 10%;
    right: 10%;
    height: 1px;
    background: linear-gradient(90deg, transparent, rgba(var(--info-rgb), 0.06) 50%, transparent);
    pointer-events: none;
  }

  .tab-btn {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px var(--space-3);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    font-weight: 500;
    color: var(--fg-muted);
    cursor: pointer;
    border: none;
    background: transparent;
    transition: background var(--transition-fast), color var(--transition-fast);
    letter-spacing: var(--tracking-normal);
  }

  .tab-btn:hover {
    background: var(--bg-tertiary);
    color: var(--fg-secondary);
  }

  .tab-btn.active {
    background: var(--bg-tertiary);
    color: var(--fg-primary);
    font-weight: 600;
  }

  .tab-count {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    background: var(--bg-primary);
    padding: 1px 5px;
    border-radius: var(--radius-full);
    color: var(--fg-dim);
  }






  .tab-spacer { flex: 1; }











  .conflict-badge {
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--warning);
    padding: 2px var(--space-2);
    background: var(--warning-dim);
    border-radius: var(--radius-sm);
    border: 1px solid rgba(var(--warning-rgb), 0.25);
    box-shadow: var(--glow-shadow-md) var(--glow-warning);
  }

  .tab-content {
    flex: 1;
    overflow-y: auto;
  }

</style>
