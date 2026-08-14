// Reasoning store - reasoning chain tracking and visualization
import { actionStore } from './action.svelte.ts';
import { createPoller } from '../utils/poller.ts';

function errorMessage(e: unknown): string {
  if (e instanceof Error) return e.message || 'Unknown error';
  if (typeof e === 'string') return e;
  try { return JSON.stringify(e); } catch { return 'Unknown error'; }
}

export interface ReasoningChain {
  id: string;
  title: string;
  status: string;
  step_count: number;
  confidence?: number;
  created_at: string;
  completed_at?: string;
}

export interface ReasoningStep {
  id: string;
  description: string;
  confidence: number;
  evidence?: string;
  created_at: string;
}

export interface ReasoningChainDetail extends ReasoningChain {
  steps: ReasoningStep[];
}

class ReasoningStore {
  chains = $state<ReasoningChain[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);

  private poller = createPoller(() => this.fetch(), 15000);

  get activeChains(): ReasoningChain[] {
    return this.chains.filter((c) => c.status === 'active' || c.status === 'in_progress');
  }

  get completedChains(): ReasoningChain[] {
    return this.chains.filter((c) => c.status === 'completed');
  }

  async fetch(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const res = await globalThis.fetch('/api/reasoning/chains');
      if (!res.ok) throw new Error(`Reasoning API: ${res.status}`);
      const data = await res.json();
      this.chains = data.chains ?? [];
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  async getChainDetail(id: string): Promise<ReasoningChainDetail | null> {
    try {
      const res = await globalThis.fetch(`/api/reasoning/chains/${id}`);
      if (!res.ok) throw new Error(`Chain detail: ${res.status}`);
      return await res.json();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
      return null;
    }
  }

  async createChain(title: string, description: string): Promise<boolean> {
    const auditId = actionStore.start(`Create reasoning chain → ${title}`, 'ReasoningPanel:create');
    try {
      const res = await globalThis.fetch('/api/reasoning/chains', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title, description }),
      });
      if (!res.ok) throw new Error(`Create chain: ${res.status}`);
      await this.fetch();
      actionStore.succeed(auditId);
      return true;
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      this.error = e instanceof Error ? e.message : String(e);
      return false;
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

export const reasoningStore = new ReasoningStore();
