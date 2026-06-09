<script>
  import { focusTrap } from '../actions/focusTrap';

  let { open = false, title = '', onClose = () => {}, children } = $props();

  function handleBackdropClick(e) {
    if (e.target === e.currentTarget) onClose();
  }

  function handleKeydown(e) {
    if (e.key === 'Escape') onClose();
  }
</script>

<svelte:window onkeydown={handleKeydown} />

{#if open}
  <!-- Backdrop is presentational: click-outside closes, but keyboard users
       dismiss via Escape (window handler) or the labelled close button, so the
       backdrop is not itself a focusable control. -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div class="modal-backdrop" onclick={handleBackdropClick}>
    <div class="modal" role="dialog" aria-modal="true" aria-label={title} use:focusTrap>
      <div class="modal-header">
        <h3 class="modal-title">{title}</h3>
        <button class="modal-close" aria-label="Close" onclick={onClose}>✕</button>
      </div>
      <div class="modal-body">
        {@render children?.()}
      </div>
    </div>
  </div>
{/if}

<style>
  .modal-backdrop {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.5);
    backdrop-filter: blur(8px);
    -webkit-backdrop-filter: blur(8px);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 900;
    animation: backdropFadeIn 0.15s ease;
  }
  .modal {
    background: var(--glass-bg);
    backdrop-filter: blur(var(--glass-blur));
    -webkit-backdrop-filter: blur(var(--glass-blur));
    border: 1px solid var(--glass-border);
    border-radius: var(--radius-lg);
    /* min() keeps the desktop width but never exceeds the viewport on
       phones — a fixed 380px overflowed a 360px screen (WCAG 1.4.10). */
    min-width: min(380px, calc(100vw - 32px));
    max-width: min(540px, calc(100vw - 32px));
    max-height: 80vh;
    display: flex;
    flex-direction: column;
    box-shadow: var(--shadow-lg);
    animation: modalSlideIn 0.2s ease-out;
  }
  .modal-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border);
  }
  .modal-title {
    font-size: 14px;
    font-weight: 600;
    color: var(--fg-primary);
  }
  .modal-close {
    font-size: 14px;
    color: var(--fg-muted);
    padding: 4px 6px;
    border-radius: var(--radius-sm);
  }
  .modal-close:hover {
    color: var(--fg-primary);
    background: var(--bg-tertiary);
  }
  .modal-body {
    padding: 16px;
    overflow-y: auto;
  }
  @keyframes backdropFadeIn {
    from { opacity: 0; }
    to   { opacity: 1; }
  }
  @keyframes modalSlideIn {
    from { opacity: 0; transform: scale(0.95) translateY(-10px); }
    to   { opacity: 1; transform: scale(1) translateY(0); }
  }
</style>
