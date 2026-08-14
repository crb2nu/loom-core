import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { millsStaffStore } from './mills_staff.svelte.ts';
import { runInTrackingEffect } from '../test-utils/tracking.svelte.ts';

// Regression: refresh() must not re-trigger the $effect that called it.
//
// MillStaffPanel starts the store's polling from its mount $effect. refresh()
// used to read reactive state synchronously (the window, plus the six report
// slots passed to fetchSlot as prev), so the effect tracked the very slots
// refresh() writes on completion — every finished round re-ran the effect,
// which re-ran refresh(), an infinite refetch loop at network round-trip
// cadence. Observed in production: the operator logged the panel's report
// quartet every ~1.5s from a single open tab (60s is the intended cadence).
//
// A `.dom.test.ts` on purpose: the node project resolves Svelte's SSR build,
// where effects never run and the test would pass vacuously.

const REPORT_COUNT = 6;

// Answer the first few rounds, then hang. Under the regression the loop is
// microtask-tight with instant responses — unbounded, it OOMs the worker
// before the assertion can report. Starving it after 5× the legitimate
// request count keeps the failure a clean count mismatch.
function countingFetch(): ReturnType<typeof vi.fn> {
  const mock = vi.fn((): Promise<Response> => {
    if (mock.mock.calls.length > REPORT_COUNT * 5) {
      return new Promise<Response>(() => {});
    }
    return Promise.resolve(
      new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }),
    );
  });
  return mock;
}

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  millsStaffStore.stopPolling();
  millsStaffStore.promotion = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.councilPromotion = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.judge = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.regressions = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.configOutcomes = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.signatures = { data: null, error: null, disabled: false, lastUpdated: null };
  millsStaffStore.window = '336h';
});

describe('millsStaffStore.refresh inside a tracking effect', () => {
  it('fetches each report exactly once — completion must not re-run the effect', async () => {
    const fetchMock = countingFetch();
    globalThis.fetch = fetchMock as unknown as typeof globalThis.fetch;

    const stop = runInTrackingEffect(() => {
      void millsStaffStore.refresh();
    });

    // Let several rounds of completions + effect flushes settle. Under the
    // loop, every round adds another six requests.
    for (let i = 0; i < 6; i++) {
      await new Promise((r) => setTimeout(r, 0));
      flushSync();
    }
    stop();

    expect(fetchMock.mock.calls.length).toBe(REPORT_COUNT);
  });
});
