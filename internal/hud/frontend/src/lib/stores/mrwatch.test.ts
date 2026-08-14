import { afterEach, describe, expect, it, vi } from 'vitest';
import { mrwatchStore } from './mrwatch.svelte.ts';

// Fetch-boundary coverage for the mrwatch registry + shepherd audit log.
//
// The registry snapshot and the shepherd's actions are independent surfaces
// behind two endpoints: the shepherd can be disabled while the poller runs, so
// an absent /api/mrwatch/actions must leave the MR table rendered. And the
// audit log is served newest-LAST (a bounded ring the shepherd appends to),
// while a feed reads newest-first.

function jsonResponse(status: number, body: string, contentType = 'application/json'): Response {
  return new Response(body, { status, headers: { 'Content-Type': contentType } });
}

function routeFetch(routes: Record<string, () => Response>): typeof globalThis.fetch {
  return vi.fn((input: RequestInfo | URL) => {
    const url = String(input);
    for (const [fragment, make] of Object.entries(routes)) {
      if (url.includes(fragment)) return Promise.resolve(make());
    }
    return Promise.resolve(jsonResponse(404, 'not found', 'text/plain'));
  }) as unknown as typeof globalThis.fetch;
}

const SUMMARY_BODY = JSON.stringify({
  merge_requests: [
    {
      repo: 'services/loom-core',
      iid: 1234,
      title: 'feat: repair pipeline',
      source_branch: 'feat/pipeline',
      target_branch: 'main',
      state: 'ci_failed_flaky',
      reason: 'pipeline failed on a known-flaky job',
      web_url: 'https://gitlab/x/-/merge_requests/1234',
      pipeline_status: 'failed',
      pipeline_id: 99,
      sha: 'deadbeef',
      last_transition_at: '2026-07-25T09:00:00Z',
      stale: false,
    },
    {
      repo: 'services/loom-core',
      iid: 1235,
      title: 'chore: retained landed MR',
      source_branch: 'feat/landed',
      state: 'merged',
      merged: true,
      merged_at: '2026-07-25T09:29:00Z',
      last_transition_at: '2026-07-25T09:30:00Z',
      stale: true,
    },
  ],
  counts: { ok: 0, ci_failed_flaky: 1, merged: 1, closed: 0 },
  last_poll_at: '2026-07-25T09:31:00Z',
  stale: true,
  projects: ['services/loom-core'],
});

const ACTIONS_BODY = JSON.stringify({
  actions: [
    {
      time: '2026-07-25T09:00:00Z',
      repo: 'services/loom-core',
      mr_iid: 1234,
      branch: 'feat/blocked',
      state: 'flaky',
      action: 'retry_pipeline',
      outcome: 'ok',
      detail: 'retried pipeline 99',
    },
    {
      time: '2026-07-25T09:20:00Z',
      repo: 'services/loom-core',
      mr_iid: 1235,
      state: 'healthy',
      action: 'arm_auto_merge',
      outcome: 'skipped',
      detail: 'MR younger than 30m',
    },
  ],
  count: 2,
  shepherd_enabled: false,
});

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  mrwatchStore.mergeRequests = [];
  mrwatchStore.actions = [];
  mrwatchStore.counts = {};
  mrwatchStore.error = null;
  mrwatchStore.unavailable = false;
  mrwatchStore.stale = false;
  mrwatchStore.shepherdEnabled = false;
});

describe('mrwatchStore.fetch', () => {
  it('parses the registry snapshot and the action log', async () => {
    globalThis.fetch = routeFetch({
      '/api/mrwatch/summary': () => jsonResponse(200, SUMMARY_BODY),
      '/api/mrwatch/actions': () => jsonResponse(200, ACTIONS_BODY),
    });

    await mrwatchStore.fetch();

    expect(mrwatchStore.unavailable).toBe(false);
    expect(mrwatchStore.mergeRequests).toHaveLength(2);
    expect(mrwatchStore.projects).toEqual(['services/loom-core']);
    expect(mrwatchStore.stale).toBe(true);
    expect(mrwatchStore.lastPollAt).toBe('2026-07-25T09:31:00Z');
    // Only non-zero buckets chip up, largest first.
    expect(mrwatchStore.countPairs).toEqual([
      ['ci_failed_flaky', 1],
      ['merged', 1],
    ]);
    expect(mrwatchStore.unhealthyCount).toBe(1);
    expect(mrwatchStore.liveMergeRequests).toHaveLength(1);
    expect(mrwatchStore.shepherdEnabled).toBe(false);
  });

  it('reverses the audit ring so the feed reads newest-first', async () => {
    globalThis.fetch = routeFetch({
      '/api/mrwatch/summary': () => jsonResponse(200, SUMMARY_BODY),
      '/api/mrwatch/actions': () => jsonResponse(200, ACTIONS_BODY),
    });

    await mrwatchStore.fetch();

    expect(mrwatchStore.actions[0].action).toBe('retry_pipeline');
    expect(mrwatchStore.recentActions[0].action).toBe('arm_auto_merge');
    // The getter must not mutate the stored order.
    expect(mrwatchStore.actions[0].action).toBe('retry_pipeline');
  });

  it('renders the registry even when the shepherd log is absent', async () => {
    globalThis.fetch = routeFetch({
      '/api/mrwatch/summary': () => jsonResponse(200, SUMMARY_BODY),
      '/api/mrwatch/actions': () => jsonResponse(404, 'not found', 'text/plain'),
    });

    await mrwatchStore.fetch();

    expect(mrwatchStore.mergeRequests).toHaveLength(2);
    expect(mrwatchStore.actions).toEqual([]);
    expect(mrwatchStore.unavailable).toBe(false);
  });

  it('flags endpoint-absent when the summary route is the SPA catch-all', async () => {
    globalThis.fetch = routeFetch({
      '/api/mrwatch/': () => jsonResponse(200, '<!doctype html><html></html>', 'text/html'),
    });

    await mrwatchStore.fetch();

    expect(mrwatchStore.unavailable).toBe(true);
    expect(mrwatchStore.error).toBeNull();
    expect(mrwatchStore.mergeRequests).toEqual([]);
  });

  it('coerces null arrays and maps so panels never spread a Go nil', async () => {
    globalThis.fetch = routeFetch({
      '/api/mrwatch/summary': () =>
        jsonResponse(
          200,
          JSON.stringify({
            merge_requests: null,
            counts: null,
            projects: null,
            last_poll_at: '2026-07-25T09:31:00Z',
            stale: false,
          }),
        ),
      '/api/mrwatch/actions': () => jsonResponse(200, JSON.stringify({ actions: null, count: 0 })),
    });

    await mrwatchStore.fetch();

    expect(mrwatchStore.mergeRequests).toEqual([]);
    expect(mrwatchStore.counts).toEqual({});
    expect(mrwatchStore.projects).toEqual([]);
    expect(mrwatchStore.actions).toEqual([]);
    expect(mrwatchStore.countPairs).toEqual([]);
  });

  it('surfaces a hard error without blanking the last snapshot', async () => {
    globalThis.fetch = routeFetch({
      '/api/mrwatch/summary': () => jsonResponse(200, SUMMARY_BODY),
      '/api/mrwatch/actions': () => jsonResponse(200, ACTIONS_BODY),
    });
    await mrwatchStore.fetch();

    globalThis.fetch = routeFetch({
      '/api/mrwatch/': () => jsonResponse(502, 'bad gateway', 'text/plain'),
    });
    await mrwatchStore.fetch();

    expect(mrwatchStore.error).toContain('502');
    expect(mrwatchStore.mergeRequests).toHaveLength(2);
  });
});
