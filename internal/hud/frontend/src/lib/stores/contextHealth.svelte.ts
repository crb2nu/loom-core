// Context-health store — per-agent context-window budget and health.
//
// Backed by internal/hud/domain/context/handlers.go over
// monitor.NewContextHealthMonitor:
//
//   GET  /api/context/health              → ContextHealthSnapshot
//   GET  /api/context/health/{agent_id}   → AgentContextHealth (unused here;
//                                            the full snapshot already carries
//                                            every agent)
//   GET  /api/context/budget              → {agents:[AgentBudget], total_budget,
//                                            total_used}
//   PUT  /api/context/budget/{agent_id}   → {token_budget:int}
//   POST /api/context/compact/{session_id}
//
// Every handler answers 503 {"error":"context health monitor not available"}
// when the monitor is unwired, which is a not-configured state rather than a
// failure — fetchJSON's absentStatuses folds it into `unavailable` so the panel
// can name the cause instead of rendering an empty table.

import { createPoller } from '../utils/poller.ts';
import { errorMessage, fetchJSON } from '../utils/apiJson.ts';
import { adminFetch } from './labsAuth.svelte.ts';

/** One agent's context-window health (monitor.AgentContextHealth). */
export interface AgentContextHealth {
  agent_id: string;
  session_id: string;
  namespace: string;
  token_budget: number;
  tokens_used: number;
  /** 0..1 fraction of the budget consumed. */
  budget_utilization: number;
  /** 0..100 composite of freshness, headroom, efficiency, and coverage. */
  health_score: number;
  compaction_needed: boolean;
  stale_entries: number;
  last_entry_age: string;
  recall_hit_rate: number;
  recommendation?: string;
}

/** monitor.ContextHealthSnapshot. */
export interface ContextHealthSnapshot {
  agents: AgentContextHealth[];
  system_health: number;
  total_budget: number;
  total_used: number;
  compaction_queue: number;
  updated_at: string;
}

/** One row of GET /api/context/budget. */
export interface AgentBudget {
  agent_id: string;
  token_budget: number;
  tokens_used: number;
  budget_utilization: number;
  compaction_needed: boolean;
}

interface BudgetResponse {
  agents?: AgentBudget[] | null;
  total_budget?: number;
  total_used?: number;
}

/** Utilization bands that drive the bar colour and the "needs compaction" cue. */
export function utilizationTone(fraction: number): 'ok' | 'warn' | 'crit' {
  if (fraction >= 0.8) return 'crit';
  if (fraction >= 0.6) return 'warn';
  return 'ok';
}

class ContextHealthStore {
  agents = $state<AgentContextHealth[]>([]);
  systemHealth = $state(0);
  totalBudget = $state(0);
  totalUsed = $state(0);
  compactionQueue = $state(0);
  updatedAt = $state<string | null>(null);

  /** Budget overview (GET /api/context/budget) — kept separate because the
   *  monitor can serve budgets with overrides applied while a health refresh
   *  is still in flight. */
  budgets = $state<AgentBudget[]>([]);

  loading = $state(false);
  error = $state<string | null>(null);
  /** True when the monitor is unwired (503) or the route is absent (404/SPA). */
  unavailable = $state(false);

  /** session_id currently being compacted, for per-row button state. */
  compacting = $state<string | null>(null);

  // 20s: token counts move at session pace, not tick pace.
  private poller = createPoller(() => this.fetch(), 20000);

  /** Agents at or over the 80% compaction threshold. */
  get needingCompaction(): AgentContextHealth[] {
    return this.agents.filter((a) => a.compaction_needed || a.budget_utilization >= 0.8);
  }

  /** Fleet-wide utilization as a 0..1 fraction (0 when no budget is known). */
  get totalUtilization(): number {
    return this.totalBudget > 0 ? this.totalUsed / this.totalBudget : 0;
  }

  /** Budget row for an agent, when the budget endpoint has been read. */
  budgetFor(agentID: string): AgentBudget | null {
    return this.budgets.find((b) => b.agent_id === agentID) ?? null;
  }

  async fetch(): Promise<void> {
    this.loading = true;
    try {
      const [health, budget] = await Promise.all([
        fetchJSON<ContextHealthSnapshot>('/api/context/health', { absentStatuses: [503] }),
        fetchJSON<BudgetResponse>('/api/context/budget', { absentStatuses: [503] }),
      ]);

      if (health === null) {
        this.unavailable = true;
        this.agents = [];
        this.budgets = [];
        this.error = null;
        return;
      }

      this.unavailable = false;
      this.agents = health.agents ?? [];
      this.systemHealth = health.system_health ?? 0;
      this.totalBudget = health.total_budget ?? 0;
      this.totalUsed = health.total_used ?? 0;
      this.compactionQueue = health.compaction_queue ?? 0;
      this.updatedAt = health.updated_at ?? null;
      // The budget endpoint is a strict subset of the health snapshot, so a
      // partial failure there must not blank the panel.
      this.budgets = budget?.agents ?? [];
      this.error = null;
    } catch (e) {
      this.error = errorMessage(e);
    } finally {
      this.loading = false;
    }
  }

  /**
   * Trigger compaction for one session. Throws on failure so callers can drive
   * the shared runAdminAction toast path.
   */
  async compact(sessionID: string): Promise<void> {
    this.compacting = sessionID;
    try {
      const res = await adminFetch(`/api/context/compact/${encodeURIComponent(sessionID)}`, {
        method: 'POST',
      });
      if (!res.ok) {
        const body = await res.text();
        let detail = body.slice(0, 200);
        try {
          const parsed = JSON.parse(body);
          if (parsed && typeof parsed.error === 'string') detail = parsed.error;
        } catch {
          // Non-JSON body; the text prefix stands.
        }
        throw new Error(`Compaction failed (HTTP ${res.status}): ${detail}`);
      }
      await this.fetch();
    } finally {
      this.compacting = null;
    }
  }

  /**
   * Override an agent's token budget. Throws on failure (runAdminAction path).
   */
  async setBudget(agentID: string, tokenBudget: number): Promise<void> {
    const res = await adminFetch(`/api/context/budget/${encodeURIComponent(agentID)}`, {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ token_budget: tokenBudget }),
    });
    if (!res.ok) {
      const body = await res.text();
      let detail = body.slice(0, 200);
      try {
        const parsed = JSON.parse(body);
        if (parsed && typeof parsed.error === 'string') detail = parsed.error;
      } catch {
        // Non-JSON body; the text prefix stands.
      }
      throw new Error(`Budget update failed (HTTP ${res.status}): ${detail}`);
    }
    await this.fetch();
  }

  startPolling(intervalMs = 20000): void {
    void this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const contextHealthStore = new ContextHealthStore();
