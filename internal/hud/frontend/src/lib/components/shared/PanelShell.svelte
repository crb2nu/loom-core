<script lang="ts">
  import type { Snippet } from 'svelte';

  /**
   * PanelShell — standard panel wrapper providing consistent header,
   * optional filter bar, scrollable content area, and empty state fallback.
   */
  let {
    title,
    icon = '',
    count = null,
    loading = false,
    error = null,
    empty = false,
    emptyIcon = '\u25A1',
    emptyMessage = 'No data yet',
    emptyHint = '',
    emptyTone = 'idle',
    errorIcon = '\u26A0',
    errorHeading = 'Refresh failed',
    header,
    toolbar,
    actions,
    emptyAction,
    errorAction,
    children,
  }: {
    title: string;
    icon?: string;
    count?: number | null;
    loading?: boolean;
    error?: string | null;
    empty?: boolean;
    emptyIcon?: string;
    emptyMessage?: string;
    emptyHint?: string;
    emptyTone?: 'idle' | 'ready' | 'error' | 'disabled';
    errorIcon?: string;
    errorHeading?: string;
    header?: Snippet;
    toolbar?: Snippet;
    actions?: Snippet;
    emptyAction?: Snippet;
    errorAction?: Snippet;
    children: Snippet;
  } = $props();
</script>

<section class="panel-shell" aria-label={title}>
  <!-- Panel header -->
  <div class="panel-shell-header">
    <div class="panel-shell-title-row">
      {#if icon}
        <span class="panel-shell-icon">{icon}</span>
      {/if}
      <h2 class="panel-shell-title">{title}</h2>
      {#if count != null}
        <span class="panel-shell-count">{count}</span>
      {/if}
    </div>
    {#if actions}
      <div class="panel-shell-actions">
        {@render actions()}
      </div>
    {/if}
  </div>

  <!-- Optional header slot (extra info below title) -->
  {#if header}
    <div class="panel-shell-header-extra">
      {@render header()}
    </div>
  {/if}

  <!-- Optional toolbar/filter bar -->
  {#if toolbar}
    <div class="panel-shell-toolbar">
      {@render toolbar()}
    </div>
  {/if}

  <!-- Loading bar -->
  {#if loading}
    <div class="loading-bar" role="status" aria-live="polite" aria-label="Loading">
      <div class="loading-bar-inner"></div>
    </div>
  {/if}

  <!-- Content area -->
  <div class="panel-shell-content" class:is-empty={(empty || error) && !loading}>
    {#if error && !loading}
      <!-- Error precedes empty: a failed fetch is not the same signal as
           "no rows". Mirrors the .empty-state shape so empty/error feel
           like one family with two states. -->
      <div class="error-state" role="alert" aria-live="polite">
        <div class="error-state-icon">{errorIcon}</div>
        <div class="error-state-message">{errorHeading}</div>
        <div class="error-state-hint">{error}</div>
        {#if errorAction}
          <div class="error-state-action">
            {@render errorAction()}
          </div>
        {/if}
      </div>
    {:else if empty && !loading}
      <div class="empty-state tone-{emptyTone}" role="status">
        <div class="empty-state-icon">{emptyIcon}</div>
        <div class="empty-state-message">{emptyMessage}</div>
        {#if emptyHint}
          <div class="empty-state-hint">{emptyHint}</div>
        {/if}
        {#if emptyAction}
          <div class="empty-state-action">
            {@render emptyAction()}
          </div>
        {/if}
      </div>
    {:else}
      {@render children()}
    {/if}
  </div>
</section>

<style>
  @import '../../styles/tokens.css';

  .panel-shell {
    display: flex;
    flex-direction: column;
    flex: 1;
    overflow: hidden;
    padding: var(--panel-padding);
  }

  .panel-shell-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: var(--space-4);
    flex-shrink: 0;
    gap: var(--space-3);
  }

  .panel-shell-title-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .panel-shell-icon {
    font-size: var(--mills-text-title);
    color: var(--mills-color-text-muted);
  }

  .panel-shell-title {
    font-size: var(--mills-text-heading);
    font-weight: 700;
    color: var(--mills-color-text);
    margin: 0;
    letter-spacing: var(--tracking-tight);
  }

  .panel-shell-count {
    font-size: var(--mills-text-label);
    font-family: var(--font-mono);
    font-weight: 600;
    color: var(--mills-color-text-secondary);
    background: var(--mills-color-surface-raised);
    padding: 2px 8px;
    border-radius: var(--mills-radius-pill);
    border: 1px solid var(--mills-color-border);
  }

  .panel-shell-actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .panel-shell-header-extra {
    margin-bottom: var(--space-4);
    flex-shrink: 0;
    padding: var(--space-3) var(--space-4);
    border: 1px solid var(--mills-color-border-subtle);
    border-radius: var(--mills-radius-surface);
    background:
      linear-gradient(180deg, color-mix(in srgb, var(--mills-color-text) 2%, transparent), transparent),
      color-mix(in srgb, var(--mills-color-surface) 92%, transparent);
  }

  .panel-shell-toolbar {
    flex-shrink: 0;
    margin-bottom: var(--space-4);
    padding: var(--space-2);
    border: 1px solid var(--mills-color-border-subtle);
    border-radius: var(--mills-radius-surface);
    background:
      linear-gradient(180deg, color-mix(in srgb, var(--mills-color-text) 2%, transparent), transparent),
      color-mix(in srgb, var(--mills-color-surface) 92%, transparent);
  }

  .panel-shell-content {
    flex: 1;
    overflow-y: auto;
    overflow-x: hidden;
  }

  /* When the panel is empty, let the empty-state fill and center within the
     full content height instead of pinning a small card to the top. */
  .panel-shell-content.is-empty {
    display: flex;
  }

  .empty-state {
    display: flex;
    flex: 1;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    padding: clamp(40px, 12vh, 96px) var(--space-4);
    color: var(--mills-color-text-muted);
    text-align: center;
    gap: var(--space-3);
    min-height: 260px;
    border: 1px dashed color-mix(in srgb, var(--tone-color) 30%, var(--mills-color-border));
    border-radius: var(--mills-radius-state);
    background:
      radial-gradient(ellipse 70% 55% at 50% 0%, var(--tone-glow), transparent 62%),
      color-mix(in srgb, var(--mills-color-surface) 72%, transparent);

    /* Per-state tone — drives the glow, border, and icon ring so the same
       empty surface can read as standing-by, error, or not-configured. */
    --tone-color: var(--mills-color-border-focus);
    --tone-glow: var(--mills-color-glow);
  }

  .empty-state.tone-ready {
    --tone-color: var(--mills-color-success);
    --tone-glow: var(--mills-color-success-glow);
  }
  .empty-state.tone-error {
    --tone-color: var(--mills-color-error);
    --tone-glow: var(--mills-color-error-glow);
  }
  .empty-state.tone-disabled {
    --tone-color: var(--mills-color-border);
    --tone-glow: transparent;
    color: var(--mills-color-text-muted);
  }

  .empty-state-icon {
    font-size: var(--mills-text-display);
    line-height: 1;
    color: color-mix(in srgb, var(--tone-color) 85%, var(--mills-color-text-secondary));
    width: 58px;
    height: 58px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--mills-radius-round);
    background: color-mix(in srgb, var(--tone-color) 8%, transparent);
    border: 1px solid color-mix(in srgb, var(--tone-color) 45%, var(--mills-color-border));
  }

  /* A slow "standing by" pulse signals the surface is live and waiting.
     Only idle/ready states pulse — errors and disabled stay static so motion
     never implies activity where there is none. */
  .empty-state.tone-idle .empty-state-icon,
  .empty-state.tone-ready .empty-state-icon {
    animation: empty-pulse 3.2s ease-in-out infinite;
  }

  @keyframes empty-pulse {
    0%, 100% {
      box-shadow: 0 0 0 0 color-mix(in srgb, var(--tone-color) 22%, transparent);
    }
    50% {
      box-shadow: 0 0 0 7px transparent;
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .empty-state-icon { animation: none !important; }
  }

  .empty-state-message {
    font-size: var(--mills-text-title);
    font-weight: 600;
    color: var(--mills-color-text);
  }

  .empty-state-hint {
    font-size: var(--mills-text-body);
    opacity: 0.85;
    max-width: 440px;
    line-height: 1.6;
  }

  .empty-state-action {
    margin-top: var(--space-2);
  }

  /* Error state mirrors .empty-state — same paddings, same icon-on-disc
     shape — so the panel feels like one family with two states. The error
     border + tinted background (mixed from the semantic error token) lets the
     operator differentiate at-a-glance from "no rows" without re-reading
     the message. */
  .error-state {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    padding: clamp(40px, 12vh, 96px) var(--space-4);
    color: var(--mills-color-text);
    text-align: center;
    gap: var(--space-3);
    min-height: 260px;
    border: 1px solid var(--mills-color-error);
    border-radius: var(--mills-radius-state);
    background:
      radial-gradient(circle at top, color-mix(in srgb, var(--mills-color-error) 14%, transparent), transparent 45%),
      color-mix(in srgb, var(--mills-color-error) 8%, var(--mills-color-surface));
  }

  .error-state-icon {
    font-size: var(--mills-text-alert);
    width: 56px;
    height: 56px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--mills-radius-round);
    background: color-mix(in srgb, var(--mills-color-error) 18%, transparent);
    border: 1px solid var(--mills-color-error);
    color: var(--mills-color-error);
    font-weight: 700;
  }

  .error-state-message {
    font-size: var(--mills-text-title);
    font-weight: 600;
    color: var(--mills-color-text);
  }

  .error-state-hint {
    font-size: var(--mills-text-body);
    color: var(--mills-color-text-secondary);
    max-width: 480px;
    line-height: 1.6;
    word-break: break-word;
  }

  .error-state-action {
    margin-top: var(--space-2);
  }
</style>
