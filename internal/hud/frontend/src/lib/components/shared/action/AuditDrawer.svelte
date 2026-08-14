<script lang="ts">
  // AuditDrawer — in-session review of every mutating action the HUD ran,
  // backed by actionStore. The drawer chrome (scrim, panel, focus trap,
  // Escape, ✕) belongs to the shared DetailDrawer; this file owns only the
  // entry list. Escape / backdrop / ✕ all funnel through the store's
  // closeDrawer().
  import { actionStore, type ActionEntry } from '../../../stores/action.svelte.ts';
  import DetailDrawer from '../DetailDrawer.svelte';

  function formatRelative(t: number): string {
    const diff = Math.floor((Date.now() - t) / 1000);
    if (diff < 5) return 'just now';
    if (diff < 60) return `${diff}s ago`;
    if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
    return `${Math.floor(diff / 3600)}h ago`;
  }

  function formatDuration(entry: ActionEntry): string {
    if (entry.endedAt == null) return '…';
    const ms = entry.endedAt - entry.startedAt;
    if (ms < 1000) return `${ms}ms`;
    return `${(ms / 1000).toFixed(1)}s`;
  }

  function statusLabel(status: ActionEntry['status']): string {
    switch (status) {
      case 'pending':     return 'PENDING';
      case 'success':     return 'OK';
      case 'error':       return 'ERROR';
      case 'rolled_back': return 'ROLLBACK';
    }
  }

  let entries = $derived(actionStore.entries);
  let open = $derived(actionStore.drawerOpen);
</script>

<DetailDrawer
  {open}
  title="Recent Actions"
  contentPadding="0"
  closeLabel="Close audit drawer"
  onClose={() => actionStore.closeDrawer()}
>
  {#snippet headerActions()}
    <button type="button" class="audit-btn" onclick={() => actionStore.clear()} disabled={entries.length === 0}>
      Clear
    </button>
  {/snippet}

  {#if entries.length === 0}
    <div class="audit-empty">
      <div class="audit-empty-icon">▢</div>
      <div class="audit-empty-text">No actions yet this session.</div>
    </div>
  {:else}
    <ul class="audit-list">
      {#each entries as entry (entry.id)}
        <li class="audit-entry" data-status={entry.status}>
          <div class="audit-entry-head">
            <span class="audit-status" data-status={entry.status}>{statusLabel(entry.status)}</span>
            <span class="audit-label">{entry.label}</span>
            {#if entry.status === 'error' && entry.retryable && actionStore.hasRetry(entry.id)}
              <button
                type="button"
                class="audit-entry-retry"
                onclick={() => actionStore.retry(entry.id)}
                aria-label="Retry {entry.label}"
                title="Retry"
              >↻</button>
            {/if}
            <button
              type="button"
              class="audit-entry-dismiss"
              onclick={() => actionStore.remove(entry.id)}
              aria-label="Remove entry"
            >✕</button>
          </div>
          <div class="audit-entry-meta">
            <span class="audit-source">{entry.source}</span>
            <span class="audit-divider">·</span>
            <span>{formatRelative(entry.startedAt)}</span>
            <span class="audit-divider">·</span>
            <span>{formatDuration(entry)}</span>
          </div>
          {#if entry.error}
            <div class="audit-error">{entry.error}</div>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
</DetailDrawer>

<style>
  .audit-btn {
    padding: 4px 10px;
    border: 1px solid var(--border-subtle);
    background: transparent;
    color: var(--fg-secondary);
    border-radius: var(--radius-xs);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    cursor: pointer;
    transition: color var(--transition-fast), border-color var(--transition-fast), background var(--transition-fast);
  }

  .audit-btn:hover:not(:disabled) {
    color: var(--fg-primary);
    border-color: var(--border-active);
    background: var(--bg-tertiary);
  }

  .audit-btn:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  /* The body opts out of the drawer gutter (contentPadding="0") so the
     entry separators run edge to edge; the empty state fills the full
     body height to keep its centering. */
  .audit-empty {
    height: 100%;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: var(--space-2);
    color: var(--fg-muted);
  }

  .audit-empty-icon {
    font-size: 32px;
    opacity: 0.5;
  }

  .audit-empty-text {
    font-size: var(--text-sm);
  }

  .audit-list {
    padding: var(--space-2) 0;
    margin: 0;
    list-style: none;
  }

  .audit-entry {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: var(--space-2) var(--space-4);
    border-bottom: 1px solid var(--border-subtle);
  }

  .audit-entry:last-child {
    border-bottom: none;
  }

  .audit-entry-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .audit-status {
    flex-shrink: 0;
    padding: 1px 6px;
    border-radius: var(--radius-xs);
    font-size: 9px;
    font-family: var(--font-mono);
    font-weight: 700;
    letter-spacing: 0.08em;
    background: var(--bg-tertiary);
    color: var(--fg-muted);
  }

  .audit-status[data-status='pending'] {
    background: var(--info-dim);
    color: var(--info);
  }

  .audit-status[data-status='success'] {
    background: var(--success-dim);
    color: var(--success);
  }

  .audit-status[data-status='error'] {
    background: var(--error-dim);
    color: var(--error);
  }

  .audit-status[data-status='rolled_back'] {
    background: var(--warning-dim);
    color: var(--warning);
  }

  .audit-label {
    flex: 1;
    min-width: 0;
    font-size: var(--text-sm);
    color: var(--fg-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .audit-entry-dismiss {
    flex-shrink: 0;
    font-size: 10px;
    color: var(--fg-muted);
    background: transparent;
    border: none;
    padding: 2px 4px;
    border-radius: var(--radius-xs);
    cursor: pointer;
  }

  .audit-entry-dismiss:hover {
    color: var(--fg-primary);
    background: var(--bg-tertiary);
  }

  .audit-entry-retry {
    flex-shrink: 0;
    font-size: 12px;
    line-height: 1;
    color: var(--info);
    background: var(--info-dim);
    border: 1px solid transparent;
    padding: 2px 6px;
    border-radius: var(--radius-xs);
    cursor: pointer;
    font-family: var(--font-mono);
    transition: color var(--transition-fast), border-color var(--transition-fast);
  }

  .audit-entry-retry:hover {
    color: var(--fg-primary);
    border-color: var(--info);
  }

  .audit-entry-meta {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: var(--text-xs);
    color: var(--fg-muted);
    font-family: var(--font-mono);
  }

  .audit-source {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 240px;
  }

  .audit-divider {
    opacity: 0.5;
  }

  .audit-error {
    margin-top: 2px;
    padding: 4px 8px;
    background: var(--error-dim);
    border-left: 2px solid var(--error);
    border-radius: var(--radius-xs);
    color: var(--fg-primary);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    line-height: 1.4;
    word-break: break-word;
  }
</style>
