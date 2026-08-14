// Mills Overseers store — supervisory-agent snapshots (groomer / sentinel /
// foreman) from the loom-mills-operator, proxied through
// GET /api/mills/overseers by the HUD's domain/mills package. Polls at 15s by
// default so the cadence agrees with the rest of the Mills panels.
//
// Empty/disabled state: a 503 from the proxy means the HUD has no operator URL
// set; surface that as a calm empty state rather than a fetch error. A hard
// error surfaces via `error` WITHOUT blanking the previously-loaded snapshot,
// so a transient operator blip doesn't blank the panel mid-poll.

import { createPoller } from '../utils/poller.ts';

// OverseerTickResult mirrors overseer.TickResult — the summary of one agent
// tick. snake_case to match the operator JSON so a panel can read a field
// directly without a mapping layer.
export interface OverseerTickResult {
  inspected: number;
  acted: number;
  planned: number;
  skipped: number;
  errored: number;
  note?: string;
}

// OverseerSuppression mirrors overseer.Suppression — the sentinel's live
// admission-suppression lease. Present only when a lease is active.
export interface OverseerSuppression {
  reason: string;
  until: string; // RFC3339 timestamp
}

// OverseerAgent is one row from GET /api/mills/overseers `agents[]`. It flattens
// the operator's overseerAgentView (embedded AgentStatus + policy gates).
export interface OverseerAgent {
  name: string;
  paused: boolean;
  last_tick_at?: string; // RFC3339; omitted before the first tick
  last_result: OverseerTickResult;
  last_error?: string;
  enabled: boolean;
  dry_run: boolean;
  suppression?: OverseerSuppression;
}

// OverseerEvent mirrors store.Event. The Go struct carries no json tags, so it
// serialises with exported (PascalCase) field names — matched here on purpose.
export interface OverseerEvent {
  ID: number;
  OccurredAt: string;
  Actor: string;
  Kind: string;
  SubjectKind: string;
  SubjectID: string;
  Payload: Record<string, unknown> | null;
}

// OverseersStatus is the full GET /api/mills/overseers response.
export interface OverseersStatus {
  enabled: boolean; // master gate
  agents: OverseerAgent[];
  recent_actions: Record<string, OverseerEvent[] | null>;
}

// zeroResult keeps a $derived from ever spreading an undefined last_result when
// an agent snapshot arrives without one.
const zeroResult: OverseerTickResult = {
  inspected: 0,
  acted: 0,
  planned: 0,
  skipped: 0,
  errored: 0,
};

function normalise(raw: OverseersStatus | null): OverseersStatus {
  if (!raw) return { enabled: false, agents: [], recent_actions: {} };
  return {
    enabled: raw.enabled === true,
    agents: (raw.agents ?? []).map((a) => ({
      ...a,
      last_result: a.last_result ?? zeroResult,
    })),
    recent_actions: raw.recent_actions ?? {},
  };
}

class MillsOverseersStore {
  status = $state<OverseersStatus | null>(null);

  loading = $state(false);
  error = $state<string | null>(null);
  disabled = $state(false);
  lastUpdated = $state<Date | null>(null);

  private poller = createPoller(() => {
    void this.refresh();
  }, 15000);

  async refresh(): Promise<void> {
    this.loading = true;
    try {
      const raw = await this.getJSON<OverseersStatus>('/api/mills/overseers');
      this.status = normalise(raw);
      this.lastUpdated = new Date();
      this.disabled = false;
      this.error = null;
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      if (msg.includes('503') || msg.toLowerCase().includes('not configured')) {
        this.disabled = true;
        this.error = null;
      } else {
        this.disabled = false;
        // Keep the last good snapshot so a transient blip doesn't blank the
        // panel — the error banner rides on top of the stale data.
        this.error = msg;
      }
    } finally {
      this.loading = false;
    }
  }

  startPolling(intervalMs = 15000): void {
    void this.refresh();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }

  private async getJSON<T>(path: string): Promise<T | null> {
    const res = await globalThis.fetch(path);
    if (res.status === 503) {
      throw new Error('mills proxy: 503 (operator not configured)');
    }
    if (res.status === 404) {
      return null;
    }
    if (!res.ok) {
      throw new Error(`${path}: ${res.status}`);
    }
    const text = await res.text();
    if (!text) return null;
    return JSON.parse(text) as T;
  }
}

export const millsOverseersStore = new MillsOverseersStore();
