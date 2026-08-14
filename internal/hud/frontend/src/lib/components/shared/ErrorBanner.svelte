<script lang="ts">
  /**
   * ErrorBanner — inline, non-blocking refresh-failure banner for panels
   * whose (possibly stale) content should stay visible under the error.
   * For full-surface failures use PanelShell's error state or EmptyState;
   * this banner is for "the poll failed, data below may be stale".
   */
  let { message, prefix = '' }: { message: string; prefix?: string } = $props();
</script>

<div class="error-banner" role="alert" aria-live="polite">
  <span class="error-banner-icon" aria-hidden="true">⚠</span>
  <span class="error-banner-text">{#if prefix}{prefix}: {/if}{message}</span>
</div>

<style>
  .error-banner {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    margin-bottom: var(--space-2);
    font-size: var(--text-xs);
    color: var(--error);
    background: rgba(var(--error-rgb), var(--opacity-subtle));
    border: 1px solid rgba(var(--error-rgb), 0.25);
    border-radius: var(--radius-md);
    flex-shrink: 0;
  }

  .error-banner-icon {
    flex-shrink: 0;
  }

  .error-banner-text {
    min-width: 0;
    word-break: break-word;
  }
</style>
