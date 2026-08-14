<script lang="ts">
  /**
   * InspectDock — the Operator Deck's right column. Selection-aware detail:
   *   mills run  → live stage/gate detail (millsStore's cached detail loader)
   *   agent      → live tool-call ring + presence + context-window health
   *   MR         → registry classification + shepherd action history + links
   *   nothing    → ambient structured context stream (recent entries)
   *
   * The dock reads module-singleton stores directly (they're already polling
   * for the board) and only owns which lens is showing.
   */
  import { millsStore } from '../../stores/mills.svelte.ts';
  import { mrwatchStore } from '../../stores/mrwatch.svelte.ts';
  import { fleetStore } from '../../stores/fleet.svelte.ts';
  import { liveSessionsStore, type LiveSession } from '../../stores/liveSessions.svelte.ts';
  import { contextHealthStore, utilizationTone } from '../../stores/contextHealth.svelte.ts';
  import { streamStore } from '../../stores/stream.svelte.ts';
  import { router } from '../../stores/router.svelte.ts';
  import { toastStore } from '../../stores/toasts.svelte.ts';
  import { navigateToAgentSessionOrTraces } from '../../utils/drilldown.ts';
  import { conversationId } from '../../utils/agents.ts';
  import { relativeTime, formatTime } from '../../utils/format.ts';
  import type { InflightRow } from '../../utils/operatorHelpers.ts';
  import { showRunVerdict, verdictCorrected } from '../../utils/runVerdict.ts';
  import VendorTranscripts from './VendorTranscripts.svelte';

  let {
    selected = null,
    onClose,
  }: {
    selected?: InflightRow | null;
    onClose: () => void;
  } = $props();

  // ---- Mills lens -----------------------------------------------------------
  // Drive the store's cached detail loader off the selection. The $effect pair
  // (open on select, close on deselect/unmount) shares the loader with the
  // Mills drawers, which is safe: only one view is mounted at a time.
  $effect(() => {
    if (selected?.kind === 'mills') {
      millsStore.openRunDetail(selected.id);
      return () => millsStore.closeRunDetail();
    }
  });

  let millsDetail = $derived(millsStore.openPipelineDetail);

  let requeueBusy = $state(false);
  async function requeue(backlogID: string) {
    if (requeueBusy) return;
    requeueBusy = true;
    const outcome = await millsStore.requeuePipelineRun(backlogID);
    requeueBusy = false;
    if (outcome.kind === 'started') toastStore.success(`Requeued ${backlogID}`);
    else toastStore.error(outcome.message);
  }

  // ---- Agent lens -----------------------------------------------------------

  // Agent rows are conversation groups: `id` is the conversation, `drillId`
  // the lead member's concrete agent_id. Session/health lookups match any
  // member of the conversation, preferring the lead.
  let agentSession = $derived.by((): LiveSession | null => {
    if (selected?.kind !== 'agent') return null;
    const lead = selected.drillId ?? selected.id;
    const fleetAgent = fleetStore.unifiedAgents.find((a) => a.agent_id === lead);
    if (fleetAgent?.session_id) {
      const hit = liveSessionsStore.sessions.get(fleetAgent.session_id);
      if (hit) return hit;
    }
    let best: LiveSession | null = null;
    for (const s of liveSessionsStore.sessions.values()) {
      if (conversationId(s.agent_id) !== selected.id && s.agent_id !== lead) continue;
      if (!best || s.last_activity > best.last_activity) best = s;
    }
    return best;
  });

  let agentHealth = $derived.by(() => {
    if (selected?.kind !== 'agent') return null;
    const lead = selected.drillId ?? selected.id;
    return (
      contextHealthStore.agents.find((a) => a.agent_id === lead) ??
      contextHealthStore.agents.find((a) => conversationId(a.agent_id) === selected.id) ??
      null
    );
  });

  function openAgentSession() {
    if (selected?.kind !== 'agent') return;
    navigateToAgentSessionOrTraces(
      router,
      { agent_id: selected.drillId ?? selected.id },
      (id) => fleetStore.sessionForAgent(id),
    );
  }

  // Project of the selected conversation's lead member — seeds the vendor
  // transcript cwd filter so the section opens on "transcripts near this
  // agent's repo" instead of the whole host.
  let agentProject = $derived.by((): string => {
    if (selected?.kind !== 'agent') return '';
    const lead = selected.drillId ?? selected.id;
    const fleetAgent = fleetStore.unifiedAgents.find((a) => a.agent_id === lead);
    return fleetAgent?.project ?? '';
  });

  // ---- MR lens --------------------------------------------------------------

  let mrRecord = $derived.by(() => {
    if (selected?.kind !== 'mr') return null;
    return (
      mrwatchStore.mergeRequests.find((m) => `${m.repo}!${m.iid}` === selected.id) ?? null
    );
  });

  let mrActions = $derived.by(() => {
    const rec = mrRecord;
    if (!rec) return [];
    return mrwatchStore.recentActions
      .filter((a) => a.repo === rec.repo && a.mr_iid === rec.iid)
      .slice(0, 8);
  });

  // ---- Ambient lens ---------------------------------------------------------

  let ambientEntries = $derived(streamStore.entries.slice(0, 25));
</script>

<aside class="dock" aria-label="Inspector">
  <header class="dock-head">
    <span class="dock-title">
      {#if selected}{selected.title}{:else}Live context stream{/if}
    </span>
    {#if selected}
      <button class="dock-close" onclick={onClose} title="Back to the ambient stream" aria-label="Close inspector">✕</button>
    {/if}
  </header>

  {#if selected?.kind === 'mills'}
    <div class="dock-body">
      <div class="kv"><span>state</span><b data-sev={selected.severity}>{selected.state}</b></div>
      <div class="kv"><span>run</span><b class="mono">{selected.id}</b></div>
      {#if millsDetail?.status === 'loaded'}
        {@const d = millsDetail.detail}
        {#if d.run.CostUSD}
          <div class="kv"><span>cost</span><b>${d.run.CostUSD.toFixed(2)}</b></div>
        {/if}
        {#if d.run.MRIID}
          <div class="kv"><span>MR</span><b>!{d.run.MRIID}</b></div>
        {/if}
        {@const runVerdict = d.evidence?.verdict}
        {#if showRunVerdict(runVerdict, d.run.State) && runVerdict}
          <div
            class:corrected={verdictCorrected(runVerdict)}
            class="run-verdict"
            data-testid="run-verdict-chip"
          >
            <span>verdict</span>
            <b class="mono">{runVerdict.class}</b>
          </div>
        {/if}
        <section class="dock-section">
          <h4>Stages</h4>
          <ul class="mini-list">
            {#each d.stages as st (st.ID)}
              <li class="mini-row" data-outcome={st.Outcome ?? 'running'}>
                <span class="mini-name">{st.Stage}</span>
                <span class="mini-meta">
                  {st.Outcome ?? 'running'}{st.Attempt > 1 ? ` ·#${st.Attempt}` : ''}
                </span>
              </li>
            {/each}
            {#if d.stages.length === 0}<li class="dock-empty">No stages yet</li>{/if}
          </ul>
        </section>
        {@const failedGates = d.gates.filter((g) => g.Outcome === 'fail')}
        {#if failedGates.length > 0}
          <section class="dock-section">
            <h4>Failing gates</h4>
            <ul class="mini-list">
              {#each failedGates as g (g.ID)}
                <li class="gate-fail">
                  <span class="mini-name">{g.GateName}</span>
                  {#if g.Reasons?.length}
                    <span class="gate-reason">{g.Reasons[0]}</span>
                  {/if}
                </li>
              {/each}
            </ul>
          </section>
        {/if}
        <div class="dock-actions">
          {#if selected.state === 'escalated'}
            <button
              class="dock-btn warn"
              disabled={requeueBusy}
              onclick={() => requeue(d.run.BacklogID)}
            >{requeueBusy ? 'Requeuing…' : 'Requeue'}</button>
          {/if}
          <button class="dock-btn" onclick={() => router.navigate('mills', 'shuttles', selected!.id)}>
            Open in Mills →
          </button>
        </div>
      {:else if millsDetail?.status === 'error'}
        <div class="dock-error">{millsDetail.message}</div>
      {:else}
        <div class="dock-empty">Loading run detail…</div>
      {/if}
    </div>

  {:else if selected?.kind === 'agent'}
    <div class="dock-body">
      <div class="kv"><span>status</span><b data-sev={selected.severity}>{selected.state}</b></div>
      {#if agentSession?.current_task}
        <div class="kv"><span>task</span><b>{agentSession.current_task}</b></div>
      {/if}
      {#if agentSession?.branch}
        <div class="kv"><span>branch</span><b class="mono">{agentSession.branch}</b></div>
      {/if}
      {#if agentHealth}
        <section class="dock-section">
          <h4>Context window</h4>
          <div class="budget-track" data-tone={utilizationTone(agentHealth.budget_utilization)}>
            <div class="budget-fill" style="width: {Math.min(100, Math.round(agentHealth.budget_utilization * 100))}%"></div>
          </div>
          <div class="kv">
            <span>{agentHealth.tokens_used.toLocaleString()} / {agentHealth.token_budget.toLocaleString()} tok</span>
            <b>{Math.round(agentHealth.budget_utilization * 100)}%</b>
          </div>
          {#if agentHealth.compaction_needed}
            <div class="dock-hint">Compaction recommended</div>
          {/if}
        </section>
      {/if}
      <section class="dock-section">
        <h4>Live activity</h4>
        {#if agentSession && agentSession.recent_calls.length > 0}
          <ul class="mini-list stream">
            {#each agentSession.recent_calls as call (call.call_id)}
              <li class="mini-row" data-outcome={call.in_flight ? 'running' : call.error ? 'error' : 'success'}>
                <span class="mini-name mono">{call.tool_name}</span>
                <span class="mini-meta">
                  {#if call.in_flight}…{:else if call.duration_ms != null}{call.duration_ms}ms{/if}
                </span>
              </li>
            {/each}
          </ul>
        {:else}
          <div class="dock-empty">No live telemetry for this agent yet.</div>
        {/if}
      </section>
      <VendorTranscripts cwdHint={agentProject} />
      <div class="dock-actions">
        <button class="dock-btn" onclick={openAgentSession}>Open session →</button>
      </div>
    </div>

  {:else if selected?.kind === 'mr'}
    <div class="dock-body">
      <div class="kv"><span>state</span><b data-sev={selected.severity}>{selected.state}</b></div>
      {#if mrRecord}
        {#if mrRecord.reason}
          <div class="kv"><span>reason</span><b>{mrRecord.reason}</b></div>
        {/if}
        {#if mrRecord.pipeline_status}
          <div class="kv"><span>pipeline</span><b>{mrRecord.pipeline_status}</b></div>
        {/if}
        <div class="kv"><span>branch</span><b class="mono">{mrRecord.source_branch}</b></div>
        {#if mrActions.length > 0}
          <section class="dock-section">
            <h4>Shepherd actions</h4>
            <ul class="mini-list">
              {#each mrActions as a (a.time + a.action)}
                <li class="mini-row">
                  <span class="mini-name">{a.action}</span>
                  <span class="mini-meta">{a.outcome} · {relativeTime(a.time)}</span>
                </li>
              {/each}
            </ul>
          </section>
        {/if}
        <div class="dock-actions">
          {#if mrRecord.web_url}
            <a class="dock-btn" href={mrRecord.web_url} target="_blank" rel="noopener noreferrer">Open MR ↗</a>
          {/if}
          {#if mrRecord.pipeline_url}
            <a class="dock-btn" href={mrRecord.pipeline_url} target="_blank" rel="noopener noreferrer">Pipeline ↗</a>
          {/if}
          <button class="dock-btn" onclick={() => router.navigate('agents', 'mrwatch')}>MR registry →</button>
        </div>
      {:else}
        <div class="dock-empty">MR is no longer live; it may have merged, closed, or expired from retained history.</div>
      {/if}
    </div>

  {:else}
    <div class="dock-body">
      {#if ambientEntries.length === 0}
        <div class="dock-empty">No recent context entries.</div>
      {:else}
        <ul class="mini-list stream">
          {#each ambientEntries as e (e.id)}
            <li class="stream-entry">
              <div class="stream-head">
                <span class="stream-type">{e.entry_type}</span>
                <span class="stream-agent mono">{e.agent}</span>
                <span class="mini-meta" title={formatTime(e.timestamp)}>{relativeTime(e.timestamp)}</span>
              </div>
              <div class="stream-title">{e.title}</div>
            </li>
          {/each}
        </ul>
      {/if}
      <div class="dock-actions">
        <button class="dock-btn" onclick={() => router.navigate('activity', 'stream')}>Full stream →</button>
      </div>
    </div>
  {/if}
</aside>

<style>
  .dock {
    display: flex;
    flex-direction: column;
    min-width: 0;
    min-height: 0;
    background: color-mix(in srgb, var(--bg-secondary) 82%, transparent);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-lg);
    overflow: hidden;
  }

  .dock-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
    flex-shrink: 0;
  }

  .dock-title {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--fg-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    min-width: 0;
    flex: 1;
  }

  .dock-close {
    color: var(--fg-muted);
    padding: 2px 6px;
    border-radius: var(--radius-sm);
    flex-shrink: 0;
  }
  .dock-close:hover { color: var(--fg-primary); background: var(--bg-tertiary); }

  .dock-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
    overflow-y: auto;
    min-height: 0;
  }

  .kv {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
    font-size: var(--text-xs);
  }
  .kv span { color: var(--fg-muted); }
  .kv b {
    color: var(--fg-primary);
    font-weight: 500;
    text-align: right;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .kv b[data-sev='error'] { color: var(--error); }
  .kv b[data-sev='warn'] { color: var(--warning); }
  .kv b[data-sev='busy'] { color: var(--info); }

  .mono { font-family: var(--font-mono); font-size: 11px; }

  .run-verdict {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
    padding: 4px 7px;
    border: 1px solid color-mix(in srgb, var(--info) 45%, var(--border-subtle));
    border-radius: var(--radius-sm);
    color: var(--info);
    font-size: var(--text-xs);
  }
  .run-verdict.corrected {
    border-color: color-mix(in srgb, var(--accent) 55%, var(--border-subtle));
    color: var(--accent);
  }
  .run-verdict b { color: currentColor; font-weight: 600; }

  .dock-section {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin-top: var(--space-2);
  }

  .dock-section h4 {
    font-size: 10px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    margin: 0;
  }

  .mini-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .mini-list.stream {
    max-height: 340px;
    overflow-y: auto;
  }

  .mini-row {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
    font-size: var(--text-xs);
    padding: 3px var(--space-1);
    border-radius: var(--radius-xs);
  }
  .mini-row[data-outcome='error'],
  .mini-row[data-outcome='gate_fail'] { color: var(--error); }
  .mini-row[data-outcome='running'] .mini-meta { color: var(--info); }

  .mini-name {
    color: var(--fg-secondary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    min-width: 0;
  }

  .mini-meta {
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--fg-dim);
    flex-shrink: 0;
  }

  .gate-fail {
    display: flex;
    flex-direction: column;
    gap: 1px;
    padding: 4px var(--space-1);
    border-left: 2px solid var(--error);
    background: color-mix(in srgb, var(--error) 7%, transparent);
    border-radius: 0 var(--radius-xs) var(--radius-xs) 0;
    font-size: var(--text-xs);
  }
  .gate-fail .mini-name { color: var(--error); }
  .gate-reason { color: var(--fg-muted); font-size: 11px; }

  .budget-track {
    height: 6px;
    border-radius: 3px;
    background: rgba(255, 255, 255, 0.06);
    overflow: hidden;
  }
  .budget-fill {
    height: 100%;
    border-radius: 3px;
    background: var(--success);
    transition: width 0.3s ease;
  }
  .budget-track[data-tone='warn'] .budget-fill { background: var(--warning); }
  .budget-track[data-tone='crit'] .budget-fill { background: var(--error); }

  .dock-hint {
    font-size: var(--text-xs);
    color: var(--warning);
  }

  .dock-actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    margin-top: var(--space-2);
  }

  .dock-btn {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    padding: 5px 10px;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    background: rgba(255, 255, 255, 0.02);
    cursor: pointer;
    text-decoration: none;
    transition: color var(--transition-fast), border-color var(--transition-fast);
  }
  .dock-btn:hover { color: var(--fg-primary); border-color: var(--border-focus); }
  .dock-btn.warn { color: var(--warning); border-color: var(--warning-dim); }
  .dock-btn:disabled { opacity: 0.6; cursor: default; }

  .dock-empty {
    font-size: var(--text-xs);
    color: var(--fg-dim);
    padding: var(--space-1) 0;
  }

  .dock-error {
    font-size: var(--text-xs);
    color: var(--error);
  }

  .stream-entry {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: var(--space-1);
    border-radius: var(--radius-xs);
    border-bottom: 1px solid var(--border-subtle);
  }
  .stream-entry:last-child { border-bottom: none; }

  .stream-head {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }

  .stream-type {
    font-size: 9px;
    font-family: var(--font-mono);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--info);
    flex-shrink: 0;
  }

  .stream-agent {
    color: var(--fg-muted);
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .stream-title {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    overflow-wrap: anywhere;
  }
</style>
