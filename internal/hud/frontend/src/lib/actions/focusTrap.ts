import type { Action } from 'svelte/action';

/**
 * focusTrap — Svelte action for modal/dialog focus management (HUD audit slice 7).
 *
 * On mount:  remembers the element that had focus, then moves focus to the first
 *            focusable element inside `node` (falling back to `node` itself).
 * On Tab:    keeps focus inside `node`, wrapping at the first/last boundary.
 * On destroy: restores focus to the element that had it before the trap mounted.
 *
 * Escape-to-close is intentionally NOT handled here — each dialog owns its own
 * dismissal — so the user always has a way out and the trap never strands focus.
 */
const FOCUSABLE_SELECTOR = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',');

export const focusTrap: Action<HTMLElement> = (node) => {
  const previouslyFocused = document.activeElement as HTMLElement | null;

  function focusable(): HTMLElement[] {
    return Array.from(node.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)).filter(
      // Skip elements hidden via display:none (offsetParent === null), but keep
      // the currently-focused one so wrapping math stays stable.
      (el) => el.offsetParent !== null || el === document.activeElement,
    );
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Tab') return;

    const items = focusable();
    if (items.length === 0) {
      // Nothing focusable inside — pin focus to the container.
      event.preventDefault();
      node.focus();
      return;
    }

    const first = items[0];
    const last = items[items.length - 1];
    const active = document.activeElement as HTMLElement | null;

    if (event.shiftKey) {
      if (active === first || !node.contains(active)) {
        event.preventDefault();
        last.focus();
      }
    } else if (active === last || !node.contains(active)) {
      event.preventDefault();
      first.focus();
    }
  }

  // The container itself must be focusable as a fallback target.
  if (!node.hasAttribute('tabindex')) {
    node.setAttribute('tabindex', '-1');
  }

  // Move focus inside on mount (the node is already in the DOM here).
  const initial = focusable();
  (initial[0] ?? node).focus();

  node.addEventListener('keydown', handleKeydown);

  return {
    destroy() {
      node.removeEventListener('keydown', handleKeydown);
      // Restore focus to the prior element if it is still connected.
      if (previouslyFocused && previouslyFocused.isConnected) {
        previouslyFocused.focus();
      }
    },
  };
};
