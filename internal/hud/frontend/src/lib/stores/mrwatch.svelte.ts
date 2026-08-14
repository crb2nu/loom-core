// MR-watch store — the classified merge-request registry and the shepherd's
// auto-action audit log.
//
// Backed by internal/hud/domain/mrwatch (registry types in
// internal/hud/mrwatch/types.go + shepherd.go):
//
//   GET /api/mrwatch/summary → registry.Snapshot
//       {merge_requests:[MergeRequest], counts:{state:int}, last_poll_at,
//        stale:bool, projects:[string]}
//   GET /api/mrwatch/actions → {actions:[ActionRecord], count:int}
//       ActionRecord = {time, repo, mr_iid, branch?, state, action, outcome,
//                       detail?}
//
// Read-only: the shepherd acts on its own bounded budget; the HUD reports what
// it did. Both endpoints guarantee non-nil arrays/maps, but an older HUD build
// registers neither — that resolves to the SPA catch-all (200 + index.html),
// which fetchJSON folds into `unavailable`.

import { createPoller } from '../utils/poller.ts';
import { errorMessage, fetchJSON } from '../utils/apiJson.ts';
import { isLiveMRWatchState } from '../utils/mrwatchStates.ts';

/** registry.MergeRequest. */
export interface MRWatchMergeRequest {
  repo: string;
  iid: number;
  title: string;
  source_branch: string;
  target_branch?: string;
  /** Classification from the bounded mrwatch State taxonomy. */
  state: string;
  reason?: string;
  web_url?: string;
  pipeline_status?: string;
  pipeline_url?: string;
  pipeline_id?: number;
  sha?: string;
  created_at?: string;
  last_transition_at: string;
  stale: boolean;
  /** True only when state is "merged"; retained merged history is terminal. */
  merged?: boolean;
  /** GitLab merge time for a retained merged MR. */
  merged_at?: string;
}

/** registry.Snapshot. */
export interface MRWatchSnapshot {
  merge_requests: MRWatchMergeRequest[];
  counts: Record<string, number>;
  last_poll_at: string;
  stale: boolean;
  projects: string[];
}

/** registry.ActionRecord — one bounded shepherd auto-action. */
export interface MRWatchAction {
  time: string;
  repo: string;
  mr_iid: number;
  branch?: string;
  state: string;
  action: string;
  outcome: string;
  detail?: string;
}

interface ActionsResponse {
  actions?: MRWatchAction[] | null;
  count?: number;
  shepherd_enabled?: boolean;
}

class MRWatchStore {
  mergeRequests = $state<MRWatchMergeRequest[]>([]);
  counts = $state<Record<string, number>>({});
  projects = $state<string[]>([]);
  lastPollAt = $state<string | null>(null);
  /** Registry-level staleness: the last poll failed for ≥1 watched project. */
  stale = $state(false);

  actions = $state<MRWatchAction[]>([]);
  shepherdEnabled = $state(false);

  loading = $state(false);
  error = $state<string | null>(null);
  /** True when this HUD build registers no /api/mrwatch routes. */
  unavailable = $state(false);

  // 30s matches the poller cadence upstream; the registry itself refreshes on
  // its own schedule, so a tighter poll only re-reads the same snapshot.
  private poller = createPoller(() => this.fetch(), 30000);

  /** Open MRs only; merged/closed records are retained history, not live work. */
  get liveMergeRequests(): MRWatchMergeRequest[] {
    return this.mergeRequests.filter((mr) => isLiveMRWatchState(mr.state));
  }

  /** Live MRs that need attention. Go considers only "ok" healthy here. */
  get unhealthyCount(): number {
    return this.liveMergeRequests.filter((mr) => mr.state !== 'ok').length;
  }

  /** Counts as sorted [state, n] pairs, dropping zero buckets. */
  get countPairs(): Array<[string, number]> {
    return Object.entries(this.counts)
      .filter(([, n]) => n > 0)
      .sort((a, b) => b[1] - a[1]);
  }

  /** Audit log newest-first (the endpoint serves it newest-LAST). */
  get recentActions(): MRWatchAction[] {
    return [...this.actions].reverse();
  }

  async fetch(): Promise<void> {
    this.loading = true;
    try {
      const [summary, actions] = await Promise.all([
        fetchJSON<MRWatchSnapshot>('/api/mrwatch/summary'),
        fetchJSON<ActionsResponse>('/api/mrwatch/actions'),
      ]);

      if (summary === null) {
        this.unavailable = true;
        this.mergeRequests = [];
        this.actions = [];
        this.error = null;
        return;
      }

      this.unavailable = false;
      this.mergeRequests = summary.merge_requests ?? [];
      this.counts = summary.counts ?? {};
      this.projects = summary.projects ?? [];
      this.lastPollAt = summary.last_poll_at ?? null;
      this.stale = summary.stale ?? false;
      // The shepherd is independently disable-able: an absent actions endpoint
      // must leave the registry table rendered.
      this.actions = actions?.actions ?? [];
      this.shepherdEnabled = actions?.shepherd_enabled ?? false;
      this.error = null;
    } catch (e) {
      this.error = errorMessage(e);
    } finally {
      this.loading = false;
    }
  }

  startPolling(intervalMs = 30000): void {
    void this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const mrwatchStore = new MRWatchStore();
