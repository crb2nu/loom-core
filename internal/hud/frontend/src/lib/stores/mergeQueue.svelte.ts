import { eventStore } from './events.svelte.ts';
import { isStaleFromTimestamp, stalenessStore } from './staleness.svelte.ts';
import {
  fetchMergeQueue,
  fetchMergeConflicts,
  type MergeCandidate,
  type MergeQueueSummary,
  type MergeConflictPair,
} from '../clients/mergeQueue.ts';

const EMPTY_SUMMARY: MergeQueueSummary = {
  total_branches: 0,
  ready_to_merge: 0,
  blocked: 0,
  conflict_pairs: 0,
};

class MergeQueueStore {
  ready = $state<MergeCandidate[]>([]);
  blocked = $state<MergeCandidate[]>([]);
  conflicts = $state<MergeConflictPair[]>([]);
  summary = $state<MergeQueueSummary>(EMPTY_SUMMARY);
  loading = $state(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);

  // Staleness (Slice B3 follow-up) — hud.fleet snapshots arrive every 15s
  // and trigger a fetch here; 90s without an update means SSE is silently
  // failing.
  staleAfter = 90_000;
  get isStale(): boolean {
    // Staleness only applies while polling is active (page mounted). An
    // unmounted page's store keeps a frozen lastUpdated forever; reporting
    // it stale would pin the global "Stale data" banner permanently.
    if (this.pollTimer === null) return false;
    return isStaleFromTimestamp(this.lastUpdated, this.staleAfter);
  }

  private pollTimer: ReturnType<typeof setInterval> | null = null;
  private eventUnsubs: Array<() => void> = [];

  get totalCount(): number {
    return this.ready.length + this.blocked.length;
  }

  get hasConflicts(): boolean {
    return this.conflicts.length > 0;
  }

  async fetch(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const [queue, conflictsRes] = await Promise.all([
        fetchMergeQueue(),
        fetchMergeConflicts(),
      ]);
      this.ready = queue.ready;
      this.blocked = queue.blocked;
      this.summary = queue.summary;
      this.conflicts = conflictsRes.conflicts;
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  startPolling(intervalMs = 60000): void {
    this.stopPolling();
    this.fetch();
    // 60s watchdog poll — fires when SSE is down OR the store has gone stale.
    this.pollTimer = setInterval(() => {
      if (!eventStore.connected || this.isStale) this.fetch();
    }, intervalMs);
    this.eventUnsubs.push(
      eventStore.on('hud.fleet', () => this.fetch()),
    );
  }

  stopPolling(): void {
    if (this.pollTimer) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
    for (const unsub of this.eventUnsubs) unsub();
    this.eventUnsubs = [];
  }
}

export const mergeQueueStore = new MergeQueueStore();
stalenessStore.register('mergeQueue', () => mergeQueueStore.isStale);
