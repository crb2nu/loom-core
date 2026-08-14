import { afterEach, describe, expect, it, vi } from 'vitest';
import { memoryStore } from './memory.svelte.ts';

// Fetch-boundary coverage for POST /api/memory/:id/{promote,demote}.
//
// These two used to return Promise<void>, which left MemoryPanel's bulk passes
// with nothing to branch on: they toasted "N items promoted" whether or not the
// daemon accepted a single one. The boolean return is the contract the panel
// counts failures against, so it is what these tests pin.

const realFetch = globalThis.fetch;

function ok(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

/**
 * Mock the whole fetch surface a mutation touches: the POST itself, plus the
 * stats/items refresh the store issues on the success path.
 */
function mockFetch(mutationStatus: number): typeof globalThis.fetch {
  return vi.fn((input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes('/promote') || url.includes('/demote')) {
      return Promise.resolve(new Response('', { status: mutationStatus }));
    }
    if (url.includes('/api/memory/items')) return Promise.resolve(ok({ items: [] }));
    return Promise.resolve(ok({ total_items: 0, total_tokens: 0 }));
  }) as unknown as typeof globalThis.fetch;
}

afterEach(() => {
  globalThis.fetch = realFetch;
  memoryStore.error = null;
});

describe('memoryStore.promote / demote outcome reporting', () => {
  it('returns true when the daemon accepts the mutation', async () => {
    globalThis.fetch = mockFetch(200);

    expect(await memoryStore.promote('mem-1')).toBe(true);
    expect(await memoryStore.demote('mem-1')).toBe(true);
  });

  it('returns false when the daemon rejects the mutation', async () => {
    globalThis.fetch = mockFetch(500);

    expect(await memoryStore.promote('mem-1')).toBe(false);
    expect(await memoryStore.demote('mem-1')).toBe(false);
  });

  it('does not throw on rejection, so a bulk pass can drain the whole selection', async () => {
    globalThis.fetch = mockFetch(403);

    const results: boolean[] = [];
    for (const id of ['a', 'b', 'c']) {
      results.push(await memoryStore.promote(id));
    }

    expect(results).toEqual([false, false, false]);
  });
});
