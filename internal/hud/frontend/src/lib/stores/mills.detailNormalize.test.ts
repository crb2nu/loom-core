import { afterEach, describe, expect, it, vi } from 'vitest';
import { millsStore } from './mills.svelte.ts';

// Regression coverage for the live-run drawer wedge (null stages/gates).
//
// The operator encodes a Go nil slice as JSON `null`, and a live run has no
// gate outcomes until its first stage completes — so `gates: null` was the
// COMMON detail payload for in-flight runs. `[...detail.gates]` inside the
// drawer's $derived then threw, Svelte tore down the component's effect
// tree, and the drawer froze at "Loading run detail…" with a dead close
// button. The store must normalise null/missing stages+gates to `[]` before
// caching so no component ever sees the wire encoding.

function jsonFetch(body: unknown): typeof globalThis.fetch {
  return vi.fn(() =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  ) as unknown as typeof globalThis.fetch;
}

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  millsStore.closeRunDetail();
  millsStore.pipelineDetailByRun = {};
});

describe('pipeline detail stages/gates normalisation', () => {
  it('caches null stages and gates as empty arrays', async () => {
    globalThis.fetch = jsonFetch({
      run: { ID: 'RUN-NULLS', State: 'implementing' },
      stages: null,
      gates: null,
    });

    millsStore.openRunDetail('RUN-NULLS');
    await vi.waitFor(() => {
      expect(millsStore.pipelineDetailByRun['RUN-NULLS']?.status).toBe('loaded');
    });

    const entry = millsStore.pipelineDetailByRun['RUN-NULLS'];
    if (entry?.status !== 'loaded') throw new Error('expected loaded entry');
    expect(entry.detail.stages).toEqual([]);
    expect(entry.detail.gates).toEqual([]);
    // The drawer's sortedGates spread must be safe on the cached payload.
    expect([...entry.detail.gates]).toEqual([]);
  });

  it('leaves populated stages and gates untouched', async () => {
    const stage = { ID: 1, Stage: 'implement', Attempt: 1 };
    const gate = { ID: 2, GateName: 'diff_size', Outcome: 'pass', AfterStage: 'implement' };
    globalThis.fetch = jsonFetch({
      run: { ID: 'RUN-FULL', State: 'reviewing' },
      stages: [stage],
      gates: [gate],
    });

    millsStore.openRunDetail('RUN-FULL');
    await vi.waitFor(() => {
      expect(millsStore.pipelineDetailByRun['RUN-FULL']?.status).toBe('loaded');
    });

    const entry = millsStore.pipelineDetailByRun['RUN-FULL'];
    if (entry?.status !== 'loaded') throw new Error('expected loaded entry');
    expect(entry.detail.stages).toHaveLength(1);
    expect(entry.detail.gates).toHaveLength(1);
  });
});
