// Forge store — the HUD daemon's view of GitLab and GitHub (internal/hud/
// domain/forge): who the operator is on each forge, the groups a project can
// be created under, and repo listings for the intake picker.
//
// Credentials never reach the browser. The daemon resolves them from env or
// the local glab/gh logins; this store only sees the non-secret status and
// the connect command when a provider is missing.

export type ForgeProvider = 'gitlab' | 'github';

export interface ForgeStatus {
  provider: ForgeProvider;
  configured: boolean;
  source?: string; // env | glab | gh
  host: string;
  api_url: string;
  user?: string;
  connect_hint?: string;
  error?: string;
  cli_present: boolean;
}

export interface ForgeRepo {
  provider: ForgeProvider;
  path: string;
  name: string;
  web_url: string;
  visibility: string;
  default_branch?: string;
  description?: string;
  last_activity?: string;
  archived: boolean;
  project_id?: number;
}

export interface ForgeGroup {
  id: number;
  full_path: string;
  name: string;
  web_url?: string;
}

export interface ForgeRepoPage {
  repos: ForgeRepo[];
  page: number;
  hasMore: boolean;
}

/** Error carrying the provider's connect hint when it is not connected. */
export class ForgeNotConnectedError extends Error {
  constructor(
    public provider: ForgeProvider,
    public connectHint: string,
  ) {
    super(`${provider} is not connected`);
  }
}

async function readError(res: Response): Promise<never> {
  let body = '';
  try {
    body = await res.text();
  } catch {
    // fall through
  }
  try {
    const parsed = JSON.parse(body) as { error?: string; provider?: ForgeProvider; connect_hint?: string };
    if (res.status === 503 && parsed.provider && parsed.connect_hint) {
      throw new ForgeNotConnectedError(parsed.provider, parsed.connect_hint);
    }
    if (parsed.error) throw new Error(parsed.error);
  } catch (e) {
    if (e instanceof ForgeNotConnectedError || (e instanceof Error && e.message !== body)) throw e;
  }
  throw new Error(body.trim() || `HTTP ${res.status}`);
}

class ForgeStore {
  auth = $state<Record<ForgeProvider, ForgeStatus> | null>(null);
  authLoading = $state(false);
  authError = $state<string | null>(null);
  /** false once /api/forge/auth 404s — an older daemon without the domain. */
  available = $state(true);

  groups = $state<ForgeGroup[]>([]);
  groupsLoading = $state(false);
  groupsError = $state<string | null>(null);

  get gitlab(): ForgeStatus | null {
    return this.auth?.gitlab ?? null;
  }
  get github(): ForgeStatus | null {
    return this.auth?.github ?? null;
  }

  async fetchAuth(): Promise<void> {
    this.authLoading = true;
    this.authError = null;
    try {
      const res = await globalThis.fetch('/api/forge/auth', { cache: 'no-store' });
      if (res.status === 404) {
        this.available = false;
        return;
      }
      if (!res.ok) await readError(res);
      const data = (await res.json()) as { providers: Record<ForgeProvider, ForgeStatus> };
      this.auth = data.providers;
      this.available = true;
    } catch (e) {
      this.authError = e instanceof Error ? e.message : String(e);
    } finally {
      this.authLoading = false;
    }
  }

  async fetchGroups(): Promise<void> {
    this.groupsLoading = true;
    this.groupsError = null;
    try {
      const res = await globalThis.fetch('/api/forge/groups', { cache: 'no-store' });
      if (!res.ok) await readError(res);
      const data = (await res.json()) as { groups: ForgeGroup[] };
      this.groups = data.groups ?? [];
    } catch (e) {
      this.groupsError = e instanceof Error ? e.message : String(e);
    } finally {
      this.groupsLoading = false;
    }
  }

  async listRepos(provider: ForgeProvider, query = '', page = 1): Promise<ForgeRepoPage> {
    const q = new URLSearchParams({ provider, page: String(page) });
    if (query.trim()) q.set('q', query.trim());
    const res = await globalThis.fetch(`/api/forge/repos?${q.toString()}`, { cache: 'no-store' });
    if (!res.ok) await readError(res);
    const data = (await res.json()) as { repos: ForgeRepo[]; page: number; has_more: boolean };
    return { repos: data.repos ?? [], page: data.page ?? page, hasMore: !!data.has_more };
  }
}

export const forgeStore = new ForgeStore();
