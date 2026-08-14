// Tasks store - task management
// v2: SSE-first with 60s fallback poll. Applies task list from hud.fleet snapshots.
import { actionStore } from './action.svelte.ts';
import { eventStore } from './events.svelte.ts';
import { isStaleFromTimestamp, stalenessStore } from './staleness.svelte.ts';
import { arraysEqualById } from '../utils/diff.ts';
import { createPoller } from '../utils/poller.ts';

function errorMessage(e: unknown): string {
  if (e instanceof Error) return e.message || 'Unknown error';
  if (typeof e === 'string') return e;
  try { return JSON.stringify(e); } catch { return 'Unknown error'; }
}

export interface Task {
  id: string;
  session_id: string;
  agent_id: string;
  agent: string;
  namespace: string;
  title: string;
  context: string;
  description: string;
  priority: 'low' | 'medium' | 'high' | 'critical';
  status: 'pending' | 'in_progress' | 'completed' | 'blocked' | 'cancelled';
  tags: string[];
  blocked_by: string[];
  // Optional: dependency IDs already satisfied, used by the tasks UI to mark
  // resolved deps in the blocked-by list. Read via `resolved_deps?.includes()`
  // in TasksTableView/TasksGroupedView/TaskDetail; may be absent on payloads
  // that don't compute it.
  resolved_deps?: string[];
  // Plan Store linkage (S7b): a task is a granular TODO under a plan slice.
  // plan_id/slice_id deep-link a task to its first-class Plan in the Work view.
  plan_id?: string;
  slice_id?: string;
  created_at: string;
  updated_at: string;
}

export interface TasksResponse {
  tasks: Task[];
}

export type TaskSortField = 'priority' | 'status' | 'updated_at' | 'created_at' | 'title';
export type TaskSortDir = 'asc' | 'desc';

const PRIORITY_ORDER: Record<string, number> = {
  critical: 0,
  high: 1,
  medium: 2,
  low: 3,
};

const STATUS_ORDER: Record<string, number> = {
  in_progress: 0,
  blocked: 1,
  pending: 2,
  completed: 3,
  cancelled: 4,
};

class TaskStore {
  tasks = $state<Task[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);

  filterStatus = $state<string>('all');
  filterPriority = $state<string>('all');
  sortField = $state<TaskSortField>('priority');
  sortDir = $state<TaskSortDir>('asc');

  // Staleness (Slice B3) — see fleet.svelte.ts for the pattern. tasks data
  // arrives bundled in hud.fleet snapshots, so this matches the fleet cadence.
  staleAfter = 90_000;
  get isStale(): boolean {
    // Staleness only applies while polling is active (page mounted). An
    // unmounted page's store keeps a frozen lastUpdated forever; reporting
    // it stale would pin the global "Stale data" banner permanently.
    if (!this.poller.running) return false;
    return isStaleFromTimestamp(this.lastUpdated, this.staleAfter);
  }

  // 60s watchdog poll — fires on SSE-down OR on stale.
  private poller = createPoller(() => {
    if (!eventStore.connected || this.isStale) this.fetch();
  }, 60000);
  private eventUnsubs: Array<() => void> = [];

  get filteredTasks(): Task[] {
    let result = [...this.tasks];

    if (this.filterStatus !== 'all') {
      result = result.filter((t) => t.status === this.filterStatus);
    }
    if (this.filterPriority !== 'all') {
      result = result.filter((t) => t.priority === this.filterPriority);
    }

    result.sort((a, b) => {
      let cmp = 0;
      switch (this.sortField) {
        case 'priority':
          cmp = (PRIORITY_ORDER[a.priority] ?? 9) - (PRIORITY_ORDER[b.priority] ?? 9);
          break;
        case 'status':
          cmp = (STATUS_ORDER[a.status] ?? 9) - (STATUS_ORDER[b.status] ?? 9);
          break;
        case 'updated_at':
          cmp = new Date(a.updated_at).getTime() - new Date(b.updated_at).getTime();
          break;
        case 'created_at':
          cmp = new Date(a.created_at).getTime() - new Date(b.created_at).getTime();
          break;
        case 'title':
          cmp = a.title.localeCompare(b.title);
          break;
      }
      return this.sortDir === 'desc' ? -cmp : cmp;
    });

    return result;
  }

  get pendingCount(): number {
    return this.tasks.filter((t) => t.status === 'pending').length;
  }

  get inProgressCount(): number {
    return this.tasks.filter((t) => t.status === 'in_progress').length;
  }

  get blockedCount(): number {
    return this.tasks.filter((t) => t.status === 'blocked').length;
  }

  get dispatchedTasks(): Task[] {
    return this.tasks.filter((t) => t.tags?.includes('dispatched'));
  }

  get dispatchedInFlightCount(): number {
    return this.dispatchedTasks.filter((t) => t.status === 'pending' || t.status === 'in_progress').length;
  }

  get dispatchedCompletionRate(): number {
    const dispatched = this.dispatchedTasks;
    if (dispatched.length === 0) return 0;
    const completed = dispatched.filter((t) => t.status === 'completed').length;
    return Math.round((completed / dispatched.length) * 100);
  }

  async fetch(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const res = await globalThis.fetch('/api/tasks');
      if (!res.ok) throw new Error(`Tasks API: ${res.status}`);
      const data: TasksResponse = await res.json();
      this.tasks = data.tasks || [];
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  /** Apply task list directly from SSE hud.fleet snapshot, avoiding an HTTP round-trip. */
  applySnapshot(data: Record<string, unknown>): void {
    const tasks = data.tasks as Task[] | undefined;
    if (!tasks) return;
    const hashTask = (t: Task) => `${t.id}|${t.status}|${t.priority}|${t.updated_at}`;
    if (!arraysEqualById(this.tasks, tasks, hashTask)) {
      this.tasks = tasks;
    }
    this.lastUpdated = new Date();
    this.error = null;
  }

  async updateStatus(taskId: string, status: string): Promise<boolean> {
    const auditId = actionStore.start(`Update task status → ${status}`, 'TasksPanel:status');
    try {
      const res = await globalThis.fetch(`/api/tasks/${taskId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ status }),
      });
      if (!res.ok) throw new Error(`Update task: ${res.status}`);
      await this.fetch();
      actionStore.succeed(auditId);
      return true;
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  async setPriority(taskId: string, priority: string): Promise<boolean> {
    const auditId = actionStore.start(`Set task priority → ${priority}`, 'TasksPanel:priority');
    try {
      const res = await globalThis.fetch(`/api/tasks/${taskId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ priority }),
      });
      if (!res.ok) throw new Error(`Set priority: ${res.status}`);
      await this.fetch();
      actionStore.succeed(auditId);
      return true;
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  async createTask(params: {
    title: string;
    priority: string;
    sessionId?: string;
    tags?: string[];
    context?: string;
    filePath?: string;
    lineNumber?: number;
    blockedBy?: string[];
  }): Promise<boolean> {
    const auditId = actionStore.start('Create task', 'TasksPanel:create');
    try {
      const body: Record<string, unknown> = {
        title: params.title,
        priority: params.priority,
      };
      if (params.sessionId) body.session_id = params.sessionId;
      if (params.tags?.length) body.tags = params.tags;
      if (params.context) body.context = params.context;
      if (params.filePath) body.file_path = params.filePath;
      if (params.lineNumber) body.line_number = params.lineNumber;
      if (params.blockedBy?.length) body.blocked_by = params.blockedBy;
      const res = await globalThis.fetch('/api/tasks', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`Create task: ${res.status}`);
      await this.fetch();
      actionStore.succeed(auditId);
      return true;
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  async resolve(taskId: string, resolution: string): Promise<boolean> {
    const auditId = actionStore.start('Resolve task', 'TasksPanel:resolve');
    try {
      const res = await globalThis.fetch(`/api/tasks/${taskId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ status: 'completed', resolution }),
      });
      if (!res.ok) throw new Error(`Resolve task: ${res.status}`);
      await this.fetch();
      actionStore.succeed(auditId);
      return true;
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  startPolling(intervalMs = 60000): void {
    this.stopPolling();
    this.fetch();
    this.poller.start(intervalMs);

    // Subscribe to SSE events: apply task list directly from hud.fleet snapshots.
    // The FleetMonitor fetches all tasks on its 15s cadence and broadcasts them.
    this.eventUnsubs.push(
      eventStore.on('hud.fleet', (e) => this.applySnapshot(e.data)),
      // Granular task creation event — trigger full refresh.
      eventStore.on('hud.task.create', () => this.fetch()),
      // Granular agent.task.update — apply single-task status change immediately.
      eventStore.on('agent.task.update', (e) => {
        const data = e.data as Record<string, unknown>;
        const taskId = data.task_id as string;
        const status = data.status as string;
        if (taskId && status) {
          this.tasks = this.tasks.map((t) =>
            t.id === taskId ? { ...t, status: status as Task['status'], updated_at: new Date().toISOString() } : t,
          );
          this.lastUpdated = new Date();
        }
      }),
    );
  }

  stopPolling(): void {
    this.poller.stop();
    for (const unsub of this.eventUnsubs) unsub();
    this.eventUnsubs = [];
  }
}

export const taskStore = new TaskStore();
stalenessStore.register('tasks', () => taskStore.isStale);
