import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { sandboxStore, type SandboxSummary } from './sandbox.svelte.ts';
import { labsAuthStore } from './labsAuth.svelte.ts';
import { runInTrackingEffect } from '../test-utils/tracking.svelte.ts';

// Regression: fetchAllProjectStatuses() must not re-trigger the $effect that
// called it.
//
// SandboxLive refreshes per-project statuses from a mount $effect. Each
// fetchProjectStatus used to read projectStatusLoading synchronously and
// immediately rewrite it with a new Set identity — a tracked read plus a
// synchronous write of the same state re-runs the effect unboundedly, and
// Svelte kills the whole effect tree with effect_update_depth_exceeded
// (the mills_staff pre-await-read class, MR !1474).
//
// A `.dom.test.ts` on purpose: the node project resolves Svelte's SSR build,
// where effects never run and the test would pass vacuously.

// Answer the first few rounds, then hang, so a regression fails as a clean
// count mismatch (or a depth error) instead of OOMing the worker.
function countingFetch(): ReturnType<typeof vi.fn> {
  const mock = vi.fn((): Promise<Response> => {
    if (mock.mock.calls.length > 10) {
      return new Promise<Response>(() => {});
    }
    return Promise.resolve(
      new Response('{"sandboxes":[]}', {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
  return mock;
}

const summary: SandboxSummary = {
  available: true,
  total_sandboxes: 2,
  running: 2,
  paused: 0,
  total_execs: 0,
  total_builds: 0,
  uptime_seconds: 0,
  projects: ['p1', 'p2'],
};

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  labsAuthStore.adminToken = '';
  sandboxStore.summary = null;
  sandboxStore.projectStatus = new Map();
  sandboxStore.projectStatusLoading = new Set();
});

describe('sandboxStore.fetchAllProjectStatuses inside a tracking effect', () => {
  it('fetches each project status exactly once', async () => {
    const fetchMock = countingFetch();
    globalThis.fetch = fetchMock as unknown as typeof globalThis.fetch;
    labsAuthStore.adminToken = 'test-token';
    sandboxStore.summary = summary;

    const stop = runInTrackingEffect(() => {
      void sandboxStore.fetchAllProjectStatuses();
    });

    for (let i = 0; i < 6; i++) {
      await new Promise((r) => setTimeout(r, 0));
      flushSync();
    }
    stop();

    // One status fetch per project.
    expect(fetchMock.mock.calls.length).toBe(2);
  });
});
