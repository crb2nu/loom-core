// Vendor-sessions store — the on-disk Claude Code / Codex transcripts of
// this workstation plus federated Mac hosts, via /api/vendor-sessions
// (internal/hud/domain/vendorsessions).
//
// One shared instance feeds both the Sessions panel (full browser) and the
// Operator Deck (title enrichment for hash-named agent rows). Transcripts
// change slowly, so the poll is a lazy 60s and page-scoped: panels
// startPolling on mount and stopPolling on destroy, per the HUD store
// conventions (poll-gated staleness, no initial poller tick).

import { createPoller } from '../utils/poller.ts';
import { errorMessage } from '../utils/apiJson.ts';
import { fetchVendorSessions, type VendorSession } from '../clients/vendorSessions.ts';
import { arraysEqualByKey } from '../utils/diff.ts';

/** Sessions fetched per poll: enough for every repo group + the linkage
 * index without turning the list endpoint into a bulk export. */
const FETCH_LIMIT = 120;

class VendorSessionsStore {
  sessions = $state<VendorSession[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  /** True when NO transcript source exists (no local bridge, no live mirror
   * host) — distinct from "zero sessions found". */
  degraded = $state(false);
  lastUpdated = $state<number | null>(null);

  private poller = createPoller(() => this.fetch(), 60000);

  async fetch(): Promise<void> {
    this.loading = true;
    try {
      const res = await fetchVendorSessions({ limit: FETCH_LIMIT });
      // Hash-gate the array replace so a metadata-identical poll doesn't
      // churn identity through every derived group/linkage (fleet store
      // invariant #2 — the flicker fix).
      if (
        !arraysEqualByKey(
          this.sessions,
          res.sessions,
          (s) => `${s.vendor}:${s.id}:${s.modified_at}:${s.size_bytes}:${s.host ?? ''}`,
        )
      ) {
        this.sessions = res.sessions;
      }
      this.degraded = res.degraded;
      this.error = null;
      this.lastUpdated = Date.now();
    } catch (e) {
      this.error = errorMessage(e);
    } finally {
      this.loading = false;
    }
  }

  startPolling(intervalMs = 60000): void {
    void this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const vendorSessionsStore = new VendorSessionsStore();
