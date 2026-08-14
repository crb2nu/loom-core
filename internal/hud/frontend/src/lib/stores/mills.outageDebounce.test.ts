import { afterEach, describe, expect, it } from 'vitest';
import { millsStore, type PipelineRun } from './mills.svelte.ts';

// Regression coverage for the Deck's "502 gremlins": the operator redeploys
// with strategy Recreate on every merged MR, so a ~30–60s unreachable window
// is ROUTINE. fetchAll used to be all-or-nothing (Promise.all): one 502 on
// any of its seven endpoints red-flagged every Mills surface for one tick,
// then flapped back. The store now:
//   - keeps fresh slices and stale slices independently (allSettled),
//   - reports partial failures via `degraded` (not `error`),
//   - sets `reconnecting` on the FIRST fully-failed tick,
//   - sets `error` (the red state seven panels render) only after TWO
//     consecutive fully-failed ticks.

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  millsStore.error = null;
  millsStore.reconnecting = false;
  millsStore.degraded = null;
  millsStore.disabled = false;
  millsStore.pipelineRuns = [];
  millsStore.backlog = [];
  millsStore.status = null;
  // Reset the private consecutive-failure counter via a clean success tick.
  stubFetch(() => ok([]));
  return millsStore.fetchAll().finally(() => {
    globalThis.fetch = realFetch;
  });
});

function ok(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

function fail(status: number): Response {
  return new Response('upstream unavailable', { status });
}

function stubFetch(route: (path: string) => Response): void {
  globalThis.fetch = ((input: RequestInfo | URL) => {
    const path = String(input);
    return Promise.resolve(route(path));
  }) as typeof fetch;
}

const RUN = { ID: 'run-1', BacklogID: 'item-1', State: 'running', Attempts: 1 } as PipelineRun;

describe('mills outage debounce', () => {
  it('first fully-failed tick sets reconnecting, keeps data, and stays un-red', async () => {
    millsStore.pipelineRuns = [RUN];
    stubFetch(() => fail(502));
    await millsStore.fetchAll();

    expect(millsStore.reconnecting).toBe(true);
    expect(millsStore.error).toBeNull();
    expect(millsStore.pipelineRuns).toEqual([RUN]); // last known state retained
  });

  it('a second consecutive fully-failed tick is a real outage → error', async () => {
    stubFetch(() => fail(502));
    await millsStore.fetchAll();
    await millsStore.fetchAll();

    expect(millsStore.error).not.toBeNull();
    expect(millsStore.reconnecting).toBe(false);
  });

  it('a partial failure degrades the named sources without redding the surface', async () => {
    stubFetch((path) => (path.includes('/pipeline/runs') ? fail(502) : ok([])));
    millsStore.pipelineRuns = [RUN];
    await millsStore.fetchAll();

    expect(millsStore.error).toBeNull();
    expect(millsStore.reconnecting).toBe(false);
    expect(millsStore.degraded).toContain('pipelines');
    expect(millsStore.pipelineRuns).toEqual([RUN]); // stale slice kept
    expect(millsStore.backlog).toEqual([]); // fresh slice applied
  });

  it('a healthy tick clears reconnecting, degraded, and the failure streak', async () => {
    stubFetch(() => fail(502));
    await millsStore.fetchAll();
    expect(millsStore.reconnecting).toBe(true);

    stubFetch(() => ok([]));
    await millsStore.fetchAll();
    expect(millsStore.reconnecting).toBe(false);
    expect(millsStore.degraded).toBeNull();
    expect(millsStore.error).toBeNull();

    // The streak reset means a later single failure is again only reconnecting.
    stubFetch(() => fail(502));
    await millsStore.fetchAll();
    expect(millsStore.error).toBeNull();
    expect(millsStore.reconnecting).toBe(true);
  });

  it('proxy 503 still means "operator not configured", never an outage', async () => {
    stubFetch(() => fail(503));
    await millsStore.fetchAll();

    expect(millsStore.disabled).toBe(true);
    expect(millsStore.error).toBeNull();
    expect(millsStore.reconnecting).toBe(false);
  });
});
