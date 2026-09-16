import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import PatternCardPicker from './PatternCardPicker.svelte';
import PatternMaterialsFields from './PatternMaterialsFields.svelte';
import type { PatternInfo, PatternMaterialField } from '../../stores/patterns.svelte.ts';
import type { RawMaterialValues } from '../../utils/spinningRoomHelpers.ts';

// Coverage for the consolidated stamp surface: the Pattern Loom page and the
// Spin dialog used to carry separate pickers and materials forms with
// diverging semantics; these components are now the single implementation.

let cleanup: (() => void) | null = null;

afterEach(() => {
  cleanup?.();
  cleanup = null;
  document.body.innerHTML = '';
});

function pattern(over: Partial<PatternInfo>): PatternInfo {
  return {
    id: 'pattern-x',
    slug: 'x',
    name: 'X',
    status: 'approved',
    makes: 'a thing',
    version: 1,
    materials_schema: [],
    ...over,
  } as PatternInfo;
}

function mountPicker(props: Record<string, unknown>): HTMLElement {
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(PatternCardPicker, { target, props: props as never });
  cleanup = () => {
    void unmount(component);
    target.remove();
  };
  flushSync();
  return target;
}

describe('PatternCardPicker', () => {
  const catalog = [
    pattern({ id: 'p-cand', name: 'Bravo', status: 'candidate' }),
    pattern({ id: 'p-appr2', name: 'Charlie', status: 'approved' }),
    pattern({ id: 'p-appr1', name: 'Alpha', status: 'approved' }),
    pattern({ id: 'p-depr', name: 'Delta', status: 'deprecated' }),
  ];

  it('orders status-grouped (approved first), name-ascending within a group', () => {
    const target = mountPicker({ patterns: catalog, selectedId: null, onPick: () => {} });
    const names = [...target.querySelectorAll('.pick-name')].map((n) => n.textContent);
    expect(names).toEqual(['Alpha', 'Charlie', 'Bravo', 'Delta']);
  });

  it('approvedOnly renders candidates visibly but not clickably (the dialog policy)', () => {
    const onPick = vi.fn();
    const target = mountPicker({ patterns: catalog, selectedId: null, onPick, approvedOnly: true });
    const buttons = [...target.querySelectorAll<HTMLButtonElement>('.pick')];
    expect(buttons).toHaveLength(4);
    const candidate = buttons.find((b) => b.textContent?.includes('Bravo'))!;
    expect(candidate.disabled).toBe(true);
    candidate.click();
    flushSync();
    expect(onPick).not.toHaveBeenCalled();
    const approved = buttons.find((b) => b.textContent?.includes('Alpha'))!;
    expect(approved.disabled).toBe(false);
    approved.click();
    flushSync();
    expect(onPick).toHaveBeenCalledWith(expect.objectContaining({ id: 'p-appr1' }));
  });

  it('without approvedOnly a candidate is selectable (the page inspects them)', () => {
    const onPick = vi.fn();
    const target = mountPicker({ patterns: catalog, selectedId: null, onPick });
    const candidate = [...target.querySelectorAll<HTMLButtonElement>('.pick')].find((b) =>
      b.textContent?.includes('Bravo'),
    )!;
    expect(candidate.disabled).toBe(false);
    candidate.click();
    flushSync();
    expect(onPick).toHaveBeenCalledWith(expect.objectContaining({ id: 'p-cand' }));
  });
});

describe('PatternMaterialsFields', () => {
  const schema: PatternMaterialField[] = [
    { name: 'topic', type: 'string', required: true },
    { name: 'flag', type: 'bool', default: 'true' },
    { name: 'shape', type: 'enum', enum: ['a', 'b'] },
  ];

  it('renders a tri-state select for bools whose empty option names the default', () => {
    const values: RawMaterialValues = {};
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = mount(PatternMaterialsFields, {
      target,
      props: { schema, values } as never,
    });
    cleanup = () => {
      void unmount(component);
      target.remove();
    };
    flushSync();

    const boolSelect = target.querySelector<HTMLSelectElement>('#mat-flag')!;
    const options = [...boolSelect.options].map((o) => o.textContent);
    // No checkbox: an untouched bool must stay omittable so the pattern's
    // default applies (the old checkbox force-wrote explicit false).
    expect(options).toEqual(['default (true)', 'true', 'false']);
    expect(target.querySelector('input[type="checkbox"]')).toBeNull();

    // Round-trip through the component's own state: the change handler
    // writes the (bindable) record and the re-render feeds value= back. A
    // plain object prop can't observe parent write-back ($bindable wraps
    // it), so the bind:values contract is exercised by the two real
    // consumers, which bind $state records.
    boolSelect.value = 'false';
    boolSelect.dispatchEvent(new Event('change'));
    flushSync();
    expect(boolSelect.value).toBe('false');

    const topic = target.querySelector<HTMLInputElement>('#mat-topic')!;
    topic.value = 'hello';
    topic.dispatchEvent(new Event('input'));
    flushSync();
    expect(topic.value).toBe('hello');
  });
});
