import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { spawnStore } from './spawn.svelte.ts';
import { runInTrackingEffect } from '../test-utils/tracking.svelte.ts';

// Regression: startPolling() must not re-trigger the $effect that called it.
//
// Panels start spawn polling from their mount $effect. startPolling used to
// read config/configLoading synchronously to decide whether to fetch the
// spawn config — the very fields fetchConfig writes on completion. When
// /api/agent/spawn/config errors, config stays null, so every completion
// re-ran the effect and refired fetchConfig: an infinite refetch loop at
// network round-trip cadence (the mills_staff class, MR !1474).
//
// A `.dom.test.ts` on purpose: the node project resolves Svelte's SSR build,
// where effects never run and the test would pass vacuously.

// Answer the first few rounds, then hang. Under the regression the loop is
// microtask-tight with instant responses — unbounded, it OOMs the worker
// before the assertion can report. Starving it after a small multiple of the
// legitimate request count keeps the failure a clean count mismatch.
function countingFetch(): ReturnType<typeof vi.fn> {
  const mock = vi.fn((input: RequestInfo | URL): Promise<Response> => {
    if (mock.mock.calls.length > 10) {
      return new Promise<Response>(() => {});
    }
    const url = String(input);
    if (url.includes('/api/agent/spawn/config')) {
      // The looping case: config fetch fails, so config stays null.
      return Promise.resolve(new Response('boom', { status: 500 }));
    }
    return Promise.resolve(
      new Response('{"spawns":[]}', {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
  return mock;
}

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  spawnStore.stopPolling();
  spawnStore.config = null;
  spawnStore.configLoading = false;
  spawnStore.configError = null;
  spawnStore.spawns = [];
  spawnStore.loading = false;
  spawnStore.error = null;
  spawnStore.lastUpdated = null;
});

describe('spawnStore.startPolling inside a tracking effect', () => {
  it('fetches config and spawns exactly once — completion must not re-run the effect', async () => {
    const fetchMock = countingFetch();
    globalThis.fetch = fetchMock as unknown as typeof globalThis.fetch;

    const stop = runInTrackingEffect(() => {
      spawnStore.startPolling(60000);
    });

    // Let several rounds of completions + effect flushes settle. Under the
    // loop, every failed config round adds another config + spawns pair.
    for (let i = 0; i < 6; i++) {
      await new Promise((r) => setTimeout(r, 0));
      flushSync();
    }
    stop();

    // One config fetch + one spawns fetch.
    expect(fetchMock.mock.calls.length).toBe(2);
  });
});
