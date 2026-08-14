import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MockInstance } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import SparksPanel from './SparksPanel.svelte';
import { millsStore } from '../../stores/mills.svelte.ts';

let target: HTMLElement;
let component: Record<string, unknown> | null;
let setIntervalSpy: MockInstance<typeof globalThis.setInterval>;
let clearIntervalSpy: MockInstance<typeof globalThis.clearInterval>;

function reset(): void {
  millsStore.stopPolling();
  millsStore.pipelineRuns = [{ ID: 'run-1', BacklogID: 'bl-live', Template: '', State: 'escalated', Attempts: 1 }];
  millsStore.archiveRuns = [];
  millsStore.relaunchCandidates = [];
  millsStore.relaunchCandidatesLoading = false;
  millsStore.relaunchCandidatesError = null;
  millsStore.backlog = [];
}

beforeEach(() => {
  reset();
  setIntervalSpy = vi.spyOn(globalThis, 'setInterval');
  clearIntervalSpy = vi.spyOn(globalThis, 'clearInterval');
  vi.spyOn(millsStore, 'startPolling').mockImplementation(() => {});
  vi.spyOn(millsStore, 'stopPolling').mockImplementation(() => {});
  vi.spyOn(millsStore, 'refreshArchiveRuns').mockResolvedValue();
  vi.spyOn(millsStore, 'fetchArchiveRunDetail').mockResolvedValue(null);
  vi.spyOn(millsStore, 'fetchRelaunchCandidates').mockResolvedValue();
  target = document.createElement('div');
  document.body.appendChild(target);
  component = mount(SparksPanel, { target }) as Record<string, unknown>;
  flushSync();
});

afterEach(() => {
  if (component) void unmount(component);
  component = null;
  reset();
  vi.restoreAllMocks();
  target.remove();
});

describe('SparksPanel relaunch queue', () => {
  it('renders candidate fields and dispatches the existing requeue action', async () => {
    const requeue = vi.spyOn(millsStore, 'requeuePipelineRun').mockResolvedValue({ kind: 'started', message: 'started' });
    const twoHoursAgo = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString();
    millsStore.relaunchCandidates = [{
      backlogId: 'bl-42', title: 'Repair', escalationClass: 'infra', failureClass: '', latestRunEndedAt: twoHoursAgo,
    }];
    flushSync();

    const queue = target.querySelector('.relaunch-queue') as HTMLElement;
    expect(queue.textContent).toContain('bl-42');
    expect(queue.textContent).toContain('infra');
    expect(queue.textContent).toContain('2h ago');
    (queue.querySelector('button') as HTMLButtonElement).click();
    expect(requeue).toHaveBeenCalledWith('bl-42');
    await vi.waitFor(() => expect(queue.textContent).toContain('started'));
    expect((queue.querySelector('button') as HTMLButtonElement).disabled).toBe(true);
  });

  it('keeps the queue visible when there are no ordinary spark rows', () => {
    millsStore.pipelineRuns = [];
    millsStore.relaunchCandidates = [{
      backlogId: 'bl-queue-only', title: '', escalationClass: 'infra', failureClass: '', latestRunEndedAt: null,
    }];
    flushSync();

    expect(target.querySelector('.relaunch-queue')?.textContent).toContain('bl-queue-only');
    expect(target.querySelector('.empty-state')).toBeNull();
  });

  it('distinguishes an empty queue from an unavailable queue', () => {
    expect(target.querySelector('.relaunch-queue')?.textContent).toContain('no relaunch candidates');
    millsStore.pipelineRuns = [];
    millsStore.relaunchCandidatesError = 'offline';
    flushSync();
    expect(target.querySelector('.relaunch-queue')?.textContent).toContain('Relaunch queue unavailable.');
    expect(target.querySelector('.relaunch-queue')?.textContent).not.toContain('no relaunch candidates');
  });

  it('starts the panel-owned fetch and stops its 60-second timer on unmount', async () => {
    const fetchCandidates = vi.mocked(millsStore.fetchRelaunchCandidates);
    expect(fetchCandidates).toHaveBeenCalledTimes(1);
    const relaunchTimer = setIntervalSpy.mock.results[
      setIntervalSpy.mock.calls.findIndex((call) => call[1] === 60000)
    ]?.value;
    expect(relaunchTimer).toBeDefined();
    const relaunchTick = setIntervalSpy.mock.calls.find((call) => call[1] === 60000)?.[0] as
      | (() => void)
      | undefined;
    expect(relaunchTick).toBeDefined();
    relaunchTick!();
    await vi.waitFor(() => expect(fetchCandidates).toHaveBeenCalledTimes(2));
    await unmount(component!);
    component = null;
    expect(clearIntervalSpy).toHaveBeenCalledWith(relaunchTimer);
    expect(fetchCandidates).toHaveBeenCalledTimes(2);
  });
});

describe('SparksPanel merge request link', () => {
  it('opens the target-project MR without opening the row and keeps copy available', async () => {
    millsStore.pipelineRuns = [{ ID: 'run-1', BacklogID: 'bl-live', Template: '', State: 'escalated', Attempts: 1, MRIID: 17 }];
    millsStore.backlog = [{ ID: 'bl-live', Title: '', State: '', Priority: '', TargetProject: 'libs/fi-accel' }];
    flushSync();
    const open = vi.spyOn(millsStore, 'openRunDetail');
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });

    const link = target.querySelector<HTMLAnchorElement>('.mr-chip')!;
    expect(link.href).toBe('https://gitlab.flexinfer.ai/libs/fi-accel/-/merge_requests/17');
    expect(link.target).toBe('_blank');
    expect(link.rel).toContain('noopener');
    link.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    expect(open).not.toHaveBeenCalled();

    target.querySelector<HTMLButtonElement>('.mr-copy')!.click();
    await vi.waitFor(() => expect(writeText).toHaveBeenCalledWith('!17'));
    expect(open).not.toHaveBeenCalled();
  });
});
