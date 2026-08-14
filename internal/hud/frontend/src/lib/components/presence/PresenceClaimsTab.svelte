<script lang="ts">
  import type { BadgeVariant } from '../../utils/tokens.ts';
  import type { BulkAction } from '../../utils/confirm.ts';
  import type { FileClaimInfo } from '../../stores/fleet.svelte.ts';
  import { releaseConfirmCopy, shortClaimPath, type PendingRelease } from './releaseCopy.ts';
  import { toastStore } from '../../stores/toasts.svelte.ts';
  import { presenceActionsStore } from '../../stores/presenceActions.svelte.ts';
  import { formatTime } from '../../utils/format.ts';
  import Badge from '../../widgets/Badge.svelte';
  import DataTable from '../shared/DataTable.svelte';
  import BulkToolbar from '../shared/BulkToolbar.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import EmptyState from '../shared/EmptyState.svelte';

  interface FileConflict {
    path: string;
    agents: string[];
  }

  let {
    claims = [],
    fileConflicts = [],
    onReleaseClaim = async () => true,
  }: {
    claims?: FileClaimInfo[];
    fileConflicts?: FileConflict[];
    onReleaseClaim?: (
      agentId: string,
      filePath: string,
      opts?: { silent?: boolean },
    ) => Promise<boolean>;
  } = $props();

  let selectedClaimIds = $state<Set<string>>(new Set());

  // Every release on this tab is a force-release — it hands a file another agent
  // is holding to whoever grabs it next — so all three routes share one gate.
  // `single` carries its claim so the dialog can name the path and agent.
  let pendingRelease = $state<PendingRelease | null>(null);
  let bulkBusy = $state(false);

  const claimColumns = [
    { key: 'file_path', label: 'File' },
    { key: 'agent_id', label: 'Agent', width: '120px' },
    { key: 'claim_type', label: 'Type', width: '80px', hideBelow: 620 },
    { key: 'reason', label: 'Reason', width: '220px', hideBelow: 860 },
    { key: 'created_at', label: 'Since', width: '90px', hideBelow: 700 },
    { key: 'actions', label: 'Actions', width: '80px' },
  ];

  function claimVariant(type: string): BadgeVariant {
    const map: Record<string, BadgeVariant> = {
      edit: 'warning',
      review: 'info',
      reserve: 'accent',
    };
    return map[type] ?? 'info';
  }

  function handleClaimSelect(ids: Set<string>) {
    selectedClaimIds = ids;
  }

  // A release that the daemon rejected must not read as a success. Per-item
  // toasts are suppressed (opts.silent) so one pass reports once.
  function reportBulk(total: number, failures: number) {
    const ok = total - failures;
    if (failures === 0) toastStore.success(`${total} claims released`);
    else if (ok === 0) toastStore.error(`Failed to release ${total} claims`);
    else toastStore.warning(`Released ${ok} of ${total} claims (${failures} failed)`);
  }

  async function bulkReleaseClaims() {
    const targets = [...selectedClaimIds]
      .map((id) => claims.find((c) => c.id === id))
      .filter((claim): claim is FileClaimInfo => !!claim);
    let failures = 0;
    for (const claim of targets) {
      if (!(await onReleaseClaim(claim.agent_id, claim.file_path, { silent: true }))) failures++;
    }
    reportBulk(targets.length, failures);
    selectedClaimIds = new Set();
  }

  function nudgeAgent(agentId: string) {
    presenceActionsStore.onOpenNudge(agentId);
  }

  async function bulkReleaseConflicts() {
    let total = 0;
    let failures = 0;
    for (const conflict of fileConflicts) {
      for (const agentId of conflict.agents) {
        total++;
        if (!(await onReleaseClaim(agentId, conflict.path, { silent: true }))) failures++;
      }
    }
    reportBulk(total, failures);
  }

  async function runPendingRelease() {
    const pending = pendingRelease;
    pendingRelease = null;
    if (!pending) return;
    bulkBusy = true;
    try {
      if (pending.kind === 'selected') await bulkReleaseClaims();
      else if (pending.kind === 'conflicts') await bulkReleaseConflicts();
      // A single release toasts its own outcome, so it is not routed through
      // reportBulk (no `silent` opt-out).
      else await onReleaseClaim(pending.claim.agent_id, pending.claim.file_path);
    } finally {
      bulkBusy = false;
    }
  }

  let conflictPathList = $derived(fileConflicts.map((c) => shortClaimPath(c.path)).join(', '));

  let releaseCopy = $derived(
    releaseConfirmCopy(pendingRelease, {
      selectedCount: selectedClaimIds.size,
      conflictPathList,
    }),
  );

  let claimBulkActions = $derived<BulkAction[]>([
    { label: 'Release Selected', variant: 'danger', onclick: () => { if (!bulkBusy) pendingRelease = { kind: 'selected' }; } },
    ...(fileConflicts.length > 0 ? [{ label: 'Release All Conflicts', variant: 'danger', onclick: () => { if (!bulkBusy) pendingRelease = { kind: 'conflicts' }; } }] : []),
  ]);

  let typeCounts = $derived.by(() => {
    const counts: Record<string, number> = { edit: 0, review: 0, reserve: 0 };
    for (const c of claims) {
      const t = c.claim_type ?? 'edit';
      if (t in counts) counts[t]++;
      else counts[t] = (counts[t] ?? 0) + 1;
    }
    return counts;
  });
</script>

<div class="card">
  <div class="card-header">
    <span class="card-title">File Claims</span>
    <span class="count-badge">{claims.length}</span>
    {#if claims.length > 0}
      <div class="type-breakdown">
        {#if typeCounts.edit > 0}<Badge text="edit {typeCounts.edit}" variant="warning" />{/if}
        {#if typeCounts.review > 0}<Badge text="review {typeCounts.review}" variant="info" />{/if}
        {#if typeCounts.reserve > 0}<Badge text="reserve {typeCounts.reserve}" variant="accent" />{/if}
      </div>
    {/if}
  </div>

  {#if fileConflicts.length > 0}
    <div class="conflict-banner">
      <span class="conflict-icon">⚠</span>
      <span>{fileConflicts.length} file(s) claimed by multiple agents:</span>
      {#each fileConflicts as conflict}
        <div class="conflict-detail">
          <span class="text-mono text-xs">{shortClaimPath(conflict.path)}</span>
          <span class="text-muted text-xs">→ {conflict.agents.join(', ')}</span>
          {#each conflict.agents as agentId}
            <button class="btn btn-xs btn-nudge-inline" onclick={() => nudgeAgent(agentId)} title="Nudge {agentId}">
              Nudge
            </button>
          {/each}
        </div>
      {/each}
    </div>
  {/if}

  {#if claims.length === 0}
    <EmptyState icon={'\u{1F4C1}'} heading="No active file claims" compact />
  {:else}
    <DataTable
      columns={claimColumns}
      rows={claims}
      selectable={true}
      stableLayout={true}
      selectedIds={selectedClaimIds}
      onSelect={handleClaimSelect}
    >
      {#snippet row({ row: claim, hiddenColumns })}
        <td class="text-mono" title={claim.file_path}>{shortClaimPath(claim.file_path)}</td>
        <td class="text-mono">{claim.agent_id}</td>
        {#if !hiddenColumns.has('claim_type')}
        <td><Badge text={claim.claim_type} variant={claimVariant(claim.claim_type)} /></td>
        {/if}
        {#if !hiddenColumns.has('reason')}
        <td class="truncate text-muted" title={claim.reason}>{claim.reason || '---'}</td>
        {/if}
        {#if !hiddenColumns.has('created_at')}
        <td class="text-mono text-muted">{formatTime(claim.created_at)}</td>
        {/if}
        <td>
          <button
            class="btn btn-xs btn-danger"
            disabled={bulkBusy}
            onclick={() => { if (!bulkBusy) pendingRelease = { kind: 'single', claim }; }}
            title="Force-release this claim"
          >
            Release
          </button>
        </td>
      {/snippet}
    </DataTable>
    <BulkToolbar
      count={selectedClaimIds.size}
      actions={claimBulkActions}
      busy={bulkBusy}
      onClearSelection={() => { selectedClaimIds = new Set(); }}
    />
  {/if}
</div>

<ConfirmDialog
  open={pendingRelease !== null}
  title={releaseCopy.title}
  message={releaseCopy.message}
  confirmLabel="Release"
  variant="danger"
  onConfirm={runPendingRelease}
  onCancel={() => (pendingRelease = null)}
/>

<style>
  .type-breakdown {
    display: flex;
    gap: 4px;
    margin-left: auto;
  }

  .count-badge {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    background: var(--bg-tertiary);
    color: var(--fg-secondary);
    padding: 1px 6px;
    border-radius: var(--radius-lg);
  }

  .conflict-banner {
    background: rgba(var(--warning-rgb), var(--opacity-subtle));
    border: 1px solid rgba(var(--warning-rgb), var(--opacity-strong));
    border-radius: var(--radius-md);
    padding: 10px 14px;
    margin: 8px 0;
    font-size: var(--text-12);
    color: var(--warning);
  }

  .conflict-icon {
    font-size: var(--text-14);
    margin-right: 4px;
  }

  .conflict-detail {
    display: flex;
    gap: 8px;
    align-items: center;
    padding: 3px 0 3px 20px;
  }

  /* Sizing comes from the global .btn/.btn-xs pair; this rule only supplies
     the outline fill. */
  .btn-nudge-inline {
    border: 1px solid var(--accent);
    background: transparent;
    color: var(--accent);
    margin-left: 4px;
  }

  .btn-nudge-inline:hover {
    background: var(--accent);
    color: var(--bg-primary);
  }
</style>
