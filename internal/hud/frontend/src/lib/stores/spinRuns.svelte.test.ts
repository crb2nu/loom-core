import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { spinRunsStore } from './spinRuns.svelte.ts';
import { toastStore } from './toasts.svelte.ts';
import type { SpinRun } from '../utils/spinRunsHelpers.ts';

// Regression coverage for the shared spin-runs poller strand.
//
// spinRunsStore is a module singleton polled by THREE mounted consumers
// (SpinningRoomCard, PlansPanel, PlansComparePanel), each driving start()/stop()
// as a lifecycle pair. Before the fix, stop() was unconditional, so one consumer
// unmounting (e.g. the Mills Overview → Work → Plans navigation) killed the
// poller for every other still-mounted consumer — a spin that finished during
// the gap was never observed (no completion toast, no landedTick board refresh)
// until a consumer remounted. The fix reference-counts start()/stop(): only the
// last release actually stops the timer.

// Cadence mirrors the store constants (not exported).
const FAST = 4000;
const IDLE = 20000;

// Private-field view of the singleton — matches the `as unknown as` convention
// the other store tests use to reach internals without production-only getters.
const internals = spinRunsStore as unknown as {
  refCount: number;
  currentInterval: number;
  poller: { running: boolean; stop(): void };
  mine: Set<string>;
  lastStatus: Map<string, string>;
};

let served: SpinRun[] = [];
let fetchMock: ReturnType<typeof vi.fn>;
const realFetch = globalThis.fetch;

// A persistent fetch stub that returns whatever `served` currently holds, so a
// test can change the served rows mid-run without resetting the call counter.
function installFetch(): void {
  fetchMock = vi.fn(async () => ({
    ok: true,
    status: 200,
    json: async () => served,
  }));
  globalThis.fetch = fetchMock as unknown as typeof globalThis.fetch;
}

// Drain the fetch() microtask chain (fetch → json → reconcileToasts →
// syncInterval) plus any 0-delay timers, robustly under fake timers.
async function flush(): Promise<void> {
  for (let i = 0; i < 5; i++) {
    await Promise.resolve();
    await vi.advanceTimersByTimeAsync(0);
  }
}

function liveRun(id = 'spin-live'): SpinRun {
  return {
    id,
    brief: 'do the thing',
    frames: ['flexinfer'],
    status: 'running',
    plan_ids: [],
    competitive: false,
    started_at: new Date().toISOString(),
  };
}

function succeededRun(id: string, planIds: string[]): SpinRun {
  return {
    id,
    brief: 'do the thing',
    frames: ['flexinfer'],
    status: 'succeeded',
    plan_ids: planIds,
    competitive: false,
    started_at: new Date().toISOString(),
    ended_at: new Date().toISOString(),
  };
}

beforeEach(() => {
  vi.useFakeTimers();
  // Fully reset the shared singleton so suites don't bleed into each other.
  internals.poller.stop();
  internals.refCount = 0;
  internals.currentInterval = 0;
  internals.mine.clear();
  internals.lastStatus.clear();
  spinRunsStore.runs = [];
  spinRunsStore.landedTick = 0;
  spinRunsStore.available = true;
  served = [];
  installFetch();
});

afterEach(() => {
  internals.poller.stop();
  vi.restoreAllMocks();
  vi.useRealTimers();
  globalThis.fetch = realFetch;
});

describe('spinRunsStore poller reference counting', () => {
  it('keeps polling when one of two consumers releases', async () => {
    served = [liveRun()];

    spinRunsStore.start(); // consumer A mounts
    spinRunsStore.start(); // consumer B mounts
    await flush();

    expect(internals.refCount).toBe(2);
    expect(internals.poller.running).toBe(true);

    spinRunsStore.stop(); // consumer A unmounts (the mid-navigation teardown)

    // The core guarantee: B still has a hold, so the timer must stay alive.
    expect(internals.refCount).toBe(1);
    expect(internals.poller.running).toBe(true);

    // Behavioral proof: a subsequent tick still fetches for the survivor.
    const before = fetchMock.mock.calls.length;
    await vi.advanceTimersByTimeAsync(IDLE);
    expect(fetchMock.mock.calls.length).toBeGreaterThan(before);
  });

  it('stops the poller only after the last consumer releases', async () => {
    served = [liveRun()];

    spinRunsStore.start();
    spinRunsStore.start();
    await flush();

    spinRunsStore.stop();
    expect(internals.poller.running).toBe(true); // still one hold

    spinRunsStore.stop();
    expect(internals.refCount).toBe(0);
    expect(internals.poller.running).toBe(false);

    // No further ticks fire once fully released.
    const before = fetchMock.mock.calls.length;
    await vi.advanceTimersByTimeAsync(IDLE * 2);
    expect(fetchMock.mock.calls.length).toBe(before);
  });

  it('re-acquires and fetches immediately across an Overview → Plans navigation', async () => {
    served = [liveRun()];

    spinRunsStore.start(); // Overview card mounted
    await flush();
    spinRunsStore.stop(); // Overview card unmounts BEFORE Plans mounts (worst case)
    expect(internals.poller.running).toBe(false);

    // Plans panel mounts — it must restart the timer AND fetch right away so a
    // spin that completed during the gap is observed on the first tick.
    const before = fetchMock.mock.calls.length;
    spinRunsStore.start();
    expect(internals.poller.running).toBe(true);
    expect(internals.refCount).toBe(1);
    await flush();
    expect(fetchMock.mock.calls.length).toBeGreaterThan(before);
  });

  it('keeps a slow idle poll alive when a mounted consumer has empty history', async () => {
    served = []; // no runs at all

    spinRunsStore.start(); // consumer mounted, refCount 1
    await flush();

    // syncInterval must NOT hard-stop the timer while a consumer is mounted, or
    // a spin started later would never be seen.
    expect(internals.poller.running).toBe(true);
    expect(internals.currentInterval).toBe(IDLE);

    const before = fetchMock.mock.calls.length;
    await vi.advanceTimersByTimeAsync(IDLE);
    expect(fetchMock.mock.calls.length).toBeGreaterThan(before);

    // And once the last consumer releases, empty history does stop the poller.
    spinRunsStore.stop();
    expect(internals.poller.running).toBe(false);
  });

  it('double-release is a no-op (refcount floored at zero)', async () => {
    spinRunsStore.start();
    await flush();

    spinRunsStore.stop();
    spinRunsStore.stop(); // unmatched second release must not underflow
    expect(internals.refCount).toBe(0);
    expect(internals.poller.running).toBe(false);
  });
});

describe('spinRunsStore track()', () => {
  it('fast-polls and toasts a tracked spin terminal transition exactly once', async () => {
    const successSpy = vi.spyOn(toastStore, 'success').mockImplementation(() => {});

    served = [{ ...liveRun('spin-1'), status: 'running' }];

    spinRunsStore.start(); // a mounted consumer holds the poller
    spinRunsStore.track('spin-1'); // user just started spin-1
    await flush();

    // track() bumps cadence to fast and registers the spin without toasting yet.
    expect(internals.currentInterval).toBe(FAST);
    expect(successSpy).not.toHaveBeenCalled();

    // The spin completes; the next fast tick observes the terminal transition.
    served = [succeededRun('spin-1', ['PLAN-9'])];
    await vi.advanceTimersByTimeAsync(FAST);

    expect(successSpy).toHaveBeenCalledTimes(1);
    expect(spinRunsStore.landedTick).toBe(1);

    // Subsequent polls must not re-toast the same terminal row.
    await vi.advanceTimersByTimeAsync(IDLE * 2);
    expect(successSpy).toHaveBeenCalledTimes(1);

    spinRunsStore.stop();
  });

  it('does not increment the refcount (no paired release to leak)', async () => {
    served = [{ ...liveRun('spin-2'), status: 'running' }];

    spinRunsStore.start(); // refCount 1
    spinRunsStore.track('spin-2'); // must NOT bump the count
    await flush();

    expect(internals.refCount).toBe(1);

    spinRunsStore.stop(); // the single consumer releases → poller stops
    expect(internals.refCount).toBe(0);
    expect(internals.poller.running).toBe(false);
  });
});
