<script lang="ts">
  import type { BadgeVariant } from '../../utils/tokens.ts';
  import type { WorktreeAssignment } from '../../stores/presence.svelte.ts';
  import { formatTime } from '../../utils/format.ts';
  import Badge from '../../widgets/Badge.svelte';
  import DataTable from '../shared/DataTable.svelte';
  import EmptyState from '../shared/EmptyState.svelte';

  let { worktrees = [] }: { worktrees?: WorktreeAssignment[] } = $props();

  const worktreeColumns = [
    { key: 'branch', label: 'Branch', width: '180px' },
    { key: 'agent_id', label: 'Agent', width: '120px' },
    { key: 'status', label: 'Status', width: '90px' },
    { key: 'git_status', label: 'Git', width: '100px', hideBelow: 740 },
    { key: 'purpose', label: 'Purpose' },
    { key: 'created_at', label: 'Created', width: '90px', hideBelow: 860 },
  ];

  function worktreeVariant(status: string): BadgeVariant {
    const map: Record<string, BadgeVariant> = {
      active: 'success',
      released: 'info',
      orphaned: 'error',
    };
    return map[status] ?? 'info';
  }
</script>

<div class="card">
  <div class="card-header">
    <span class="card-title">Git Worktrees</span>
    <span class="count-badge">{worktrees.length}</span>
  </div>
  {#if worktrees.length === 0}
    <EmptyState icon={'\u{1F333}'} heading="No active worktrees" compact />
  {:else}
    <DataTable
      columns={worktreeColumns}
      rows={worktrees}
      idKey="assignment_id"
      stableLayout={true}
    >
      {#snippet row({ row: wt, hiddenColumns })}
        <td class="text-mono">{wt.branch}</td>
        <td class="text-mono">{wt.agent_id}</td>
        <td><Badge text={wt.status} variant={worktreeVariant(wt.status)} /></td>
        {#if !hiddenColumns.has('git_status')}
        <td class="text-mono text-muted text-xs" title={wt.git_status}>{wt.git_status || 'clean'}</td>
        {/if}
        <td class="truncate text-muted" title={wt.purpose}>{wt.purpose || '---'}</td>
        {#if !hiddenColumns.has('created_at')}
        <td class="text-mono text-muted">{formatTime(wt.created_at)}</td>
        {/if}
      {/snippet}
    </DataTable>
  {/if}
</div>

<style>
  .count-badge {
    font-family: var(--font-mono);
    font-size: 11px;
    background: var(--bg-tertiary);
    color: var(--fg-secondary);
    padding: 1px 6px;
    border-radius: var(--radius-lg);
  }
</style>
