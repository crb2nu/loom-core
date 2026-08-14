import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import BoltsPanel from './BoltsPanel.svelte';
import { millsStore } from '../../stores/mills.svelte.ts';

let target: HTMLElement;
let component: Record<string, unknown>;

beforeEach(() => {
  const run = { ID: 'bolt-1', BacklogID: 'bl-bolt', Template: '', State: 'merged', Attempts: 1, MRIID: 8 };
  vi.spyOn(millsStore, 'startPolling').mockImplementation(() => {});
  vi.spyOn(millsStore, 'stopPolling').mockImplementation(() => {});
  vi.spyOn(millsStore, 'fetchArchiveRuns').mockResolvedValue([run]);
  millsStore.archiveRuns = [run];
  millsStore.backlog = [{ ID: 'bl-bolt', Title: '', State: '', Priority: '', TargetProject: 'services/OtherRepo' }];
  target = document.createElement('div');
  document.body.appendChild(target);
  component = mount(BoltsPanel, { target }) as Record<string, unknown>;
  flushSync();
});

afterEach(() => {
  void unmount(component);
  vi.restoreAllMocks();
  target.remove();
});

describe('BoltsPanel merge request link', () => {
  it('opens the target-project MR without opening the row and keeps copy available', async () => {
    const open = vi.spyOn(millsStore, 'openRunDetail');
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });

    const link = target.querySelector<HTMLAnchorElement>('.bolt-chip')!;
    expect(link.href).toBe('https://gitlab.flexinfer.ai/services/OtherRepo/-/merge_requests/8');
    expect(link.target).toBe('_blank');
    expect(link.rel).toContain('noopener');
    link.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    expect(open).not.toHaveBeenCalled();

    target.querySelector<HTMLButtonElement>('.mr-copy')!.click();
    await vi.waitFor(() => expect(writeText).toHaveBeenCalledWith('!8'));
    expect(open).not.toHaveBeenCalled();
  });
});
