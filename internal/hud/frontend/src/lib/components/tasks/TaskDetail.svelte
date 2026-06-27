<script lang="ts">
  /**
   * TaskDetail — drill-down drawer for a single task with footer Resolve
   * action.
   *
   * @type {{
   *   task: any | null,
   *   onClose: () => void,
   *   onResolve: (task: any) => void,
   *   onOpenPlan?: (planId: string) => void,
   *   onSelectTask?: (taskId: string) => void,
   *   lookupTask?: (taskId: string) => { id: string, title: string, status?: string } | undefined,
   * }}
   */
  let { task, onClose, onResolve, onOpenPlan, onSelectTask, lookupTask } = $props();

  import { coordinationStore } from '../../stores/coordination.svelte.ts';
  import { relativeTime, statusVariant, priorityVariant } from '../../utils/format.ts';
  import { taskAgentName } from '../../utils/tasksHelpers';
  import Badge from '../../widgets/Badge.svelte';
  import DetailDrawer from '../shared/DetailDrawer.svelte';

  let coordinationBlockers = $derived(coordinationStore.blockers ?? []);
  let taskRelations = $derived.by(() => {
    if (!task?.id) return [];
    return coordinationBlockers.filter((blocker) => blocker.task_id === task.id);
  });
</script>

<DetailDrawer
  open={!!task}
  title={task?.title ?? ''}
  subtitle={task ? taskAgentName(task) : 'Unassigned'}
  {onClose}
>
  {#snippet header()}
    {#if task}
      <div class="detail-stats">
        <div class="stat-chip">
          <Badge text={task.priority ?? 'medium'} variant={priorityVariant(task.priority)} />
        </div>
        <div class="stat-chip">
          <Badge text={task.status ?? 'pending'} variant={statusVariant(task.status)} />
        </div>
        {#if task.created_at}
          <div class="stat-chip">
            <span class="stat-chip-value">{relativeTime(task.created_at)}</span>
            <span class="stat-chip-label">created</span>
          </div>
        {/if}
      </div>
    {/if}
  {/snippet}

  {#if task}
    {#if task.plan_id}
      <div class="section">
        <div class="section-title text-xs uppercase text-muted">Plan</div>
        <div class="plan-links">
          <button
            type="button"
            class="plan-link"
            title="Open plan {task.plan_id}"
            onclick={() => onOpenPlan?.(task.plan_id)}
          >
            ◈ {task.plan_id}
          </button>
          {#if task.slice_id}
            <span class="slice-chip" title="Plan slice">slice {task.slice_id}</span>
          {/if}
        </div>
      </div>
    {/if}
    {#if task.context}
      <div class="section">
        <div class="section-title text-xs uppercase text-muted">Context</div>
        <pre class="detail-pre">{task.context}</pre>
      </div>
    {/if}
    {#if task.file_path}
      <div class="section">
        <div class="section-title text-xs uppercase text-muted">File</div>
        <span class="text-mono text-sm">{task.file_path}</span>
      </div>
    {/if}
    {#if task.tags?.length}
      <div class="section">
        <div class="section-title text-xs uppercase text-muted">Tags</div>
        <div class="tag-chips">
          {#each task.tags as tag}
            <span class="tag-chip">{tag}</span>
          {/each}
        </div>
      </div>
    {/if}
    {#if task.blocked_by?.length}
      <div class="section">
        <div class="section-title text-xs uppercase text-muted">Blocked By</div>
        <div class="dep-list">
          {#each task.blocked_by as depId}
            {@const dep = lookupTask?.(depId)}
            {#if dep && onSelectTask}
              <button
                type="button"
                class="blocked-id blocked-link"
                class:resolved={task.resolved_deps?.includes(depId)}
                title="Open blocking task: {dep.title}"
                onclick={() => onSelectTask(depId)}
              >
                {dep.title}
              </button>
            {:else}
              <span class="blocked-id" class:resolved={task.resolved_deps?.includes(depId)}>
                {depId.slice(0, 12)}
              </span>
            {/if}
          {/each}
        </div>
      </div>
    {/if}
    {#if taskRelations.length > 0}
      <div class="section">
        <div class="section-title text-xs uppercase text-muted">Dependency Relations</div>
        <div class="relation-cards">
          {#each taskRelations as relation}
            <div class="relation-card" class:relation-card-cross={relation.cross_agent}>
              <div class="relation-card-title">{relation.blocked_by_task_title || relation.blocked_by_task_id}</div>
              <div class="relation-card-meta">
                {relation.blocked_by_status || 'unknown'} · {relation.cross_agent ? 'cross-agent' : 'local'}
                {#if relation.resolved} · resolved{/if}
              </div>
              <div class="relation-card-detail">
                {relation.blocked_by_agent_id || 'unassigned'} · {relation.blocked_by_namespace || 'unscoped'}
              </div>
            </div>
          {/each}
        </div>
      </div>
    {/if}
    {#if task.resolution}
      <div class="section">
        <div class="section-title text-xs uppercase text-muted">Resolution</div>
        <pre class="detail-pre">{task.resolution}</pre>
      </div>
    {/if}
  {/if}

  {#snippet footer()}
    {#if task && task.status !== 'completed' && task.status !== 'cancelled'}
      <button class="btn btn-success" onclick={() => { onClose(); onResolve(task); }}>
        Resolve Task
      </button>
    {/if}
  {/snippet}
</DetailDrawer>

<style>
  .tag-chips { display: flex; gap: var(--space-1); flex-wrap: wrap; }
  .tag-chip {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    padding: 2px 6px;
    background: var(--bg-tertiary);
    border-radius: var(--radius-sm);
    color: var(--fg-secondary);
    border: 1px solid var(--border-subtle);
  }
  .plan-links { display: flex; gap: var(--space-2); flex-wrap: wrap; align-items: center; }
  .plan-link {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    padding: 2px 8px;
    background: color-mix(in srgb, var(--accent, #2af) 12%, var(--bg-tertiary));
    border: 1px solid color-mix(in srgb, var(--accent, #2af) 35%, var(--border));
    border-radius: var(--radius-sm);
    color: var(--accent, #2af);
    cursor: pointer;
  }
  .plan-link:hover { background: color-mix(in srgb, var(--accent, #2af) 22%, var(--bg-tertiary)); }
  .slice-chip {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    padding: 2px 6px;
    background: var(--bg-tertiary);
    border-radius: var(--radius-sm);
    color: var(--fg-secondary);
    border: 1px solid var(--border-subtle);
  }
  .dep-list { display: flex; gap: var(--space-1); flex-wrap: wrap; }
  .blocked-id {
    font-size: var(--text-xs);
    padding: 1px 4px;
    background: var(--bg-tertiary);
    border-radius: var(--radius-sm);
    color: var(--fg-secondary);
    border: 1px solid var(--border-subtle);
  }
  .blocked-link { cursor: pointer; color: var(--fg-primary); }
  .blocked-link:hover { border-color: var(--accent, #2af); color: var(--accent, #2af); }
  .blocked-id.resolved { opacity: 0.4; text-decoration: line-through; }
  .relation-cards { display: flex; flex-direction: column; gap: var(--space-2); }
  .relation-card {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    background: var(--bg-secondary);
    display: flex;
    flex-direction: column;
    gap: 4px;
    position: relative;
  }
  .relation-card::before {
    content: '';
    position: absolute;
    inset: 0;
    border-radius: inherit;
    background: var(--surface-highlight);
    pointer-events: none;
  }
  .relation-card-cross {
    border-color: color-mix(in srgb, var(--warning) 40%, var(--border));
    box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--warning) 15%, transparent);
  }
  .relation-card-title {
    font-size: var(--text-sm);
    color: var(--fg-primary);
    font-weight: 600;
  }
  .relation-card-meta,
  .relation-card-detail {
    font-size: var(--text-xs);
    color: var(--fg-dim);
    font-family: var(--font-mono);
    letter-spacing: var(--tracking-normal);
  }
</style>
