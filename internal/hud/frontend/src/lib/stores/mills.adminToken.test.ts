import { afterEach, describe, expect, it, vi } from 'vitest';
import { millsStore } from './mills.svelte.ts';
import { labsAuthStore } from './labsAuth.svelte.ts';

// Regression coverage for the "handoff of slices to mill returned 401 on the
// backlog endpoint" bug.
//
// Mills mutations are double-gated: the HUD's own requireAdminToken gate
// (X-Admin-Token) runs BEFORE the proxy injects the operator's admin bearer.
// `postJSON` used to issue a bare fetch() with no admin token, so every mills
// mutation from this store — createBacklog (the Run-in-Mills slice handoff),
// startPipeline, runCouncil, escalateRun, the kill-switch, policy proposals —
// hit that HUD gate and got 401. The fix routes postJSON through adminFetch so
// the request carries the HUD admin token from the Labs access bar.

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  labsAuthStore.adminToken = '';
  millsStore.backlog = [];
  millsStore.archiveRuns = [];
  millsStore.disabled = false;
});

function jsonResponse(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

function headerOf(init: RequestInit | undefined, name: string): string | null {
  const h = init?.headers;
  if (!h) return null;
  if (h instanceof Headers) return h.get(name);
  const rec = h as Record<string, string>;
  return rec[name] ?? rec[name.toLowerCase()] ?? null;
}

describe('mills mutations attach the HUD admin token', () => {
  it('createBacklog sends X-Admin-Token from the Labs access bar', async () => {
    // Set the token directly rather than via setAdminToken() so we don't
    // trigger its background /api/labs/auth-check validate() fetch.
    labsAuthStore.adminToken = 'hud-admin-token';

    let postInit: RequestInit | undefined;
    globalThis.fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString();
      if (url === '/api/mills/backlog' && init?.method === 'POST') {
        postInit = init;
        return Promise.resolve(jsonResponse({ id: 'bl-x' }, 201));
      }
      // createBacklog calls fetchAll() after the POST — answer its GET reads.
      return Promise.resolve(jsonResponse([], 200));
    }) as unknown as typeof globalThis.fetch;

    const res = await millsStore.createBacklog({ id: 'bl-x', title: 't' });

    expect(res?.id).toBe('bl-x');
    expect(postInit).toBeDefined();
    expect(headerOf(postInit, 'X-Admin-Token')).toBe('hud-admin-token');
  });

  it('rejects with a clear hint (no tokenless request) when the bar is empty', async () => {
    labsAuthStore.adminToken = '';

    const fetchSpy = vi.fn(() =>
      Promise.reject(new Error('fetch must not be called without a token')),
    ) as unknown as typeof globalThis.fetch;
    globalThis.fetch = fetchSpy;

    await expect(
      millsStore.createBacklog({ id: 'bl-y', title: 't' }),
    ).rejects.toThrow(/requires an admin token/i);
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});

describe('gradeRun optimistic archive updates', () => {
  it('updates immediately and supports regrading with the current note', async () => {
    labsAuthStore.adminToken = 'hud-admin-token';
    millsStore.archiveRuns = [{
      ID: 'run-grade', BacklogID: 'bl-grade', Template: 'implement', State: 'done', Attempts: 1,
    }];

    let release!: (response: Response) => void;
    globalThis.fetch = vi.fn(() => new Promise<Response>((resolve) => { release = resolve; })) as typeof globalThis.fetch;
    const first = millsStore.gradeRun('run-grade', 'keep', 'ship more');
    expect(millsStore.archiveRuns[0]).toMatchObject({ Grade: 'keep', GradeNote: 'ship more' });
    release(jsonResponse({ run_id: 'run-grade', grade: 'keep', note: 'ship more' }, 200));
    await first;

    globalThis.fetch = vi.fn(() => Promise.resolve(
      jsonResponse({ run_id: 'run-grade', grade: 'meh', note: 'mixed' }, 200),
    )) as typeof globalThis.fetch;
    await millsStore.gradeRun('run-grade', 'meh', 'mixed');
    expect(millsStore.archiveRuns[0]).toMatchObject({ Grade: 'meh', GradeNote: 'mixed' });
  });

  it('restores the exact previous run object when the request is rejected', async () => {
    labsAuthStore.adminToken = 'hud-admin-token';
    const previous = {
      ID: 'run-grade', BacklogID: 'bl-grade', Template: 'implement', State: 'done', Attempts: 1,
      Grade: 'keep' as const, GradeNote: 'good',
    };
    millsStore.archiveRuns = [previous];
    globalThis.fetch = vi.fn(() => Promise.resolve(jsonResponse({}, 422))) as typeof globalThis.fetch;

    await expect(millsStore.gradeRun('run-grade', 'regret', 'bad')).rejects.toThrow(/422/);
    expect(millsStore.archiveRuns[0]).toBe(previous);
  });
});

describe('fetchArchiveRuns grade projection', () => {
  it('restores persisted backlog grades onto terminal runs', async () => {
    globalThis.fetch = vi.fn((input) => {
      const url = String(input);
      if (url.includes('/pipeline/runs?')) {
        return Promise.resolve(jsonResponse([{
          ID: 'run-grade', BacklogID: 'bl-grade', Template: 'implement', State: 'done', Attempts: 1,
        }], 200));
      }
      return Promise.resolve(jsonResponse([{
        ID: 'bl-grade', Title: 'graded work', State: 'merged', Priority: 'P2',
        Grade: 'keep', GradeNote: 'ship more',
      }], 200));
    }) as typeof globalThis.fetch;

    await expect(millsStore.fetchArchiveRuns()).resolves.toEqual([
      expect.objectContaining({ ID: 'run-grade', Grade: 'keep', GradeNote: 'ship more' }),
    ]);
  });
});
