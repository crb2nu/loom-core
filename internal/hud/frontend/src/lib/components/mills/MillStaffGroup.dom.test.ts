import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import MillStaffGroup from './MillStaffGroup.svelte';
import { millsOverseersStore } from '../../stores/mills_overseers.svelte.ts';
import { mrwatchStore } from '../../stores/mrwatch.svelte.ts';
import { mergeQueueStore } from '../../stores/mergeQueue.svelte.ts';

let target: HTMLElement;
let component: Record<string, unknown> | null;

beforeEach(() => {
  target = document.createElement('div');
  document.body.appendChild(target);
  vi.spyOn(millsOverseersStore, 'startPolling').mockImplementation(() => {});
  vi.spyOn(millsOverseersStore, 'stopPolling').mockImplementation(() => {});
  vi.spyOn(mrwatchStore, 'startPolling').mockImplementation(() => {});
  vi.spyOn(mrwatchStore, 'stopPolling').mockImplementation(() => {});
  vi.spyOn(mergeQueueStore, 'startPolling').mockImplementation(() => {});
  vi.spyOn(mergeQueueStore, 'stopPolling').mockImplementation(() => {});
  mergeQueueStore.ready = [];
  mergeQueueStore.blocked = [];
  mergeQueueStore.summary = { total_branches: 0, ready_to_merge: 0, blocked: 0, conflict_pairs: 0 };
  mergeQueueStore.loading = false;
  mergeQueueStore.error = null;
  mergeQueueStore.lastUpdated = null;
  component = mount(MillStaffGroup, { target }) as Record<string, unknown>;
  flushSync();
});

afterEach(() => {
  if (component) void unmount(component);
  component = null;
  target.remove();
  vi.restoreAllMocks();
});

describe('MillStaffGroup', () => {
  it('groups the three staff surfaces under an accessible shared shell', () => {
    const group = target.querySelector<HTMLElement>('section[role="group"][aria-label="Mill Staff"]');
    expect(group).not.toBeNull();
    expect(group?.querySelector('[aria-label="Overseer status"]')).not.toBeNull();
    expect(group?.querySelector('[aria-label="MR watch status"]')).not.toBeNull();
    expect(group?.querySelector('[aria-label="Merge queue status"]')).not.toBeNull();
  });

  it('collapses without unmounting child panels', () => {
    const toggle = target.querySelector<HTMLButtonElement>(
      'button[aria-controls="mill-staff-group-content"]',
    );
    const content = target.querySelector<HTMLElement>('#mill-staff-group-content');
    const mrwatch = content?.querySelector('[aria-label="MR watch status"]');

    expect(toggle?.getAttribute('aria-expanded')).toBe('true');
    expect(content?.hidden).toBe(false);
    toggle?.click();
    flushSync();
    expect(toggle?.getAttribute('aria-expanded')).toBe('false');
    expect(content?.hidden).toBe(true);
    expect(content?.querySelector('[aria-label="MR watch status"]')).toBe(mrwatch);
    expect(mrwatchStore.stopPolling).not.toHaveBeenCalled();
    expect(mergeQueueStore.stopPolling).not.toHaveBeenCalled();

    toggle?.click();
    flushSync();
    expect(content?.hidden).toBe(false);
  });

  it('keeps merge queue loading, unavailable, empty, and active states distinct', () => {
    const queue = target.querySelector<HTMLElement>('[aria-label="Merge queue status"]');
    expect(queue?.textContent).toContain('Merge queue is empty.');

    mergeQueueStore.loading = true;
    flushSync();
    expect(queue?.textContent).toContain('Loading merge queue…');

    mergeQueueStore.loading = false;
    mergeQueueStore.error = 'gateway timeout';
    flushSync();
    expect(queue?.textContent).toContain('Merge queue unavailable — gateway timeout');

    mergeQueueStore.error = null;
    mergeQueueStore.ready = [{
      agent_id: 'codex-1', branch: 'feat/staff', status: 'active', merge_ready: true,
      conflict_files: 0, blocked_tasks: 0, task_count: 1,
    }];
    mergeQueueStore.summary = { total_branches: 1, ready_to_merge: 1, blocked: 0, conflict_pairs: 0 };
    flushSync();
    expect(queue?.textContent).toContain('ready1');
  });
});
