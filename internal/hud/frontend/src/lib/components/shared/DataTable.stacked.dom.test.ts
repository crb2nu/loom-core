// DataTable's ≤800px stacked-card mode hides the <thead>, so each cell must
// carry its own column label or a phone shows a card of bare values. The
// engine stamps `data-label` on every consumer-rendered <td> after render;
// the CSS shows it as a caption. This pins the stamping contract.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, tick, unmount } from 'svelte';
import DataTableHarness from './__fixtures__/DataTableHarness.svelte';

interface Row {
  id: string;
  name: string;
}

let cleanup: (() => void) | null = null;
let originalMatchMedia: typeof globalThis.matchMedia | undefined;

function stubViewport(matches: boolean): void {
  globalThis.matchMedia = ((query: string) => ({
    matches,
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  })) as unknown as typeof globalThis.matchMedia;
}

beforeEach(() => {
  originalMatchMedia = globalThis.matchMedia;
  // ResizeObserver gates the whole effect; happy-dom lacks it.
  vi.stubGlobal('ResizeObserver', class {
    observe() {}
    disconnect() {}
  });
});

afterEach(() => {
  cleanup?.();
  cleanup = null;
  document.body.innerHTML = '';
  if (originalMatchMedia) globalThis.matchMedia = originalMatchMedia;
  vi.unstubAllGlobals();
});

function mountRows(rows: Row[]): HTMLElement {
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(DataTableHarness, { target, props: { rows } });
  flushSync();
  cleanup = () => void unmount(component);
  return target;
}

describe('DataTable stacked-card labels', () => {
  it('stamps each cell with its column label when the viewport is stacked', async () => {
    stubViewport(true);
    const target = mountRows([{ id: 'a', name: 'alpha' }, { id: 'b', name: 'bravo' }]);
    await tick();
    const cells = [...target.querySelectorAll<HTMLTableCellElement>('tbody td.cell-name')];
    expect(cells.map((td) => td.dataset.label)).toEqual(['Name', 'Name']);
  });

  it('leaves cells unlabelled on a wide viewport where the header is visible', async () => {
    stubViewport(false);
    const target = mountRows([{ id: 'a', name: 'alpha' }]);
    await tick();
    const td = target.querySelector<HTMLTableCellElement>('tbody td.cell-name')!;
    expect(td.dataset.label).toBeUndefined();
  });
});
