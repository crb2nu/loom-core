// Cost store — fetches cost/usage data from GET /api/cost
// and subscribes to SSE hud.cost events for real-time updates.
import { eventStore } from './events.svelte.ts';
import { isStaleFromTimestamp, stalenessStore } from './staleness.svelte.ts';
import { createPoller } from '../utils/poller.ts';

export interface CostSnapshot {
  enabled: boolean;
  total_calls: number;
  total_errors: number;
  total_denied: number;
  total_cached: number;
  total_duration_ms: number;
  by_agent?: CostAgentSummary[];
  by_server?: CostServerSummary[];
}

export interface CostAgentSummary {
  agent_id: string;
  call_count: number;
  errors: number;
  denied: number;
  cached: number;
}

export interface CostServerSummary {
  server: string;
  call_count: number;
  errors: number;
}

class CostStore {
  data = $state<CostSnapshot | null>(null);
  loading = $state(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);

  // Staleness (Slice B3 follow-up) — hud.cost snapshots arrive every 10s;
  // 90s without an update means SSE is silently failing.
  staleAfter = 90_000;
  get isStale(): boolean {
    // Staleness only applies while polling is active (page mounted). An
    // unmounted page's store keeps a frozen lastUpdated forever; reporting
    // it stale would pin the global "Stale data" banner permanently.
    if (!this.poller.running) return false;
    return isStaleFromTimestamp(this.lastUpdated, this.staleAfter);
  }

  // 60s watchdog poll — fires when SSE is down OR the store has gone stale.
  private poller = createPoller(() => {
    if (!eventStore.connected || this.isStale) this.fetch();
  }, 60000);
  private eventUnsubs: Array<() => void> = [];

  get enabled(): boolean {
    return this.data?.enabled ?? false;
  }

  get totalCalls(): number {
    return this.data?.total_calls ?? 0;
  }

  get totalErrors(): number {
    return this.data?.total_errors ?? 0;
  }

  get totalDenied(): number {
    return this.data?.total_denied ?? 0;
  }

  get totalCached(): number {
    return this.data?.total_cached ?? 0;
  }

  async fetch(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const res = await globalThis.fetch('/api/cost');
      if (!res.ok) throw new Error(`Cost API: ${res.status}`);
      this.data = await res.json() as CostSnapshot;
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  applySnapshot(data: Record<string, unknown>): void {
    this.data = data as unknown as CostSnapshot;
    this.lastUpdated = new Date();
    this.error = null;
  }

  startPolling(intervalMs = 60000): void {
    this.stopPolling();
    this.fetch();
    this.poller.start(intervalMs);

    this.eventUnsubs.push(
      eventStore.on('hud.cost', (e) => this.applySnapshot(e.data)),
    );
  }

  stopPolling(): void {
    this.poller.stop();
    for (const unsub of this.eventUnsubs) unsub();
    this.eventUnsubs = [];
  }
}

export const costStore = new CostStore();
stalenessStore.register('cost', () => costStore.isStale);
