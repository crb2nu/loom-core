// Shared JSON fetch boundary for HUD stores that talk to routes an older
// deployment may not register.
//
// Two failure modes look identical to a naive `await res.json()` and must not:
//
//   1. Route absent — the connected HUD build predates the endpoint. Go's mux
//      404s, but the SPA catch-all answers many unknown paths with 200 +
//      index.html, which JSON.parse throws SyntaxError on. Both mean "this
//      deployment has no such endpoint", and a panel should say so rather than
//      render an error or a convincing all-zero dashboard.
//   2. Real failure — a 500/502, a network drop, or a malformed JSON body from
//      a route that DOES exist. That is an error the operator should see.
//
// fetchJSON collapses case 1 to `null` and throws on case 2, so every caller
// gets the same three-way (data / absent / error) contract. The pattern was
// first hand-rolled in mills.svelte.ts's fetchWiring; this is that logic made
// reusable for the blocked / context-health / mrwatch / engrams stores.

/** Thrown when a route exists but answered with a non-2xx status. */
export class HttpError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = 'HttpError';
    this.status = status;
  }
}

export interface FetchJSONOptions extends RequestInit {
  /**
   * Statuses to treat as "route absent" (→ null) in addition to 404. The
   * context-health endpoints answer 503 when the monitor is not wired, which
   * is a not-configured state, not an error.
   */
  absentStatuses?: number[];
}

/**
 * fetchJSON GETs (or otherwise requests) `url` and decodes JSON.
 *
 * Returns `null` when the endpoint is absent on this deployment: a 404, one of
 * `absentStatuses`, an empty body, or a 200 whose body is not JSON (the SPA
 * catch-all serving index.html). Throws HttpError/Error on any other failure.
 */
export async function fetchJSON<T>(url: string, opts: FetchJSONOptions = {}): Promise<T | null> {
  const { absentStatuses = [], ...init } = opts;

  const res = await globalThis.fetch(url, init);

  if (res.status === 404 || absentStatuses.includes(res.status)) return null;

  const body = await res.text();

  if (!res.ok) {
    // Prefer the handler's own {"error": "..."} message over a bare status.
    let detail = body.slice(0, 200);
    try {
      const parsed = JSON.parse(body);
      if (parsed && typeof parsed.error === 'string') detail = parsed.error;
    } catch {
      // Non-JSON error body — the raw text prefix is the best we have.
    }
    throw new HttpError(res.status, detail ? `HTTP ${res.status}: ${detail}` : `HTTP ${res.status}`);
  }

  if (body.trim() === '') return null;

  try {
    return JSON.parse(body) as T;
  } catch {
    // 200 + non-JSON on a 2xx path is the SPA catch-all answering a route this
    // build does not register — same meaning as a 404.
    return null;
  }
}

/** Normalize a thrown value into a display message. */
export function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
