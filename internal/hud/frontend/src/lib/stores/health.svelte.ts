// Health store - server health, latency sparklines
// v2: SSE-first with 60s fallback poll. Applies hud.health snapshots directly.
import { eventStore } from './events.svelte.ts';
import { isStaleFromTimestamp, stalenessStore } from './staleness.svelte.ts';
import { arraysEqualByKey } from '../utils/diff.ts';
import { createPoller } from '../utils/poller.ts';

export interface HealthEndpoint {
  healthy: boolean;
  consecFails: number;
  avgLatencyMs: number;
  errorMessage: string;
}

export interface ServerHealth {
  local: HealthEndpoint;
  hub: HealthEndpoint;
  target: string;
  transport: string;
}

export interface HealthResponse {
  servers: Record<string, ServerHealth>;
}

export interface ServerInfo {
  name: string;
  categories: string[];
  description: string;
  running: boolean;
  tool_count?: number;
}

export interface ServersResponse {
  servers: ServerInfo[];
}

export type ServerStatus = 'healthy' | 'idle' | 'degraded' | 'down';

export interface TunnelInfo {
  name: string;
  state: string;
  remote_host: string;
  uptime?: string;
  reconnects: number;
}

export interface CacheStats {
  entries: number;
  size?: string;
  size_bytes?: number;
  max_bytes?: number;
  enabled?: boolean;
  hit_rate: number;
  /**
   * True when the HUD could not reach the daemon's cache RPC and fell back to
   * its own local counters (internal/hud/app_routes_observability.go). The
   * `hit_rate` is then a placeholder 0.0, NOT a measured 0% — without this flag
   * a daemon outage and a stone-cold cache render identically. Absent on HUD
   * builds older than the flag, which is correctly read as "not degraded".
   */
  degraded?: boolean;
}

export interface MergedServer {
  name: string;
  categories: string[];
  description: string;
  running: boolean;
  health: ServerHealth | null;
  latencyHistory: number[];
  // Derived view-model fields for direct template binding.
  status: ServerStatus;
  latency: number;
  target: string;
  transport: string;
  error_message: string;
  tool_count: number;
  consec_fails: number;
}

const SPARKLINE_BUFFER_SIZE = 60;

class HealthStore {
  servers = $state<MergedServer[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);

  // Per-panel UI state (Slice B2.2 — moved out of ServersPanel.svelte so the
  // panel becomes a pure composition shell).
  searchQuery = $state<string>('');
  categoryFilter = $state<string>('');
  statusFilter = $state<string>('');
  sortKey = $state<string>('name');
  sortDir = $state<'asc' | 'desc'>('asc');

  setSearch(value: string): void { this.searchQuery = value; }
  setCategoryFilter(value: string): void { this.categoryFilter = value; }
  setStatusFilter(value: string): void { this.statusFilter = value; }
  setSort(key: string, dir: 'asc' | 'desc'): void { this.sortKey = key; this.sortDir = dir; }
  clearFilters(): void {
    this.searchQuery = '';
    this.categoryFilter = '';
    this.statusFilter = '';
  }

  // Staleness (Slice B3) — see fleet.svelte.ts for the pattern. Daemon emits
  // hud.health every 5s, so 90s gives ~18 cycles of grace before we flag stale.
  staleAfter = 90_000;
  get isStale(): boolean {
    // Staleness only applies while polling is active (page mounted). An
    // unmounted page's store keeps a frozen lastUpdated forever; reporting
    // it stale would pin the global "Stale data" banner permanently.
    if (!this.poller.running) return false;
    return isStaleFromTimestamp(this.lastUpdated, this.staleAfter);
  }

  // 60s watchdog poll (SSE is the primary data source). A tick only fetches
  // when SSE is disconnected OR the store has gone stale despite a healthy
  // SSE. The latter handles the "SSE connected but quiet" case — e.g. no
  // fleet activity to push for staleAfter ms — which previously left the
  // staleness banner false-firing forever. A successful fetch bumps
  // lastUpdated and clears the banner; a failure surfaces an honest error.
  // The watchdog gate lives in shouldTick so the SSE-invalidation path
  // (refreshCoalesced) is not suppressed by it.
  private poller = createPoller(() => this.fetch(), 60000, {
    shouldTick: () => !eventStore.connected || this.isStale,
  });
  private latencyBuffers: Map<string, number[]> = new Map();
  private eventUnsubs: Array<() => void> = [];

  get healthyCount(): number {
    return this.servers.filter((s) => s.status === 'healthy').length;
  }

  get idleCount(): number {
    return this.servers.filter((s) => s.status === 'idle').length;
  }

  get degradedCount(): number {
    return this.servers.filter((s) => s.status === 'degraded').length;
  }

  get downCount(): number {
    return this.servers.filter((s) => s.status === 'down').length;
  }

  /** Running + idle = all available servers. */
  get availableCount(): number {
    return this.healthyCount + this.idleCount;
  }

  async fetch(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const [healthRes, serversRes] = await Promise.all([
        globalThis.fetch('/api/health'),
        globalThis.fetch('/api/servers'),
      ]);
      if (!healthRes.ok) throw new Error(`Health API: ${healthRes.status}`);
      if (!serversRes.ok) throw new Error(`Servers API: ${serversRes.status}`);

      const healthData: HealthResponse = await healthRes.json();
      const serversData: ServersResponse = await serversRes.json();

      const serverList = serversData.servers || [];
      const merged: MergedServer[] = serverList.map((srv) => {
        const health = healthData.servers?.[srv.name] ?? null;
        const latency = health?.local?.avgLatencyMs ?? 0;

        // Update ring buffer
        let buffer = this.latencyBuffers.get(srv.name);
        if (!buffer) {
          buffer = [];
          this.latencyBuffers.set(srv.name, buffer);
        }
        buffer.push(latency);
        if (buffer.length > SPARKLINE_BUFFER_SIZE) {
          buffer.shift();
        }

        // Derive status from running + health state.
        const localHealthy = health?.local?.healthy ?? false;
        let status: ServerStatus;
        if (srv.running && localHealthy) {
          status = 'healthy';
        } else if (srv.running && !localHealthy) {
          status = 'degraded';
        } else if (!srv.running && localHealthy) {
          status = 'idle'; // On-demand server, available but not started.
        } else {
          status = 'down';
        }

        return {
          name: srv.name,
          categories: srv.categories || [],
          description: srv.description,
          running: srv.running,
          health,
          latencyHistory: [...buffer],
          status,
          latency,
          target: health?.target ?? '',
          transport: health?.transport ?? '',
          error_message: health?.local?.errorMessage ?? '',
          tool_count: srv.tool_count ?? 0,
          consec_fails: health?.local?.consecFails ?? 0,
        };
      });

      const keyFn = (s: MergedServer) => s.name;
      const hashFn = (s: MergedServer) => `${s.status}|${s.latency}|${s.tool_count}|${s.error_message}|${s.consec_fails}`;
      if (!arraysEqualByKey(this.servers, merged, keyFn, hashFn)) {
        this.servers = merged;
      }
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  async fetchTunnels(): Promise<TunnelInfo[]> {
    try {
      const res = await globalThis.fetch('/api/tunnels');
      if (!res.ok) throw new Error(`Tunnels API: ${res.status}`);
      const data = await res.json();
      return data.tunnels ?? [];
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      return [];
    }
  }

  async fetchCacheStats(): Promise<CacheStats | null> {
    try {
      const res = await globalThis.fetch('/api/cache');
      if (!res.ok) throw new Error(`Cache API: ${res.status}`);
      return await res.json();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      return null;
    }
  }

  /** Apply health snapshot directly from SSE hud.health event. */
  applySnapshot(data: Record<string, unknown>): void {
    const entries = data.servers as Array<Record<string, unknown>> | undefined;
    if (!entries) return;

    const merged: MergedServer[] = entries.map((entry) => {
      const name = entry.name as string;
      const latency = (entry.avg_latency_ms as number) ?? 0;

      // Update ring buffer
      let buffer = this.latencyBuffers.get(name);
      if (!buffer) {
        buffer = [];
        this.latencyBuffers.set(name, buffer);
      }
      buffer.push(latency);
      if (buffer.length > SPARKLINE_BUFFER_SIZE) {
        buffer.shift();
      }

      const running = entry.running as boolean;
      const healthy = entry.healthy as boolean;
      let status: ServerStatus;
      if (running && healthy) {
        status = 'healthy';
      } else if (running && !healthy) {
        status = 'degraded';
      } else if (!running && healthy) {
        status = 'idle';
      } else {
        status = 'down';
      }

      return {
        name,
        categories: (entry.categories as string[]) ?? [],
        description: (entry.description as string) ?? '',
        running,
        health: null,
        latencyHistory: entry.latency_history as number[] ?? [...buffer],
        status,
        latency,
        target: (entry.target as string) ?? '',
        transport: (entry.transport as string) ?? '',
        error_message: (entry.error_message as string) ?? '',
        tool_count: (entry.tool_count as number) ?? 0,
        consec_fails: (entry.consec_fails as number) ?? 0,
      };
    });

    const keyFn = (s: MergedServer) => s.name;
    const hashFn = (s: MergedServer) => `${s.status}|${s.latency}|${s.tool_count}|${s.error_message}|${s.consec_fails}`;
    if (!arraysEqualByKey(this.servers, merged, keyFn, hashFn)) {
      this.servers = merged;
    }
    // lastUpdated must advance on EVERY snapshot, not only when data
    // changes — otherwise a steady-state SSE feed (same statuses, same
    // latencies push after push) makes the staleness watchdog think we
    // haven't heard from the server after staleAfter ms, and the
    // "Stale data — no recent updates from servers" banner fires
    // even though SSE is healthy.
    this.lastUpdated = new Date();
    this.error = null;
  }

  startPolling(intervalMs = 60000): void {
    this.stopPolling();
    this.fetch();
    this.poller.start(intervalMs);

    // Subscribe to SSE events: apply data directly from hud.health snapshots.
    this.eventUnsubs.push(
      eventStore.on('hud.health', (e) => this.applySnapshot(e.data)),
      // Legacy daemon events still trigger a full refresh as fallback, routed
      // through the poller so a burst collapses into one fetch.
      eventStore.on('server.health', () => this.poller.refreshCoalesced()),
      eventStore.on('config.reload', () => this.poller.refreshCoalesced()),
      eventStore.on('process.start', () => this.poller.refreshCoalesced()),
      eventStore.on('process.stop', () => this.poller.refreshCoalesced()),
    );
  }

  stopPolling(): void {
    this.poller.stop();
    for (const unsub of this.eventUnsubs) unsub();
    this.eventUnsubs = [];
  }
}

export const healthStore = new HealthStore();
// Registered under "servers" so the stale pill reads naturally in the UI
// (the panel that consumes this store is ServersPanel).
stalenessStore.register('servers', () => healthStore.isStale);
