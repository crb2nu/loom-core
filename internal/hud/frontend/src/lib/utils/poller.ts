// Shared polling primitive for HUD stores.
//
// Owns the four things every store used to hand-roll around setInterval:
//   1. timer lifecycle (start/stop/restart without leaks)
//   2. tab-visibility pause — ticks are skipped while document.hidden, and
//      an immediate catch-up tick fires when the tab becomes visible again,
//      so a hidden HUD stops generating backend load without going stale
//      on return
//   3. overlap guard — a tick never fires while the previous async tick is
//      still in flight, so slow responses can't pile up requests
//   4. coalescing — refreshCoalesced() lets SSE handlers request a refresh
//      without routing around (1) and (3); a burst of pushes collapses into
//      a single fetch and hidden tabs are skipped entirely
//
// Store-specific scheduling policy goes in the `shouldTick` option (e.g. the
// SSE-watchdog stores' `!eventStore.connected || this.isStale`) rather than
// inside the tick, so that refresh()/refreshCoalesced() can bypass it. Stores
// that fetch once on start should keep that explicit fetch() call — the poller
// does not fire an initial tick.
//
// Safe under vitest's node environment: all document access is guarded.

export interface Poller {
  /** (Re)start ticking. Restarting with a new interval replaces the timer. */
  start(intervalMs?: number): void;
  /** Stop ticking and detach the visibility listener. */
  stop(): void;
  /** Run one tick now (respects the overlap guard, ignores visibility). */
  refresh(): Promise<void>;
  /**
   * Request a tick from an external trigger (typically an SSE push). Skipped
   * while the tab is hidden, trailing-debounced so an event storm collapses
   * into one fetch, and subject to the same overlap guard as a timer tick.
   */
  refreshCoalesced(): void;
  readonly running: boolean;
}

export interface PollerOptions {
  /** Fire a catch-up tick when the tab becomes visible again. Default true. */
  refreshOnVisible?: boolean;
  /** Quiet period a refreshCoalesced() burst collapses into. Default 750ms. */
  coalesceMs?: number;
  /**
   * Gate for *scheduled* ticks only — the timer and the visibility catch-up.
   * The SSE-watchdog stores pass their `!eventStore.connected || this.isStale`
   * policy here instead of burying it inside the tick, because refresh() and
   * refreshCoalesced() must bypass it: an explicit trigger already knows the
   * data changed. Default always-true.
   */
  shouldTick?: () => boolean;
}

export function createPoller(
  tick: () => void | Promise<void>,
  defaultIntervalMs: number,
  opts: PollerOptions = {},
): Poller {
  const { refreshOnVisible = true, coalesceMs = 750, shouldTick = () => true } = opts;
  const hasDocument = typeof document !== 'undefined';

  let timer: ReturnType<typeof setInterval> | null = null;
  let pendingTimer: ReturnType<typeof setTimeout> | null = null;
  let intervalMs = defaultIntervalMs;
  let inFlight = false;

  async function run(): Promise<void> {
    if (inFlight) return;
    inFlight = true;
    try {
      await tick();
    } finally {
      inFlight = false;
    }
  }

  function onTimer(): void {
    if (hasDocument && document.hidden) return;
    if (!shouldTick()) return;
    void run();
  }

  function onVisibilityChange(): void {
    if (!document.hidden && refreshOnVisible && timer !== null && shouldTick()) void run();
  }

  function refreshCoalesced(): void {
    if (hasDocument && document.hidden) return;
    if (pendingTimer !== null) clearTimeout(pendingTimer);
    pendingTimer = setTimeout(() => {
      pendingTimer = null;
      void run();
    }, coalesceMs);
  }

  function start(nextIntervalMs = intervalMs): void {
    stop();
    intervalMs = nextIntervalMs;
    timer = setInterval(onTimer, intervalMs);
    if (hasDocument) document.addEventListener('visibilitychange', onVisibilityChange);
  }

  function stop(): void {
    if (timer !== null) {
      clearInterval(timer);
      timer = null;
    }
    if (pendingTimer !== null) {
      clearTimeout(pendingTimer);
      pendingTimer = null;
    }
    if (hasDocument) document.removeEventListener('visibilitychange', onVisibilityChange);
  }

  return {
    start,
    stop,
    refresh: run,
    refreshCoalesced,
    get running() {
      return timer !== null;
    },
  };
}
