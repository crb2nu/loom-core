import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// Minimal document stub so the poller's visibility handling is exercised
// under vitest's node environment (which has no DOM).
type Listener = () => void;

function installDocumentStub() {
  const listeners = new Set<Listener>();
  const doc = {
    hidden: false,
    addEventListener: (type: string, fn: Listener) => {
      if (type === 'visibilitychange') listeners.add(fn);
    },
    removeEventListener: (type: string, fn: Listener) => {
      if (type === 'visibilitychange') listeners.delete(fn);
    },
    setHidden(hidden: boolean) {
      doc.hidden = hidden;
      for (const fn of [...listeners]) fn();
    },
    listenerCount: () => listeners.size,
  };
  (globalThis as Record<string, unknown>).document = doc;
  return doc;
}

// Import AFTER the stub exists so hasDocument sees it.
const { createPoller } = await (async () => {
  installDocumentStub();
  return import('./poller.ts');
})();

describe('createPoller', () => {
  let doc: ReturnType<typeof installDocumentStub>;

  beforeEach(() => {
    vi.useFakeTimers();
    doc = installDocumentStub();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('ticks on the given interval', async () => {
    const tick = vi.fn();
    const poller = createPoller(tick, 1000);
    poller.start();
    expect(tick).not.toHaveBeenCalled(); // no initial tick
    await vi.advanceTimersByTimeAsync(3000);
    expect(tick).toHaveBeenCalledTimes(3);
    poller.stop();
  });

  it('stop() halts ticking and removes the visibility listener', async () => {
    const tick = vi.fn();
    const poller = createPoller(tick, 1000);
    poller.start();
    expect(doc.listenerCount()).toBe(1);
    poller.stop();
    expect(doc.listenerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(5000);
    expect(tick).not.toHaveBeenCalled();
    expect(poller.running).toBe(false);
  });

  it('restart replaces the timer instead of stacking', async () => {
    const tick = vi.fn();
    const poller = createPoller(tick, 1000);
    poller.start();
    poller.start(); // must not double-tick
    await vi.advanceTimersByTimeAsync(1000);
    expect(tick).toHaveBeenCalledTimes(1);
    expect(doc.listenerCount()).toBe(1);
    poller.stop();
  });

  it('skips ticks while the tab is hidden', async () => {
    const tick = vi.fn();
    const poller = createPoller(tick, 1000, { refreshOnVisible: false });
    poller.start();
    doc.setHidden(true);
    await vi.advanceTimersByTimeAsync(5000);
    expect(tick).not.toHaveBeenCalled();
    doc.setHidden(false);
    await vi.advanceTimersByTimeAsync(1000);
    expect(tick).toHaveBeenCalledTimes(1);
    poller.stop();
  });

  it('fires a catch-up tick when the tab becomes visible again', async () => {
    const tick = vi.fn();
    const poller = createPoller(tick, 1000);
    poller.start();
    doc.setHidden(true);
    await vi.advanceTimersByTimeAsync(5000);
    expect(tick).not.toHaveBeenCalled();
    doc.setHidden(false);
    await vi.runOnlyPendingTimersAsync();
    expect(tick.mock.calls.length).toBeGreaterThanOrEqual(1);
    poller.stop();
  });

  it('does not overlap ticks when the previous tick is still in flight', async () => {
    let resolveTick: (() => void) | null = null;
    const tick = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          resolveTick = resolve;
        }),
    );
    const poller = createPoller(tick, 1000);
    poller.start();
    await vi.advanceTimersByTimeAsync(3000); // first tick still pending
    expect(tick).toHaveBeenCalledTimes(1);
    resolveTick!();
    await vi.advanceTimersByTimeAsync(1000);
    expect(tick).toHaveBeenCalledTimes(2);
    poller.stop();
  });

  it('refresh() runs a tick on demand even while hidden', async () => {
    const tick = vi.fn();
    const poller = createPoller(tick, 1000);
    poller.start();
    doc.setHidden(true);
    await poller.refresh();
    expect(tick).toHaveBeenCalledTimes(1);
    poller.stop();
  });

  it('shouldTick gates scheduled ticks but not explicit refreshes', async () => {
    const tick = vi.fn();
    let allow = false;
    const poller = createPoller(tick, 1000, { shouldTick: () => allow });
    poller.start();
    await vi.advanceTimersByTimeAsync(3000);
    expect(tick).not.toHaveBeenCalled();
    await poller.refresh();
    expect(tick).toHaveBeenCalledTimes(1);
    poller.refreshCoalesced();
    await vi.advanceTimersByTimeAsync(750);
    expect(tick).toHaveBeenCalledTimes(2);
    allow = true;
    await vi.advanceTimersByTimeAsync(1000);
    expect(tick).toHaveBeenCalledTimes(3);
    poller.stop();
  });

  it('refreshCoalesced() performs no tick while the tab is hidden', async () => {
    const tick = vi.fn();
    const poller = createPoller(tick, 60_000, { refreshOnVisible: false });
    poller.start();
    doc.setHidden(true);
    poller.refreshCoalesced();
    await vi.advanceTimersByTimeAsync(5000);
    expect(tick).not.toHaveBeenCalled();
    poller.stop();
  });

  it('collapses a burst of refreshCoalesced() calls into one tick', async () => {
    const tick = vi.fn();
    const poller = createPoller(tick, 60_000, { coalesceMs: 750 });
    poller.start();
    for (let i = 0; i < 5; i++) {
      poller.refreshCoalesced();
      await vi.advanceTimersByTimeAsync(100);
    }
    expect(tick).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(750);
    expect(tick).toHaveBeenCalledTimes(1);
    poller.stop();
  });

  it('refreshCoalesced() does not start a second tick while one is in flight', async () => {
    let resolveTick: (() => void) | null = null;
    const tick = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          resolveTick = resolve;
        }),
    );
    const poller = createPoller(tick, 60_000, { coalesceMs: 750 });
    poller.start();
    void poller.refresh(); // never settles until resolveTick()
    expect(tick).toHaveBeenCalledTimes(1);
    poller.refreshCoalesced();
    await vi.advanceTimersByTimeAsync(1000);
    expect(tick).toHaveBeenCalledTimes(1); // still in flight
    resolveTick!();
    poller.refreshCoalesced();
    await vi.advanceTimersByTimeAsync(1000);
    expect(tick).toHaveBeenCalledTimes(2);
    poller.stop();
  });
});
