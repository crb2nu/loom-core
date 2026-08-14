// Graph store - knowledge graph visualization
import { untrack } from 'svelte';
import { arraysEqualById } from '../utils/diff.ts';
import { createPoller } from '../utils/poller.ts';

export interface Relation {
  source: string;
  source_name?: string;
  target: string;
  target_name?: string;
  relation_type: string;
  type?: string;
}

export interface GraphStats {
  entity_count: number;
  relation_count: number;
  total_entities: number;
  total_relations: number;
  entity_types: Record<string, number>;
  relation_types: Record<string, number>;
  namespaces: string[];
  all_relations: Relation[];
}

export interface Entity {
  id: string;
  name: string;
  entity_type: string;
  type: string;
  properties: Record<string, unknown>;
  relations: Relation[];
  inbound_relations: Relation[];
  outbound_relations: Relation[];
}

export interface EntitiesResponse {
  entities: Entity[];
}

class GraphStore {
  stats = $state<GraphStats>({
    entity_count: 0,
    relation_count: 0,
    total_entities: 0,
    total_relations: 0,
    entity_types: {},
    relation_types: {},
    namespaces: [],
    all_relations: [],
  });
  entities = $state<Entity[]>([]);
  relations = $state<Relation[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);

  searchQuery = $state<string>('');
  filterType = $state<string>('all');

  private poller = createPoller(() => this.fetch(), 15000);

  get entityTypeList(): string[] {
    return Object.keys(this.stats.entity_types);
  }

  get relationTypeList(): string[] {
    return Object.keys(this.stats.relation_types);
  }

  async fetch(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      // Snapshot filters OUTSIDE the caller's tracking context — fetch()
      // runs synchronously inside panel mount $effects (startPolling); a
      // tracked filter read re-runs those effects on every filter write
      // (the mills_staff pre-await-read class, MR !1474).
      const { searchQuery, filterType } = untrack(() => ({
        searchQuery: this.searchQuery,
        filterType: this.filterType,
      }));
      const params = new URLSearchParams();
      if (searchQuery) params.set('q', searchQuery);
      if (filterType !== 'all') params.set('type', filterType);
      params.set('limit', '100');

      const [statsRes, entitiesRes] = await Promise.all([
        globalThis.fetch('/api/graph/stats'),
        globalThis.fetch(`/api/graph/entities?${params.toString()}`),
      ]);

      if (!statsRes.ok) throw new Error(`Graph stats: ${statsRes.status}`);
      if (!entitiesRes.ok) throw new Error(`Graph entities: ${entitiesRes.status}`);

      this.stats = await statsRes.json();
      const data: EntitiesResponse = await entitiesRes.json();
      const newEntities = data.entities || [];
      const hashFn = (e: Entity) => `${e.name}|${e.entity_type}`;
      if (!arraysEqualById(this.entities, newEntities, hashFn)) {
        this.entities = newEntities;
      }
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  async search(query: string, type?: string, limit?: number): Promise<void> {
    this.searchQuery = query;
    if (type) this.filterType = type;
    if (limit) {
      const params = new URLSearchParams();
      if (query) params.set('q', query);
      if (type && type !== 'all') params.set('type', type);
      params.set('limit', String(limit));

      this.loading = true;
      this.error = null;
      try {
        const [statsRes, entitiesRes] = await Promise.all([
          globalThis.fetch('/api/graph/stats'),
          globalThis.fetch(`/api/graph/entities?${params.toString()}`),
        ]);

        if (!statsRes.ok) throw new Error(`Graph stats: ${statsRes.status}`);
        if (!entitiesRes.ok) throw new Error(`Graph entities: ${entitiesRes.status}`);

        this.stats = await statsRes.json();
        const data: EntitiesResponse = await entitiesRes.json();
        const newEntities = data.entities || [];
        const hashFn = (e: Entity) => `${e.name}|${e.entity_type}`;
        if (!arraysEqualById(this.entities, newEntities, hashFn)) {
          this.entities = newEntities;
        }
        this.lastUpdated = new Date();
      } catch (e) {
        this.error = e instanceof Error ? e.message : String(e);
      } finally {
        this.loading = false;
      }
    } else {
      await this.fetch();
    }
  }

  async addEntity(name: string, entityType: string, namespace: string, props?: Record<string, unknown>): Promise<boolean> {
    try {
      const body: Record<string, unknown> = { name, entity_type: entityType, namespace };
      if (props && Object.keys(props).length > 0) body.properties = props;
      const res = await globalThis.fetch('/api/graph/entities', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`Add entity: ${res.status}`);
      await this.fetch();
      return true;
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  async deleteEntity(id: string): Promise<boolean> {
    try {
      const res = await globalThis.fetch(`/api/graph/entities/${id}`, { method: 'DELETE' });
      if (!res.ok) throw new Error(`Delete entity: ${res.status}`);
      await this.fetch();
      return true;
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  async getEntityDetail(id: string): Promise<Record<string, unknown> | null> {
    try {
      const res = await globalThis.fetch(`/api/graph/entities/${id}`);
      if (!res.ok) throw new Error(`Entity detail: ${res.status}`);
      return await res.json();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      return null;
    }
  }

  async addRelation(sourceId: string, targetId: string, relationType: string): Promise<boolean> {
    try {
      const res = await globalThis.fetch('/api/graph/relations', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ source_id: sourceId, target_id: targetId, relation_type: relationType }),
      });
      if (!res.ok) throw new Error(`Add relation: ${res.status}`);
      await this.fetch();
      return true;
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  async deleteRelation(id: string): Promise<boolean> {
    try {
      const res = await globalThis.fetch(`/api/graph/relations/${id}`, { method: 'DELETE' });
      if (!res.ok) throw new Error(`Delete relation: ${res.status}`);
      await this.fetch();
      return true;
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      return false;
    }
  }

  async findPath(fromId: string, toId: string, maxDepth = 5): Promise<Entity[] | null> {
    try {
      const params = new URLSearchParams({ from: fromId, to: toId, max_depth: String(maxDepth) });
      const res = await globalThis.fetch(`/api/graph/path?${params.toString()}`);
      if (!res.ok) throw new Error(`Find path: ${res.status}`);
      const data = await res.json();
      return data.path ?? data.entities ?? [];
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      return null;
    }
  }

  startPolling(intervalMs = 15000): void {
    this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const graphStore = new GraphStore();
