import { afterEach, describe, expect, it } from 'vitest';
import { engramsStore } from './engrams.svelte.ts';

// Regression coverage for the Pattern Loom tech tree's eternal
// "loading engram graph…" (production, 2026-08-15). Two stacked causes:
//
//   1. The catalog endpoints 502'd (required-root graph contract + the
//      typed proof decode) — fixed server-side, but the frontend must
//      still render honestly when a catalog fetch fails.
//   2. A promise race: fetchCatalog's rejection was caught by fetchAll's
//      shared catch (error set) — and then the SLOWER summary fetch
//      resolved and ran `this.error = null`, clobbering the message.
//      Result: graph null + error null → the loading branch, forever.
//
// The store now gives the catalog its own error channel (catalogError)
// owned by fetchCatalog itself; the summary channel cannot clobber it.

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  engramsStore.summary = null;
  engramsStore.error = null;
  engramsStore.unavailable = false;
  engramsStore.catalogUnavailable = false;
  engramsStore.catalogError = null;
  engramsStore.graph = null;
});

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

const SUMMARY = { total: 16, by_status: { unverified: 12, stale: 4 }, by_tier: { 'tier:1': 16 }, degraded: false };

describe('engrams store catalog error channel', () => {
  it('a fast catalog 502 survives a slower summary success (the race)', async () => {
    let releaseSummary!: (r: Response) => void;
    const summaryGate = new Promise<Response>((resolve) => { releaseSummary = resolve; });

    globalThis.fetch = ((input: RequestInfo | URL) => {
      const path = String(input);
      if (path.includes('/api/engrams/summary')) return summaryGate;
      // Both catalog endpoints fail fast, like the live 502s (~110ms).
      return Promise.resolve(json(502, { error: 'engram graph' }));
    }) as typeof fetch;

    const all = engramsStore.fetchAll();
    // Let the catalog rejections settle first, then release the summary.
    await new Promise((r) => setTimeout(r, 0));
    releaseSummary(json(200, SUMMARY));
    await all;

    expect(engramsStore.catalogError).toContain('engram graph');
    expect(engramsStore.summary?.total).toBe(16);
    // The summary channel stays clean — and must NOT have erased the
    // catalog failure (the old clobber left every channel null → loading).
    expect(engramsStore.error).toBeNull();
    expect(engramsStore.catalogUnavailable).toBe(false);
  });

  it('missing catalog routes read as unavailable, not as an error', async () => {
    globalThis.fetch = ((input: RequestInfo | URL) => {
      const path = String(input);
      if (path.includes('/api/engrams/summary')) return Promise.resolve(json(200, SUMMARY));
      return Promise.resolve(new Response('not found', { status: 404 }));
    }) as typeof fetch;

    await engramsStore.fetchAll();

    expect(engramsStore.catalogUnavailable).toBe(true);
    expect(engramsStore.catalogError).toBeNull();
    expect(engramsStore.graph).toBeNull();
  });

  it('a successful catalog fetch clears a prior failure and keeps rich nodes', async () => {
    engramsStore.catalogError = 'stale failure';
    const nodes = [{ id: 'engram://go-cli/flagset', name: 'Go CLI tool', tier: 1, proof_status: 'stale', prerequisites: [], proof: { refs: [] } }];
    globalThis.fetch = ((input: RequestInfo | URL) => {
      const path = String(input);
      if (path.includes('/api/engrams/summary')) return Promise.resolve(json(200, SUMMARY));
      if (path.includes('/api/engrams/graph')) return Promise.resolve(json(200, { nodes, edges: [], degraded: false }));
      return Promise.resolve(json(200, { engrams: nodes, degraded: false }));
    }) as typeof fetch;

    await engramsStore.fetchAll();

    expect(engramsStore.catalogError).toBeNull();
    expect(engramsStore.graph?.nodes).toHaveLength(1);
    expect(engramsStore.graph?.nodes[0]?.id).toBe('engram://go-cli/flagset');
  });

  it('a transient catalog failure keeps the last-good graph', async () => {
    engramsStore.graph = { nodes: [], edges: [], degraded: false };
    globalThis.fetch = ((input: RequestInfo | URL) => {
      const path = String(input);
      if (path.includes('/api/engrams/summary')) return Promise.resolve(json(200, SUMMARY));
      return Promise.resolve(json(502, { error: 'blip' }));
    }) as typeof fetch;

    await engramsStore.fetchAll();

    expect(engramsStore.catalogError).toContain('blip');
    expect(engramsStore.graph).not.toBeNull();
  });
});
