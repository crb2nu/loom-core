import { afterEach, describe, expect, it, vi } from 'vitest';
import { contextHealthStore, utilizationTone } from './contextHealth.svelte.ts';

// Fetch-boundary coverage for the context-health domain.
//
// Two shapes matter. First, every handler answers 503
// {"error":"context health monitor not available"} when the monitor is
// unwired — a not-configured state that must read as "unavailable", not as an
// error or an empty fleet. Second, /api/context/health and
// /api/context/budget are fetched together, and the budget endpoint is a
// strict subset: losing it must not blank the per-agent table.

function jsonResponse(status: number, body: string, contentType = 'application/json'): Response {
  return new Response(body, { status, headers: { 'Content-Type': contentType } });
}

/** Route-aware fetch stub: maps a URL substring to its response factory. */
function routeFetch(routes: Record<string, () => Response>): typeof globalThis.fetch {
  return vi.fn((input: RequestInfo | URL) => {
    const url = String(input);
    for (const [fragment, make] of Object.entries(routes)) {
      if (url.includes(fragment)) return Promise.resolve(make());
    }
    return Promise.resolve(jsonResponse(404, 'not found', 'text/plain'));
  }) as unknown as typeof globalThis.fetch;
}

const HEALTH_BODY = JSON.stringify({
  agents: [
    {
      agent_id: 'claude-code',
      session_id: 'sess-1',
      namespace: 'loom-core/feature',
      token_budget: 100000,
      tokens_used: 85000,
      budget_utilization: 0.85,
      health_score: 42,
      compaction_needed: true,
      stale_entries: 7,
      last_entry_age: '12m',
      recall_hit_rate: 0.61,
      recommendation: 'Compact: 85% of budget consumed',
    },
    {
      agent_id: 'codex',
      session_id: 'sess-2',
      namespace: 'loom-mills',
      token_budget: 100000,
      tokens_used: 1000,
      budget_utilization: 0.01,
      health_score: 91,
      compaction_needed: false,
      stale_entries: 0,
      last_entry_age: '42s',
      recall_hit_rate: 0,
    },
  ],
  system_health: 66,
  total_budget: 200000,
  total_used: 86000,
  compaction_queue: 1,
  updated_at: '2026-07-25T10:00:00Z',
});

const BUDGET_BODY = JSON.stringify({
  agents: [
    {
      agent_id: 'claude-code',
      token_budget: 100000,
      tokens_used: 85000,
      budget_utilization: 0.85,
      compaction_needed: true,
    },
  ],
  total_budget: 200000,
  total_used: 86000,
});

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  contextHealthStore.agents = [];
  contextHealthStore.budgets = [];
  contextHealthStore.error = null;
  contextHealthStore.unavailable = false;
});

describe('contextHealthStore.fetch', () => {
  it('parses the health snapshot and the budget overview together', async () => {
    globalThis.fetch = routeFetch({
      '/api/context/health': () => jsonResponse(200, HEALTH_BODY),
      '/api/context/budget': () => jsonResponse(200, BUDGET_BODY),
    });

    await contextHealthStore.fetch();

    expect(contextHealthStore.unavailable).toBe(false);
    expect(contextHealthStore.agents).toHaveLength(2);
    expect(contextHealthStore.systemHealth).toBe(66);
    expect(contextHealthStore.totalBudget).toBe(200000);
    expect(contextHealthStore.totalUsed).toBe(86000);
    expect(contextHealthStore.compactionQueue).toBe(1);
    expect(contextHealthStore.totalUtilization).toBeCloseTo(0.43);
    // Both the explicit flag and the 80% threshold count as needing compaction.
    expect(contextHealthStore.needingCompaction.map((a) => a.agent_id)).toEqual(['claude-code']);
    expect(contextHealthStore.budgetFor('claude-code')?.token_budget).toBe(100000);
    expect(contextHealthStore.budgetFor('codex')).toBeNull();
  });

  it('reads a 503 from the health endpoint as unavailable, not an error', async () => {
    globalThis.fetch = routeFetch({
      '/api/context/health': () =>
        jsonResponse(503, JSON.stringify({ error: 'context health monitor not available' })),
      '/api/context/budget': () =>
        jsonResponse(503, JSON.stringify({ error: 'context health monitor not available' })),
    });

    await contextHealthStore.fetch();

    expect(contextHealthStore.unavailable).toBe(true);
    expect(contextHealthStore.error).toBeNull();
    expect(contextHealthStore.agents).toEqual([]);
  });

  it('keeps the agent table when only the budget endpoint is missing', async () => {
    globalThis.fetch = routeFetch({
      '/api/context/health': () => jsonResponse(200, HEALTH_BODY),
      '/api/context/budget': () => jsonResponse(404, 'not found', 'text/plain'),
    });

    await contextHealthStore.fetch();

    expect(contextHealthStore.unavailable).toBe(false);
    expect(contextHealthStore.agents).toHaveLength(2);
    expect(contextHealthStore.budgets).toEqual([]);
  });

  it('surfaces a hard error from the health endpoint', async () => {
    globalThis.fetch = routeFetch({
      '/api/context/health': () => jsonResponse(500, 'boom', 'text/plain'),
      '/api/context/budget': () => jsonResponse(200, BUDGET_BODY),
    });

    await contextHealthStore.fetch();

    expect(contextHealthStore.error).toContain('500');
    expect(contextHealthStore.unavailable).toBe(false);
  });
});

describe('contextHealthStore.compact', () => {
  it('throws with the handler message so runAdminAction can toast it', async () => {
    globalThis.fetch = routeFetch({
      '/api/context/compact/': () =>
        jsonResponse(502, JSON.stringify({ error: 'compaction failed' })),
    });

    await expect(contextHealthStore.compact('sess-1')).rejects.toThrow(/compaction failed/);
    // The per-row spinner must clear even on the failure path.
    expect(contextHealthStore.compacting).toBeNull();
  });

  it('refreshes the snapshot after a successful compaction', async () => {
    globalThis.fetch = routeFetch({
      '/api/context/compact/': () =>
        jsonResponse(200, JSON.stringify({ status: 'compaction_triggered', session_id: 'sess-1' })),
      '/api/context/health': () => jsonResponse(200, HEALTH_BODY),
      '/api/context/budget': () => jsonResponse(200, BUDGET_BODY),
    });

    await contextHealthStore.compact('sess-1');

    expect(contextHealthStore.agents).toHaveLength(2);
    expect(contextHealthStore.compacting).toBeNull();
  });
});

describe('utilizationTone', () => {
  it('bands utilization at the monitor thresholds', () => {
    expect(utilizationTone(0)).toBe('ok');
    expect(utilizationTone(0.59)).toBe('ok');
    expect(utilizationTone(0.6)).toBe('warn');
    expect(utilizationTone(0.79)).toBe('warn');
    // 0.8 is DefaultCompactionThresh — the point auto-compaction fires.
    expect(utilizationTone(0.8)).toBe('crit');
    expect(utilizationTone(1.2)).toBe('crit');
  });
});
