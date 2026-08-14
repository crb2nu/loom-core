<script lang="ts">
  import type { Snippet } from 'svelte';

  /**
   * PanelHeader — lightweight panel identity row for panels that compose
   * their own body instead of using PanelShell. Renders the same
   * title/icon/count language as PanelShell's header so every panel
   * announces itself identically, plus optional inline stats (badges,
   * filter pills) and right-aligned actions.
   *
   * Use PanelShell when the panel also wants managed loading/empty/error
   * surfaces; use PanelHeader when the panel owns its own layout.
   */
  let {
    title,
    icon = '',
    count = null,
    stats,
    actions,
  }: {
    title: string;
    icon?: string;
    count?: number | null;
    stats?: Snippet;
    actions?: Snippet;
  } = $props();
</script>

<div class="panel-header">
  <div class="panel-header-lead">
    <div class="panel-header-title-row">
      {#if icon}
        <span class="panel-header-icon" aria-hidden="true">{icon}</span>
      {/if}
      <h2 class="panel-header-title">{title}</h2>
      {#if count != null}
        <span class="panel-header-count">{count}</span>
      {/if}
    </div>
    {#if stats}
      <div class="panel-header-stats">
        {@render stats()}
      </div>
    {/if}
  </div>
  {#if actions}
    <div class="panel-header-actions">
      {@render actions()}
    </div>
  {/if}
</div>

<style>
  .panel-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    flex-wrap: wrap;
    padding: var(--space-2) 0;
    margin-bottom: var(--space-2);
    border-bottom: 1px solid var(--border);
    flex-shrink: 0;
    position: relative;
  }

  .panel-header-lead {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
    min-width: 0;
  }

  .panel-header-title-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .panel-header-icon {
    font-size: var(--text-lg);
    color: var(--fg-muted);
  }

  /* Mirrors .panel-shell-title so PanelShell and PanelHeader panels read
     as the same family. */
  .panel-header-title {
    font-size: clamp(18px, 1.7vw, 24px);
    font-weight: 700;
    color: var(--fg-primary);
    margin: 0;
    letter-spacing: var(--tracking-tight);
  }

  .panel-header-count {
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    font-weight: 600;
    color: var(--fg-secondary);
    background: var(--bg-tertiary);
    padding: 2px 8px;
    border-radius: var(--radius-full);
    border: 1px solid var(--border);
  }

  .panel-header-stats {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
    font-size: var(--text-sm);
  }

  .panel-header-actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  @media (max-width: 768px) {
    .panel-header {
      flex-direction: column;
      align-items: flex-start;
    }
  }
</style>
