import { persistGet, persistRemove, persistSet } from './persist.ts';

const STORAGE_KEY = 'labs.admin_token';

class LabsAuthStore {
  adminToken = $state(persistGet<string>(STORAGE_KEY, ''));
  /** null = not checked, true = valid, false = invalid */
  tokenValid = $state<boolean | null>(null);
  validating = $state(false);

  // Tokenless server-side authorization: the HUD grants admin without a pasted
  // token via EITHER Cloudflare Access SSO (Gmail, off-LAN) OR a trusted LAN
  // network (internal ingress, on-LAN). checkAccess() detects either through a
  // tokenless auth-check, so the token bar becomes optional. `accessVia` says
  // which path authorized us ("access" | "network").
  accessAuthorized = $state(false);
  accessVia = $state<string | null>(null);
  accessEmail = $state<string | null>(null);
  accessChecked = $state(false);

  get hasToken(): boolean {
    return this.adminToken.trim().length > 0;
  }

  /** True when the browser can perform admin actions — via a stored token OR a
   *  verified Cloudflare Access SSO identity. */
  get isAdmin(): boolean {
    return this.hasToken || this.accessAuthorized;
  }

  /** Probe /api/labs/auth-check WITHOUT a token: a 200 means the HUD already
   *  authorized us server-side (Cloudflare Access SSO or trusted LAN network).
   *  Safe to call once on load; it never throws and only grants (never revokes)
   *  on success. A `token`-via result is ignored here (we sent none). */
  async checkAccess(): Promise<boolean> {
    try {
      const res = await globalThis.fetch('/api/labs/auth-check', { cache: 'no-store' });
      if (!res.ok) {
        this.accessChecked = true;
        return false;
      }
      const data = (await res.json().catch(() => ({}))) as { via?: string; email?: string };
      // Any tokenless 200 means the server authorized us on its own (access or
      // network) — the token bar is then optional.
      this.accessAuthorized = true;
      this.accessVia = data.via ?? null;
      this.accessEmail = data.email ?? null;
      this.accessChecked = true;
      return true;
    } catch {
      this.accessChecked = true;
      return false;
    }
  }

  setAdminToken(token: string): void {
    this.adminToken = token;
    const trimmed = token.trim();
    if (trimmed) {
      persistSet(STORAGE_KEY, trimmed);
      this.validate();
    } else {
      persistRemove(STORAGE_KEY);
      this.tokenValid = null;
    }
  }

  clearAdminToken(): void {
    this.adminToken = '';
    this.tokenValid = null;
    persistRemove(STORAGE_KEY);
  }

  async validate(): Promise<boolean> {
    const token = this.adminToken.trim();
    if (!token) {
      this.tokenValid = null;
      return false;
    }
    this.validating = true;
    try {
      const res = await globalThis.fetch('/api/labs/auth-check', {
        headers: { 'X-Admin-Token': token },
      });
      this.tokenValid = res.ok;
      return res.ok;
    } catch {
      this.tokenValid = false;
      return false;
    } finally {
      this.validating = false;
    }
  }

  requiredMessage(action: string): string {
    return `${action} requires an admin token.`;
  }
}

export const labsAuthStore = new LabsAuthStore();

export interface AdminFetchInit extends RequestInit {
  requireToken?: boolean;
  action?: string;
}

export async function adminFetch(input: RequestInfo | URL, init: AdminFetchInit = {}): Promise<Response> {
  const {
    requireToken = false,
    action = 'This action',
    headers,
    ...requestInit
  } = init;

  const token = labsAuthStore.adminToken.trim();
  // A verified Cloudflare Access SSO identity satisfies requireToken with no
  // local token — Cloudflare injects the identity header at the edge, so the
  // HUD gate authorizes the request server-side.
  if (requireToken && !token && !labsAuthStore.accessAuthorized) {
    throw new Error(labsAuthStore.requiredMessage(action));
  }

  const nextHeaders = new Headers(headers);
  if (token) {
    nextHeaders.set('X-Admin-Token', token);
  }

  return globalThis.fetch(input, {
    ...requestInit,
    headers: nextHeaders,
  });
}
