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
    // The item's CURRENT state must be escalated for the run to be an open
    // spark; an item the backlog reports as anything else is history only.
    millsStore.backlog = [{ ID: 'bl-live', Title: '', State: 'escalated', Priority: '', TargetProject: 'libs/fi-accel' }];
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

describe('SparksPanel table structure', () => {
  it('emits one td per declared column and keeps every td a real table-cell', () => {
    millsStore.backlog = [{ ID: 'bl-live', Title: '', State: 'escalated', Priority: 'P1', TargetProject: '' }];
    flushSync();
    const ths = target.querySelectorAll('thead th').length;
    const row = target.querySelector('tbody tr')!;
    const tds = row.querySelectorAll(':scope > td');
    expect(tds.length).toBe(ths);
    // A td that is itself a flex container leaves the table's cell structure
    // (adjacent ones fuse into one anonymous cell and shift every header), so
    // the badge/id row must live in a wrapper INSIDE the cell, never on it.
    expect(row.querySelector('td.warp-cell > .cell-flex')).not.toBeNull();
    expect(row.querySelector('td.class-cell > .cell-flex')).not.toBeNull();
    expect(row.querySelector('td.warp-cell')?.textContent).toContain('P1');
    expect(row.querySelector('td.warp-cell')?.textContent).toContain('bl-live');
  });
});

describe('SparksPanel scope', () => {
  it('leads with open sparks and keeps merged-since attempts behind the history toggle', () => {
    // run-1 (bl-live) still needs a human; run-old (bl-done) escalated once
    // but its item has since merged — history, not an open spark.
    millsStore.archiveRuns = [{ ID: 'run-old', BacklogID: 'bl-done', Template: '', State: 'escalated', Attempts: 1 }];
    millsStore.backlog = [
      { ID: 'bl-live', Title: '', State: 'escalated', Priority: '', TargetProject: '' },
      { ID: 'bl-done', Title: '', State: 'merged', Priority: '', TargetProject: '' },
    ];
    flushSync();

    expect(target.querySelector('.panel-shell-count')?.textContent).toBe('1');
    let ids = [...target.querySelectorAll('tbody tr .warp-id')].map((el) => el.textContent);
    expect(ids).toEqual(['bl-live']);

    const buttons = [...target.querySelectorAll<HTMLButtonElement>('.scope-btn')];
    expect(buttons.map((b) => b.getAttribute('aria-pressed'))).toEqual(['true', 'false']);
    expect(buttons[0].textContent).toContain('1');
    expect(buttons[1].textContent).toContain('2');

    buttons[1].click();
    flushSync();
    ids = [...target.querySelectorAll('tbody tr .warp-id')].map((el) => el.textContent);
    expect(ids).toEqual(['bl-live', 'bl-done']);
    // The header count is the open-spark number regardless of scope, so it
    // always equals the nav badge and the floor spine.
    expect(target.querySelector('.panel-shell-count')?.textContent).toBe('1');
  });

  it('says nothing needs a human when the open set is empty and no filter is active', () => {
    millsStore.pipelineRuns = [];
    millsStore.archiveRuns = [{ ID: 'run-old', BacklogID: 'bl-done', Template: '', State: 'escalated', Attempts: 1 }];
    millsStore.backlog = [{ ID: 'bl-done', Title: '', State: 'merged', Priority: '', TargetProject: '' }];
    flushSync();

    const clear = target.querySelector('.spark-clear');
    expect(clear?.textContent).toContain('nothing needs a human');
    expect(clear?.textContent).toContain('history');
    expect(target.querySelector('.data-table')).toBeNull();
    expect(target.querySelector('.empty-state')).toBeNull();
  });
});

describe('SparksPanel failing-gate column', () => {
  it('reads "outside a gate" when the run detail carries no failing gate', async () => {
    // A fresh run ID: run-1 was already resolved (as unavailable) by the
    // default mock at mount, and a resolved run is never fetched again.
    vi.mocked(millsStore.fetchArchiveRunDetail).mockResolvedValue({
      run: { ID: 'run-gate', BacklogID: 'bl-gate', Template: '', State: 'escalated', Attempts: 1 },
      stages: [],
      gates: [{ GateName: 'lint', Outcome: 'pass' }],
    } as never);
    millsStore.backlog = [{ ID: 'bl-gate', Title: '', State: 'escalated', Priority: '', TargetProject: '' }];
    millsStore.pipelineRuns = [{ ID: 'run-gate', BacklogID: 'bl-gate', Template: '', State: 'escalated', Attempts: 1 }];
    flushSync();
    await vi.waitFor(() => expect(target.querySelector('.why-clean')?.textContent).toBe('outside a gate'));
    expect(target.textContent).not.toContain('no failing gate recorded');
  });

  it('marks a run whose detail fetch failed as unavailable instead of pending forever', async () => {
    const fetchDetail = vi.mocked(millsStore.fetchArchiveRunDetail).mockResolvedValue(null);
    millsStore.backlog = [{ ID: 'bl-live', Title: '', State: 'escalated', Priority: '', TargetProject: '' }];
    millsStore.pipelineRuns = [{ ID: 'run-1', BacklogID: 'bl-live', Template: '', State: 'escalated', Attempts: 1 }];
    flushSync();
    await vi.waitFor(() => expect(target.querySelector('.why-cell .why-pending')?.getAttribute('title')).toContain('could not be loaded'));
    // Re-render with the same run: the budget must not be re-spent on it.
    const calls = fetchDetail.mock.calls.length;
    millsStore.pipelineRuns = [...millsStore.pipelineRuns];
    flushSync();
    await Promise.resolve();
    expect(fetchDetail.mock.calls.length).toBe(calls);
  });
});
