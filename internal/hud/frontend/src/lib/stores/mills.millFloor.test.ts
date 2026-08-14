import { afterEach, describe, expect, it } from 'vitest';
import { millsStore, type BacklogItem, type PipelineRun } from './mills.svelte.ts';

// Coverage for the two mill-floor tallies that are rendered in more than one
// place at once: the Warps/Shuttles panel headers, the mills sub-tab badges,
// and the spine ribbon on every mill-floor view all read these getters. If a
// consumer ever recomputes its own version, the screen shows two different
// numbers for the same thing — which is the bug these getters exist to kill.

function item(over: Partial<BacklogItem>): BacklogItem {
  return { ID: 'b-1', Title: 'x', State: 'queued', Priority: 'P2', ...over } as BacklogItem;
}

function run(over: Partial<PipelineRun>): PipelineRun {
  return {
    ID: 'r-1',
    BacklogID: 'b-1',
    Template: 'implement',
    State: 'running',
    Attempts: 1,
    ...over,
  } as PipelineRun;
}

afterEach(() => {
  millsStore.backlog = [];
  millsStore.pipelineRuns = [];
  millsStore.archiveRuns = [];
});

describe('strungCount', () => {
  it('counts only what the warp bands render — queued + paused, any priority', () => {
    millsStore.backlog = [
      item({ ID: 'a', State: 'queued', Priority: 'P0' }),
      item({ ID: 'b', State: 'paused', Priority: 'P3' }),
      item({ ID: 'c', State: 'queued', Priority: 'weird' }), // lands in `other`
      item({ ID: 'd', State: 'merged', Priority: 'P1' }), // off the beam
      item({ ID: 'e', State: 'running', Priority: 'P1' }), // has a shuttle
    ];
    expect(millsStore.strungCount).toBe(3);
  });

  it('is zero for an all-merged backlog (a bare beam, not 5 warps)', () => {
    millsStore.backlog = [item({ ID: 'a', State: 'merged' }), item({ ID: 'b', State: 'done' })];
    expect(millsStore.strungCount).toBe(0);
  });

  it('always equals the sum of the rendered buckets', () => {
    millsStore.backlog = [
      item({ ID: 'a', State: 'queued', Priority: 'P0' }),
      item({ ID: 'b', State: 'queued', Priority: 'P0' }),
      item({ ID: 'c', State: 'paused', Priority: 'P2' }),
    ];
    const summed = Object.values(millsStore.backlogByPriority).reduce((n, b) => n + b.length, 0);
    expect(millsStore.strungCount).toBe(summed);
  });
});

describe('activeShuttleCount', () => {
  it('excludes struck, held, and terminal runs', () => {
    millsStore.pipelineRuns = [
      run({ ID: '1', State: 'running' }),
      run({ ID: '2', State: 'planning' }),
      run({ ID: '3', State: 'escalated' }),
      run({ ID: '4', State: 'paused' }),
      run({ ID: '5', State: 'MERGED' }), // case-insensitive
      run({ ID: '6', State: 'done' }),
    ];
    expect(millsStore.activeShuttleCount).toBe(2);
  });

  it('feeds the spine shuttle node from the same number', () => {
    millsStore.pipelineRuns = [run({ ID: '1' }), run({ ID: '2' }), run({ ID: '3', State: 'paused' })];
    const shuttle = millsStore.millFloorSpine.find((s) => s.kind === 'shuttle') as {
      count: number;
      active: boolean;
    };
    expect(shuttle.count).toBe(millsStore.activeShuttleCount);
    expect(shuttle.count).toBe(2);
    expect(shuttle.active).toBe(true);
  });
});

describe('millFloorSpine bolt/spark tallies', () => {
  it('reports the archive the Sparks and Bolts views read, not zero', () => {
    millsStore.pipelineRuns = [run({ ID: 'live', State: 'escalated' })];
    millsStore.archiveRuns = [
      run({ ID: 'm1', State: 'merged' }),
      run({ ID: 'm2', State: 'done' }),
      run({ ID: 'e1', State: 'escalated' }),
    ];
    const bolt = millsStore.millFloorSpine.find((s) => s.kind === 'bolt') as { count: number };
    const spark = millsStore.millFloorSpine.find((s) => s.kind === 'spark') as { count: number };
    expect(bolt.count).toBe(millsStore.boltRuns.length);
    expect(bolt.count).toBe(2);
    expect(spark.count).toBe(millsStore.escalatedRuns.length);
    expect(spark.count).toBe(2); // one live + one archived, de-duped by id
  });
});
