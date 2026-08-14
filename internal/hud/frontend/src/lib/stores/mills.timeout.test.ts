import { afterEach, describe, expect, it, vi } from 'vitest';
import { millsStore } from './mills.svelte.ts';

// Regression coverage for the Mills pipeline-detail drawer hang fix.
//
// Before the fix, `getJSON` issued a bare `fetch()` with no timeout and no
// abort: a slow/unreachable operator pinned the drawer in "Loading…" forever
// and the close button appeared dead. These tests pin down the two guarantees
// that fixed it:
//   1. every request has a client deadline (timeout → retryable error), and
//   2. closing the drawer aborts the in-flight request without leaking a
//      spurious error state.

/**
 * A fetch stub that never resolves on its own — it only rejects when the
 * request's AbortSignal fires (our timeout deadline or an external close).
 * This is exactly the "operator stalled" condition the fix defends against.
 */
function stalledFetch(): typeof globalThis.fetch {
  return vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
    return new Promise<Response>((_resolve, reject) => {
      const signal = init?.signal;
      const fail = () => reject(new DOMException('aborted', 'AbortError'));
      if (signal) {
        if (signal.aborted) fail();
        else signal.addEventListener('abort', fail, { once: true });
      }
    });
  }) as unknown as typeof globalThis.fetch;
}

const realFetch = globalThis.fetch;

afterEach(() => {
  vi.useRealTimers();
  globalThis.fetch = realFetch;
  // Reset the singleton's drawer state so suites don't bleed into each other.
  millsStore.closeRunDetail();
  millsStore.pipelineDetailByRun = {};
});

describe('getJSON request deadline', () => {
  it('rejects with a human timeout message when the operator stalls', async () => {
    vi.useFakeTimers();
    globalThis.fetch = stalledFetch();

    // getJSON is private; exercise it directly — it is the single choke point
    // every Mills request flows through.
    const p = (millsStore as unknown as { getJSON(path: string): Promise<unknown> }).getJSON(
      '/api/mills/status',
    );
    const assertion = expect(p).rejects.toThrow(/timed out after 12s/);

    await vi.advanceTimersByTimeAsync(12_000);
    await assertion;
  });

  it('re-tags a caller abort as AbortError so close can be swallowed', async () => {
    globalThis.fetch = stalledFetch();
    const external = new AbortController();

    const p = (
      millsStore as unknown as {
        getJSON(path: string, opts: { signal: AbortSignal }): Promise<unknown>;
      }
    ).getJSON('/api/mills/pipeline/runs/x', { signal: external.signal });

    external.abort();
    await expect(p).rejects.toMatchObject({ name: 'AbortError' });
  });
});

describe('pipeline-detail drawer', () => {
  it('closeRunDetail aborts the in-flight fetch and leaves no error state', async () => {
    globalThis.fetch = stalledFetch();

    millsStore.openRunDetail('RUN-1');
    expect(millsStore.selectedRunID).toBe('RUN-1');
    expect(millsStore.pipelineDetailByRun['RUN-1']?.status).toBe('loading');

    // Close while the request is still pending — the original bug was that the
    // user could not escape this state.
    millsStore.closeRunDetail();
    // Let the aborted fetch's rejection propagate through fetchPipelineDetail.
    await new Promise((r) => setTimeout(r, 0));

    expect(millsStore.selectedRunID).toBeNull();
    // Crucially, the abort is swallowed: the cached entry must NOT be flipped
    // to 'error', or reopening the same run would flash a phantom failure.
    expect(millsStore.pipelineDetailByRun['RUN-1']?.status).toBe('loading');
  });

  it('surfaces a stalled load as a retryable error state after the deadline', async () => {
    vi.useFakeTimers();
    globalThis.fetch = stalledFetch();

    millsStore.openRunDetail('RUN-2');
    expect(millsStore.pipelineDetailByRun['RUN-2']?.status).toBe('loading');

    await vi.advanceTimersByTimeAsync(12_000);
    await Promise.resolve();

    const entry = millsStore.pipelineDetailByRun['RUN-2'];
    expect(entry?.status).toBe('error');
    expect(entry && 'message' in entry ? entry.message : '').toMatch(/timed out/);
  });
});
