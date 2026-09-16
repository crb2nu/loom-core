import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import BoltsPanel from './BoltsPanel.svelte';
import { millsStore, type MergeQueueEntry } from '../../stores/mills.svelte.ts';

let target: HTMLElement;
let component: Record<string, unknown>;

function pressEntry(over: Partial<MergeQueueEntry>): MergeQueueEntry {
  return {
    id: 1,
    pipeline_run_id: 'PIPE-1',
    backlog_id: 'bl-press-1',
    project: 'services/loom-core',
    mr_iid: 1612,
    source_branch: 'feat/x',
    target_branch: 'main',
    enqueued_sha: 'aaa',
    current_sha: 'bbb',
    state: 'queued',
    attempts: 1,
    enqueued_at: '2026-08-15T12:00:00Z',
    updated_at: '2026-08-15T12:01:00Z',
    ...over,
  };
}

beforeEach(() => {
  const run = { ID: 'bolt-1', BacklogID: 'bl-bolt', Template: '', State: 'merged', Attempts: 1, MRIID: 8 };
  vi.spyOn(millsStore, 'startPolling').mockImplementation(() => {});
  vi.spyOn(millsStore, 'stopPolling').mockImplementation(() => {});
  vi.spyOn(millsStore, 'fetchArchiveRuns').mockResolvedValue([run]);
  vi.spyOn(millsStore, 'fetchMergeQueue').mockResolvedValue(undefined);
  millsStore.archiveRuns = [run];
  millsStore.backlog = [{ ID: 'bl-bolt', Title: '', State: '', Priority: '', TargetProject: 'services/OtherRepo' }];
  millsStore.mergeQueue = null;
  millsStore.mergeQueueError = null;
  target = document.createElement('div');
  document.body.appendChild(target);
  component = mount(BoltsPanel, { target }) as Record<string, unknown>;
  flushSync();
});

afterEach(() => {
  void unmount(component);
  vi.restoreAllMocks();
  millsStore.mergeQueue = null;
  millsStore.mergeQueueError = null;
  millsStore.mergeQueueActive = false;
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

describe('BoltsPanel work visibility', () => {
  // Multi-repo, multi-origin fixture mirroring the live floor: cross-repo
  // work alongside home work, plan-emitted alongside operator-directed.
  function seedMixedFloor(): void {
    const runs = [
      { ID: 'r-home', BacklogID: 'bl-home', Template: '', State: 'merged', Attempts: 1, MRIID: 1 },
      { ID: 'r-deck', BacklogID: 'bl-deck', Template: '', State: 'merged', Attempts: 1, MRIID: 2 },
      { ID: 'r-proc', BacklogID: 'bl-proc', Template: '', State: 'merged', Attempts: 1, MRIID: 3 },
    ];
    millsStore.archiveRuns = runs;
    millsStore.backlog = [
      {
        ID: 'bl-home',
        Title: 'Harden failure classifier',
        State: 'merged',
        Priority: 'P2',
        CreatedBy: 'mills:plan-slice-emitter',
      },
      {
        ID: 'bl-deck',
        Title: 'flexdeck perf budget from CI',
        State: 'merged',
        Priority: 'P1',
        TargetProject: 'services/flexdeck',
        CreatedBy: 'operator-directed',
      },
      {
        ID: 'bl-proc',
        Title: 'procmodel roadmap refresh',
        State: 'merged',
        Priority: 'P3',
        TargetProject: 'services/procmodel',
        CreatedBy: 'portfolio-sensor',
        Labels: ['maintenance-loop', 'portfolio-sensor'],
      },
    ];
    flushSync();
  }

  it('shows the why-line so a merged row answers "what was this"', () => {
    seedMixedFloor();
    const whys = [...target.querySelectorAll('.bolts-why')].map((n) => n.textContent?.trim());
    expect(whys).toContain('Harden failure classifier');
    expect(whys).toContain('flexdeck perf budget from CI');
  });

  it('renders the target repo, marking cross-repo work as the exception', () => {
    seedMixedFloor();
    const repos = [...target.querySelectorAll('[data-repo]')].map((n) => n.getAttribute('data-repo'));
    expect(repos).toContain('loom-core');
    expect(repos).toContain('flexdeck');
    expect(repos).toContain('procmodel');
    // Home work stays quiet; only cross-repo carries the marker class.
    expect(target.querySelectorAll('[data-repo].cross')).toHaveLength(2);
  });

  it('renders origin, separating mill-emitted from human and sensor work', () => {
    seedMixedFloor();
    const origins = [...target.querySelectorAll('[data-origin]')].map((n) =>
      n.getAttribute('data-origin'),
    );
    expect(origins).toContain('plan');
    expect(origins).toContain('operator');
    expect(origins).toContain('sensor');
  });

  it('filters by repo and reports what it is hiding', () => {
    seedMixedFloor();
    const flexdeck = [...target.querySelectorAll<HTMLButtonElement>('.facet')].find((b) =>
      b.textContent?.startsWith('flexdeck'),
    )!;
    expect(flexdeck).toBeDefined();
    flexdeck.click();
    flushSync();

    const whys = [...target.querySelectorAll('.bolts-why')].map((n) => n.textContent?.trim());
    expect(whys).toEqual(['flexdeck perf budget from CI']);
    // The operator must always be able to see the filter is on and undo it.
    expect(target.querySelector('.facet-clear')?.textContent).toContain('showing 1 of 3');

    target.querySelector<HTMLButtonElement>('.facet-clear')!.click();
    flushSync();
    expect(target.querySelectorAll('.bolts-why')).toHaveLength(3);
  });

  it('filters by origin and toggles off when the same facet is re-clicked', () => {
    seedMixedFloor();
    const sensor = [...target.querySelectorAll<HTMLButtonElement>('.facet')].find(
      (b) => b.textContent?.startsWith('sensor'),
    )!;
    sensor.click();
    flushSync();
    expect(target.querySelectorAll('.bolts-why')).toHaveLength(1);
    expect(sensor.getAttribute('aria-pressed')).toBe('true');

    sensor.click();
    flushSync();
    expect(target.querySelectorAll('.bolts-why')).toHaveLength(3);
  });

  it('counts both encodings of the home repo as one facet', () => {
    // 460 live items carry an empty TargetProject and 63 the explicit path;
    // if these split, the facet counts mislead.
    millsStore.archiveRuns = [
      { ID: 'r1', BacklogID: 'b1', Template: '', State: 'merged', Attempts: 1 },
      { ID: 'r2', BacklogID: 'b2', Template: '', State: 'merged', Attempts: 1 },
      { ID: 'r3', BacklogID: 'b3', Template: '', State: 'merged', Attempts: 1 },
    ];
    millsStore.backlog = [
      { ID: 'b1', Title: 'a', State: '', Priority: '' },
      { ID: 'b2', Title: 'b', State: '', Priority: '', TargetProject: 'services/loom-core' },
      { ID: 'b3', Title: 'c', State: '', Priority: '', TargetProject: 'services/flexdeck' },
    ];
    flushSync();

    const home = [...target.querySelectorAll<HTMLButtonElement>('.facet')].find((b) =>
      b.textContent?.startsWith('loom-core'),
    );
    expect(home?.querySelector('.facet-n')?.textContent).toBe('2');
  });
});

describe('BoltsPanel press (serial merge queue)', () => {
  it('opts into the shared-tick refresh while mounted', () => {
    expect(millsStore.mergeQueueActive).toBe(true);
    expect(millsStore.fetchMergeQueue).toHaveBeenCalled();
  });

  it('shows the idle note when the lane is clear', () => {
    millsStore.mergeQueue = { active: [], recent_settled: [], summary: { depth: 0, lanes: {}, enabled: true } };
    flushSync();
    expect(target.querySelector('.press-note')?.textContent).toContain('press idle');
  });

  it('renders an evicted history row with its reason chip and MR link', () => {
    millsStore.archiveRuns = [];
    millsStore.mergeQueue = {
      active: [],
      recent_settled: [pressEntry({
        id: 10,
        state: 'evicted',
        eviction_reason: 'rebase_conflict',
        mr_iid: 1615,
        settled_at: '2026-08-15T12:02:00Z',
      })],
      summary: { depth: 0, lanes: {}, enabled: true },
    };
    flushSync();

    const row = target.querySelector('.press-settled-entry.press-evicted');
    expect(row?.querySelector('.press-reason')?.textContent).toBe('rebase_conflict');
    expect(row?.querySelector<HTMLAnchorElement>('a.bolt-chip')?.href)
      .toBe('https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/1615');
    expect(target.querySelector('.press-note')).toBeNull();
    expect(target.querySelector('.empty-state')).toBeNull();
  });

  it('renders entries with per-lane positions, state labels, and MR links', () => {
    millsStore.mergeQueue = {
      active: [
        pressEntry({ id: 1, state: 'merging', mr_iid: 1612 }),
        pressEntry({ id: 2, state: 'awaiting_pipeline', mr_iid: 1613, backlog_id: 'bl-press-2', attempts: 2 }),
        pressEntry({ id: 3, state: 'queued', mr_iid: 44, project: 'services/other', backlog_id: 'bl-other' }),
      ],
      summary: {
        depth: 3,
        lanes: { 'services/loom-core→main': 2, 'services/other→main': 1 },
        enabled: true,
      },
    };
    flushSync();

    const rows = [...target.querySelectorAll('.press-entry')];
    expect(rows).toHaveLength(3);
    // Positions are per-lane: the third entry is #1 of its own lane.
    expect(rows[0]?.querySelector('.press-pos')?.textContent).toBe('#1');
    expect(rows[1]?.querySelector('.press-pos')?.textContent).toBe('#2');
    expect(rows[2]?.querySelector('.press-pos')?.textContent).toBe('#1');
    // awaiting_pipeline reads as the operator verb, not the wire enum.
    expect(rows[1]?.querySelector('.press-state')?.textContent).toBe('re-proving');
    expect(rows[1]?.textContent).toContain('attempt 2');
    const link = rows[0]?.querySelector<HTMLAnchorElement>('a.bolt-chip');
    expect(link?.href).toBe('https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/1612');
    expect(target.querySelector('.press-mode')?.textContent).toContain('3 in lane');
  });

  it('says so plainly when policy has the serial lane off', () => {
    millsStore.mergeQueue = { active: [], summary: { depth: 0, lanes: {}, enabled: false } };
    flushSync();
    expect(target.querySelector('.press-note')?.textContent).toContain('disabled by policy');
    expect(target.querySelector('.press-mode')?.textContent).toContain('off');
  });

  it('keeps the panel out of the empty state while entries wait in the lane', () => {
    millsStore.archiveRuns = [];
    millsStore.mergeQueue = {
      active: [pressEntry({ id: 9 })],
      summary: { depth: 1, lanes: { 'services/loom-core→main': 1 }, enabled: true },
    };
    flushSync();
    expect(target.querySelector('.press-entry')).not.toBeNull();
    expect(target.querySelector('.empty-state')).toBeNull();
  });
});
