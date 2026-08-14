import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { catalogStore } from './catalog.svelte.ts';
import { graphStore } from './graph.svelte.ts';
import { knowledgeStore } from './knowledge.svelte.ts';
import { memoryStore } from './memory.svelte.ts';
import { patternsStore } from './patterns.svelte.ts';
import { streamStore } from './stream.svelte.ts';
import { presenceDiagnosticsStore } from './presenceDiagnostics.svelte.ts';
import { runInTrackingEffect } from '../test-utils/tracking.svelte.ts';

// Regression: fetch entry points reachable from panel mount $effects must not
// read their own filter/gate $state in the synchronous pre-await slice.
//
// A tracked filter read couples the mount effect to every filter write: each
// keystroke or toggle re-runs the effect, refiring startPolling/fetch — a
// duplicate fetch plus a poller restart per input event (and, when the method
// also writes what it read, the full mills_staff infinite loop, MR !1474).
// Explicit refetch-on-filter-change lives in the stores' setter methods.
//
// A `.dom.test.ts` on purpose: the node project resolves Svelte's SSR build,
// where effects never run and the test would pass vacuously.

// Answer the first few rounds, then hang, so a regression fails as a clean
// count mismatch instead of OOMing the worker.
function countingFetch(body = '{}'): ReturnType<typeof vi.fn> {
  const mock = vi.fn((): Promise<Response> => {
    if (mock.mock.calls.length > 12) {
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

function useMock(body?: string): ReturnType<typeof vi.fn> {
  const mock = countingFetch(body);
  globalThis.fetch = mock as unknown as typeof globalThis.fetch;
  return mock;
}

afterEach(() => {
  globalThis.fetch = realFetch;
  catalogStore.stopPolling();
  catalogStore.searchQuery = '';
  catalogStore.categoryFilter = 'all';
  catalogStore.statusFilter = 'all';
  graphStore.stopPolling();
  graphStore.searchQuery = '';
  graphStore.filterType = 'all';
  knowledgeStore.stopPolling();
  knowledgeStore.searchQuery = '';
  knowledgeStore.filterCategory = 'all';
  memoryStore.stopPolling();
  memoryStore.searchQuery = '';
  memoryStore.filterTier = 'all';
  patternsStore.statusFilter = 'approved';
  streamStore.stopPolling();
  streamStore.paused = false;
  presenceDiagnosticsStore.diagnosticsAgentId = '';
});

describe('filter writes must not re-run the tracking effect that started the fetch', () => {
  it('catalog: a search-query write adds no fetch', async () => {
    const mock = useMock();
    const stop = runInTrackingEffect(() => {
      catalogStore.startPolling(30000);
    });
    await settle();
    const baseline = mock.mock.calls.length;
    expect(baseline).toBeGreaterThan(0);

    catalogStore.searchQuery = 'zz';
    await settle();
    stop();

    expect(mock.mock.calls.length).toBe(baseline);
  });

  it('graph: a filter-type write adds no fetch', async () => {
    const mock = useMock();
    const stop = runInTrackingEffect(() => {
      graphStore.startPolling(15000);
    });
    await settle();
    const baseline = mock.mock.calls.length;
    expect(baseline).toBeGreaterThan(0);

    graphStore.filterType = 'service';
    await settle();
    stop();

    expect(mock.mock.calls.length).toBe(baseline);
  });

  it('knowledge: a category write adds no fetch', async () => {
    const mock = useMock();
    const stop = runInTrackingEffect(() => {
      knowledgeStore.startPolling(30000);
    });
    await settle();
    const baseline = mock.mock.calls.length;
    expect(baseline).toBeGreaterThan(0);

    knowledgeStore.filterCategory = 'decision';
    await settle();
    stop();

    expect(mock.mock.calls.length).toBe(baseline);
  });

  it('memory: a tier write adds no fetch', async () => {
    const mock = useMock();
    const stop = runInTrackingEffect(() => {
      memoryStore.startPolling(60000);
    });
    await settle();
    const baseline = mock.mock.calls.length;
    expect(baseline).toBeGreaterThan(0);

    memoryStore.filterTier = 'working';
    await settle();
    stop();

    expect(mock.mock.calls.length).toBe(baseline);
  });

  it('patterns: a status-filter write adds no fetch', async () => {
    const mock = useMock();
    const stop = runInTrackingEffect(() => {
      void patternsStore.fetch();
    });
    await settle();
    const baseline = mock.mock.calls.length;
    expect(baseline).toBeGreaterThan(0);

    patternsStore.statusFilter = 'candidate';
    await settle();
    stop();

    expect(mock.mock.calls.length).toBe(baseline);
  });

  it('stream: a pause round-trip adds no fetch', async () => {
    const mock = useMock('{"entries":[]}');
    const stop = runInTrackingEffect(() => {
      streamStore.startPolling(60000);
    });
    await settle();
    const baseline = mock.mock.calls.length;
    expect(baseline).toBeGreaterThan(0);

    streamStore.paused = true;
    await settle();
    streamStore.paused = false;
    await settle();
    stop();

    expect(mock.mock.calls.length).toBe(baseline);
  });

  it('presence diagnostics: an agent-id write adds no fetch', async () => {
    const mock = useMock();
    presenceDiagnosticsStore.diagnosticsAgentId = 'agent-1';
    const stop = runInTrackingEffect(() => {
      void presenceDiagnosticsStore.fetchDiagnostics();
    });
    await settle();
    const baseline = mock.mock.calls.length;
    expect(baseline).toBeGreaterThan(0);

    presenceDiagnosticsStore.diagnosticsAgentId = 'agent-2';
    await settle();
    stop();

    expect(mock.mock.calls.length).toBe(baseline);
  });
});
