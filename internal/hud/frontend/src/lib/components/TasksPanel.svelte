<script lang="ts">
  /**
   * TasksPanel — composition shell for the Activity → Tasks view. The
   * heavy zones live in `lib/components/tasks/*` and pure helpers in
   * `lib/utils/tasksHelpers.ts` per the panel decomposition pattern
   * (`docs/HUD_PANEL_DECOMP.md`).
   */
  import { router } from '../stores/router.svelte.ts';
  import { taskStore } from '../stores/tasks.svelte.ts';
  import { agentStore } from '../stores/agents.svelte.ts';
  import { coordinationStore } from '../stores/coordination.svelte.ts';
  import { toastStore } from '../stores/toasts.svelte.ts';
  import Badge from '../widgets/Badge.svelte';
  import FilterBar from './shared/FilterBar.svelte';
  import PanelHeader from './shared/PanelHeader.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';
  import BulkToolbar from './shared/BulkToolbar.svelte';
  import TasksRadar from './tasks/TasksRadar.svelte';
  import TasksTableView from './tasks/TasksTableView.svelte';
  import TasksGroupedView from './tasks/TasksGroupedView.svelte';
  import CreateTaskModal from './tasks/CreateTaskModal.svelte';
  import ResolveTaskModal from './tasks/ResolveTaskModal.svelte';
  import TaskDetail from './tasks/TaskDetail.svelte';
  import {
    filterTasks,
    sortTasks,
    groupTasksByStatus,
    agentOptionsFrom,
    PRIORITY_CYCLE,
  } from '../utils/tasksHelpers';

  $effect(() => {
    taskStore.startPolling(60000);
    agentStore.startPolling(30000);
    coordinationStore.startPolling(30000);
    return () => {
      taskStore.stopPolling();
      agentStore.stopPolling();
      coordinationStore.stopPolling();
    };
  });

  let tasks = $derived(taskStore.tasks ?? []);

  // Panel-wide filter/sort/view state. Per the B1 contract this is the
  // candidate set for the store contract, but tasks already has parallel
  // store filter state owned by OverlayShell; keeping the panel state
  // local avoids breaking that consumer.
  let searchQuery = $state('');
  let priorityFilter = $state('');
  let agentFilter = $state('');
  let statusFilter = $state('');
  let viewMode = $state<'flat' | 'grouped'>('flat');
  let collapsedGroups = $state<Set<string>>(new Set());
  let sortKey = $state('created_at');
  let sortDir = $state<'asc' | 'desc'>('desc');

  let showCreateModal = $state(false);
  let showResolveModal = $state(false);
  let resolveTaskId = $state('');
  let resolveTaskTitle = $state('');
  let selectedTask = $state<any>(null);
  let selectedTaskIds = $state<Set<string>>(new Set());

  let pendingCt = $derived(tasks.filter((t) => t.status === 'pending').length);
  let inProgressCt = $derived(tasks.filter((t) => t.status === 'in_progress').length);
  let blockedCt = $derived(tasks.filter((t) => t.status === 'blocked').length);
  let completedCt = $derived(tasks.filter((t) => t.status === 'completed').length);

  let agentOptions = $derived(agentOptionsFrom(tasks));

  let filterDefs = $derived([
    {
      key: 'priority',
      label: 'All Priority',
      value: priorityFilter,
      options: [
        { value: 'critical', label: 'Critical' },
        { value: 'high', label: 'High' },
        { value: 'medium', label: 'Medium' },
        { value: 'low', label: 'Low' },
      ],
    },
    { key: 'agent', label: 'All Agents', value: agentFilter, options: agentOptions },
    {
      key: 'status',
      label: 'All Status',
      value: statusFilter,
      options: [
        { value: 'pending', label: 'Pending' },
        { value: 'in_progress', label: 'In Progress' },
        { value: 'blocked', label: 'Blocked' },
        { value: 'completed', label: 'Completed' },
      ],
    },
  ]);

  let hasActiveFilters = $derived(
    searchQuery.trim() !== '' || priorityFilter !== '' || agentFilter !== '' || statusFilter !== ''
  );
  let filtered = $derived(filterTasks(tasks, searchQuery, priorityFilter, agentFilter, statusFilter));
  let sorted = $derived(sortTasks(filtered, sortKey, sortDir));
  let grouped = $derived(groupTasksByStatus(filtered));

  // Clear selection when filters change.
  $effect(() => {
    searchQuery; priorityFilter; agentFilter; statusFilter;
    selectedTaskIds = new Set();
  });

  function handleFilter(key: string, val: string) {
    if (key === 'priority') priorityFilter = val;
    else if (key === 'agent') agentFilter = val;
    else if (key === 'status') statusFilter = val;
  }
  // Header stat pills double as status filters, matching the Plans panel's
  // clickable phase pills so the same visual pattern behaves the same way
  // across the Work tab.
  function toggleStatusFilter(status: string) {
    statusFilter = statusFilter === status ? '' : status;
  }
  function clearFilters() { searchQuery = ''; priorityFilter = ''; agentFilter = ''; statusFilter = ''; }
  function toggleGroup(status: string) {
    const next = new Set(collapsedGroups);
    next.has(status) ? next.delete(status) : next.add(status);
    collapsedGroups = next;
  }

  async function cyclePriority(task: any) {
    const idx = PRIORITY_CYCLE.indexOf(task.priority ?? 'medium');
    const next = PRIORITY_CYCLE[(idx + 1) % PRIORITY_CYCLE.length];
    const ok = await taskStore.setPriority(task.id, next);
    if (ok) toastStore.info(`Priority → ${next}`);
    else toastStore.error(`Failed to set priority → ${next}`);
  }
  async function changeStatus(task: any, newStatus: string) {
    const ok = await taskStore.updateStatus(task.id, newStatus);
    if (ok) toastStore.info(`Status → ${newStatus.replaceAll('_', ' ')}`);
    else toastStore.error(`Failed to set status → ${newStatus.replaceAll('_', ' ')}`);
  }
  function openResolve(task: any) {
    resolveTaskId = task.id;
    resolveTaskTitle = task.title;
    showResolveModal = true;
  }
  function selectTask(task: any) {
    selectedTask = selectedTask?.id === task.id ? null : task;
  }
  // Select a task by id (used by in-drawer cross-links); switches to the
  // target rather than toggling, so following a blocked-by chain works.
  function selectTaskById(taskId: string) {
    const next = tasks.find((t) => t.id === taskId);
    if (next) selectedTask = next;
  }
  function lookupTask(taskId: string) {
    return tasks.find((t) => t.id === taskId);
  }
  // Deep-link a task to its Plan Store plan in the Work → Plans sub-view.
  function openPlan(planId: string) {
    selectedTask = null;
    router.navigate('tasks', 'plans', planId);
  }

  function reportBulk(verb: string, total: number, failures: number) {
    const ok = total - failures;
    if (failures === 0) toastStore.success(`${total} tasks ${verb}`);
    else if (ok === 0) toastStore.error(`Failed to ${verb} ${total} tasks`);
    else toastStore.warning(`${verb}: ${ok} of ${total} succeeded (${failures} failed)`);
  }
  // A bulk pass is a sequential loop of mutations; bulkBusy locks the toolbar
  // for its duration so a second click cannot overlap a draining selection.
  let bulkBusy = $state(false);
  async function bulkComplete() {
    const ids = [...selectedTaskIds];
    let failures = 0;
    bulkBusy = true;
    try {
      for (const id of ids) {
        if (!(await taskStore.updateStatus(id, 'completed'))) failures++;
      }
    } finally {
      bulkBusy = false;
    }
    reportBulk('completed', ids.length, failures);
    selectedTaskIds = new Set();
  }
  async function bulkCancel() {
    const ids = [...selectedTaskIds];
    let failures = 0;
    bulkBusy = true;
    try {
      for (const id of ids) {
        if (!(await taskStore.updateStatus(id, 'cancelled'))) failures++;
      }
    } finally {
      bulkBusy = false;
    }
    reportBulk('cancelled', ids.length, failures);
    selectedTaskIds = new Set();
  }
  async function bulkHighPriority() {
    const ids = [...selectedTaskIds];
    let failures = 0;
    for (const id of ids) {
      if (!(await taskStore.setPriority(id, 'high'))) failures++;
    }
    reportBulk('set to high priority', ids.length, failures);
    selectedTaskIds = new Set();
  }
  let bulkActions = $derived([
    { label: 'Complete', variant: 'success', onclick: bulkComplete },
    {
      label: 'Cancel',
      variant: 'danger',
      onclick: bulkCancel,
      confirm: {
        title: 'Cancel selected tasks?',
        message: `Cancel ${selectedTaskIds.size} task(s)? This cannot be undone from the HUD.`,
        confirmLabel: 'Cancel tasks',
      },
    },
    { label: 'High Priority', variant: 'warning', onclick: bulkHighPriority },
  ]);
</script>

<div class="panel tasks-panel">
  <PanelHeader title="Tasks" icon={'☑'} count={tasks.length}>
    {#snippet stats()}
      <button class="pill-btn" class:pill-active={statusFilter === 'pending'} aria-pressed={statusFilter === 'pending'} onclick={() => toggleStatusFilter('pending')} title="Filter to pending">
        <Badge text="{pendingCt} pending" variant="warning" />
      </button>
      <button class="pill-btn" class:pill-active={statusFilter === 'in_progress'} aria-pressed={statusFilter === 'in_progress'} onclick={() => toggleStatusFilter('in_progress')} title="Filter to in-progress">
        <Badge text="{inProgressCt} in-progress" variant="info" />
      </button>
      <button class="pill-btn" class:pill-active={statusFilter === 'blocked'} aria-pressed={statusFilter === 'blocked'} onclick={() => toggleStatusFilter('blocked')} title="Filter to blocked">
        <Badge text="{blockedCt} blocked" variant="error" />
      </button>
      <button class="pill-btn" class:pill-active={statusFilter === 'completed'} aria-pressed={statusFilter === 'completed'} onclick={() => toggleStatusFilter('completed')} title="Filter to completed">
        <Badge text="{completedCt} completed" variant="success" />
      </button>
    {/snippet}
    {#snippet actions()}
      <button class="btn btn-success" onclick={() => showCreateModal = true}>+ New Task</button>
      <div class="view-toggle" role="group" aria-label="Task list layout">
        <button class="btn btn-ghost" class:active-toggle={viewMode === 'flat'} aria-pressed={viewMode === 'flat'} onclick={() => viewMode = 'flat'}>Flat</button>
        <button class="btn btn-ghost" class:active-toggle={viewMode === 'grouped'} aria-pressed={viewMode === 'grouped'} onclick={() => viewMode = 'grouped'}>By Status</button>
      </div>
    {/snippet}
  </PanelHeader>

  {#if taskStore.error}
    <!-- A failed task poll previously showed stale rows with no signal. -->
    <ErrorBanner prefix="Task refresh failed" message={taskStore.error} />
  {/if}

  <FilterBar
    search={searchQuery}
    placeholder="Search tasks..."
    filters={filterDefs}
    resultCount={filtered.length}
    onSearch={(val) => searchQuery = val}
    onFilter={handleFilter}
    onClear={clearFilters}
  />

  <div class="tasks-layout">
    <div class="task-main">
      <div class="task-content">
        {#if viewMode === 'flat'}
          <TasksTableView
            rows={sorted}
            {sortKey}
            {sortDir}
            {hasActiveFilters}
            selectedIds={selectedTaskIds}
            onSort={(key, dir) => { sortKey = key; sortDir = dir; }}
            onRowClick={selectTask}
            onSelect={(ids) => selectedTaskIds = ids}
            onCyclePriority={cyclePriority}
            onChangeStatus={changeStatus}
            onResolve={openResolve}
            onClearFilters={clearFilters}
          />
          <BulkToolbar
            count={selectedTaskIds.size}
            actions={bulkActions}
            busy={bulkBusy}
            onClearSelection={() => { selectedTaskIds = new Set(); }}
          />
        {:else}
          <TasksGroupedView
            groups={grouped}
            {collapsedGroups}
            {hasActiveFilters}
            onToggleGroup={toggleGroup}
            onClearFilters={clearFilters}
            onCyclePriority={cyclePriority}
            onChangeStatus={changeStatus}
            onResolve={openResolve}
          />
        {/if}
      </div>
    </div>
    <TasksRadar />
  </div>
</div>

<CreateTaskModal open={showCreateModal} onClose={() => showCreateModal = false} />
<ResolveTaskModal open={showResolveModal} taskId={resolveTaskId} taskTitle={resolveTaskTitle} onClose={() => showResolveModal = false} />
<TaskDetail
  task={selectedTask}
  onClose={() => selectedTask = null}
  onResolve={openResolve}
  onOpenPlan={openPlan}
  onSelectTask={selectTaskById}
  {lookupTask}
/>

<style>
  .tasks-panel { display: flex; flex-direction: column; overflow: hidden; }
  .pill-btn { background: none; border: none; padding: 0; cursor: pointer; border-radius: var(--radius-full); transition: filter var(--transition-fast); }
  .pill-btn:hover { filter: brightness(1.25); }
  .pill-btn:focus-visible { outline: 2px solid color-mix(in srgb, var(--info) 55%, transparent); outline-offset: 2px; }
  .pill-btn.pill-active { outline: 2px solid var(--accent); outline-offset: 1px; border-radius: var(--radius-sm); }
  .view-toggle { display: flex; gap: 2px; background: var(--bg-tertiary); border-radius: var(--radius-sm); padding: 2px; }
  .active-toggle {
    background: var(--bg-elevated) !important; color: var(--fg-primary) !important;
    box-shadow: var(--glow-shadow-sm) rgba(var(--info-rgb), 0.1);
  }
  .task-content {
    flex: 1; min-height: 0; overflow-y: auto;
    background: var(--bg-secondary); border: 1px solid var(--border);
    border-radius: var(--radius-md); position: relative;
  }
  .task-content::before {
    content: ''; position: absolute; inset: 0; border-radius: inherit;
    background: var(--surface-highlight); pointer-events: none; z-index: 1;
  }
  .tasks-layout {
    flex: 1; min-height: 0; display: grid;
    grid-template-columns: minmax(0, 1fr) 300px;
    gap: var(--space-3); margin-top: var(--space-2);
  }
  .task-main { min-height: 0; display: flex; flex-direction: column; }
  @media (max-width: 1200px) { .tasks-layout { grid-template-columns: 1fr; } }
</style>
