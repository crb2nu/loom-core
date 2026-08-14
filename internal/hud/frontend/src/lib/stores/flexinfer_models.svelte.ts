// FlexInfer model registry — read-only mirror of the proxy's /v1/models
// snapshot, gated through the HUD endpoint /api/aimodels/flexinfer/models.
//
// Panels that render model names (Mills Audit pool, Council debate
// participants) use `statusFor(modelId)` to tag each name with its
// current Ready/Idle/Unknown state. Without this, operators reading a
// pool of qwen3-8b + qwen3-14b can't tell at a glance whether the
// dispatch is failing because those models are absent — they just see
// score=0 / cost=$0.

import { createPoller } from '../utils/poller.ts';

export type ModelStatus = 'ready' | 'idle' | 'unknown';

export interface FlexInferModelEntry {
  id: string;
  ready: boolean;
  phase?: string;
}

interface FlexInferModelsResponse {
  models: FlexInferModelEntry[];
  source: string;
  error?: string;
}

class FlexInferModelsStore {
  models = $state<FlexInferModelEntry[]>([]);
  source = $state('');
  error = $state<string | null>(null);
  loading = $state(false);
  lastUpdated = $state<Date | null>(null);

  private poller = createPoller(() => { void this.fetch(); }, 60_000);
  private byId = $state<Map<string, FlexInferModelEntry>>(new Map());

  /** Returns 'ready' | 'idle' | 'unknown' for the given model name.
   *  Unknown covers both "model not in registry" and "registry not yet
   *  fetched", since panels should render the same affordance either way
   *  (a neutral marker) rather than misleadingly green or red. */
  statusFor(modelId: string): ModelStatus {
    if (!modelId) return 'unknown';
    const m = this.byId.get(modelId);
    if (!m) return 'unknown';
    return m.ready ? 'ready' : 'idle';
  }

  async fetch(): Promise<void> {
    this.loading = true;
    try {
      const res = await globalThis.fetch('/api/aimodels/flexinfer/models');
      if (!res.ok) throw new Error(`flexinfer models: ${res.status}`);
      const body = (await res.json()) as FlexInferModelsResponse;
      this.models = body.models ?? [];
      this.source = body.source ?? '';
      this.error = body.error ?? null;
      const next = new Map<string, FlexInferModelEntry>();
      for (const m of this.models) next.set(m.id, m);
      this.byId = next;
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  // Model registry shifts on the order of cluster deploys — minutes,
  // not seconds. 60s polling keeps the network footprint small while
  // still catching the operator's "I just deployed a new model" moment
  // within a reasonable window.
  startPolling(intervalMs = 60_000): void {
    void this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const flexinferModelsStore = new FlexInferModelsStore();
