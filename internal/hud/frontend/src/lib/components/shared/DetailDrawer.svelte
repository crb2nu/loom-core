<script lang="ts">
  import type { Snippet } from 'svelte';
  import { focusTrap } from '../../actions/focusTrap';

  /**
   * DetailDrawer — slide-in panel from the right edge for drill-down detail views.
   * Traps focus when open (via the shared focusTrap action), closes on Escape or
   * backdrop click.
   */
  let {
    open = false,
    title = '',
    subtitle = '',
    width = 'var(--drawer-width)',
    contentPadding = 'var(--space-3) var(--space-4)',
    closeLabel = 'Close detail panel',
    onClose = () => {},
    titleContent,
    headerActions,
    header,
    footer,
    children,
  }: {
    open?: boolean;
    title?: string;
    subtitle?: string;
    width?: string;
    /**
     * Padding applied to the scrolling body. Defaults to the standard drawer
     * gutter; pass "0" when the body's own sections are full-bleed and pad
     * themselves (the Mills run drawers), otherwise the gutters double up.
     */
    contentPadding?: string;
    /**
     * Accessible name for the ✕. Defaults to the generic string; pass a
     * surface-specific one ("Close run detail") where a screen-reader user
     * benefits from hearing which panel the button dismisses.
     */
    closeLabel?: string;
    onClose?: () => void;
    /**
     * Rendered in place of the plain <h3>{title}</h3> when the title is richer
     * than a string (e.g. a kicker + state chip). `title` should still be set —
     * it is what the dialog's aria-label reads from.
     */
    titleContent?: Snippet;
    /** Controls placed to the LEFT of the ✕ on the title row. */
    headerActions?: Snippet;
    header?: Snippet;
    footer?: Snippet;
    children: Snippet;
  } = $props();

  // Escape closes the drawer from the panel itself. stopPropagation keeps one
  // Escape to one surface, so a ConfirmDialog rendered over the drawer isn't
  // dismissed along with its parent.
  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.stopPropagation();
      onClose();
    }
  }

  // …but the scoped handler is only reachable while focus is still inside the
  // panel, and focus can be ORPHANED out of an open drawer. focusTrap moves
  // focus to the first focusable child on mount, and that child can unmount
  // underneath the user — a run drawer's Escalate button vanishes the instant
  // a background poll flips the run terminal — at which point
  // document.activeElement falls back to <body>, the <aside> is no longer on
  // the event path, and Escape silently stops closing the drawer. Same shape
  // after any nested-dialog round-trip whose previously-focused control is
  // gone by the time focusTrap restores it. This window listener re-arms the
  // key for exactly that state.
  //
  // The gate is "focus is orphaned", NOT the broader "focus is outside this
  // node": a ConfirmDialog or the audit drawer opened over this one
  // legitimately holds focus outside the subtree, and Escape belongs to that
  // surface alone. For the same reason the drawer must not re-home focus on
  // focusout — it would yank focus straight back out of those dialogs.
  function focusOrphaned(): boolean {
    const active = document.activeElement;
    return (
      !active ||
      active === document.body ||
      active === document.documentElement ||
      !active.isConnected
    );
  }

  function handleWindowKeydown(e: KeyboardEvent) {
    if (e.key !== 'Escape' || !focusOrphaned()) return;
    onClose();
  }

  function handleBackdropClick() {
    onClose();
  }
</script>

<svelte:window onkeydown={open ? handleWindowKeydown : undefined} />

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="drawer-backdrop" onclick={handleBackdropClick} onkeydown={handleKeydown}></div>
  <aside
    class="drawer"
    style:width={width}
    role="dialog"
    aria-modal="true"
    aria-label={title || 'Detail panel'}
    tabindex="-1"
    onkeydown={handleKeydown}
    use:focusTrap
  >
    <!-- Header -->
    <div class="drawer-header">
      <div class="drawer-header-text">
        {#if titleContent}
          {@render titleContent()}
        {:else if title}
          <h3 class="drawer-title">{title}</h3>
        {/if}
        {#if subtitle}
          <span class="drawer-subtitle">{subtitle}</span>
        {/if}
      </div>
      <div class="drawer-header-actions">
        {#if headerActions}
          {@render headerActions()}
        {/if}
        <button class="drawer-close btn btn-ghost" onclick={onClose} aria-label={closeLabel}>
          {'\u2715'}
        </button>
      </div>
    </div>

    {#if header}
      <div class="drawer-header-extra">
        {@render header()}
      </div>
    {/if}

    <!-- Content -->
    <div class="drawer-content" style:padding={contentPadding}>
      {@render children()}
    </div>

    <!-- Footer -->
    {#if footer}
      <div class="drawer-footer">
        {@render footer()}
      </div>
    {/if}
  </aside>
{/if}

<style>
  .drawer-backdrop {
    position: fixed;
    inset: 0;
    background: rgba(6, 12, 16, 0.6);
    backdrop-filter: blur(4px);
    z-index: var(--z-drawer);
    animation: fadeIn 0.15s ease-out;
  }

  @keyframes fadeIn {
    from { opacity: 0; }
    to   { opacity: 1; }
  }

  .drawer {
    position: fixed;
    top: 0;
    right: 0;
    bottom: 0;
    z-index: calc(var(--z-drawer) + 1);
    background: var(--bg-secondary);
    border-left: 1px solid var(--border);
    box-shadow: -8px 0 32px rgba(0, 0, 0, 0.4), 0 0 1px rgba(0, 200, 255, 0.1);
    display: flex;
    flex-direction: column;
    animation: drawerSlideIn var(--duration-normal) cubic-bezier(0.4, 0, 0.2, 1);
    outline: none;
  }

  @keyframes drawerSlideIn {
    from { transform: translateX(100%); opacity: 0.8; }
    to   { transform: translateX(0); opacity: 1; }
  }

  .drawer-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border);
    flex-shrink: 0;
    position: relative;
  }

  /* Top-edge glow on drawer header */
  .drawer-header::before {
    content: '';
    position: absolute;
    top: 0;
    left: 10%;
    right: 10%;
    height: 1px;
    background: linear-gradient(90deg, transparent, rgba(0, 200, 255, 0.12), transparent);
    pointer-events: none;
  }

  .drawer-header-text {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }

  .drawer-title {
    font-size: var(--text-base);
    font-weight: 600;
    color: var(--fg-primary);
    margin: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    letter-spacing: var(--tracking-tight);
  }

  .drawer-subtitle {
    font-size: var(--text-xs);
    color: var(--fg-dim);
    font-family: var(--font-mono);
    letter-spacing: var(--tracking-normal);
  }

  /* Slot for consumer-supplied controls, sat next to the ✕ on the title row. */
  .drawer-header-actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-shrink: 0;
  }

  .drawer-close {
    flex-shrink: 0;
    font-size: var(--text-lg);
    padding: var(--space-1);
    transition: color var(--transition-fast), background var(--transition-fast);
  }

  .drawer-close:hover {
    color: var(--fg-primary);
    background: var(--bg-tertiary);
  }

  .drawer-header-extra {
    padding: var(--space-2) var(--space-4);
    border-bottom: 1px solid var(--border-subtle);
    flex-shrink: 0;
  }

  /* Padding comes from the `contentPadding` prop (inline style) so a body of
     full-bleed, self-padded sections can opt out with "0". */
  .drawer-content {
    flex: 1;
    overflow-y: auto;
  }

  .drawer-footer {
    padding: var(--space-3) var(--space-4);
    border-top: 1px solid var(--border);
    flex-shrink: 0;
  }

  /* ≤800px — full-screen drawer (Slice B5 of the HUD UX overhaul). The
     side-panel pattern is replaced by a full-viewport sheet that slides up
     from the bottom. Leaves room above the bottom-fixed nav bar so the
     close button stays reachable. */
  @media (max-width: 800px) {
    .drawer {
      width: 100vw !important;
      left: 0;
      right: 0;
      top: 0;
      /* Stop short of the bottom-fixed nav (64px + safe-area). */
      bottom: calc(64px + env(safe-area-inset-bottom, 0px));
      border-left: none;
      border-bottom: 1px solid var(--border);
      animation: drawerSlideUp var(--duration-normal) cubic-bezier(0.4, 0, 0.2, 1);
    }
    .drawer-close {
      min-width: 44px;
      min-height: 44px;
      display: flex;
      align-items: center;
      justify-content: center;
    }
  }

  @keyframes drawerSlideUp {
    from { transform: translateY(20%); opacity: 0.8; }
    to   { transform: translateY(0);   opacity: 1; }
  }
</style>
