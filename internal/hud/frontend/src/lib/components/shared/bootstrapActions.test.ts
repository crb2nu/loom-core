import { afterEach, describe, expect, it, vi } from 'vitest';
import { submitBootstrap } from './bootstrapActions.ts';
import { labsAuthStore } from '../../stores/labsAuth.svelte.ts';

// Bootstrapping mints a repo + re-scopes a plan and is admin-gated at the HUD
// (requireAdminToken runs before the proxy injects the operator bearer). Route
// the POST through adminFetch so the Labs access-bar token rides along, and an
// empty bar fails fast client-side with no tokenless request.

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  labsAuthStore.adminToken = '';
});

function textResponse(body: string, status: number): Response {
  return new Response(body, { status });
}

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

describe('submitBootstrap attaches the HUD admin token', () => {
  it('sends X-Admin-Token and returns the bootstrap result', async () => {
    labsAuthStore.adminToken = 'hud-admin-token';
    let postInit: RequestInit | undefined;
    let postUrl = '';
    globalThis.fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      postUrl = typeof input === 'string' ? input : input.toString();
      postInit = init;
      return Promise.resolve(
        jsonResponse(
          { project: 'services/procmodel', web_url: 'https://gl/services/procmodel', plan_rescoped: true },
          201,
        ),
      );
    }) as unknown as typeof globalThis.fetch;

    const res = await submitBootstrap({ plan_id: 'plan-abc', path: 'services/procmodel' });

    expect(res.project).toBe('services/procmodel');
    expect(res.plan_rescoped).toBe(true);
    expect(postUrl).toBe('/api/mills/projects/bootstrap');
    expect(postInit?.method).toBe('POST');
    expect(headerOf(postInit, 'X-Admin-Token')).toBe('hud-admin-token');
  });

  it('rejects with a clear hint and fires NO request when the bar is empty', async () => {
    labsAuthStore.adminToken = '';
    const spy = vi.fn(() => Promise.resolve(jsonResponse({}, 201)));
    globalThis.fetch = spy as unknown as typeof globalThis.fetch;

    await expect(submitBootstrap({ plan_id: 'p', path: 'services/x' })).rejects.toThrow(
      /requires an admin token/i,
    );
    expect(spy).not.toHaveBeenCalled();
  });

  it('maps a 503 to the two-key policy hint', async () => {
    labsAuthStore.adminToken = 'hud-admin-token';
    globalThis.fetch = vi.fn(() =>
      Promise.resolve(textResponse('project bootstrap disabled in policy', 503)),
    ) as unknown as typeof globalThis.fetch;

    await expect(submitBootstrap({ plan_id: 'p', path: 'services/x' })).rejects.toThrow(
      /allow_bootstrapped/,
    );
  });

  it('surfaces a 409 conflict body (already scoped / re-mint)', async () => {
    labsAuthStore.adminToken = 'hud-admin-token';
    globalThis.fetch = vi.fn(() =>
      Promise.resolve(textResponse('bootstrap: plan already bootstrapped: services/x', 409)),
    ) as unknown as typeof globalThis.fetch;

    await expect(submitBootstrap({ plan_id: 'p', path: 'services/x' })).rejects.toThrow(
      /already bootstrapped/,
    );
  });
});
