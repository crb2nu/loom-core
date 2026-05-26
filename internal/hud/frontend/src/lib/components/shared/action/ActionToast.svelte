<script lang="ts">
  // Action-derived toast feeder. Subscribes to actionStore entries and pops
  // a rich toast on every pending → terminal transition that happens after
  // this component mounts. Auto-dismiss is paused while the pointer is over
  // a toast; error toasts can be retried inline.
  //
  // Plain string toasts (toastStore) keep flowing through widgets/Toast.svelte
  // for code paths that aren't routed through useAction.

  import { actionStore, type ActionEntry, type ActionStatus } from '../../../stores/action.svelte.ts';

  const TOAST_TTL_SUCCESS_MS = 4000;
  const TOAST_TTL_ERROR_MS = 8000;
  const TOAST_TTL_ROLLBACK_MS = 8000;
  const REAP_INTERVAL_MS = 250;

  type Toast = {
    id: string;
    entry: ActionEntry;
    dismissAt: number;
    pausedRemainingMs: number;
  };

  // Skip entries that already existed at mount: pre-seed each one with its
  // current status so the first $effect tick treats them as no-op.
  const lastSeenStatus = new Map<string, ActionStatus>();
  for (const e of actionStore.entries) lastSeenStatus.set(e.id, e.status);

  let toasts = $state<Toast[]>([]);
  let hoveredId = $state<string | null>(null);

  function ttlFor(status: ActionStatus): number {
    switch (status) {
      case 'success':     return TOAST_TTL_SUCCESS_MS;
      case 'rolled_back': return TOAST_TTL_ROLLBACK_MS;
      case 'error':       return TOAST_TTL_ERROR_MS;
      default:            return TOAST_TTL_SUCCESS_MS;
    }
  }

  function appendToast(entry: ActionEntry): void {
    const dismissAt = Date.now() + ttlFor(entry.status);
    const existing = toasts.find((t) => t.id === entry.id);
    if (existing) {
      // Status transitioned again (e.g. error → rolled_back). Refresh the
      // entry snapshot + dismissAt without re-appending.
      existing.entry = entry;
      existing.dismissAt = dismissAt;
      existing.pausedRemainingMs = 0;
      toasts = [...toasts];
      return;
    }
    toasts = [...toasts, { id: entry.id, entry, dismissAt, pausedRemainingMs: 0 }];
  }

  function isSilent(entry: ActionEntry): boolean {
    if (entry.status === 'success') return entry.silentSuccess;
    if (entry.status === 'error' || entry.status === 'rolled_back') return entry.silentError;
    return false;
  }

  $effect(() => {
    for (const e of actionStore.entries) {
      const prev = lastSeenStatus.get(e.id);
      lastSeenStatus.set(e.id, e.status);
      if (e.status === 'pending') continue;
      if (prev === e.status) continue;
      if (isSilent(e)) continue;
      // Only toast entries we observed in 'pending' first — skips historical
      // sessionStorage replays on mount/reload. Also allow error → rolled_back
      // refresh so the toast badge updates after a useAction() rollback hook.
      if (prev === 'pending' || prev === 'error') appendToast(e);
    }
  });

  $effect(() => {
    const handle = setInterval(() => {
      const now = Date.now();
      toasts = toasts.filter((t) => t.id === hoveredId || t.dismissAt > now);
    }, REAP_INTERVAL_MS);
    return () => clearInterval(handle);
  });

  function dismiss(id: string): void {
    toasts = toasts.filter((t) => t.id !== id);
    if (hoveredId === id) hoveredId = null;
  }

  function onEnter(id: string): void {
    const t = toasts.find((x) => x.id === id);
    if (!t) return;
    t.pausedRemainingMs = Math.max(0, t.dismissAt - Date.now());
    hoveredId = id;
  }

  function onLeave(id: string): void {
    const t = toasts.find((x) => x.id === id);
    if (t && t.pausedRemainingMs > 0) {
      t.dismissAt = Date.now() + t.pausedRemainingMs;
      t.pausedRemainingMs = 0;
      toasts = [...toasts];
    }
    if (hoveredId === id) hoveredId = null;
  }

  function retryAction(entry: ActionEntry): void {
    const promise = actionStore.retry(entry.id);
    if (promise) {
      // Dismiss the failed toast so the retry can produce its own outcome
      // toast cleanly; if the retry fails again, useAction will surface it.
      dismiss(entry.id);
      void promise;
    }
  }

  function openAudit(id: string): void {
    actionStore.openDrawer();
    dismiss(id);
  }

  function statusGlyph(s: ActionStatus): string {
    switch (s) {
      case 'success':     return '✓';
      case 'error':       return '✕';
      case 'rolled_back': return '↺';
      default:            return '…';
    }
  }

  function statusLabel(s: ActionStatus): string {
    switch (s) {
      case 'success':     return 'OK';
      case 'error':       return 'ERROR';
      case 'rolled_back': return 'ROLLBACK';
      default:            return 'PENDING';
    }
  }
</script>

{#if toasts.length > 0}
  <div class="action-toast-stack" aria-live="polite">
    {#each toasts as toast (toast.id)}
      <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
      <div
        class="action-toast"
        data-status={toast.entry.status}
        role="status"
        onmouseenter={() => onEnter(toast.id)}
        onmouseleave={() => onLeave(toast.id)}
        onclick={() => openAudit(toast.id)}
      >
        <span class="glyph" data-status={toast.entry.status}>{statusGlyph(toast.entry.status)}</span>
        <div class="body">
          <div class="head">
            <span class="status" data-status={toast.entry.status}>{statusLabel(toast.entry.status)}</span>
            <span class="label" title={toast.entry.label}>{toast.entry.label}</span>
          </div>
          {#if toast.entry.error}
            <div class="error" title={toast.entry.error}>{toast.entry.error}</div>
          {:else}
            <div class="source" title={toast.entry.source}>{toast.entry.source}</div>
          {/if}
        </div>
        {#if toast.entry.status === 'error' && toast.entry.retryable && actionStore.hasRetry(toast.entry.id)}
          <button
            type="button"
            class="retry"
            aria-label="Retry {toast.entry.label}"
            onclick={(e) => { e.stopPropagation(); retryAction(toast.entry); }}
          >↻ Retry</button>
        {/if}
        <button
          type="button"
          class="dismiss"
          aria-label="Dismiss"
          onclick={(e) => { e.stopPropagation(); dismiss(toast.id); }}
        >✕</button>
      </div>
    {/each}
  </div>
{/if}

<style>
  .action-toast-stack {
    position: fixed;
    bottom: 16px;
    right: 16px;
    display: flex;
    flex-direction: column;
    gap: 8px;
    z-index: 950;
    pointer-events: none;
    max-width: min(420px, 92vw);
  }

  .action-toast {
    pointer-events: auto;
    display: grid;
    grid-template-columns: 20px 1fr auto auto;
    align-items: center;
    gap: 10px;
    padding: 10px 12px;
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-left: 3px solid var(--border-active);
    border-radius: var(--radius-sm);
    box-shadow: var(--shadow-md);
    cursor: pointer;
    animation: toastSlideIn 0.18s ease-out;
    transition: border-color var(--transition-fast);
  }

  .action-toast:hover {
    border-color: var(--border-active);
  }

  .action-toast[data-status='success']     { border-left-color: var(--success); }
  .action-toast[data-status='error']       { border-left-color: var(--error); }
  .action-toast[data-status='rolled_back'] { border-left-color: var(--warning); }

  .glyph {
    font-size: 14px;
    line-height: 1;
    text-align: center;
    color: var(--fg-muted);
  }
  .glyph[data-status='success']     { color: var(--success); }
  .glyph[data-status='error']       { color: var(--error); }
  .glyph[data-status='rolled_back'] { color: var(--warning); }

  .body {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }

  .head {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }

  .status {
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
  .status[data-status='success']     { background: var(--success-dim); color: var(--success); }
  .status[data-status='error']       { background: var(--error-dim); color: var(--error); }
  .status[data-status='rolled_back'] { background: var(--warning-dim); color: var(--warning); }

  .label {
    flex: 1;
    min-width: 0;
    font-size: var(--text-sm);
    color: var(--fg-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .source {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .error {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .retry {
    padding: 4px 8px;
    border: 1px solid var(--border);
    background: var(--info-dim);
    color: var(--info);
    border-radius: var(--radius-xs);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    cursor: pointer;
    transition: color var(--transition-fast), border-color var(--transition-fast);
  }
  .retry:hover {
    color: var(--fg-primary);
    border-color: var(--info);
  }

  .dismiss {
    font-size: 10px;
    color: var(--fg-muted);
    background: transparent;
    border: none;
    padding: 2px 4px;
    border-radius: var(--radius-xs);
    cursor: pointer;
  }
  .dismiss:hover {
    color: var(--fg-primary);
    background: var(--bg-tertiary);
  }

  @keyframes toastSlideIn {
    from { opacity: 0; transform: translateX(20px); }
    to   { opacity: 1; transform: translateX(0); }
  }

  @media (max-width: 480px) {
    .action-toast-stack {
      left: 8px;
      right: 8px;
      bottom: 8px;
      max-width: none;
    }
  }
</style>
