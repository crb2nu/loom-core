import { afterEach, describe, expect, it } from 'vitest';
import { millsStore, type MergeQueueSnapshot } from './mills.svelte.ts';

// Coverage for the press feed (fetchMergeQueue): the serial merge lane's
// open read. Contract mirrors fetchPipelineHistory — errors stay local to
// mergeQueueError (never the panel-wide red), a failed read holds the
// last-good snapshot, and 503 (operator unconfigured) stays quiet because
// fetchAll already surfaces that via `disabled`.

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  millsStore.mergeQueue = null;
  millsStore.mergeQueueError = null;
  millsStore.mergeQueueActive = false;
});

function stubFetch(status: number, body: unknown): void {
  globalThis.fetch = (() =>
    Promise.resolve(
      new Response(body == null ? '' : JSON.stringify(body), {
        status,
        headers: { 'content-type': 'application/json' },
      }),
    )) as typeof fetch;
}

const SNAP: MergeQueueSnapshot = {
  active: [
    {
      id: 7,
      pipeline_run_id: 'PIPE-x',
      backlog_id: 'bl-x',
      project: 'services/loom-core',
      mr_iid: 1612,
      source_branch: 'feat/x',
      target_branch: 'main',
      enqueued_sha: 'aaa',
      current_sha: 'bbb',
      state: 'rebasing',
      attempts: 1,
      enqueued_at: '2026-08-15T12:00:00Z',
      updated_at: '2026-08-15T12:01:00Z',
    },
  ],
  recent_settled: [
    {
      id: 6,
      pipeline_run_id: 'PIPE-old',
      backlog_id: 'bl-old',
      project: 'services/loom-core',
      mr_iid: 1609,
      source_branch: 'feat/old',
      target_branch: 'main',
      enqueued_sha: 'old',
      current_sha: 'old',
      state: 'evicted',
      eviction_reason: 'ci_red',
      attempts: 2,
      enqueued_at: '2026-08-15T10:00:00Z',
      updated_at: '2026-08-15T11:00:00Z',
      settled_at: '2026-08-15T11:00:00Z',
    },
  ],
  summary: { depth: 1, lanes: { 'services/loom-core→main': 1 }, enabled: true },
};

describe('fetchMergeQueue', () => {
  it('stores the snapshot and clears the local error on success', async () => {
    millsStore.mergeQueueError = 'stale failure';
    stubFetch(200, SNAP);
    await millsStore.fetchMergeQueue();

    expect(millsStore.mergeQueue?.summary.depth).toBe(1);
    expect(millsStore.mergeQueue?.active[0]?.mr_iid).toBe(1612);
    expect(millsStore.mergeQueue?.recent_settled?.[0]?.eviction_reason).toBe('ci_red');
    expect(millsStore.mergeQueueError).toBeNull();
  });

  it('accepts an older additive snapshot with recent_settled absent', async () => {
    const { recent_settled: _omitted, ...legacy } = SNAP;
    stubFetch(200, legacy);
    await millsStore.fetchMergeQueue();

    expect(millsStore.mergeQueue?.recent_settled ?? []).toEqual([]);
  });

  it('holds the last-good snapshot and reports locally on a non-503 failure', async () => {
    millsStore.mergeQueue = SNAP;
    stubFetch(500, null);
    await millsStore.fetchMergeQueue();

    expect(millsStore.mergeQueue).toEqual(SNAP); // last known lane retained
    expect(millsStore.mergeQueueError).toContain('500');
  });

  it('goes quiet (null snapshot, no error) when the operator is unconfigured', async () => {
    millsStore.mergeQueue = SNAP;
    stubFetch(503, null);
    await millsStore.fetchMergeQueue();

    expect(millsStore.mergeQueue).toBeNull();
    expect(millsStore.mergeQueueError).toBeNull();
  });
});
