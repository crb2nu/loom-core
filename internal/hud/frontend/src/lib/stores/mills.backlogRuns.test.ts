import { afterEach, describe, expect, it } from 'vitest';
import { millsStore, type PipelineRun } from './mills.svelte.ts';

// Coverage for the drawer's run cross-link surviving the poll windows: the
// server-fetched per-item list (backlogRunsByID) merges with the active +
// terminal windows, and a response from an OLDER operator — which ignores
// unknown query params and serves the unfiltered active union — must be
// stripped to the item's own rows instead of rendered.

function run(over: Partial<PipelineRun>): PipelineRun {
  return {
    ID: 'r-1',
    BacklogID: 'bl-x',
    Template: 'default',
    State: 'escalated',
    Attempts: 1,
    ...over,
  } as PipelineRun;
}

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  millsStore.selectedBacklogID = null;
  millsStore.backlogRunsByID = {};
  millsStore.backlogDetailByID = {};
  millsStore.backlogEventsByID = {};
  millsStore.pipelineRuns = [];
  millsStore.pipelineHistory = [];
});

describe('pipelineRunsForBacklog', () => {
  it('merges the server per-item list with the poll windows, deduped newest-first', () => {
    // An old escalated run only the server list still remembers…
    millsStore.backlogRunsByID = {
      'bl-x': [
        run({ ID: 'r-old', StartedAt: '2026-08-01T00:00:00Z' }),
        // …plus a row the active window also carries (dedupe target).
        run({ ID: 'r-live', State: 'testing', StartedAt: '2026-08-29T00:00:00Z' }),
      ],
    };
    millsStore.pipelineRuns = [
      run({ ID: 'r-live', State: 'testing', StartedAt: '2026-08-29T00:00:00Z' }),
    ];
    millsStore.pipelineHistory = [
      run({ ID: 'r-mid', State: 'done', StartedAt: '2026-08-20T00:00:00Z' }),
      run({ ID: 'other', BacklogID: 'bl-other', StartedAt: '2026-08-28T00:00:00Z' }),
    ];

    const ids = millsStore.pipelineRunsForBacklog('bl-x').map((r) => r.ID);
    expect(ids).toEqual(['r-live', 'r-mid', 'r-old']);
  });

  it('still works from the poll windows alone when the server list is absent', () => {
    millsStore.pipelineHistory = [run({ ID: 'r-h', StartedAt: '2026-08-20T00:00:00Z' })];
    expect(millsStore.pipelineRunsForBacklog('bl-x').map((r) => r.ID)).toEqual(['r-h']);
  });
});

describe('openBacklogDetail run fetch', () => {
  it("strips rows for other items — an older operator ignores backlog_id and serves the active union", async () => {
    const union = [
      run({ ID: 'r-mine', BacklogID: 'bl-x' }),
      run({ ID: 'r-not-mine', BacklogID: 'bl-unrelated' }),
    ];
    globalThis.fetch = ((input: RequestInfo | URL) => {
      const url = String(input);
      let body: unknown = {};
      if (url.includes('/api/mills/pipeline/runs?backlog_id=')) body = union;
      else if (url.includes('/events')) body = { backlog_id: 'bl-x', partial: true, events: [] };
      else if (url.includes('/api/mills/backlog/'))
        body = { ID: 'bl-x', Title: 't', State: 'escalated', Priority: 'P2' };
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
      );
    }) as typeof globalThis.fetch;

    millsStore.openBacklogDetail('bl-x');
    // Let the three drawer fetches settle.
    await new Promise((r) => setTimeout(r, 0));
    await new Promise((r) => setTimeout(r, 0));

    expect((millsStore.backlogRunsByID['bl-x'] ?? []).map((r) => r.ID)).toEqual(['r-mine']);
  });
});
