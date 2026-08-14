// Blocked-sessions store — "Waiting on you".
//
// GET /api/blocked (internal/hud/api_blocked.go) serves the sessions the
// flightdeck bridge folded out of agent.blocked / agent.unblocked daemon
// events: an agent that has stalled on a permission prompt and is burning wall
// clock until a human answers. The mobile dashboard has surfaced this since
// MBL-1; the desktop HUD shipped the route and then rendered it nowhere, so the
// one signal that literally means "an operator is the blocker" was invisible on
// the operator's own screen.
//
// Wire shape:
//   { "blocked": [BlockedSessionInfo...], "count": N }
// with BlockedSessionInfo = {session_id, agent_id, reason, tool_name?, cwd?,
// since (RFC3339Nano), waited_seconds}. `blocked` is always an array, ordered
// longest wait first, and the backend prunes anything past a 30-minute TTL.

import { createPoller } from '../utils/poller.ts';
import { errorMessage, fetchJSON } from '../utils/apiJson.ts';

export interface BlockedSession {
  session_id: string;
  agent_id: string;
  reason: string;
  tool_name?: string;
  cwd?: string;
  since: string;
  waited_seconds: number;
}

interface BlockedResponse {
  blocked?: BlockedSession[] | null;
  count?: number;
}

class BlockedStore {
  sessions = $state<BlockedSession[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  /** True when this HUD build does not register /api/blocked. */
  unavailable = $state(false);
  lastUpdated = $state<Date | null>(null);

  // 15s: a human-blocked agent is idle the whole time, so the cost of a stale
  // card is real wall clock. Cheap endpoint (in-memory map).
  private poller = createPoller(() => this.fetch(), 15000);

  get count(): number {
    return this.sessions.length;
  }

  /** Longest current wait in seconds (0 when nothing is blocked). */
  get longestWaitSeconds(): number {
    return this.sessions.reduce((max, s) => Math.max(max, s.waited_seconds ?? 0), 0);
  }

  async fetch(): Promise<void> {
    this.loading = true;
    try {
      const data = await fetchJSON<BlockedResponse>('/api/blocked');
      if (data === null) {
        // Route absent on this build — say so instead of claiming "nothing is
        // blocked", which is the same visual as a healthy fleet.
        this.unavailable = true;
        this.sessions = [];
        this.error = null;
        return;
      }
      this.unavailable = false;
      this.sessions = data.blocked ?? [];
      this.error = null;
      this.lastUpdated = new Date();
    } catch (e) {
      // Keep the last known list: an agent blocked a tick ago is still the
      // best available answer to "who is waiting on me".
      this.error = errorMessage(e);
    } finally {
      this.loading = false;
    }
  }

  startPolling(intervalMs = 15000): void {
    void this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const blockedStore = new BlockedStore();
