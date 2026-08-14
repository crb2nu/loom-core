// Stream store - live context stream with SSE-first data flow

import { untrack } from 'svelte';
import { eventStore } from './events.svelte.ts';
import { isStaleFromTimestamp, stalenessStore } from './staleness.svelte.ts';
import { createPoller } from '../utils/poller.ts';

export interface StreamEntry {
  id: string;
  entry_type: string;
  session_id?: string;
  agent_id: string;
  agent: string;
  namespace: string;
  title: string;
  timestamp: string;
  content: string;
}

export interface StreamResponse {
  entries: StreamEntry[];
}

class StreamStore {
  entries = $state<StreamEntry[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);

  paused = $state(false);
  filterType = $state<string>('all');
  filterAgent = $state<string>('all');

  // Staleness (Slice B3) — see fleet.svelte.ts for the pattern. Stream events
  // arrive every 5s when healthy; 90s without an update means SSE is silently
  // failing or the daemon stopped emitting.
  staleAfter = 90_000;
  get isStale(): boolean {
    // Staleness only applies while polling is active (page mounted). An
    // unmounted page's store keeps a frozen lastUpdated forever; reporting
    // it stale would pin the global "Stale data" banner permanently.
    if (!this.poller.running) return false;
    return isStaleFromTimestamp(this.lastUpdated, this.staleAfter);
  }

  // 60s watchdog poll — fires on SSE-down OR on stale (silent SSE failure).
  private poller = createPoller(() => {
    if (!eventStore.connected || this.isStale) this.fetch();
  }, 60000);
  private lastTimestamp: string | null = null;
  private eventUnsubs: Array<() => void> = [];
  private seenIds = new Set<string>();

  get filteredEntries(): StreamEntry[] {
    let result = [...this.entries];
    if (this.filterType !== 'all') {
      result = result.filter((e) => e.entry_type === this.filterType);
    }
    if (this.filterAgent !== 'all') {
      result = result.filter((e) => e.agent === this.filterAgent);
    }
    return result;
  }

  get entryTypes(): string[] {
    const types = new Set(this.entries.map((e) => e.entry_type));
    return Array.from(types).sort();
  }

  get agents(): string[] {
    const agents = new Set(this.entries.map((e) => e.agent).filter(Boolean));
    return Array.from(agents).sort();
  }

  /** Apply entries from either SSE or HTTP, deduplicating against existing IDs. */
  private applyEntries(newEntries: StreamEntry[]): void {
    if (!newEntries || newEntries.length === 0) return;

    const unique = newEntries.filter((e) => {
      if (!e.id || this.seenIds.has(e.id)) return false;
      this.seenIds.add(e.id);
      return true;
    });

    if (unique.length === 0) return;

    // Prepend new entries, keep max 500.
    this.entries = [...unique, ...this.entries].slice(0, 500);

    // Trim seenIds to match entries.
    if (this.seenIds.size > 600) {
      this.seenIds = new Set(this.entries.map((e) => e.id));
    }

    // Track latest timestamp for incremental HTTP fetching.
    if (unique[0].timestamp) {
      this.lastTimestamp = unique[0].timestamp;
    }

    this.lastUpdated = new Date();
  }

  async fetch(): Promise<void> {
    // Untracked: fetch() runs synchronously inside panel mount $effects
    // (startPolling); a tracked `paused` read makes a pause toggle re-run
    // those effects, restarting the poller and SSE subscription
    // (the mills_staff pre-await-read class, MR !1474).
    if (untrack(() => this.paused)) return;

    this.loading = true;
    this.error = null;
    try {
      const params = new URLSearchParams();
      if (this.lastTimestamp) {
        params.set('since', this.lastTimestamp);
      }
      params.set('limit', '100');

      const res = await globalThis.fetch(`/api/stream?${params.toString()}`);
      if (!res.ok) throw new Error(`Stream API: ${res.status}`);

      const data: StreamResponse = await res.json();
      this.applyEntries(data.entries || []);
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  togglePause(): void {
    this.paused = !this.paused;
  }

  clear(): void {
    this.entries = [];
    this.lastTimestamp = null;
    this.seenIds.clear();
  }

  startPolling(intervalMs = 60000): void {
    this.stopPolling();

    // Subscribe to SSE events for real-time delivery.
    const unsub = eventStore.on('hud.stream', (event) => {
      if (this.paused) return;
      const data = event.data as Record<string, unknown>;
      if (data && Array.isArray(data.entries)) {
        this.applyEntries(data.entries as StreamEntry[]);
      }
    });
    this.eventUnsubs.push(unsub);

    // Initial HTTP fetch; the poller handles the fallback watchdog.
    this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
    for (const unsub of this.eventUnsubs) {
      unsub();
    }
    this.eventUnsubs = [];
  }
}

export const streamStore = new StreamStore();
stalenessStore.register('stream', () => streamStore.isStale);
