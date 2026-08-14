import { afterEach, describe, expect, it, vi } from 'vitest';
import { adminFetch, labsAuthStore } from './labsAuth.svelte.ts';

// Cloudflare Access SSO → HUD admin (frontend half). When the HUD is reached
// through Cloudflare Access, /api/labs/auth-check returns 200 {via:'access'}
// WITHOUT a token, so the token bar becomes optional and adminFetch's
// requireToken is satisfied by the SSO identity.

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
  labsAuthStore.adminToken = '';
  labsAuthStore.accessAuthorized = false;
  labsAuthStore.accessVia = null;
  labsAuthStore.accessEmail = null;
  labsAuthStore.accessChecked = false;
});

function jsonResponse(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

describe('Cloudflare Access SSO admin', () => {
  it('checkAccess grants admin from a tokenless auth-check', async () => {
    globalThis.fetch = vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString();
      expect(url).toBe('/api/labs/auth-check');
      return Promise.resolve(jsonResponse({ valid: true, via: 'access', email: 'cody@gmail.com' }, 200));
    }) as unknown as typeof globalThis.fetch;

    const ok = await labsAuthStore.checkAccess();

    expect(ok).toBe(true);
    expect(labsAuthStore.accessAuthorized).toBe(true);
    expect(labsAuthStore.accessEmail).toBe('cody@gmail.com');
    expect(labsAuthStore.isAdmin).toBe(true);
  });

  it('checkAccess grants admin on the trusted-network path (via=network)', async () => {
    globalThis.fetch = vi.fn(() =>
      Promise.resolve(jsonResponse({ valid: true, via: 'network', client_ip: '192.168.50.153' }, 200)),
    ) as unknown as typeof globalThis.fetch;

    const ok = await labsAuthStore.checkAccess();

    expect(ok).toBe(true);
    expect(labsAuthStore.accessAuthorized).toBe(true);
    expect(labsAuthStore.accessVia).toBe('network');
    expect(labsAuthStore.isAdmin).toBe(true);
  });

  it('checkAccess does NOT grant admin on 401 (no SSO identity)', async () => {
    globalThis.fetch = vi.fn(() =>
      Promise.resolve(jsonResponse({ error: 'invalid admin token' }, 401)),
    ) as unknown as typeof globalThis.fetch;

    const ok = await labsAuthStore.checkAccess();

    expect(ok).toBe(false);
    expect(labsAuthStore.accessAuthorized).toBe(false);
    expect(labsAuthStore.isAdmin).toBe(false);
  });

  it('adminFetch(requireToken) succeeds with NO token when Access-authorized', async () => {
    labsAuthStore.accessAuthorized = true;

    let sentInit: RequestInit | undefined;
    globalThis.fetch = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
      sentInit = init;
      return Promise.resolve(jsonResponse({ ok: true }, 200));
    }) as unknown as typeof globalThis.fetch;

    // No token in the bar, but requireToken must NOT throw — the SSO identity
    // (injected server-side by Cloudflare) authorizes the request.
    const res = await adminFetch('/api/mills/spin/async', {
      method: 'POST',
      requireToken: true,
      action: 'Spinning a plan',
    });

    expect(res.ok).toBe(true);
    // No local token → no X-Admin-Token header (Cloudflare injects identity).
    const h = new Headers(sentInit?.headers);
    expect(h.get('X-Admin-Token')).toBeNull();
  });

  it('adminFetch(requireToken) still throws when neither token nor Access', async () => {
    labsAuthStore.accessAuthorized = false;
    const spy = vi.fn(() => Promise.resolve(jsonResponse({}, 200)));
    globalThis.fetch = spy as unknown as typeof globalThis.fetch;

    await expect(
      adminFetch('/api/mills/spin/async', { method: 'POST', requireToken: true, action: 'Spinning a plan' }),
    ).rejects.toThrow(/requires an admin token/i);
    expect(spy).not.toHaveBeenCalled();
  });
});
