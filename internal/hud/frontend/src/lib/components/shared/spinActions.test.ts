import { afterEach, describe, expect, it, vi } from 'vitest';
import { submitAsyncSpin } from './spinActions.ts';
import { labsAuthStore } from '../../stores/labsAuth.svelte.ts';

// Regression coverage for "spin failed: invalid admin token".
//
// Spinning is admin-gated at the HUD (requireAdminToken runs before the proxy
// injects the operator bearer). The Spinning Room dialog used to fire a bare
// fetch() with no admin token, so every spin hit that gate and 401'd. The fix
// routes the POST through adminFetch so the Labs access-bar token rides along
// (and an empty bar fails fast, client-side, with no tokenless request).

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

describe('submitAsyncSpin attaches the HUD admin token', () => {
  it('sends X-Admin-Token from the Labs access bar and returns the spin_id', async () => {
    labsAuthStore.adminToken = 'hud-admin-token';

    let postInit: RequestInit | undefined;
    let postUrl = '';
    globalThis.fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      postUrl = typeof input === 'string' ? input : input.toString();
      postInit = init;
      return Promise.resolve(jsonResponse({ spin_id: 'spin-42' }, 202));
    }) as unknown as typeof globalThis.fetch;

    const id = await submitAsyncSpin({ brief: 'b', frame: 'jacquard' });

    expect(id).toBe('spin-42');
    expect(postUrl).toBe('/api/mills/spin/async');
    expect(postInit?.method).toBe('POST');
    expect(headerOf(postInit, 'X-Admin-Token')).toBe('hud-admin-token');
  });

  it('rejects with a clear hint and fires NO request when the bar is empty', async () => {
    labsAuthStore.adminToken = '';
    const spy = vi.fn(() => Promise.resolve(jsonResponse({}, 202)));
    globalThis.fetch = spy as unknown as typeof globalThis.fetch;

    await expect(submitAsyncSpin({ brief: 'b', frame: 'jacquard' })).rejects.toThrow(
      /requires an admin token/i,
    );
    expect(spy).not.toHaveBeenCalled();
  });

  it('maps a 401 to an actionable message (not a bare status)', async () => {
    labsAuthStore.adminToken = 'wrong-token';
    globalThis.fetch = vi.fn(() =>
      Promise.resolve(textResponse('invalid admin token', 401)),
    ) as unknown as typeof globalThis.fetch;

    await expect(submitAsyncSpin({ brief: 'b', frame: 'jacquard' })).rejects.toThrow(
      /admin token missing or invalid — set it in the Labs access bar/,
    );
  });

  it('maps a 404 to the still-deploying message', async () => {
    labsAuthStore.adminToken = 'hud-admin-token';
    globalThis.fetch = vi.fn(() =>
      Promise.resolve(textResponse('not found', 404)),
    ) as unknown as typeof globalThis.fetch;

    await expect(submitAsyncSpin({ brief: 'b', frames: ['a', 'b'] })).rejects.toThrow(
      /still deploying/,
    );
  });
});
