<script lang="ts">
  import type { BadgeVariant } from '../../utils/tokens.ts';
  import type { HandoffRecord } from '../../clients/presenceActions.ts';
  import { formatTime } from '../../utils/format.ts';
  import Badge from '../../widgets/Badge.svelte';
  import DataTable from '../shared/DataTable.svelte';
  import EmptyState from '../shared/EmptyState.svelte';

  let {
    handoffs = [],
    handoffLoading = false,
    handoffError = '',
    onOpenHandoffModal = () => {},
    onAcceptHandoff = () => {},
  }: {
    handoffs?: HandoffRecord[];
    handoffLoading?: boolean;
    handoffError?: string;
    onOpenHandoffModal?: () => void;
    onAcceptHandoff?: (id: string, targetAgentID: string) => void;
  } = $props();

  const handoffColumns = [
    { key: 'from_agent', label: 'From', width: '100px' },
    { key: 'to_agent', label: 'To', width: '100px' },
    { key: 'summary', label: 'Summary' },
    { key: 'status', label: 'Status', width: '90px' },
    { key: 'created_at', label: 'Created', width: '90px', hideBelow: 720 },
    { key: 'actions', label: 'Actions', width: '80px' },
  ];

  function handoffStatusVariant(status: string): BadgeVariant {
    const map: Record<string, BadgeVariant> = {
      pending: 'warning',
      accepted: 'success',
      expired: 'error',
    };
    return map[status] ?? 'info';
  }
</script>

<div class="card">
  <div class="card-header">
    <span class="card-title">Agent Handoffs</span>
    <span class="count-badge">{handoffs.length}</span>
    <div class="card-actions">
      <button class="btn btn-sm" onclick={onOpenHandoffModal}>+ Handoff</button>
    </div>
  </div>

  {#if handoffLoading}
    <div class="loading-bar"><div class="loading-bar-inner"></div></div>
  {/if}

  {#if handoffError}
    <div class="text-xs text-muted" style="padding: 4px 12px;">Failed to load handoffs</div>
  {/if}

  {#if handoffs.length === 0 && !handoffLoading}
    <EmptyState icon={'\u{1F91D}'} heading="No handoffs" compact />
  {:else if handoffs.length > 0}
    <DataTable
      columns={handoffColumns}
      rows={handoffs}
      stableLayout={true}
    >
      {#snippet row({ row: handoff, hiddenColumns })}
        <td class="text-mono">{handoff.from_agent || '---'}</td>
        <td class="text-mono">{handoff.target_agent_id || handoff.to_agent || '---'}</td>
        <td class="truncate" title={handoff.summary}>{handoff.summary}</td>
        <td><Badge text={handoff.status} variant={handoffStatusVariant(handoff.status)} /></td>
        {#if !hiddenColumns.has('created_at')}
        <td class="text-mono text-muted">{formatTime(handoff.created_at)}</td>
        {/if}
        <td>
          {#if handoff.status === 'pending'}
            <button class="btn btn-xs btn-success" onclick={() => onAcceptHandoff(handoff.id, handoff.target_agent_id || handoff.to_agent || '')}>
              Accept
            </button>
          {:else}
            <span class="text-muted text-xs">{handoff.accepted_at ? formatTime(handoff.accepted_at) : '---'}</span>
          {/if}
        </td>
      {/snippet}
    </DataTable>
  {/if}

</div>

<style>
  .count-badge {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    background: var(--bg-tertiary);
    color: var(--fg-secondary);
    padding: 1px 6px;
    border-radius: var(--radius-lg);
  }

  .card-actions {
    margin-left: auto;
  }

  .loading-bar {
    height: 2px;
    background: var(--bg-tertiary);
    border-radius: 1px;
    overflow: hidden;
    margin-bottom: 4px;
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
