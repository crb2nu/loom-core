import { eventStore } from './events.svelte.ts';
import { isStaleFromTimestamp, stalenessStore } from './staleness.svelte.ts';
import {
  fetchShuttleStatus,
  type CapacityInfo,
  type DispatchRecommendation,
  type ShuttleSnapshot,
} from '../clients/shuttle.ts';
import { createPoller } from '../utils/poller.ts';

class ShuttleStore {
  capacities = $state<CapacityInfo[]>([]);
  recommendations = $state<DispatchRecommendation[]>([]);
  pendingTasks = $state(0);
  activeAgents = $state(0);
  systemLoad = $state(0);
  updatedAt = $state('');
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
    if (!this.poller.running) return false;
    return isStaleFromTimestamp(this.lastUpdated, this.staleAfter);
  }

  // 60s watchdog poll — fires when SSE is down OR the store has gone stale.
  // The watchdog gate lives in shouldTick so the SSE-invalidation path
  // (refreshCoalesced) is not suppressed by it.
  private poller = createPoller(() => this.fetch(), 60000, {
    shouldTick: () => !eventStore.connected || this.isStale,
  });
  private eventUnsubs: Array<() => void> = [];

  get hasRecommendations(): boolean {
    return this.recommendations.length > 0;
  }

  get systemLoadPct(): string {
    return `${Math.round(this.systemLoad * 100)}%`;
  }

  applySnapshot(data: ShuttleSnapshot): void {
    this.capacities = data.capacities ?? [];
    this.recommendations = data.recommendations ?? [];
    this.pendingTasks = data.pending_tasks ?? 0;
    this.activeAgents = data.active_agents ?? 0;
    this.systemLoad = data.system_load ?? 0;
    this.updatedAt = data.updated_at ?? '';
    this.lastUpdated = new Date();
    this.error = null;
  }

  async fetch(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const data = await fetchShuttleStatus();
      this.applySnapshot(data);
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  startPolling(intervalMs = 60000): void {
    this.stopPolling();
    this.fetch();
    this.poller.start(intervalMs);
    this.eventUnsubs.push(
      eventStore.on('hud.fleet', () => this.poller.refreshCoalesced()),
    );
  }

  stopPolling(): void {
    this.poller.stop();
    for (const unsub of this.eventUnsubs) unsub();
    this.eventUnsubs = [];
  }
}

export const shuttleStore = new ShuttleStore();
stalenessStore.register('shuttle', () => shuttleStore.isStale);
