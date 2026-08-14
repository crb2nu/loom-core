// DataTable's j/k row cursor and the containment that keeps it from
// double-firing App-level shortcuts.
//
// NOTE ON SCOPE: DataTable does not itself consult `router.subView` or
// `dialogStore.openCount` — it has no reference to either. Suppression is a
// two-part contract and this file pins both halves:
//
//   1. DataTable stops propagation on the keys it consumes. App.svelte binds a
//      window-level handler where `j`/`k` are Mills sub-view keys, so without
//      stopPropagation one `j` on the Sparks table moved the row cursor AND
//      navigated to Patterns. That is the regression; it is a containment
//      property of DataTable, not a guard.
//   2. While a dialog is open, `dialogStore.openCount > 0` — the flag
//      App.svelte's handler reads to stand its bare-letter shortcuts down.
//      focusTrap is what maintains it, so the count is asserted through the
//      action rather than through App.svelte (which would require mounting the
//      entire shell and its polling bootstrap).
//
// Mounted rather than server-rendered: all of this is listener behavior, which
// svelte/server never attaches.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import DataTableHarness from './__fixtures__/DataTableHarness.svelte';
import { focusTrap } from '../../actions/focusTrap.ts';
import { dialogStore } from '../../stores/dialogs.svelte.ts';

interface Row {
  id: string;
  name: string;
}

const ROWS: Row[] = [
  { id: 'a', name: 'alpha' },
  { id: 'b', name: 'bravo' },
  { id: 'c', name: 'charlie' },
];

let cleanup: (() => void) | null = null;

afterEach(() => {
  cleanup?.();
  cleanup = null;
  document.body.innerHTML = '';
  // The store is a module singleton; a leaked claim would silently disable
  // shortcuts for every later test.
  while (dialogStore.openCount > 0) dialogStore.pop();
});

interface MountOpts {
  rows?: Row[];
  loading?: boolean;
  keyboardNav?: boolean;
  onRowClick?: (row: Row) => void;
}

function mountTable(opts: MountOpts = {}): HTMLElement {
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(DataTableHarness, {
    target,
    props: { rows: opts.rows ?? ROWS, loading: opts.loading ?? false, keyboardNav: opts.keyboardNav ?? true, onRowClick: opts.onRowClick },
  });
  cleanup = () => { void unmount(component); target.remove(); };

  const wrap = target.querySelector<HTMLElement>('.data-table-wrap');
  if (!wrap) throw new Error('table wrap did not render');
  return wrap;
}

/** Dispatch a bubbling, cancelable keydown from `from` (defaults to the wrap). */
function press(wrap: HTMLElement, key: string, from?: HTMLElement): KeyboardEvent {
  const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
  (from ?? wrap).dispatchEvent(event);
  flushSync();
  return event;
}

/** Index of the row currently carrying the keyboard cursor, or -1. */
function focusedIndex(wrap: HTMLElement): number {
  const focused = wrap.querySelector('tr.keyboard-focused');
  return focused ? Number(focused.getAttribute('data-row-index')) : -1;
}

describe('DataTable — j/k row cursor', () => {
  it('moves the cursor down on j and up on k', () => {
    const wrap = mountTable();
    expect(focusedIndex(wrap)).toBe(-1);

    press(wrap, 'j');
    expect(focusedIndex(wrap)).toBe(0);
    press(wrap, 'j');
    expect(focusedIndex(wrap)).toBe(1);
    press(wrap, 'k');
    expect(focusedIndex(wrap)).toBe(0);
  });

  it('clamps at both ends instead of wrapping', () => {
    const wrap = mountTable();
    for (let i = 0; i < 6; i++) press(wrap, 'j');
    expect(focusedIndex(wrap)).toBe(ROWS.length - 1);
    for (let i = 0; i < 6; i++) press(wrap, 'k');
    expect(focusedIndex(wrap)).toBe(0);
  });

  it('treats the arrow keys as aliases', () => {
    const wrap = mountTable();
    press(wrap, 'ArrowDown');
    expect(focusedIndex(wrap)).toBe(0);
    press(wrap, 'ArrowDown');
    expect(focusedIndex(wrap)).toBe(1);
    press(wrap, 'ArrowUp');
    expect(focusedIndex(wrap)).toBe(0);
  });

  it('opens the focused row on Enter', () => {
    const onRowClick = vi.fn();
    const wrap = mountTable({ onRowClick });
    press(wrap, 'j');
    press(wrap, 'Enter');
    expect(onRowClick).toHaveBeenCalledWith(ROWS[0]);
  });
});

describe('DataTable — containment from App-level shortcuts', () => {
  it('stops j and k from reaching a window-level handler', () => {
    // Stands in for App.svelte's <svelte:window onkeydown>, where j/k are
    // Mills sub-view keys. Both firing is the regression.
    const appShortcut = vi.fn();
    window.addEventListener('keydown', appShortcut);
    try {
      const wrap = mountTable();

      press(wrap, 'j');
      press(wrap, 'k');

      expect(focusedIndex(wrap)).toBe(0);
      expect(appShortcut).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener('keydown', appShortcut);
    }
  });

  it('also preventDefaults so the keys do not scroll the viewport', () => {
    const wrap = mountTable();
    expect(press(wrap, 'j').defaultPrevented).toBe(true);
    expect(press(wrap, 'ArrowDown').defaultPrevented).toBe(true);
  });

  it('lets keys it does not consume bubble to the app', () => {
    // Containment must be surgical: `r` (refresh) and digits (view switching)
    // are App-level shortcuts that still have to work over a focused table.
    const appShortcut = vi.fn();
    window.addEventListener('keydown', appShortcut);
    try {
      const wrap = mountTable();

      press(wrap, 'r');
      press(wrap, '2');

      expect(appShortcut).toHaveBeenCalledTimes(2);
      expect(focusedIndex(wrap)).toBe(-1);
    } finally {
      window.removeEventListener('keydown', appShortcut);
    }
  });
});

describe('DataTable — when the cursor stands down', () => {
  it('does not move while the event came from a text input', () => {
    // Typing a `j` into an inline editor must not also drive the cursor.
    const wrap = mountTable();
    const input = document.createElement('input');
    wrap.appendChild(input);

    press(wrap, 'j', input);

    expect(focusedIndex(wrap)).toBe(-1);
  });

  it.each(['TEXTAREA', 'SELECT'])('does not move while the event came from a %s', (tag) => {
    const wrap = mountTable();
    const el = document.createElement(tag.toLowerCase());
    wrap.appendChild(el);

    press(wrap, 'j', el as HTMLElement);

    expect(focusedIndex(wrap)).toBe(-1);
  });

  it('does not move while the table is loading', () => {
    const wrap = mountTable({ loading: true });
    press(wrap, 'j');
    expect(focusedIndex(wrap)).toBe(-1);
  });

  it('does not move when there are no rows', () => {
    const wrap = mountTable({ rows: [] });
    press(wrap, 'j');
    expect(focusedIndex(wrap)).toBe(-1);
  });

  it('does nothing at all when keyboardNav is off', () => {
    const appShortcut = vi.fn();
    window.addEventListener('keydown', appShortcut);
    try {
      const wrap = mountTable({ keyboardNav: false });

      press(wrap, 'j');

      expect(focusedIndex(wrap)).toBe(-1);
      // Opted out means the key belongs to the app again.
      expect(appShortcut).toHaveBeenCalledTimes(1);
      expect(wrap.getAttribute('tabindex')).toBe('-1');
    } finally {
      window.removeEventListener('keydown', appShortcut);
    }
  });
});

describe('dialogStore — the flag App-level shortcuts stand down on', () => {
  it('is raised while a focus-trapped surface is mounted and released on destroy', () => {
    // App.svelte's handleKeydown returns early on `dialogStore.openCount > 0`,
    // which is what suppresses j/k sub-view navigation behind an open dialog.
    // focusTrap is the only thing that maintains the count.
    expect(dialogStore.openCount).toBe(0);

    const node = document.createElement('div');
    document.body.appendChild(node);
    const trap = focusTrap(node);

    expect(dialogStore.openCount).toBe(1);

    trap?.destroy?.();
    expect(dialogStore.openCount).toBe(0);
    node.remove();
  });

  it('counts nested surfaces so an inner dismissal does not un-suppress the outer', () => {
    const outer = document.createElement('div');
    const inner = document.createElement('div');
    document.body.append(outer, inner);

    const outerTrap = focusTrap(outer);
    const innerTrap = focusTrap(inner);
    expect(dialogStore.openCount).toBe(2);

    innerTrap?.destroy?.();
    // A dialog is still open — shortcuts must stay suppressed.
    expect(dialogStore.openCount).toBe(1);

    outerTrap?.destroy?.();
    expect(dialogStore.openCount).toBe(0);
    outer.remove();
    inner.remove();
  });

  it('never goes negative on an unbalanced release', () => {
    dialogStore.pop();
    expect(dialogStore.openCount).toBe(0);
  });
});
