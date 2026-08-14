// Memory store - tiered memory management
// v2: SSE-first for stats with 60s fallback poll. Items still fetched on demand.
import { untrack } from 'svelte';
import { actionStore } from './action.svelte.ts';
import { eventStore } from './events.svelte.ts';
import { isStaleFromTimestamp, stalenessStore } from './staleness.svelte.ts';
import { arraysEqualById } from '../utils/diff.ts';
import { createPoller } from '../utils/poller.ts';

function errorMessage(e: unknown): string {
  if (e instanceof Error) return e.message || 'Unknown error';
  if (typeof e === 'string') return e;
  try { return JSON.stringify(e); } catch { return 'Unknown error'; }
}

export interface TierStats {
  items: number;
  tokens: number;
  // Configured tier capacity (backend MaxItems). Read by the memory gauges
  // (MemoryPanel, MemoryTiersCard) as `max_items ?? <default>`; the HUD stats
  // endpoint does not currently serialize it, so it is optional.
  max_items?: number;
  // Per-tier policy fields the MemoryPanel renders (`ttl ?? '---'`, and a
  // guarded compression_ratio); optional because the HUD stats endpoint does
  // not currently serialize them.
  ttl?: string | number;
  compression_ratio?: number;
}

export interface MemoryStats {
  working_memory: TierStats;
  short_term_memory: TierStats;
  long_term_memory: TierStats;
  total_items: number;
  total_tokens: number;
  compression?: {
    ratio: number;
    overall_ratio?: number;
    compressed_items: number;
    tokens_saved: number;
    added_24h?: number;
    compressed_24h?: number;
    expired_24h?: number;
  };
}

export interface MemoryItem {
  id: string;
  title: string;
  content: string;
  tier: 'working' | 'short_term' | 'long_term';
  importance: string | number;
  tokens: number;
  status: string;
  category: string;
  accessed_at: string;
  last_accessed: string;
}

export interface MemoryItemsResponse {
  items: MemoryItem[];
}

class MemoryStore {
  stats = $state<MemoryStats>({
    working_memory: { items: 0, tokens: 0 },
    short_term_memory: { items: 0, tokens: 0 },
    long_term_memory: { items: 0, tokens: 0 },
    total_items: 0,
    total_tokens: 0,
  });
  items = $state<MemoryItem[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);

  filterTier = $state<string>('all');
  searchQuery = $state<string>('');

  // Staleness (Slice B3 follow-up) — see fleet.svelte.ts for the pattern.
  // hud.memory snapshots arrive every 10s; 90s without an update means SSE
  // is silently failing or the daemon stopped emitting.
  staleAfter = 90_000;
  get isStale(): boolean {
    // Staleness only applies while polling is active (page mounted). An
    // unmounted page's store keeps a frozen lastUpdated forever; reporting
    // it stale would pin the global "Stale data" banner permanently.
    if (!this.poller.running) return false;
    return isStaleFromTimestamp(this.lastUpdated, this.staleAfter);
  }

  // 60s fallback poll. SSE is the primary data source; a tick only fetches
  // when SSE is disconnected OR the store has gone stale (silent SSE failure).
  // The watchdog gate lives in shouldTick so the SSE-invalidation path
  // (refreshCoalesced) is not suppressed by it.
  private poller = createPoller(() => this.fetch(), 60000, {
    shouldTick: () => !eventStore.connected || this.isStale,
  });
  private eventUnsubs: Array<() => void> = [];

  get filteredItems(): MemoryItem[] {
    let result = [...this.items];
    if (this.filterTier !== 'all') {
      result = result.filter((i) => i.tier === this.filterTier);
    }
    if (this.searchQuery) {
      const q = this.searchQuery.toLowerCase();
      result = result.filter(
        (i) => i.title.toLowerCase().includes(q) || i.category.toLowerCase().includes(q)
      );
    }
    return result;
  }

  async fetch(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      // Snapshot filters OUTSIDE the caller's tracking context — fetch()
      // runs synchronously inside panel mount $effects (startPolling); a
      // tracked filter read re-runs those effects on every filter write
      // (the mills_staff pre-await-read class, MR !1474).
      const { filterTier, searchQuery } = untrack(() => ({
        filterTier: this.filterTier,
        searchQuery: this.searchQuery,
      }));
      const params = new URLSearchParams();
      if (filterTier !== 'all') params.set('tier', filterTier);
      if (searchQuery) params.set('query', searchQuery);
      params.set('limit', '100');

      const [statsRes, itemsRes] = await Promise.all([
        globalThis.fetch('/api/memory/stats'),
        globalThis.fetch(`/api/memory/items?${params.toString()}`),
      ]);

      if (!statsRes.ok) throw new Error(`Memory stats: ${statsRes.status}`);
      if (!itemsRes.ok) throw new Error(`Memory items: ${itemsRes.status}`);

      this.stats = await statsRes.json();
      const data: MemoryItemsResponse = await itemsRes.json();
      this.items = data.items || [];
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  /** Apply stats directly from SSE hud.memory event. */
  applyStats(data: Record<string, unknown>): void {
    // The hud.memory event carries MemoryStatsResult from the monitor.
    // Bridge uses item_count/token_count, but frontend expects items/tokens.
    const mapTier = (raw: Record<string, unknown> | undefined): TierStats => ({
      items: (raw?.item_count as number) ?? (raw?.items as number) ?? 0,
      tokens: (raw?.token_count as number) ?? (raw?.tokens as number) ?? 0,
    });

    // Map compression data if present.
    const rawComp = data.compression as Record<string, unknown> | undefined;
    const compression = rawComp ? {
      ratio: (rawComp.ratio as number) ?? 0,
      compressed_items: (rawComp.compressed_items as number) ?? 0,
      tokens_saved: (rawComp.tokens_saved as number) ?? 0,
      added_24h: (rawComp.added_24h as number) ?? 0,
      compressed_24h: (rawComp.compressed_24h as number) ?? 0,
    } : this.stats.compression;

    const prevTotal = this.stats.total_items;
    this.stats = {
      ...this.stats,
      working_memory: mapTier(data.working_memory as Record<string, unknown>),
      short_term_memory: mapTier(data.short_term_memory as Record<string, unknown>),
      long_term_memory: mapTier(data.long_term_memory as Record<string, unknown>),
      total_items: (data.total_items as number) ?? this.stats.total_items,
      total_tokens: (data.total_tokens as number) ?? this.stats.total_tokens,
      compression,
    };
    this.lastUpdated = new Date();
    this.error = null;

    // If the item count changed, re-fetch the items list so it stays in sync.
    if (this.stats.total_items !== prevTotal) {
      this.fetchItems();
    }
  }

  /** Fetch only the items list (not stats) to keep items in sync after SSE stat changes. */
  private async fetchItems(): Promise<void> {
    try {
      const params = new URLSearchParams();
      if (this.filterTier !== 'all') params.set('tier', this.filterTier);
      if (this.searchQuery) params.set('query', this.searchQuery);
      params.set('limit', '100');
      const res = await globalThis.fetch(`/api/memory/items?${params.toString()}`);
      if (!res.ok) return;
      const data: MemoryItemsResponse = await res.json();
      const next = data.items || [];
      const hashItem = (i: MemoryItem) => `${i.id}|${i.status}|${i.importance}`;
      if (!arraysEqualById(this.items, next, hashItem)) {
        this.items = next;
      }
    } catch {
      // Non-critical: items will refresh on next poll cycle.
    }
  }

  // Reports the outcome per call, like addItem/deleteItem below. `this.error` is
  // a shared field any other request can write, so reading it after the fact
  // cannot tell whether *this* mutation landed; callers — single-row and bulk
  // alike — need this return value to avoid reporting a success the daemon
  // never granted.
  async promote(itemId: string): Promise<boolean> {
    const auditId = actionStore.start('Promote memory item', 'MemoryPanel:promote');
    try {
      const res = await globalThis.fetch(`/api/memory/${itemId}/promote`, {
        method: 'POST',
      });
      if (!res.ok) throw new Error(`Promote: ${res.status}`);
      await this.fetch();
      actionStore.succeed(auditId);
      return true;
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  async demote(itemId: string): Promise<boolean> {
    const auditId = actionStore.start('Demote memory item', 'MemoryPanel:demote');
    try {
      const res = await globalThis.fetch(`/api/memory/${itemId}/demote`, {
        method: 'POST',
      });
      if (!res.ok) throw new Error(`Demote: ${res.status}`);
      await this.fetch();
      actionStore.succeed(auditId);
      return true;
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  async addItem(title: string, content: string, tier: string, importance: string, category?: string): Promise<boolean> {
    const auditId = actionStore.start(`Add memory item → ${tier}`, 'MemoryPanel:add');
    try {
      const body: Record<string, unknown> = { title, content, tier, importance };
      if (category) body.category = category;
      const res = await globalThis.fetch('/api/memory', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`Add memory: ${res.status}`);
      await this.fetch();
      actionStore.succeed(auditId);
      return true;
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  async deleteItem(id: string): Promise<boolean> {
    const auditId = actionStore.start('Delete memory item', 'MemoryPanel:delete');
    try {
      const res = await globalThis.fetch(`/api/memory/${id}`, {
        method: 'DELETE',
      });
      if (!res.ok) throw new Error(`Delete memory: ${res.status}`);
      await this.fetch();
      actionStore.succeed(auditId);
      return true;
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  async fetchCompaction(): Promise<Record<string, unknown> | null> {
    try {
      const res = await globalThis.fetch('/api/memory/compaction');
      if (!res.ok) throw new Error(`Compaction status: ${res.status}`);
      return await res.json();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      return null;
    }
  }

  async recall(tier?: string, query?: string, limit?: number): Promise<void> {
    if (tier) this.filterTier = tier;
    if (query !== undefined) this.searchQuery = query;
    const params = new URLSearchParams();
    if (tier) params.set('tier', tier);
    if (query) params.set('query', query);
    if (limit) params.set('limit', String(limit));

    this.loading = true;
    this.error = null;
    try {
      const [statsRes, itemsRes] = await Promise.all([
        globalThis.fetch('/api/memory/stats'),
        globalThis.fetch(`/api/memory/items?${params.toString()}`),
      ]);

      if (!statsRes.ok) throw new Error(`Memory stats: ${statsRes.status}`);
      if (!itemsRes.ok) throw new Error(`Memory items: ${itemsRes.status}`);

      this.stats = await statsRes.json();
      const data: MemoryItemsResponse = await itemsRes.json();
      this.items = data.items || [];
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
    this.poller.start(intervalMs);

    // Subscribe to SSE events: apply stats directly from hud.memory snapshots.
    this.eventUnsubs.push(
      eventStore.on('hud.memory', (e) => this.applyStats(e.data)),
      // Granular memory mutation events — trigger full refresh for items + stats,
      // routed through the poller so a burst collapses into one fetch.
      eventStore.on('hud.memory.add', () => this.poller.refreshCoalesced()),
      eventStore.on('hud.memory.delete', () => this.poller.refreshCoalesced()),
      eventStore.on('hud.memory.promote', () => this.poller.refreshCoalesced()),
      eventStore.on('hud.memory.demote', () => this.poller.refreshCoalesced()),
    );
  }

  stopPolling(): void {
    this.poller.stop();
    for (const unsub of this.eventUnsubs) unsub();
    this.eventUnsubs = [];
  }
}

export const memoryStore = new MemoryStore();
stalenessStore.register('memory', () => memoryStore.isStale);
