// Knowledge store - cross-agent context aggregation
import { untrack } from 'svelte';
import { arraysEqualById } from '../utils/diff.ts';
import { createPoller } from '../utils/poller.ts';

export interface KnowledgeEntry {
  id: string;
  agent_id: string;
  session_id: string;
  namespace: string;
  entry_type: string;
  title: string;
  content: string;
  file_path: string;
  tags: string[];
  timestamp: string;
  token_count: number;
  metadata: Record<string, unknown>;
}

export interface KnowledgeResponse {
  ok: boolean;
  entries: KnowledgeEntry[];
  grouped: Record<string, KnowledgeEntry[]>;
  count: number;
  total_tokens: number;
  token_budget: number;
}

class KnowledgeStore {
  entries = $state<KnowledgeEntry[]>([]);
  grouped = $state<Record<string, KnowledgeEntry[]>>({});
  loading = $state(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);

  totalTokens = $state(0);
  tokenBudget = $state(0);

  searchQuery = $state('');
  filterCategory = $state('all');
  filterAgent = $state('all');

  private poller = createPoller(() => this.fetch(), 30000);

  get categories(): string[] {
    return Object.keys(this.grouped).sort();
  }

  get agents(): string[] {
    const agents = new Set(this.entries.map((e) => e.agent_id).filter(Boolean));
    return Array.from(agents).sort();
  }

  get filteredEntries(): KnowledgeEntry[] {
    let result = [...this.entries];
    if (this.filterCategory !== 'all') {
      result = result.filter((e) => e.entry_type === this.filterCategory);
    }
    if (this.filterAgent !== 'all') {
      result = result.filter((e) => e.agent_id === this.filterAgent);
    }
    return result;
  }

  async fetch(query?: string): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      // Snapshot filters OUTSIDE the caller's tracking context — fetch()
      // runs synchronously inside the panel's mount $effect (startPolling);
      // a tracked filter read re-runs that effect on every filter write
      // (the mills_staff pre-await-read class, MR !1474).
      const { searchQuery, filterCategory } = untrack(() => ({
        searchQuery: this.searchQuery,
        filterCategory: this.filterCategory,
      }));
      const params = new URLSearchParams();
      if (query || searchQuery) {
        params.set('query', query || searchQuery);
      }
      if (filterCategory !== 'all') {
        params.set('category', filterCategory);
      }
      params.set('budget', '8000');

      const res = await globalThis.fetch(`/api/knowledge?${params.toString()}`);
      if (!res.ok) throw new Error(`Knowledge API: ${res.status}`);

      const data: KnowledgeResponse = await res.json();
      const newEntries = data.entries ?? [];
      const hashFn = (e: KnowledgeEntry) => `${e.entry_type}|${e.title}|${e.timestamp}`;
      if (!arraysEqualById(this.entries, newEntries, hashFn)) {
        this.entries = newEntries;
        this.grouped = data.grouped ?? {};
      }
      this.totalTokens = data.total_tokens ?? 0;
      this.tokenBudget = data.token_budget ?? 0;
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  async search(query: string): Promise<void> {
    this.searchQuery = query;
    return this.fetch(query);
  }

  startPolling(intervalMs = 30000): void {
    this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const knowledgeStore = new KnowledgeStore();
