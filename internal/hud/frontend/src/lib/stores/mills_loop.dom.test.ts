import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { millsStore } from './mills.svelte.ts';
import { runInTrackingEffect } from '../test-utils/tracking.svelte.ts';

// Regression: the drilldown/lazy-load entry points must not re-trigger the
// $effect that called them.
//
// InspectDock opens the pipeline drawer from a selection $effect, WarpsPanel
// opens the backlog drawer from a router-sync $effect, and BacklogDetail
// lazily loads workflow runs from its open-drawer $effect. Each entry point
// used to read the very cache it writes on fetch completion — every finished
// round re-ran the caller's effect, an infinite refetch loop at network
// round-trip cadence (the mills_staff class, MR !1474).
//
// A `.dom.test.ts` on purpose: the node project resolves Svelte's SSR build,
// where effects never run and the test would pass vacuously.

// Answer the first few rounds, then hang. Under the regression the loop is
// microtask-tight with instant responses — starving it after a small
// multiple of the legitimate count keeps the failure a clean count mismatch.
function countingFetch(body = '{}'): ReturnType<typeof vi.fn> {
  const mock = vi.fn((): Promise<Response> => {
    if (mock.mock.calls.length > 10) {
      return new Promise<Response>(() => {});
    }
    return Promise.resolve(
      new Response(body, { status: 200, headers: { 'Content-Type': 'application/json' } }),
    );
  });
  return mock;
}

async function settle(): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await new Promise((r) => setTimeout(r, 0));
    flushSync();
  }
}

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  millsStore.closeRunDetail();
  millsStore.closeBacklogDetail();
  millsStore.pipelineDetailByRun = {};
  millsStore.backlogDetailByID = {};
  millsStore.workflowRuns = [];
  millsStore.workflowLoading = false;
  millsStore.workflowError = null;
  millsStore.disabled = false;
  millsStore.wiring = null;
  millsStore.wiringUnavailable = false;
  millsStore.wiringError = null;
  millsStore.wiringLoading = false;
});

describe('millsStore drilldown entry points inside a tracking effect', () => {
  it('openRunDetail fetches the detail exactly once', async () => {
    const fetchMock = countingFetch();
    globalThis.fetch = fetchMock as unknown as typeof globalThis.fetch;

    const stop = runInTrackingEffect(() => {
      millsStore.openRunDetail('r-loop');
    });
    await settle();
    stop();

    expect(fetchMock.mock.calls.length).toBe(1);
  });

  it('openBacklogDetail fetches the detail exactly once', async () => {
    const fetchMock = countingFetch();
    globalThis.fetch = fetchMock as unknown as typeof globalThis.fetch;

    const stop = runInTrackingEffect(() => {
      millsStore.openBacklogDetail('b-loop');
    });
    await settle();
    stop();

    expect(fetchMock.mock.calls.length).toBe(1);
  });

  it('ensureWorkflowRunsLoaded with zero runs fetches exactly once', async () => {
    // The looping case: the mill has no workflow runs, so every completion
    // used to write a fresh empty array and re-arm the length===0 guard.
    const fetchMock = countingFetch('{"runs":[]}');
    globalThis.fetch = fetchMock as unknown as typeof globalThis.fetch;

    const stop = runInTrackingEffect(() => {
      millsStore.ensureWorkflowRunsLoaded();
    });
    await settle();
    stop();

    expect(fetchMock.mock.calls.length).toBe(1);
  });

  it('fetchWiring does not couple the caller effect to `disabled` transitions', async () => {
    const fetchMock = countingFetch();
    globalThis.fetch = fetchMock as unknown as typeof globalThis.fetch;

    const stop = runInTrackingEffect(() => {
      void millsStore.fetchWiring();
    });
    await settle();
    expect(fetchMock.mock.calls.length).toBe(1);

    // An operator outage flips `disabled` true and back; a tracked read in
    // fetchWiring would re-run the effect on each transition and refetch.
    millsStore.disabled = true;
    await settle();
    millsStore.disabled = false;
    await settle();
    stop();

    expect(fetchMock.mock.calls.length).toBe(1);
  });
});
