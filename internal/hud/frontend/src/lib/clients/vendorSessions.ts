// Cross-vendor transcript client — GET /api/vendor-sessions[/search].
// Bridges to the agent-context vendor session tools, which read the on-disk
// session transcripts of the vendor CLIs (Claude Code, Codex) on the
// workstation running the daemon. Read-only; `degraded: true` marks the
// bridge-unavailable placeholder, distinct from "no sessions found".

export interface VendorSession {
  vendor: string;
  id: string;
  path: string;
  cwd?: string;
  source?: string;
  /** Human handle: the conversation summary or first user prompt line.
   * Best-effort — absent on older senders/transcripts. */
  title?: string;
  /** Non-interactive transcript tag: "automation" (codex scheduled runs) or
   * "sidechain" (claude subagents + codex worker threads). Absent for a
   * normal user chat. */
  kind?: string;
  started_at?: string;
  modified_at: string;
  size_bytes: number;
  /** Source workstation for federated rows (mirror pushes); absent for
   * transcripts read by this HUD's own bridge. */
  host?: string;
}

export interface VendorSessionMatch {
  vendor: string;
  session_id: string;
  path: string;
  cwd?: string;
  /** 0 when the federating mirror tail-seeked a large transcript and the
   * absolute line number is unknown. */
  line: number;
  role?: string;
  timestamp?: string;
  snippet: string;
  host?: string;
}

export interface VendorSessionListResult {
  sessions: VendorSession[];
  count: number;
  degraded: boolean;
}

export interface VendorSessionSearchResult {
  query: string;
  matches: VendorSessionMatch[];
  count: number;
  degraded: boolean;
}

export interface VendorSessionFilters {
  vendor?: 'claude' | 'codex';
  cwdContains?: string;
  sinceHours?: number;
  limit?: number;
}

export interface VendorSessionSearchFilters extends VendorSessionFilters {
  maxResults?: number;
  maxPerSession?: number;
}

async function parseResponse(res: Response): Promise<any> {
  let data: any = null;
  try {
    data = await res.json();
  } catch {
    data = null;
  }
  if (!res.ok) {
    const msg = data?.error || `${res.status} ${res.statusText}`;
    throw new Error(msg);
  }
  return data;
}

function filterParams(filters: VendorSessionFilters): URLSearchParams {
  const params = new URLSearchParams();
  if (filters.vendor) params.set('vendor', filters.vendor);
  if (filters.cwdContains) params.set('cwd_contains', filters.cwdContains);
  if (filters.sinceHours && filters.sinceHours > 0) params.set('since_hours', String(filters.sinceHours));
  if (filters.limit && filters.limit > 0) params.set('limit', String(filters.limit));
  return params;
}

export async function fetchVendorSessions(
  filters: VendorSessionFilters = {},
): Promise<VendorSessionListResult> {
  const params = filterParams(filters);
  const qs = params.size > 0 ? `?${params.toString()}` : '';
  const res = await globalThis.fetch(`/api/vendor-sessions${qs}`);
  const data = await parseResponse(res);
  return {
    sessions: data?.sessions ?? [],
    count: data?.count ?? 0,
    degraded: data?.degraded ?? false,
  };
}

export async function searchVendorSessions(
  query: string,
  filters: VendorSessionSearchFilters = {},
): Promise<VendorSessionSearchResult> {
  const params = filterParams(filters);
  params.set('query', query);
  if (filters.maxResults && filters.maxResults > 0) params.set('max_results', String(filters.maxResults));
  if (filters.maxPerSession && filters.maxPerSession > 0) {
    params.set('max_per_session', String(filters.maxPerSession));
  }
  const res = await globalThis.fetch(`/api/vendor-sessions/search?${params.toString()}`);
  const data = await parseResponse(res);
  return {
    query: data?.query ?? query,
    matches: data?.matches ?? [],
    count: data?.count ?? 0,
    degraded: data?.degraded ?? false,
  };
}
