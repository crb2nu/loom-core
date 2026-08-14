// Patterns store — the Pattern Loom front door. Lists approved patterns from
// the catalog (GET /api/patterns) and stamps one with user-supplied materials
// (POST /api/patterns/stamp), which expands the pattern into a Plan that Mills
// executes. The tools go live after an mcp-agent-context redeploy; until then
// the list is empty and the panel renders a "no patterns yet" state.

import { untrack } from 'svelte';
import { createPoller } from '../utils/poller.ts';
import { adminFetch } from './labsAuth.svelte.ts';

export interface PatternMaterialField {
  name: string;
  type: string; // string | int | bool | enum | list | object
  required?: boolean;
  description?: string;
  enum?: string[];
  default?: string;
  example?: string;
}

export interface PatternPin {
  axis: string;
  value: string;
}

export interface PatternGauge {
  description?: string;
  commands?: string[];
  assertions?: string[];
}

export interface PatternProvenance {
  author?: string;
  approved_by?: string;
  instances_shipped_green?: number;
  notes?: string;
}

export interface PatternInfo {
  id: string;
  slug: string;
  name: string;
  makes: string;
  description?: string;
  version: string;
  status: string;
  materials_schema?: PatternMaterialField[];
  pins?: PatternPin[];
  gauge?: PatternGauge;
  engrams?: string[];
  deploy_contract?: string;
  provenance?: PatternProvenance;
  tags?: string[];
}

export interface StampResult {
  ok: boolean;
  plan_id: string;
  pattern_id: string;
  pattern_version?: string;
  slice_count: number;
  tools_required?: string[];
  deploy_contract?: string;
  slices?: Array<Record<string, unknown>>;
  note?: string;
  /** Present when the stamp also queued a Mills backlog item (enqueue:true). */
  enqueued?: boolean;
  backlog_id?: string;
  state?: string;
}

export interface PatternInstance {
  stamped_at: string;
  plan_id: string;
  target_project: string;
  run_id?: string;
  run_status?: string;
  run_outcome?: string;
  mr_ref?: string;
  mr_url?: string;
  mr_status?: string;
  mr_outcome?: string;
}

export type PatternStatusFilter = 'approved' | 'candidate' | 'all';

class PatternsStore {
  patterns = $state<PatternInfo[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);
  statusFilter = $state<PatternStatusFilter>('approved');

  // Stamp action state.
  stamping = $state(false);
  stampError = $state<string | null>(null);
  lastResult = $state<StampResult | null>(null);
  instances = $state<PatternInstance[]>([]);
  instancesLoading = $state(false);
  instancesError = $state<string | null>(null);
  instancesDegraded = $state(false);
  private instancesRequest = 0;

  // 30s poll — refreshes the approved-patterns list.
  private poller = createPoller(() => { void this.fetch(); }, 30000);

  async fetch(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      // Snapshot the filter OUTSIDE the caller's tracking context — fetch()
      // runs synchronously inside panel $effects (FactoryPanel, startPolling);
      // a tracked read re-runs those effects on every filter write
      // (the mills_staff pre-await-read class, MR !1474).
      const statusFilter = untrack(() => this.statusFilter);
      const params = new URLSearchParams();
      if (statusFilter !== 'all') params.set('status', statusFilter);
      const url = params.toString() ? `/api/patterns?${params}` : '/api/patterns';
      const res = await globalThis.fetch(url);
      if (!res.ok) throw new Error(`Patterns API: ${res.status}`);
      const data = await res.json();
      this.patterns = data.patterns ?? [];
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  setStatusFilter(s: PatternStatusFilter): void {
    this.statusFilter = s;
    void this.fetch();
  }

  // stamp posts materials for a pattern and returns the result (or null on
  // error, with stampError set). Clears lastResult while in flight.
  //
  // enqueue:true additionally projects the stamped plan as a queued Mills
  // backlog item (the J1 stamp→beam seam). The HUD admin-gates the enqueue
  // path BEFORE stamping (api_patterns.go), so that variant must ride
  // adminFetch — a bare fetch reaches the gate tokenless and 401s.
  async stamp(
    patternId: string,
    materials: Record<string, unknown>,
    project: string,
    opts: { enqueue?: boolean } = {}
  ): Promise<StampResult | null> {
    this.stamping = true;
    this.stampError = null;
    this.lastResult = null;
    try {
      const body = JSON.stringify({
        pattern_id: patternId,
        materials,
        project,
        ...(opts.enqueue ? { enqueue: true } : {}),
      });
      const res = opts.enqueue
        ? await adminFetch('/api/patterns/stamp', {
            method: 'POST',
            headers: { 'content-type': 'application/json' },
            body,
            requireToken: true,
            action: 'Stamping onto the beam',
          })
        : await globalThis.fetch('/api/patterns/stamp', {
            method: 'POST',
            headers: { 'content-type': 'application/json' },
            body,
          });
      const data = await res.json().catch(() => null);
      if (!res.ok) {
        const msg = data && typeof data.error === 'string' ? data.error : `Stamp failed: ${res.status}`;
        throw new Error(msg);
      }
      this.lastResult = data as StampResult;
      return this.lastResult;
    } catch (e) {
      this.stampError = e instanceof Error ? e.message : String(e);
      return null;
    } finally {
      this.stamping = false;
    }
  }

  clearResult(): void {
    this.lastResult = null;
    this.stampError = null;
  }

  async fetchInstances(patternId: string): Promise<void> {
    const request = ++this.instancesRequest;
    this.instancesLoading = true;
    this.instancesError = null;
    this.instancesDegraded = false;
    this.instances = [];
    try {
      const res = await globalThis.fetch(`/api/patterns/${encodeURIComponent(patternId)}/instances`);
      if (!res.ok) throw new Error(`Pattern instances API: ${res.status}`);
      const data = await res.json() as { instances?: PatternInstance[]; degraded?: boolean };
      if (request !== this.instancesRequest) return;
      this.instances = data.instances ?? [];
      this.instancesDegraded = data.degraded === true;
    } catch (e) {
      if (request !== this.instancesRequest) return;
      this.instancesError = e instanceof Error ? e.message : String(e);
    } finally {
      if (request === this.instancesRequest) this.instancesLoading = false;
    }
  }

  startPolling(intervalMs = 30000): void {
    void this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const patternsStore = new PatternsStore();
