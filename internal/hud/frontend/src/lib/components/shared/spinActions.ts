// Spinning Room async-spin request — extracted from SpinPlanDialog so the
// admin-token attach is unit-testable (there's no component-test harness).
//
// Spinning mutates the Plan Store, so the operator DOUBLE-gates it: the HUD's
// own requireAdminToken check (X-Admin-Token) runs BEFORE the proxy injects the
// operator bearer (internal/hud/domain/mills/{mills,proxy}.go). A bare fetch
// reaches that gate with no token and 401s "invalid admin token" — the same
// class of bug the Run-in-Mills store fix cleaned up. Route through adminFetch
// so the Labs access-bar token rides along, and an empty bar fails fast with a
// clear message instead of a confusing server 401.
import { adminFetch } from '../../stores/labsAuth.svelte.ts';

export interface SpinRequestBody {
  brief: string;
  /** Single-frame spin keeps the legacy {frame} shape. */
  frame?: string;
  /** 2+ frames switch to competitive {frames}. */
  frames?: string[];
  priority?: string;
  project?: string;
  namespace?: string;
  /** Respin: link the fresh draft back to the plan it redoes. */
  respun_from?: string;
}

/**
 * Fire an async spin (POST /api/mills/spin/async → 202 + spin_id). Returns the
 * spin_id to poll. Throws a user-facing message on failure:
 * - empty Labs token → "Spinning a plan requires an admin token." (client-side,
 *   no network hit)
 * - 401/403 → "admin token missing or invalid — set it in the Labs access bar"
 * - 404 → operator still deploying
 * - otherwise the server body or `HTTP <status>`
 */
export async function submitAsyncSpin(body: SpinRequestBody): Promise<string> {
  const res = await adminFetch('/api/mills/spin/async', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
    requireToken: true,
    action: 'Spinning a plan',
  });
  if (!res.ok) {
    if (res.status === 404) {
      throw new Error('async spins need the operator update (still deploying) — try again shortly');
    }
    if (res.status === 401 || res.status === 403) {
      throw new Error('admin token missing or invalid — set it in the Labs access bar');
    }
    const msg = (await res.text()).trim();
    throw new Error(msg || `HTTP ${res.status}`);
  }
  const data = await res.json();
  return data.spin_id as string;
}
