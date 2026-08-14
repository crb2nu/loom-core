// Engram summary store — proof-status and tier counts for the engram tech
// tree.
//
// GET /api/engrams/summary (internal/hud/api_engrams.go) aggregates
// agent_engram_list so a panel can render a one-line badge without walking the
// whole library:
//
//   {"total":int,
//    "by_status":{"unverified":int,"verified":int,"stale":int,"failing":int},
//    "by_tier":{"tier:1":int,"tier:2":int,"tier:3":int},
//    "degraded":bool}
//
// `degraded: true` is the endpoint's own admission that the agent bridge is
// unavailable and the zeros are placeholders — the whole reason it exists is
// that a bridge outage and a genuinely empty catalog are both a 200 with
// all-zero counts. A HUD older than that field omits it entirely, which is
// indistinguishable from `false`; treating only an explicit `true` as degraded
// keeps the old build reading as "real data", which is what it is.

import { createPoller } from '../utils/poller.ts';
import { errorMessage, fetchJSON } from '../utils/apiJson.ts';

export interface EngramSummary {
  total: number;
  by_status: Record<string, number>;
  by_tier: Record<string, number>;
  degraded: boolean;
}

export interface EngramProof {
  kind?: string;
  refs: string[];
}

export interface EngramInfo {
  id: string;
  name: string;
  tier: number;
  proof_status: string;
  description?: string;
  prerequisites: string[];
  last_verified_at?: string;
  proof: EngramProof;
}

export interface EngramGraphEdge { from: string; to: string }
export interface EngramGraph {
  nodes: EngramInfo[];
  edges: EngramGraphEdge[];
  degraded: boolean;
}

/** Proof statuses in the order the badge strip renders them. */
export const proofStatusOrder = ['verified', 'unverified', 'stale', 'failing'] as const;
export type ProofStatus = (typeof proofStatusOrder)[number];

class EngramsStore {
  summary = $state<EngramSummary | null>(null);
  loading = $state(false);
  error = $state<string | null>(null);
  /** True when this HUD build does not register /api/engrams/summary. */
  unavailable = $state(false);
  /** True when either catalog endpoint is not registered by this HUD build. */
  catalogUnavailable = $state(false);
  engrams = $state<EngramInfo[]>([]);
  graph = $state<EngramGraph | null>(null);

  // 60s: the catalog changes at authoring pace, not run pace.
  private poller = createPoller(() => this.fetchAll(), 60000);

  /** True when the endpoint reported the agent bridge as unavailable. */
  get degraded(): boolean {
    return this.summary?.degraded === true;
  }

  get total(): number {
    return this.summary?.total ?? 0;
  }

  /** [status, n] pairs in proofStatusOrder, zero buckets included so the strip
   *  keeps a stable width across refreshes. */
  get statusPairs(): Array<[ProofStatus, number]> {
    const byStatus = this.summary?.by_status ?? {};
    return proofStatusOrder.map((s) => [s, byStatus[s] ?? 0]);
  }

  /** [tier, n] pairs sorted by tier number, zero buckets dropped. */
  get tierPairs(): Array<[string, number]> {
    const byTier = this.summary?.by_tier ?? {};
    return Object.entries(byTier)
      .filter(([, n]) => n > 0)
      .sort((a, b) => a[0].localeCompare(b[0], undefined, { numeric: true }));
  }

  async fetch(): Promise<void> {
    this.loading = true;
    try {
      const data = await fetchJSON<Partial<EngramSummary>>('/api/engrams/summary');
      if (data === null) {
        this.unavailable = true;
        this.summary = null;
        this.error = null;
        return;
      }
      this.unavailable = false;
      this.summary = {
        total: data.total ?? 0,
        by_status: data.by_status ?? {},
        by_tier: data.by_tier ?? {},
        degraded: data.degraded === true,
      };
      this.error = null;
    } catch (e) {
      this.error = errorMessage(e);
    } finally {
      this.loading = false;
    }
  }

  async fetchCatalog(): Promise<void> {
    const [list, graph] = await Promise.all([
      fetchJSON<{ engrams?: EngramInfo[]; degraded?: boolean }>('/api/engrams'),
      fetchJSON<Partial<EngramGraph>>('/api/engrams/graph'),
    ]);
    if (list === null || graph === null) {
      this.catalogUnavailable = true;
      this.engrams = [];
      this.graph = null;
      return;
    }
    this.catalogUnavailable = false;
    this.engrams = list.engrams ?? [];
    this.graph = {
      nodes: graph.nodes ?? [],
      edges: graph.edges ?? [],
      degraded: list.degraded === true || graph.degraded === true,
    };
  }

  async fetchAll(): Promise<void> {
    this.loading = true;
    try {
      await Promise.all([this.fetch(), this.fetchCatalog()]);
      if (!this.error) this.error = null;
    } catch (e) {
      this.error = errorMessage(e);
    } finally {
      this.loading = false;
    }
  }

  startPolling(intervalMs = 60000): void {
    void this.fetchAll();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const engramsStore = new EngramsStore();
