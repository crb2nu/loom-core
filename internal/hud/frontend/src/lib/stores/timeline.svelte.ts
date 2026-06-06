// Timeline store - backed by initial HTTP load + SSE subscription.
import { eventStore } from './events.svelte.ts';
import { isStaleFromTimestamp, stalenessStore } from './staleness.svelte.ts';

export interface TimelineEntry {
  timestamp: string;
  event_type: string;
  agent_id?: string;
  agent_type?: string;
  data?: Record<string, unknown>;
}

class TimelineStore {
  entries = $state<TimelineEntry[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);

  // Staleness — mirror StreamStore. Timeline gets a fresh entry on every
  // agent.* SSE event (heartbeat ~15s, tasks/sessions far less often), so
  // a longer window is appropriate here than for the every-5s stream;
  // 120s without any update means SSE is silently failing or no agent
  // events have hit the bus in a long time.
  staleAfter = 120_000;
  get isStale(): boolean {
    return isStaleFromTimestamp(this.lastUpdated, this.staleAfter);
  }

  private eventUnsubs: Array<() => void> = [];
  private pollTimer: ReturnType<typeof setInterval> | null = null;

  async fetch(limit = 200): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const res = await globalThis.fetch(`/api/timeline?limit=${limit}`);
      if (!res.ok) throw new Error(`Timeline API: ${res.status}`);
      const data = await res.json();
      this.entries = data.entries ?? [];
      this.lastUpdated = new Date();
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    } finally {
      this.loading = false;
    }
  }

  startPolling(intervalMs = 30000): void {
    this.stopPolling();
    this.fetch();

    // 30s fallback poll.
    this.pollTimer = setInterval(() => { if (!eventStore.connected) this.fetch(); }, intervalMs);

    // Subscribe to all agent.* SSE events for live updates.
    const agentEvents = [
      'agent.session.start', 'agent.session.end', 'agent.session.reaped',
      'agent.heartbeat', 'agent.task.update', 'agent.task.dispatched',
      'hud.conflict', 'hud.approval_needed', 'hud.claim.released',
    ];

    for (const eventType of agentEvents) {
      this.eventUnsubs.push(
        eventStore.on(eventType, (e) => {
          const entry: TimelineEntry = {
            timestamp: new Date().toISOString(),
            event_type: eventType,
            agent_id: (e.data as Record<string, unknown>)?.agent_id as string | undefined,
            agent_type: (e.data as Record<string, unknown>)?.agent_type as string | undefined,
            data: e.data as Record<string, unknown>,
          };
          // Prepend (newest first) and cap at 500. Bump lastUpdated so
          // SSE-driven entries clear the staleness signal too — without
          // it, the only thing keeping isStale low was the 30s fallback
          // poll, which defeats the point of having SSE.
          this.entries = [entry, ...this.entries].slice(0, 500);
          this.lastUpdated = new Date();
        }),
      );
    }
  }

  stopPolling(): void {
    if (this.pollTimer) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
    for (const unsub of this.eventUnsubs) unsub();
    this.eventUnsubs = [];
  }
}

export const timelineStore = new TimelineStore();
stalenessStore.register('timeline', () => timelineStore.isStale);
