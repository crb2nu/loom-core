import { afterEach, describe, expect, it } from 'vitest';
import { millsStore, type BacklogItem, type PipelineRun } from './mills.svelte.ts';

function item(over: Partial<BacklogItem> & { ID: string; State: string }): BacklogItem {
  return { Title: over.ID, Priority: 'P2', ...over } as BacklogItem;
}

function run(over: Partial<PipelineRun> & { ID: string; BacklogID: string; State: string }): PipelineRun {
  return { ...over } as PipelineRun;
}

afterEach(() => {
  millsStore.backlog = [];
  millsStore.pipelineRuns = [];
  millsStore.archiveRuns = [];
});

describe('openSparks', () => {
  it('collapses escalated runs to one per backlog item and drops items no longer escalated', () => {
    millsStore.backlog = [
      item({ ID: 'still-escalated', State: 'escalated' }),
      item({ ID: 'held', State: 'paused' }),
      item({ ID: 'since-merged', State: 'merged' }),
      item({ ID: 'since-retired', State: 'retired' }),
    ];
    millsStore.pipelineRuns = [run({ ID: 'r1', BacklogID: 'still-escalated', State: 'escalated' })];
    millsStore.archiveRuns = [
      run({ ID: 'r2', BacklogID: 'still-escalated', State: 'escalated' }), // second attempt of the same item
      run({ ID: 'r3', BacklogID: 'held', State: 'paused' }),
      run({ ID: 'r4', BacklogID: 'since-merged', State: 'escalated' }), // history: merged since
      run({ ID: 'r5', BacklogID: 'since-merged', State: 'escalated' }),
      run({ ID: 'r6', BacklogID: 'since-retired', State: 'escalated' }),
    ];

    // The history view still sees every escalated run …
    expect(millsStore.escalatedRuns.length).toBe(6);
    // … but the Deck strip counts items that still need a human.
    expect(millsStore.openSparks.map((r) => r.BacklogID)).toEqual(['still-escalated', 'held']);
    // Active run wins over the archived attempt for the same item.
    expect(millsStore.openSparks[0].ID).toBe('r1');
  });

  it('keeps runs whose item is not in the loaded backlog', () => {
    millsStore.backlog = [];
    millsStore.archiveRuns = [
      run({ ID: 'r1', BacklogID: 'unknown-a', State: 'escalated' }),
      run({ ID: 'r2', BacklogID: 'unknown-a', State: 'escalated' }),
      run({ ID: 'r3', BacklogID: 'unknown-b', State: 'escalated' }),
    ];
    expect(millsStore.openSparks.map((r) => r.ID)).toEqual(['r1', 'r3']);
  });
});
