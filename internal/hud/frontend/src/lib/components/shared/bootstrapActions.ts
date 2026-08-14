// Plan→repo bootstrap request — extracted from BootstrapRepoDialog so the
// admin-token attach + error mapping are unit-testable (no component-test
// harness).
//
// Bootstrapping mints a GitLab project AND re-scopes the plan, so the operator
// DOUBLE-gates it: the HUD's requireAdminToken check runs before the proxy
// injects the operator bearer (internal/hud/domain/mills/{mills,proxy}.go). A
// bare fetch reaches that gate with no token and 401s — route through
// adminFetch so the Labs access-bar token rides along and an empty bar fails
// fast with a clear message. Mirrors submitAsyncSpin in spinActions.ts.
import { adminFetch } from '../../stores/labsAuth.svelte.ts';

export interface BootstrapRequestBody {
  /** The Spinning Room plan the new repo hosts. */
  plan_id: string;
  /** Full GitLab project path to mint, e.g. "services/procmodel". */
  path: string;
  /** Optional GitLab project description (defaults to the plan title). */
  description?: string;
  /** Optional visibility ("private" default). */
  visibility?: string;
}

export interface BootstrapResult {
  project: string;
  web_url: string;
  plan_id: string;
  namespace: string;
  seed_commit: string;
  plan_rescoped: boolean;
  warning?: string;
}

/**
 * Mint a repo from a plan (POST /api/mills/projects/bootstrap → 201). Returns
 * the bootstrap result. Throws a user-facing message on failure:
 * - empty Labs token → client-side, no network hit
 * - 400 → invalid path/plan (the server body explains which)
 * - 409 → plan already scoped elsewhere, or the path was minted before
 * - 401/403 → admin token missing/invalid
 * - 503 → not wired, or the two-key policy gate is off
 * - otherwise the server body or `HTTP <status>`
 */
export async function submitBootstrap(body: BootstrapRequestBody): Promise<BootstrapResult> {
  const res = await adminFetch('/api/mills/projects/bootstrap', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
    requireToken: true,
    action: 'Bootstrapping a repo',
  });
  if (!res.ok) {
    if (res.status === 401 || res.status === 403) {
      throw new Error('admin token missing or invalid — set it in the Labs access bar');
    }
    if (res.status === 503) {
      throw new Error(
        'repo bootstrap is off — enable cross_repo.enabled + cross_repo.allow_bootstrapped in Mills policy (or the operator is still deploying)'
      );
    }
    const msg = (await res.text()).trim();
    throw new Error(msg || `HTTP ${res.status}`);
  }
  return (await res.json()) as BootstrapResult;
}
