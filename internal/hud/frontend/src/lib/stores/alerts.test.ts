import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { alertsStore, formatGoDuration, isZeroTime } from './alerts.svelte.ts';
import { labsAuthStore } from './labsAuth.svelte.ts';

// Fetch-boundary + action coverage for the alerting/auto-fix store.
//
// Three things here are backend contracts rather than frontend conventions,
// and each has a test that fails if the Go side changes under us:
//
//   1. AutoFixProposal has NO status field. "Pending" is derived from the
//      absence of an execution naming the proposal — approve and reject both
//      append one (autofix.go ExecuteAutoFix / RejectProposal).
//   2. The auto-fix list routes answer 200-with-[] whether the engine is nil
//      or idle, so "not configured" is only observable from a mutation's 503.
//   3. Alert rules carry Go time.Durations, i.e. NANOSECONDS, and a never-
//      fired rule serialises last_fired as Go's zero time.

function jsonResponse(status: number, body: string, contentType = 'application/json'): Response {
  return new Response(body, { status, headers: { 'Content-Type': contentType } });
}

interface RecordedCall {
  url: string;
  method: string;
  body: string | null;
}

let calls: RecordedCall[] = [];

function routeFetch(routes: Record<string, () => Response>): typeof globalThis.fetch {
  return vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    calls.push({
      url,
      method: init?.method ?? 'GET',
      body: typeof init?.body === 'string' ? init.body : null,
    });
    // Longest fragment first so '/api/alerts/rules' isn't swallowed by '/api/alerts'.
    const ordered = Object.entries(routes).sort((a, b) => b[0].length - a[0].length);
    for (const [fragment, make] of ordered) {
      if (url.includes(fragment)) return Promise.resolve(make());
    }
    return Promise.resolve(jsonResponse(404, 'not found', 'text/plain'));
  }) as unknown as typeof globalThis.fetch;
}

const ALERTS_BODY = JSON.stringify({
  alerts: [
    {
      id: 'alert-1',
      rule_id: 'pipeline-stuck',
      rule_name: 'Pipeline Stuck',
      severity: 'warning',
      title: 'Pipeline stuck: services/loom-core',
      message: 'Pipeline 20845 has been running for 30m0s.',
      pipeline: {
        id: 20845,
        project: 'services/loom-core',
        ref: 'feat/x',
        status: 'running',
        url: 'https://gitlab/x/-/pipelines/20845',
      },
      fired_at: '2026-07-25T17:32:15Z',
    },
    {
      id: 'alert-2',
      rule_id: 'consecutive-failures',
      rule_name: 'Consecutive Failures',
      severity: 'critical',
      title: '3 consecutive failures',
      message: 'services/loom-core main failed 3 times in a row.',
      pipeline: { id: 20800, project: 'services/loom-core', ref: 'main', status: 'failed' },
      fired_at: '2026-07-25T16:00:00Z',
    },
    {
      id: 'alert-3',
      rule_id: 'pipeline-failed',
      rule_name: 'Pipeline Failed',
      severity: 'warning',
      title: 'Pipeline failed',
      message: 'already triaged',
      pipeline: { id: 20700, project: 'services/loom-core', ref: 'main', status: 'failed' },
      fired_at: '2026-07-25T15:00:00Z',
      acked_at: '2026-07-25T15:05:00Z',
      acked_by: 'hud-user',
    },
  ],
});

const RULES_BODY = JSON.stringify({
  rules: [
    {
      id: 'pipeline-failed',
      name: 'Pipeline Failed',
      enabled: true,
      condition: { type: 'pipeline_failed', threshold: 0 },
      severity: 'warning',
      cooldown: 300000000000,
      last_fired: '0001-01-01T00:00:00Z',
    },
    {
      id: 'pipeline-stuck',
      name: 'Pipeline Stuck',
      enabled: false,
      condition: { type: 'pipeline_stuck', threshold: 0, duration: 1800000000000 },
      severity: 'warning',
      cooldown: 900000000000,
      last_fired: '2026-07-25T17:32:15Z',
    },
  ],
});

const PROPOSALS_BODY = JSON.stringify({
  proposals: [
    {
      id: 'proposal-2',
      diagnosis_id: 'services/loom-core:20845',
      description: 'Retry the runner-flaked job.',
      strategy: 'retry',
      confidence: 0.9,
      requires_approval: false,
      created_at: '2026-07-25T17:40:00Z',
    },
    {
      id: 'proposal-1',
      diagnosis_id: 'services/loom-core:20800',
      description: 'Fix the failing unit test.',
      strategy: 'agent_fix',
      estimated_files: ['internal/hud/alerting/engine.go'],
      confidence: 0.82,
      requires_approval: true,
      created_at: '2026-07-25T16:10:00Z',
    },
  ],
});

// proposal-1 already has an execution; proposal-2 does not.
const EXECUTIONS_BODY = JSON.stringify({
  executions: [
    {
      id: 'exec-1',
      proposal_id: 'proposal-1',
      status: 'running',
      spawn_id: 'spawn-abc',
      started_at: '2026-07-25T16:11:00Z',
    },
  ],
});

const OK_ROUTES = {
  '/api/alerts/rules': () => jsonResponse(200, RULES_BODY),
  '/api/alerts?limit=100': () => jsonResponse(200, ALERTS_BODY),
  '/api/autofix/proposals': () => jsonResponse(200, PROPOSALS_BODY),
  '/api/autofix/executions': () => jsonResponse(200, EXECUTIONS_BODY),
};

const realFetch = globalThis.fetch;

beforeEach(() => {
  calls = [];
});

afterEach(() => {
  globalThis.fetch = realFetch;
  alertsStore.alerts = [];
  alertsStore.rules = [];
  alertsStore.proposals = [];
  alertsStore.executions = [];
  alertsStore.diagnoses = {};
  alertsStore.error = null;
  alertsStore.unavailable = false;
  alertsStore.autofixUnavailable = false;
  labsAuthStore.accessAuthorized = false;
});

describe('alertsStore.fetch', () => {
  it('parses alerts, rules, proposals, and executions', async () => {
    globalThis.fetch = routeFetch(OK_ROUTES);

    await alertsStore.fetch();

    expect(alertsStore.unavailable).toBe(false);
    expect(alertsStore.alerts).toHaveLength(3);
    expect(alertsStore.rules).toHaveLength(2);
    expect(alertsStore.proposals).toHaveLength(2);
    expect(alertsStore.executions).toHaveLength(1);
    expect(alertsStore.enabledRuleCount).toBe(1);
  });

  it('splits active from acknowledged alerts', async () => {
    globalThis.fetch = routeFetch(OK_ROUTES);

    await alertsStore.fetch();

    // The server withholds resolved alerts, so acked_at is what moves a served
    // alert out of the triage list.
    expect(alertsStore.activeAlerts.map((a) => a.id)).toEqual(['alert-1', 'alert-2']);
    expect(alertsStore.handledAlerts.map((a) => a.id)).toEqual(['alert-3']);
    expect(alertsStore.criticalCount).toBe(1);
    // Only non-zero buckets chip up, largest first.
    expect(alertsStore.severityCounts).toEqual([
      ['warning', 1],
      ['critical', 1],
    ]);
  });

  it('derives pending proposals from the absence of an execution', async () => {
    globalThis.fetch = routeFetch(OK_ROUTES);

    await alertsStore.fetch();

    // AutoFixProposal has no status field: proposal-1 is decided purely because
    // exec-1 names it, and proposal-2 is pending purely because nothing does.
    expect(alertsStore.pendingProposals.map((p) => p.id)).toEqual(['proposal-2']);
    expect(alertsStore.executionsFor('proposal-1').map((e) => e.id)).toEqual(['exec-1']);
    expect(alertsStore.executionsFor('proposal-2')).toEqual([]);
  });

  it('flags endpoint-absent when the alert route is the SPA catch-all', async () => {
    globalThis.fetch = routeFetch({
      '/api/': () => jsonResponse(200, '<!doctype html><html></html>', 'text/html'),
    });

    await alertsStore.fetch();

    expect(alertsStore.unavailable).toBe(true);
    expect(alertsStore.error).toBeNull();
    expect(alertsStore.alerts).toEqual([]);
  });

  it('renders alerts even when the auto-fix routes are absent', async () => {
    globalThis.fetch = routeFetch({
      '/api/alerts/rules': () => jsonResponse(200, RULES_BODY),
      '/api/alerts?limit=100': () => jsonResponse(200, ALERTS_BODY),
      '/api/autofix/': () => jsonResponse(404, 'not found', 'text/plain'),
    });

    await alertsStore.fetch();

    expect(alertsStore.unavailable).toBe(false);
    expect(alertsStore.alerts).toHaveLength(3);
    expect(alertsStore.proposals).toEqual([]);
    expect(alertsStore.executions).toEqual([]);
  });

  it('coerces null arrays so panels never spread a Go nil', async () => {
    globalThis.fetch = routeFetch({
      '/api/alerts/rules': () => jsonResponse(200, JSON.stringify({ rules: null })),
      '/api/alerts?limit=100': () => jsonResponse(200, JSON.stringify({ alerts: null })),
      '/api/autofix/proposals': () => jsonResponse(200, JSON.stringify({ proposals: null })),
      '/api/autofix/executions': () => jsonResponse(200, JSON.stringify({ executions: null })),
    });

    await alertsStore.fetch();

    expect(alertsStore.alerts).toEqual([]);
    expect(alertsStore.rules).toEqual([]);
    expect(alertsStore.proposals).toEqual([]);
    expect(alertsStore.pendingProposals).toEqual([]);
    expect(alertsStore.severityCounts).toEqual([]);
  });

  it('surfaces a hard error without blanking the last snapshot', async () => {
    globalThis.fetch = routeFetch(OK_ROUTES);
    await alertsStore.fetch();

    globalThis.fetch = routeFetch({
      '/api/': () => jsonResponse(502, 'bad gateway', 'text/plain'),
    });
    await alertsStore.fetch();

    expect(alertsStore.error).toContain('502');
    expect(alertsStore.alerts).toHaveLength(3);
  });
});

describe('alertsStore.ack', () => {
  it('posts acked_by to the alert-scoped ack route and refetches', async () => {
    globalThis.fetch = routeFetch({
      ...OK_ROUTES,
      '/ack': () => jsonResponse(200, JSON.stringify({ acked: true, id: 'alert-1' })),
    });

    await alertsStore.ack('alert-1', 'cody');

    const ack = calls.find((c) => c.url.includes('/ack'));
    expect(ack?.url).toContain('/api/alerts/alert-1/ack');
    expect(ack?.method).toBe('POST');
    expect(JSON.parse(ack?.body ?? '{}')).toEqual({ acked_by: 'cody' });
    // The ack mutates engine state, so the store re-reads it.
    expect(calls.some((c) => c.method === 'GET' && c.url.includes('/api/alerts?limit=100'))).toBe(true);
    expect(alertsStore.acking).toBeNull();
  });

  it('throws on a not-found alert so runAdminAction can toast it', async () => {
    globalThis.fetch = routeFetch({
      '/ack': () => jsonResponse(404, JSON.stringify({ error: 'alerting: alert "gone" not found' })),
    });

    await expect(alertsStore.ack('gone')).rejects.toThrow(/not found/);
  });
});

describe('alertsStore.diagnose', () => {
  it('posts project + pipeline_id and stores the diagnosis by alert id', async () => {
    labsAuthStore.accessAuthorized = true;
    const diagnoseBody = JSON.stringify({
      diagnosis: {
        pipeline_id: 20845,
        project: 'services/loom-core',
        root_cause: 'runner lost connection',
        category: 'infra',
        suggested_fix: 'retry the pipeline',
        confidence: 0.91,
        failed_jobs: ['test:go'],
      },
      proposal: {
        id: 'proposal-3',
        diagnosis_id: 'services/loom-core:20845',
        description: 'retry the pipeline',
        strategy: 'retry',
        confidence: 0.91,
        requires_approval: false,
        created_at: '2026-07-25T18:00:00Z',
      },
    });
    globalThis.fetch = routeFetch({
      ...OK_ROUTES,
      '/api/alerts/diagnose': () => jsonResponse(200, diagnoseBody),
    });
    await alertsStore.fetch();

    const alert = alertsStore.alerts[0];
    const result = await alertsStore.diagnose(alert);

    const call = calls.find((c) => c.url.includes('/api/alerts/diagnose'));
    expect(call?.method).toBe('POST');
    expect(JSON.parse(call?.body ?? '{}')).toEqual({
      project: 'services/loom-core',
      pipeline_id: 20845,
    });
    expect(result?.diagnosis.category).toBe('infra');
    expect(alertsStore.diagnoses['alert-1'].proposal?.id).toBe('proposal-3');
    expect(alertsStore.diagnosing).toBeNull();
  });

  it('flips autofixUnavailable on the not-configured 503', async () => {
    labsAuthStore.accessAuthorized = true;
    globalThis.fetch = routeFetch({
      ...OK_ROUTES,
      '/api/alerts/diagnose': () =>
        jsonResponse(503, JSON.stringify({ error: 'auto-fix engine not configured' })),
    });
    await alertsStore.fetch();

    // Every shipped HUD lands here: App.autofixEngine is never assigned, so
    // the mutation is the only thing that can reveal it.
    await expect(alertsStore.diagnose(alertsStore.alerts[0])).rejects.toThrow(/not configured/);
    expect(alertsStore.autofixUnavailable).toBe(true);
  });

  it('refuses to fire without an admin credential', async () => {
    globalThis.fetch = routeFetch(OK_ROUTES);
    await alertsStore.fetch();

    // POST /api/alerts/diagnose is admin-gated server-side; adminFetch refuses
    // locally rather than burning a round trip on a guaranteed 401.
    await expect(alertsStore.diagnose(alertsStore.alerts[0])).rejects.toThrow(/admin token/);
    expect(calls.some((c) => c.url.includes('/api/alerts/diagnose'))).toBe(false);
  });
});

describe('alertsStore.approve / reject', () => {
  it('approves through the proposal-scoped route and refetches', async () => {
    labsAuthStore.accessAuthorized = true;
    globalThis.fetch = routeFetch({
      ...OK_ROUTES,
      '/approve': () =>
        jsonResponse(202, JSON.stringify({ execution: { id: 'exec-2', proposal_id: 'proposal-2' } })),
    });

    await alertsStore.approve('proposal-2');

    const call = calls.find((c) => c.url.includes('/approve'));
    expect(call?.url).toContain('/api/autofix/proposals/proposal-2/approve');
    expect(call?.method).toBe('POST');
    expect(alertsStore.busyProposal).toBeNull();
  });

  it('rejects without requiring a token, matching the ungated handler', async () => {
    globalThis.fetch = routeFetch({
      ...OK_ROUTES,
      '/reject': () =>
        jsonResponse(200, JSON.stringify({ rejected: true, proposal_id: 'proposal-2' })),
    });

    // handleRejectProposal is the one auto-fix mutation with no
    // RequireAdminToken call, so the store must not gate it locally either.
    await alertsStore.reject('proposal-2');

    const call = calls.find((c) => c.url.includes('/reject'));
    expect(call?.url).toContain('/api/autofix/proposals/proposal-2/reject');
    expect(call?.method).toBe('POST');
  });
});

describe('Go wire-format helpers', () => {
  it('renders time.Duration nanoseconds as human spans', () => {
    expect(formatGoDuration(300000000000)).toBe('5m');
    expect(formatGoDuration(1800000000000)).toBe('30m');
    expect(formatGoDuration(7200000000000)).toBe('2h');
    expect(formatGoDuration(0)).toBe('—');
    expect(formatGoDuration(undefined)).toBe('—');
  });

  it('recognises Go zero time as never-fired', () => {
    expect(isZeroTime('0001-01-01T00:00:00Z')).toBe(true);
    expect(isZeroTime('')).toBe(true);
    expect(isZeroTime('2026-07-25T17:32:15Z')).toBe(false);
  });
});
