<script lang="ts">
  // BulkToolbar — sticky bottom toolbar for bulk actions on selected rows.
  //
  // A bulk pass is N sequential mutations, so it needs both guards: `confirm`
  // gates the destructive ones through the shared ConfirmDialog, and `busy`
  // locks every button for the duration so a second click cannot start an
  // overlapping pass over a selection the first one is still draining.
  import ConfirmDialog from './ConfirmDialog.svelte';
  import type { BulkAction } from '../../utils/confirm.ts';

  let {
    count = 0,
    actions = [],
    busy = false,
    onClearSelection,
  }: {
    count?: number;
    actions?: BulkAction[];
    busy?: boolean;
    onClearSelection: () => void;
  } = $props();

  let visible = $derived(count > 0);

  let pendingAction = $state<BulkAction | null>(null);

  function invoke(action: BulkAction): void {
    if (action.confirm) pendingAction = action;
    else action.onclick();
  }
</script>

<div class="bulk-toolbar" class:visible aria-live="polite">
  <div class="bulk-left">
    <span class="bulk-count">{count} selected</span>
    <button class="btn btn-ghost btn-sm" onclick={onClearSelection}>Clear</button>
  </div>
  <div class="bulk-right">
    {#each actions as action}
      <button
        class="btn btn-sm {action.variant ? `btn-${action.variant}` : 'btn-ghost'}"
        disabled={busy}
        onclick={() => invoke(action)}
      >{action.label}</button>
    {/each}
  </div>
</div>

<ConfirmDialog
  open={pendingAction !== null}
  title={pendingAction?.confirm?.title ?? ''}
  message={pendingAction?.confirm?.message ?? ''}
  confirmLabel={pendingAction?.confirm?.confirmLabel ?? 'Confirm'}
  variant={pendingAction?.confirm?.variant
    ?? (pendingAction?.variant === 'danger' ? 'danger' : 'default')}
  onConfirm={() => {
    const action = pendingAction;
    pendingAction = null;
    action?.onclick();
  }}
  onCancel={() => (pendingAction = null)}
/>

<style>
  .bulk-toolbar {
    position: sticky;
    bottom: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: var(--space-2) var(--space-3);
    background: var(--bg-secondary);
    border-top: 1px solid var(--border);
    backdrop-filter: blur(8px);
    box-shadow: var(--elevation-2);
    z-index: 10;
    transform: translateY(100%);
    opacity: 0;
    transition: transform 0.2s ease, opacity 0.2s ease;
    pointer-events: none;
  }

  .bulk-toolbar.visible {
    transform: translateY(0);
    opacity: 1;
    pointer-events: auto;
  }

  .bulk-left {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .bulk-count {
    font-size: var(--text-sm);
    font-weight: 600;
    font-family: var(--font-mono);
    color: var(--fg-primary);
  }

  .bulk-right {
    display: flex;
    align-items: center;
    gap: var(--space-1);
  }
</style>
