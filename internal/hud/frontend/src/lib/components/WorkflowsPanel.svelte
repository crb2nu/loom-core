<script lang="ts">
  import type { BadgeVariant } from '../utils/tokens.ts';
  import type { WorkflowSummary, WorkflowDefinition } from '../stores/workflows.svelte.ts';
  import { workflowStore } from '../stores/workflows.svelte.ts';
  import { toastStore } from '../stores/toasts.svelte.ts';
  import { formatTime, formatDateTime, relativeTime, statusVariant } from '../utils/format.ts';
  import Badge from '../widgets/Badge.svelte';
  import DagView from '../widgets/DagView.svelte';
  import ConfirmDialog from './shared/ConfirmDialog.svelte';
  import EmptyState from './shared/EmptyState.svelte';
  import FilterBar from './shared/FilterBar.svelte';
  import PanelHeader from './shared/PanelHeader.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';

  $effect(() => {
    workflowStore.startPolling(5000);
    return () => { workflowStore.stopPolling(); };
  });

  let searchQuery = $state('');
  let filterStatus = $state('');

  let workflows = $derived(workflowStore.workflows ?? []);
  let definitions = $derived(workflowStore.uniqueDefinitions ?? []);

  let filteredWorkflows = $derived.by(() => {
    let result = workflows;
    if (searchQuery) {
      const q = searchQuery.toLowerCase();
      result = result.filter(w => (w.name ?? w.id).toLowerCase().includes(q));
    }
    if (filterStatus) {
      result = result.filter(w => w.status === filterStatus);
    }
    return result;
  });

  let filteredDefinitions = $derived.by(() => {
    if (!searchQuery) return definitions;
    const q = searchQuery.toLowerCase();
    return definitions.filter(d => d.name.toLowerCase().includes(q));
  });
  let selected = $derived(workflowStore.selectedWorkflow ?? null);

  /** Track whether we're viewing an instance or a definition. */
  let viewMode = $state<'instance' | 'definition' | null>(null);
  let selectedDef = $state<WorkflowDefinition | null>(null);

  function selectWorkflow(wf: WorkflowSummary) {
    viewMode = 'instance';
    selectedDef = null;
    workflowStore.fetchDetail(wf.id);
  }

  function selectDefinition(def: WorkflowDefinition) {
    viewMode = 'definition';
    selectedDef = def;
  }


  function stepProgress(wf: WorkflowSummary) {
    if (!wf.steps?.length) return '0/0';
    const done = wf.steps.filter(s =>
      s.status === 'completed' || s.status === 'approved'
    ).length;
    return `${done}/${wf.steps.length}`;
  }

  function stepProgressPct(wf: WorkflowSummary) {
    // Use explicit progress field if available.
    if (typeof wf.progress === 'number') return wf.progress * 100;
    if (!wf.steps?.length) return 0;
    const done = wf.steps.filter(s =>
      s.status === 'completed' || s.status === 'approved'
    ).length;
    return (done / wf.steps.length) * 100;
  }

  /**
   * Label for a list row's progress slot. The SSE summary
   * (workflows.svelte.ts applySnapshot) carries `progress` + `current_step`
   * but NOT the `steps[]` array, so stepProgress() would render a misleading
   * "0/0 steps" for every running workflow. Prefer a real step count when
   * present (REST detail rows), then the progress %, then the current step —
   * never imply zero total steps.
   */
  function listProgressLabel(wf: WorkflowSummary) {
    if (wf.steps?.length) {
      const done = wf.steps.filter(s =>
        s.status === 'completed' || s.status === 'approved'
      ).length;
      return `${done}/${wf.steps.length} steps`;
    }
    if (typeof wf.progress === 'number') return `${Math.round(wf.progress * 100)}% complete`;
    if (wf.current_step) return `@ ${wf.current_step}`;
    return '';
  }

  function dagSteps(wf: WorkflowSummary) {
    if (!wf.steps) return [];
    return wf.steps.map(s => ({
      id: s.id ?? s.name,
      name: s.name ?? s.id,
      status: s.status ?? 'pending',
      depends_on: s.depends_on ?? [],
    }));
  }


  function eventVariant(eventType: string): BadgeVariant {
    const map: Record<string, BadgeVariant> = {
      started: 'info',
      completed: 'success',
      failed: 'error',
      approved: 'success',
      rejected: 'error',
      waiting_approval: 'warning',
      step_started: 'info',
      step_completed: 'success',
      step_failed: 'error',
    };
    return map[eventType] ?? 'info';
  }

  let hasWaitingStep = $derived.by(() => {
    if (!selected?.steps) return false;
    return selected.steps.some(s => s.status === 'waiting_approval');
  });

  let waitingStep = $derived.by(() => {
    if (!selected?.steps) return null;
    return selected.steps.find(s => s.status === 'waiting_approval');
  });

  // Approve / Reject / Cancel are applied to a RUNNING autonomous workflow, so
  // each one is stray-click gated through the shared ConfirmDialog and reports
  // its outcome as a toast. The target ids are captured at click time: the
  // panel re-polls detail every 5s, so `selected` / `waitingStep` can move
  // between the click and the confirm.
  type PendingDecision =
    | { kind: 'approve' | 'reject'; label: string; workflowId: string; stepId: string }
    | { kind: 'cancel'; label: string; workflowId: string };

  let pending = $state<PendingDecision | null>(null);
  let busy = $state(false);

  const DIALOG_TITLES = {
    approve: 'Approve this step?',
    reject: 'Reject this step?',
    cancel: 'Cancel this workflow?',
  } as const;

  const SUCCESS_COPY = {
    approve: 'Step approved',
    reject: 'Step rejected',
    cancel: 'Workflow cancelled',
  } as const;

  const ACTION_VERBS = {
    approve: 'Approve',
    reject: 'Reject',
    cancel: 'Cancel workflow',
  } as const;

  function requestDecision(kind: 'approve' | 'reject') {
    const step = waitingStep;
    if (!step || !selected) return;
    pending = {
      kind,
      label: `Step "${step.name ?? step.id}" of ${selected.name ?? selected.id}`,
      workflowId: selected.id,
      stepId: step.id ?? step.name,
    };
  }

  function requestCancel() {
    if (!selected) return;
    pending = {
      kind: 'cancel',
      label: `Workflow ${selected.name ?? selected.id}`,
      workflowId: selected.id,
    };
  }

  async function runPending(): Promise<void> {
    const req = pending;
    if (!req) return;
    busy = true;
    try {
      if (req.kind === 'approve') await workflowStore.approveStep(req.workflowId, req.stepId);
      else if (req.kind === 'reject') await workflowStore.rejectStep(req.workflowId, req.stepId);
      else await workflowStore.cancelWorkflow(req.workflowId);
      const failure = workflowStore.actionError;
      if (failure) toastStore.error(`${ACTION_VERBS[req.kind]} failed: ${failure}`);
      else toastStore.success(SUCCESS_COPY[req.kind]);
    } finally {
      busy = false;
      pending = null;
    }
  }

  function clearFilters() {
    searchQuery = '';
    filterStatus = '';
  }

  // Cancelling an already-terminal workflow is a no-op server-side; keep
  // the button visible for layout stability but disable it, matching how
  // Approve/Reject are gated on hasWaitingStep.
  const TERMINAL_STATUSES = new Set(['completed', 'failed', 'cancelled']);
  let selectedIsTerminal = $derived(TERMINAL_STATUSES.has(selected?.status ?? ''));
</script>

<div class="panel workflows-panel">
  <PanelHeader title="Workflows" icon={'⚙'} count={workflows.length}>
    {#snippet stats()}
      <span class="text-muted text-xs">{definitions.length} definition{definitions.length === 1 ? '' : 's'}</span>
    {/snippet}
  </PanelHeader>

  {#if workflowStore.error}
    <ErrorBanner prefix="Workflow refresh failed" message={workflowStore.error} />
  {/if}

  <div class="wf-body">
  <!-- Left sidebar: workflow list -->
  <div class="wf-sidebar">
    <div class="sidebar-filter">
      <FilterBar
        search={searchQuery}
        placeholder="Filter workflows..."
        filters={[
          { key: 'status', label: 'Status', options: [
            { value: 'running', label: 'Running' },
            { value: 'pending', label: 'Pending' },
            { value: 'waiting_approval', label: 'Waiting' },
            { value: 'completed', label: 'Completed' },
            { value: 'failed', label: 'Failed' },
            { value: 'cancelled', label: 'Cancelled' },
          ], value: filterStatus },
        ]}
        resultCount={filteredWorkflows.length + filteredDefinitions.length}
        onSearch={(q) => { searchQuery = q; }}
        onFilter={(key, val) => { if (key === 'status') filterStatus = val; }}
        onClear={clearFilters}
      />
    </div>
    {#if workflowStore.loading}
      <div class="loading-bar"><div class="loading-bar-inner"></div></div>
    {/if}
    <div class="wf-list">
      <!-- Workflow instances — the list mixes every lifecycle state
           (running, waiting, cancelled, failed), so the heading must not
           claim "Running"; each row's status badge carries the state. -->
      {#if filteredWorkflows.length > 0}
        <div class="wf-section-label">Instances</div>
        {#each filteredWorkflows as wf (wf.id)}
          <button
            class="wf-item"
            class:wf-selected={viewMode === 'instance' && selected?.id === wf.id}
            onclick={() => selectWorkflow(wf)}
          >
            <div class="wf-item-top">
              <span class="wf-name truncate">{wf.name ?? wf.id}</span>
              <Badge text={wf.status ?? 'pending'} variant={statusVariant(wf.status)} />
            </div>
            <div class="wf-item-bottom">
              <span class="wf-progress text-mono text-xs">{listProgressLabel(wf)}</span>
              <span class="wf-time text-mono text-xs text-muted">{relativeTime(wf.started_at)}</span>
            </div>
            <div class="wf-progress-track">
              <div class="wf-progress-fill" style="width: {stepProgressPct(wf).toFixed(0)}%"></div>
            </div>
          </button>
        {/each}
      {/if}

      <!-- Registered definitions -->
      {#if filteredDefinitions.length > 0}
        <div class="wf-section-label">Definitions</div>
        {#each filteredDefinitions as def (def.id)}
          <button
            class="wf-item wf-def-item"
            class:wf-selected={viewMode === 'definition' && selectedDef?.id === def.id}
            onclick={() => selectDefinition(def)}
          >
            <div class="wf-item-top">
              <span class="wf-name truncate">{def.name}</span>
              <Badge text="{def.step_count} steps" variant="info" />
            </div>
            <div class="wf-item-bottom">
              <span class="wf-progress text-mono text-xs text-muted">{def.created_by}</span>
              <span class="wf-time text-mono text-xs text-muted">{relativeTime(def.created_at)}</span>
            </div>
          </button>
        {/each}
      {/if}

      <!-- Empty state -->
      {#if filteredWorkflows.length === 0 && filteredDefinitions.length === 0}
        {#if searchQuery || filterStatus}
          <EmptyState icon={'\u2699'} heading="No matches" description="Try adjusting your search or filters" compact />
        {:else}
          <EmptyState icon={'\u2699'} heading="No workflows" compact />
        {/if}
      {/if}
    </div>
  </div>

  <!-- Right main area: detail -->
  <div class="wf-detail">
    {#if workflowStore.loading && viewMode === 'instance'}
      <div class="loading-bar"><div class="loading-bar-inner"></div></div>
    {/if}
    {#if viewMode === 'instance' && selected}
      <!-- Instance detail view -->
      <div class="detail-top">
        <div class="detail-title-row">
          <h2 class="detail-title">{selected.name ?? selected.id}</h2>
          <Badge text={selected.status ?? 'pending'} variant={statusVariant(selected.status)} />
        </div>
        <div class="detail-meta">
          <span class="text-mono text-xs text-muted">
            Started: {formatDateTime(selected.started_at)}
          </span>
          {#if selected.completed_at}
            <span class="text-mono text-xs text-muted">
              Completed: {formatDateTime(selected.completed_at)}
            </span>
          {/if}
          <span class="text-mono text-xs text-muted">
            Progress: {stepProgress(selected)}
          </span>
        </div>
      </div>

      <!-- Center: DAG -->
      <div class="detail-dag">
        <DagView steps={dagSteps(selected)} />
      </div>

      <!-- Bottom: Event timeline -->
      <div class="detail-events">
        <div class="events-header">
          <span class="card-title">Event Timeline</span>
        </div>
        <div class="events-scroll">
          {#if selected.events?.length}
            {#each selected.events as event, i (event.id ?? `${event.timestamp}-${i}`)}
              <div class="event-row">
                <span class="event-time text-mono">{formatTime(event.timestamp)}</span>
                <Badge text={event.event_type ?? 'event'} variant={eventVariant(event.event_type ?? '')} />
                {#if event.step_name}
                  <span class="event-step text-mono">{event.step_name}</span>
                {/if}
                {#if event.details}
                  <span class="event-details text-muted truncate">{event.details}</span>
                {/if}
              </div>
            {/each}
          {:else}
            <EmptyState icon={'\u25B6'} heading="No events yet" compact />
          {/if}
        </div>
      </div>

      <!-- Action bar -->
      <div class="action-bar">
        <button
          class="btn btn-success"
          disabled={!hasWaitingStep || busy}
          onclick={() => requestDecision('approve')}
        >
          Approve
        </button>
        <button
          class="btn btn-danger"
          disabled={!hasWaitingStep || busy}
          onclick={() => requestDecision('reject')}
        >
          Reject
        </button>
        <div class="toolbar-spacer"></div>
        <button
          class="btn btn-ghost"
          disabled={selectedIsTerminal || busy}
          title={selectedIsTerminal ? `Workflow already ${selected?.status}` : 'Cancel this workflow'}
          onclick={requestCancel}
        >
          Cancel Workflow
        </button>
      </div>

    {:else if viewMode === 'definition' && selectedDef}
      <!-- Definition detail view -->
      <div class="detail-top">
        <div class="detail-title-row">
          <h2 class="detail-title">{selectedDef.name}</h2>
          <Badge text="definition" variant="info" />
        </div>
        <div class="detail-meta">
          <span class="text-mono text-xs text-muted">
            Steps: {selectedDef.step_count}
          </span>
          <span class="text-mono text-xs text-muted">
            Created by: {selectedDef.created_by}
          </span>
          {#if selectedDef.namespace}
            <span class="text-mono text-xs text-muted">
              Namespace: {selectedDef.namespace}
            </span>
          {/if}
          <span class="text-mono text-xs text-muted">
            Registered: {formatDateTime(selectedDef.created_at)}
          </span>
        </div>
      </div>

      <div class="def-detail-body">
        {#if selectedDef.description}
          <div class="def-description">
            <span class="card-title">Description</span>
            <p class="def-description-text">{selectedDef.description}</p>
          </div>
        {/if}

        <div class="def-info-grid">
          <div class="def-info-card">
            <span class="def-info-label">ID</span>
            <span class="def-info-value text-mono">{selectedDef.id}</span>
          </div>
          <div class="def-info-card">
            <span class="def-info-label">Steps</span>
            <span class="def-info-value">{selectedDef.step_count}</span>
          </div>
          <div class="def-info-card">
            <span class="def-info-label">Created By</span>
            <span class="def-info-value">{selectedDef.created_by}</span>
          </div>
          {#if selectedDef.namespace}
            <div class="def-info-card">
              <span class="def-info-label">Namespace</span>
              <span class="def-info-value">{selectedDef.namespace}</span>
            </div>
          {/if}
        </div>

        <div class="def-usage">
          <span class="card-title">Usage</span>
          <code class="def-usage-code">agent_workflow_start(definition_id="{selectedDef.id}", input=&#123;...&#125;)</code>
        </div>
      </div>

    {:else}
      <EmptyState icon={'\u2699'} heading="Select a workflow" description="Choose a workflow from the list to view its DAG and events" />
    {/if}
  </div>
  </div>
</div>

<ConfirmDialog
  open={pending !== null}
  title={pending ? DIALOG_TITLES[pending.kind] : ''}
  message={pending
    ? `${pending.label} — this decision is applied to the running autonomous workflow immediately.`
    : ''}
  confirmLabel={pending ? ACTION_VERBS[pending.kind] : 'Confirm'}
  variant={pending?.kind === 'approve' ? 'default' : 'danger'}
  onConfirm={runPending}
  onCancel={() => (pending = null)}
/>

<style>
  .workflows-panel {
    display: flex;
    flex-direction: column;
    overflow: hidden;
    gap: 0;
  }

  .wf-body {
    display: flex;
    flex: 1;
    min-height: 0;
    overflow: hidden;
  }

  /* Sidebar */
  .wf-sidebar {
    width: 30%;
    min-width: 220px;
    border-right: 1px solid var(--border);
    display: flex;
    flex-direction: column;
    background: var(--bg-secondary);
  }

  .sidebar-filter {
    border-bottom: 1px solid var(--border);
  }

  .wf-list {
    flex: 1;
    overflow-y: auto;
  }

  .wf-item {
    display: flex;
    flex-direction: column;
    gap: 4px;
    width: 100%;
    padding: var(--space-2) var(--space-3);
    text-align: left;
    border-bottom: 1px solid var(--border-subtle);
    cursor: pointer;
    transition: background var(--transition-fast);
  }

  .wf-item:hover {
    background: var(--bg-tertiary);
  }

  .wf-item:focus-visible {
    outline: 2px solid var(--info);
    outline-offset: 2px;
    border-radius: var(--radius-sm);
  }

  .wf-selected {
    background: var(--info-dim) !important;
    border-left: 3px solid var(--info);
    padding-left: 11px;
    box-shadow: 0 0 6px var(--glow-accent);
  }

  .wf-item-top {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .wf-name {
    font-weight: 500;
    color: var(--fg-primary);
    font-size: var(--text-sm);
  }

  .wf-item-bottom {
    display: flex;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .wf-progress {
    color: var(--fg-secondary);
  }

  .wf-time {
    flex-shrink: 0;
  }

  /* Detail area */
  .wf-detail {
    flex: 1;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    min-width: 0;
  }

  .detail-top {
    position: relative;
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border);
  }

  .detail-top::after {
    content: '';
    position: absolute;
    bottom: 0;
    left: 10%;
    right: 10%;
    height: 1px;
    background: linear-gradient(90deg, transparent, rgba(var(--info-rgb), 0.06) 50%, transparent);
    pointer-events: none;
  }

  .detail-title-row {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 6px;
  }

  .detail-title {
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--fg-primary);
    margin: 0;
  }

  .detail-meta {
    display: flex;
    gap: var(--space-4);
    flex-wrap: wrap;
  }

  .detail-dag {
    flex: 1;
    min-height: 120px;
    overflow: auto;
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border);
  }

  .detail-events {
    flex: 1;
    min-height: 100px;
    max-height: 200px;
    display: flex;
    flex-direction: column;
    border-bottom: 1px solid var(--border);
  }

  .events-header {
    padding: var(--space-2) var(--space-4);
    border-bottom: 1px solid var(--border);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }

  .events-scroll {
    flex: 1;
    overflow-y: auto;
  }

  .event-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: 5px var(--space-4);
    font-size: var(--text-xs);
    border-bottom: 1px solid var(--border-subtle);
    transition: background var(--transition-fast);
  }

  .event-row:hover {
    background: var(--bg-elevated);
  }

  .event-row:last-child {
    border-bottom: none;
  }

  .event-time {
    color: var(--fg-dim);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    flex-shrink: 0;
    width: 65px;
    letter-spacing: var(--tracking-normal);
  }

  .event-step {
    color: var(--fg-secondary);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    flex-shrink: 0;
  }

  .event-details {
    flex: 1;
    min-width: 0;
    font-size: var(--text-xs);
    color: var(--fg-secondary);
  }

  /* Action bar */
  .action-bar {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-4);
    background: var(--bg-secondary);
    border-top: 1px solid var(--border);
    position: relative;
  }

  .action-bar::before {
    content: '';
    position: absolute;
    inset: 0;
    border-radius: inherit;
    background: var(--surface-highlight);
    pointer-events: none;
  }

  .action-bar .btn:disabled {
    opacity: 0.35;
    cursor: not-allowed;
  }

  /* Section labels */
  .wf-section-label {
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-dim);
    padding: var(--space-2) var(--space-3) var(--space-1);
    background: var(--bg-secondary);
  }

  .wf-def-item {
    opacity: 0.85;
  }

  /* Definition detail view */
  .def-detail-body {
    flex: 1;
    overflow-y: auto;
    padding: var(--space-4);
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }

  .def-description {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .def-description-text {
    color: var(--fg-secondary);
    font-size: var(--text-sm);
    line-height: 1.5;
    margin: 0;
  }

  .def-info-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(160px, 1fr));
    gap: var(--space-2);
  }

  .def-info-card {
    display: flex;
    flex-direction: column;
    gap: 3px;
    padding: var(--space-2) var(--space-3);
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    position: relative;
    transition: border-color var(--transition-fast);
  }

  .def-info-card::before {
    content: '';
    position: absolute;
    inset: 0;
    border-radius: inherit;
    background: var(--surface-highlight);
    pointer-events: none;
  }

  .def-info-card:hover {
    border-color: color-mix(in srgb, var(--info) 30%, var(--border));
  }

  .def-info-label {
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-dim);
  }

  .def-info-value {
    font-size: var(--text-sm);
    color: var(--fg-primary);
    word-break: break-all;
    font-family: var(--font-mono);
  }

  .def-usage {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .def-usage-code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    background: var(--bg-tertiary);
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-md);
    border: 1px solid var(--border-subtle);
    white-space: pre-wrap;
    word-break: break-all;
    letter-spacing: var(--tracking-normal);
  }

  .wf-progress-track {
    height: 3px;
    background: var(--bg-tertiary);
    border-radius: 2px;
    overflow: hidden;
    margin-top: 2px;
  }

  .wf-progress-fill {
    height: 100%;
    background: var(--success);
    border-radius: 2px;
    transition: width var(--transition-normal);
    box-shadow: 0 0 4px var(--glow-success);
  }

  /* Loading bar */
  .loading-bar {
    height: 2px;
    background: var(--bg-tertiary);
    border-radius: 1px;
    overflow: hidden;
  }

  .loading-bar-inner {
    width: 40%;
    height: 100%;
    background: var(--accent);
    border-radius: 1px;
    animation: loadingSlide 1s ease-in-out infinite;
  }

  @keyframes loadingSlide {
    0% { transform: translateX(-100%); }
    100% { transform: translateX(300%); }
  }
</style>
