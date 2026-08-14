import { afterEach, describe, expect, it, vi } from 'vitest';
import { engramsStore } from './engrams.svelte.ts';

// Fetch-boundary coverage for GET /api/engrams/summary.
//
// The endpoint's whole reason for carrying `degraded` is that a bridge outage
// and an empty catalog are otherwise the same 200 with all-zero counts. The
// store must therefore distinguish three states that look alike: real zeros,
// degraded zeros, and a HUD build that predates the field (where an absent
// `degraded` is correctly NOT degraded).

function respond(status: number, body: string, contentType = 'application/json'): typeof globalThis.fetch {
  return vi.fn(() =>
    Promise.resolve(new Response(body, { status, headers: { 'Content-Type': contentType } })),
  ) as unknown as typeof globalThis.fetch;
}

const realFetch = globalThis.fetch;

afterEach(() => {
  vi.useRealTimers();
  engramsStore.stopPolling();
  globalThis.fetch = realFetch;
  engramsStore.summary = null;
  engramsStore.error = null;
  engramsStore.unavailable = false;
  engramsStore.catalogUnavailable = false;
  engramsStore.engrams = [];
  engramsStore.graph = null;
});

describe('engramsStore catalog and graph', () => {
  it('passes through typed list/graph shapes and degraded state', async () => {
    globalThis.fetch = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      const body = url.endsWith('/graph')
        ? { nodes: [{ id: 'b', name: 'Bridge', tier: 2, proof_status: 'stale', prerequisites: ['a'], proof: { refs: [] } }], edges: [{ from: 'b', to: 'a' }], degraded: true }
        : { engrams: [{ id: 'a', name: 'Base', tier: 1, proof_status: 'verified', prerequisites: [], proof: { refs: ['proof.md'] } }], degraded: false };
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }) as typeof globalThis.fetch;

    await engramsStore.fetchCatalog();

    expect(engramsStore.engrams[0]?.id).toBe('a');
    expect(engramsStore.graph?.edges).toEqual([{ from: 'b', to: 'a' }]);
    expect(engramsStore.graph?.degraded).toBe(true);
  });

  it('keeps a genuine empty graph distinct from unavailable data', async () => {
    globalThis.fetch = vi.fn(() => Promise.resolve(new Response(JSON.stringify({ engrams: [], nodes: [], edges: [], degraded: false }), { status: 200, headers: { 'Content-Type': 'application/json' } }))) as typeof globalThis.fetch;
    await engramsStore.fetchCatalog();
    expect(engramsStore.unavailable).toBe(false);
    expect(engramsStore.graph).toEqual({ nodes: [], edges: [], degraded: false });
  });

  it('tracks catalog unavailability independently from the summary', async () => {
    globalThis.fetch = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith('/summary')) {
        return Promise.resolve(new Response(JSON.stringify({ total: 1, by_status: {}, by_tier: {}, degraded: false }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
      }
      return Promise.resolve(new Response('', { status: 404 }));
    }) as typeof globalThis.fetch;

    await engramsStore.fetchAll();

    expect(engramsStore.unavailable).toBe(false);
    expect(engramsStore.catalogUnavailable).toBe(true);
    expect(engramsStore.summary?.total).toBe(1);
    expect(engramsStore.graph).toBeNull();
  });

  it('clears catalog unavailability after the endpoints recover', async () => {
    engramsStore.catalogUnavailable = true;
    globalThis.fetch = vi.fn((input: RequestInfo | URL) => {
      const body = String(input).endsWith('/graph')
        ? { nodes: [], edges: [], degraded: false }
        : { engrams: [], degraded: false };
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }) as typeof globalThis.fetch;

    await engramsStore.fetchCatalog();

    expect(engramsStore.catalogUnavailable).toBe(false);
  });

  it('polls all three response shapes and stops cleanly', async () => {
    vi.useFakeTimers();
    globalThis.fetch = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      const body = url.endsWith('/summary') ? { total: 0, by_status: {}, by_tier: {}, degraded: false }
        : url.endsWith('/graph') ? { nodes: [], edges: [], degraded: false }
        : { engrams: [], degraded: false };
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }) as typeof globalThis.fetch;
    engramsStore.startPolling(1000);
    await vi.runOnlyPendingTimersAsync();
    expect(vi.mocked(globalThis.fetch).mock.calls.map(([url]) => String(url))).toEqual(
      expect.arrayContaining(['/api/engrams/summary', '/api/engrams', '/api/engrams/graph']),
    );
    engramsStore.stopPolling();
    const count = vi.mocked(globalThis.fetch).mock.calls.length;
    await vi.advanceTimersByTimeAsync(2000);
    expect(globalThis.fetch).toHaveBeenCalledTimes(count);
  });
});

describe('engramsStore.fetch', () => {
  it('parses the live summary and orders the status/tier chips', async () => {
    globalThis.fetch = respond(
      200,
      JSON.stringify({
        total: 10,
        by_status: { failing: 0, stale: 2, unverified: 8, verified: 0 },
        by_tier: { 'tier:1': 7, 'tier:2': 3, 'tier:3': 0 },
        degraded: false,
      }),
    );

    await engramsStore.fetch();

    expect(engramsStore.degraded).toBe(false);
    expect(engramsStore.unavailable).toBe(false);
    expect(engramsStore.total).toBe(10);
    // Fixed display order, zero buckets retained so the strip keeps its width.
    expect(engramsStore.statusPairs).toEqual([
      ['verified', 0],
      ['unverified', 8],
      ['stale', 2],
      ['failing', 0],
    ]);
    // Tiers sorted numerically, empty tiers dropped.
    expect(engramsStore.tierPairs).toEqual([
      ['tier:1', 7],
      ['tier:2', 3],
    ]);
  });

  it('flags degraded so all-zero counts are not shown as measurements', async () => {
    globalThis.fetch = respond(
      200,
      JSON.stringify({
        total: 0,
        by_status: { unverified: 0, verified: 0, stale: 0, failing: 0 },
        by_tier: {},
        degraded: true,
      }),
    );

    await engramsStore.fetch();

    expect(engramsStore.degraded).toBe(true);
    expect(engramsStore.total).toBe(0);
  });

  it('reads a genuinely empty catalog as real data, not degraded', async () => {
    globalThis.fetch = respond(
      200,
      JSON.stringify({ total: 0, by_status: {}, by_tier: {}, degraded: false }),
    );

    await engramsStore.fetch();

    expect(engramsStore.degraded).toBe(false);
    expect(engramsStore.total).toBe(0);
    expect(engramsStore.tierPairs).toEqual([]);
  });

  it('treats a payload with no degraded field as not degraded', async () => {
    // A HUD older than the flag. Its counts are real; only the field is
    // missing, so inferring degraded from its absence would mute live data.
    globalThis.fetch = respond(
      200,
      JSON.stringify({ total: 4, by_status: { verified: 4 }, by_tier: { 'tier:1': 4 } }),
    );

    await engramsStore.fetch();

    expect(engramsStore.degraded).toBe(false);
    expect(engramsStore.total).toBe(4);
  });

  it('flags endpoint-absent on a 200 index.html (SPA catch-all)', async () => {
    globalThis.fetch = respond(200, '<!doctype html><html></html>', 'text/html');

    await engramsStore.fetch();

    expect(engramsStore.unavailable).toBe(true);
    expect(engramsStore.summary).toBeNull();
    expect(engramsStore.error).toBeNull();
    // The getters must stay safe with no summary loaded.
    expect(engramsStore.total).toBe(0);
    expect(engramsStore.statusPairs.map(([s]) => s)).toEqual([
      'verified',
      'unverified',
      'stale',
      'failing',
    ]);
  });

  it('surfaces a hard error', async () => {
    globalThis.fetch = respond(502, JSON.stringify({ error: 'engram summary' }));

    await engramsStore.fetch();

    expect(engramsStore.error).toContain('engram summary');
    expect(engramsStore.unavailable).toBe(false);
  });
});
