import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, tick, unmount } from 'svelte';
import RescueShelf from './RescueShelf.svelte';
import { rescueRows, type RescueRow } from '../../utils/rescueHelpers.ts';
let target: HTMLDivElement;
let component: ReturnType<typeof mount> | undefined;
beforeEach(() => { target = document.createElement('div'); document.body.append(target); });
afterEach(async () => { if (component) await unmount(component); component = undefined; target.remove(); vi.restoreAllMocks(); });
function row(id = 'rescue'): RescueRow {
  return { id, title: id, priority: 'P1', age: 86400000, runID: 'r', escalationClass: 'config', failureClass: 'configuration', mrIID: 1951, project: 'services/loom-core', rescueDraft: null, scopeKnown: true,
    violations: [{ file: 'cmd/devbox/main.go', rule: 'sensitive-path', admitted: false, slice_index: -1 }], verdict: 'widen', verdictText: 'WIDEN SCOPE: review named files.', amendCommand: 'mills_backlog_amend_scope({"id":"rescue","add_files":["cmd/devbox/main.go"]})' };
}
describe('RescueShelf', () => {
  it('renders expandable violations, MR link and copies only the amend command', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    component = mount(RescueShelf, { target, props: { rows: [row()] } }); flushSync();
    expect(target.querySelector('details')?.open).toBe(false);
    target.querySelector('details')!.open = true;
    target.querySelector<HTMLDetailsElement>('.rescue-row')!.open = true;
    expect(target.querySelector('table')?.textContent).toContain('cmd/devbox/main.go');
    expect(target.querySelector('table')?.textContent).toContain('sensitive-pathNo');
    expect(target.querySelector('a')?.href).toBe('https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/1951');
    expect(target.textContent).toContain('MR title: unknown');
    target.querySelector('button')!.click();
    await vi.waitFor(() => expect(writeText).toHaveBeenCalledWith(row().amendCommand));
    await vi.waitFor(() => expect(target.textContent).toContain('Copied amend command'));
  });
  it('caps at twelve, preserves oldest-first order and filters before the cap', async () => {
    const items = Array.from({ length: 14 }, (_, n) => ({ ID: String(n), Title: `item-${n}`, State: 'escalated', Priority: 'P1', CreatedAt: new Date(n * 86400000).toISOString() })).reverse();
    const rows = rescueRows(items, {}); rows[13].escalationClass = 'infra';
    component = mount(RescueShelf, { target, props: { rows } }); flushSync();
    expect(target.querySelector('.count')?.textContent).toBe('14');
    expect(target.querySelectorAll('.rescue-row')).toHaveLength(12);
    expect(target.querySelector('.rescue-row')?.textContent).toContain('item-0');
    await tick();
    const select = target.querySelector('select')!;
    const option = [...select.options].find(o => o.value === 'infra')!;
    option.selected = true;
    select.dispatchEvent(new Event('change', { bubbles: true }));
    await tick();
    flushSync();
    expect(target.querySelectorAll('.rescue-row')).toHaveLength(1);
    expect(target.querySelector('.rescue-row')?.textContent).toContain('item-13');
    expect(target.querySelector('button')).toBeNull();
  });
  it('hides an empty shelf', () => {
    component = mount(RescueShelf, { target, props: { rows: [] } }); flushSync();
    expect(target.querySelector('details')).toBeNull(); expect(target.textContent).toBe('');
  });
  it('reports clipboard rejection without claiming success', async () => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: vi.fn().mockRejectedValue(new Error('denied')) } });
    component = mount(RescueShelf, { target, props: { rows: [row()] } }); flushSync();
    target.querySelector('button')!.click();
    await vi.waitFor(() => expect(target.textContent).toContain('Clipboard unavailable'));
    expect(target.textContent).not.toContain('Copied amend command');
  });
});
