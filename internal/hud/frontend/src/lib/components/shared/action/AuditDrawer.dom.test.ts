// Chrome contract for AuditDrawer after its collapse onto shared/DetailDrawer.
//
// The drawer retired its hand-rolled scrim/panel/Escape wiring (the last
// overlay in the app still carrying ad-hoc z-index 900/901); what must survive
// the move is pinned here: the dialog keeps its accessible names, Escape /
// backdrop / ✕ all still close it through actionStore.closeDrawer(), and the
// Clear control lives on the header row. A `.dom.test.ts` because the keyboard
// half of the contract needs mounted listeners, not server markup.

import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import AuditDrawer from './AuditDrawer.svelte';
import { actionStore } from '../../../stores/action.svelte.ts';

let cleanup: (() => void) | null = null;

afterEach(() => {
  cleanup?.();
  cleanup = null;
  actionStore.clear();
  actionStore.closeDrawer();
  document.body.innerHTML = '';
});

function mountDrawer(): HTMLElement {
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(AuditDrawer, { target });
  cleanup = () => {
    void unmount(component);
    target.remove();
  };
  flushSync();
  return target;
}

function openWithEntry(label = 'Approve inbox item'): HTMLElement {
  actionStore.start(label, 'OverviewPanel:inbox/approve', false);
  actionStore.openDrawer();
  return mountDrawer();
}

function press(el: Element, key: string): void {
  el.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true }));
}

describe('AuditDrawer — drawer chrome', () => {
  it('renders a labelled modal dialog with its entries', () => {
    const target = openWithEntry('Approve inbox item');

    const dialog = target.querySelector('[role="dialog"]');
    expect(dialog).not.toBeNull();
    expect(dialog?.getAttribute('aria-modal')).toBe('true');
    expect(dialog?.getAttribute('aria-label')).toBe('Recent Actions');
    // Preserved verbatim from the hand-rolled <aside> this replaced —
    // DetailDrawer's generic default would have renamed the ✕.
    expect(target.querySelector('.drawer-close')?.getAttribute('aria-label')).toBe(
      'Close audit drawer',
    );
    expect(target.textContent).toContain('Approve inbox item');
  });

  it('renders nothing while the drawer is closed', () => {
    actionStore.start('Approve inbox item', 'OverviewPanel:inbox/approve', false);
    const target = mountDrawer();

    expect(target.querySelector('[role="dialog"]')).toBeNull();
  });

  it('closes on Escape', () => {
    const target = openWithEntry();

    press(target.querySelector('[role="dialog"]')!, 'Escape');
    flushSync();

    expect(actionStore.drawerOpen).toBe(false);
    expect(target.querySelector('[role="dialog"]')).toBeNull();
  });

  it('closes on a backdrop click', () => {
    const target = openWithEntry();

    target.querySelector<HTMLElement>('.drawer-backdrop')?.click();
    flushSync();

    expect(actionStore.drawerOpen).toBe(false);
  });

  it('closes via the ✕', () => {
    const target = openWithEntry();

    target.querySelector<HTMLButtonElement>('.drawer-close')?.click();
    flushSync();

    expect(actionStore.drawerOpen).toBe(false);
  });
});

describe('AuditDrawer — Clear control', () => {
  it('sits on the header row and empties the ring', () => {
    const target = openWithEntry();

    const clear = target.querySelector<HTMLButtonElement>('.drawer-header-actions .audit-btn');
    expect(clear).not.toBeNull();
    expect(clear?.disabled).toBe(false);

    clear?.click();
    flushSync();

    expect(actionStore.entries).toHaveLength(0);
    expect(target.textContent).toContain('No actions yet this session.');
  });

  it('is disabled when there is nothing to clear', () => {
    actionStore.openDrawer();
    const target = mountDrawer();

    expect(
      target.querySelector<HTMLButtonElement>('.drawer-header-actions .audit-btn')?.disabled,
    ).toBe(true);
  });
});
