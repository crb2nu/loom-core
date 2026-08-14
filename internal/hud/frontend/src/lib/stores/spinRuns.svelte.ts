// Spinning Room status store — the durable replacement for PlansPanel's old
// ephemeral inflight-spin tracking.
//
// Before: PlansPanel held an in-memory `inflightSpins` array, polled each id it
// personally started, and rendered a bare "N spinning…" chip. That state died
// on refresh, never showed spins started elsewhere, and surfaced no per-spin
// status or stuck detection.
//
// Now: this store polls the operator's DURABLE spin-runs endpoint
// (GET /api/mills/spin/runs → the SQLite spin_runs table, migration 007), so
// the tray reflects every recent spin regardless of which tab/session started
// it and survives a reload. Completion toasts still fire only for spins the
// user started THIS session (tracked via track()), so we don't re-toast
// historical rows on first load or spam for other operators' spins.
//
// Polling cadence is adaptive: fast while a spin is live, slow when idle, and
// it stops entirely once there's nothing live and no history worth refreshing.
import { createPoller } from '../utils/poller.ts';
import { toastStore } from './toasts.svelte.ts';
import {
  hasLiveSpin,
  isTerminalStatus,
  liveCount as liveCountOf,
  stuckCount as stuckCountOf,
  visibleRuns,
  competitiveGroups,
  type SpinRun,
  type CompetitiveGroup,
} from '../utils/spinRunsHelpers.ts';

const FAST_INTERVAL_MS = 4000; // a spin is live — poll for the transition
const IDLE_INTERVAL_MS = 20000; // only terminal history — keep it fresh, cheaply
const LIST_LIMIT = 50;

class SpinRunsStore {
  runs = $state<SpinRun[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  available = $state(true);
  lastUpdated = $state<Date | null>(null);

  // Bumped whenever a spin the user started reaches `succeeded`, so a consumer
  // (PlansPanel) can refresh its board the instant a draft lands.
  landedTick = $state(0);

  // Spins started in THIS browser session — the only ones we toast on
  // completion. A plain Set (not reactive): it gates side effects, not render.
  private mine = new Set<string>();
  // Last status seen per run id — dedupes completion toasts across polls.
  private lastStatus = new Map<string, string>();
  private currentInterval = 0;

  // How many mounted consumers currently want the poller alive. The store is a
  // module singleton shared by SpinningRoomCard, PlansPanel, and
  // PlansComparePanel; each drives start()/stop() as a lifecycle pair. Without
  // reference counting, one consumer unmounting (e.g. Overview → Plans swap)
  // would stop the poller for every other still-mounted consumer, stranding a
  // spin that finishes during the gap. start()/stop() are acquire/release.
  private refCount = 0;

  private poller = createPoller(() => this.fetch(), IDLE_INTERVAL_MS);

  get hasLive(): boolean {
    return hasLiveSpin(this.runs);
  }
  get liveCount(): number {
    return liveCountOf(this.runs);
  }
  stuckCount(now: number): number {
    return stuckCountOf(this.runs, now);
  }
  visible(now: number): SpinRun[] {
    return visibleRuns(this.runs, { now });
  }
  /** plan id → competitive spin it belongs to, for board sibling badges. */
  get competitiveGroups(): Map<string, CompetitiveGroup> {
    return competitiveGroups(this.runs);
  }

  async fetch(): Promise<void> {
    this.loading = true;
    try {
      const res = await fetch(`/api/mills/spin/runs?limit=${LIST_LIMIT}`, { cache: 'no-store' });
      if (res.status === 404) {
        // Operator predates async spins (still deploying) — degrade quietly.
        this.available = false;
        this.error = null;
        return;
      }
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      // Endpoint returns a BARE array (not enveloped).
      const next: SpinRun[] = Array.isArray(data) ? data : (data.runs ?? []);
      this.available = true;
      this.error = null;
      this.reconcileToasts(next);
      this.runs = next;
      this.lastUpdated = new Date();
      this.syncInterval();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  // Fire a completion toast for each tracked spin that newly reached a terminal
  // state, then remember every run's status so the next poll doesn't repeat it.
  private reconcileToasts(next: SpinRun[]): void {
    for (const run of next) {
      const prev = this.lastStatus.get(run.id);
      const newlyTerminal = isTerminalStatus(run.status) && prev !== run.status;
      if (this.mine.has(run.id) && newlyTerminal) {
        this.toastTerminal(run);
        if (run.status === 'succeeded') this.landedTick += 1;
        this.mine.delete(run.id); // one toast per spin
      }
      this.lastStatus.set(run.id, run.status);
    }
  }

  private toastTerminal(run: SpinRun): void {
    const frames = run.frames.join(' + ') || 'a frame';
    switch (run.status) {
      case 'succeeded': {
        const ids = run.plan_ids ?? [];
        const n = ids.length;
        toastStore.success(
          `Spin done on ${frames}: ${n} draft${n === 1 ? '' : 's'}${n ? ` (${ids.join(', ')})` : ''}`,
        );
        if (run.error) toastStore.warning(run.error); // partial competitive failure note
        break;
      }
      case 'failed':
        toastStore.error(`Spin failed on ${frames}: ${run.error || 'unknown error'}`);
        break;
      case 'timeout':
        toastStore.warning(`Spin timed out on ${frames} — retry.`);
        break;
    }
  }

  // Speed up while a spin is live; slow down when only history remains; stop
  // when there's nothing at all to watch AND no consumer is mounted.
  private syncInterval(): void {
    if (this.runs.length === 0) {
      // No history to refresh. Only fully stop the timer when no consumer is
      // mounted — a mounted consumer with empty history must keep a slow idle
      // poll alive so a spin started elsewhere is still picked up. Stopping
      // here while refCount > 0 (combined with the shared singleton) is another
      // way to strand a live spin.
      if (this.refCount === 0) {
        this.poller.stop();
        this.currentInterval = 0;
        return;
      }
      if (this.currentInterval !== IDLE_INTERVAL_MS || !this.poller.running) {
        this.currentInterval = IDLE_INTERVAL_MS;
        this.poller.start(IDLE_INTERVAL_MS);
      }
      return;
    }
    const want = this.hasLive ? FAST_INTERVAL_MS : IDLE_INTERVAL_MS;
    if (want !== this.currentInterval || !this.poller.running) {
      this.currentInterval = want;
      this.poller.start(want);
    }
  }

  /** Register a spin the user just started so its completion toasts. */
  track(spinId: string): void {
    if (!spinId) return;
    this.mine.add(spinId);
    // A brand-new spin means there's live work — poll now + go fast.
    //
    // Deliberately does NOT touch refCount: track() has no paired release (a
    // spin's completion is observed by the poll loop, not by an unmount), so
    // incrementing here would leak a permanent hold. A spin can only be started
    // from a mounted consumer, so refCount is already ≥ 1 and the poller is
    // live. poller.start() below still (re)arms the timer defensively in case
    // it somehow isn't running, without changing the refcount.
    this.currentInterval = FAST_INTERVAL_MS;
    this.poller.start(FAST_INTERVAL_MS);
    void this.poller.refresh();
  }

  /**
   * Acquire the poller (idempotent per consumer). Increments the refcount;
   * the first consumer (0→1) starts the timer at idle cadence. Always fetches
   * once immediately so a newly-mounted consumer gets fresh data right away.
   */
  start(): void {
    this.refCount += 1;
    if (!this.poller.running) {
      this.currentInterval = IDLE_INTERVAL_MS;
      this.poller.start(IDLE_INTERVAL_MS);
    }
    void this.fetch();
  }

  /**
   * Release the poller. Decrements the refcount (floored at 0, guarding against
   * double-release); only the last consumer (1→0) actually stops the timer.
   * This is the fix: a mid-navigation unmount no longer strands other consumers.
   */
  stop(): void {
    if (this.refCount === 0) return; // double-release / unmatched stop — ignore
    this.refCount -= 1;
    if (this.refCount === 0) {
      this.poller.stop();
      this.currentInterval = 0;
    }
  }
}

export const spinRunsStore = new SpinRunsStore();
