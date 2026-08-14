// liveSessions store — Phase 3 of the spectator plan
// (`.loom/99-implementation-plan-agent-telemetry-spectator-2026-05-04.md`).
//
// Subscribes to the daemon EventBus events emitted by Phase 2.x:
//   - session.start / session.end          (from agentcontext + hooks)
//   - tool.call.start / tool.call.end      (from spawn telemetry + hooks)
//   - agent.status.change                  (from presence transitions)
//
// Data model: a per-agent session map keyed by `session_id`, each carrying
// a fixed-size ring buffer of the last `RECENT_CALLS_PER_SESSION` tool calls.
// All bookkeeping is event-driven — no polling — so the store stays cheap
// even when many sessions are active.
//
// Event payloads are best-effort; missing fields fall back to safe defaults
// rather than crashing the renderer (the producer side at
// `cmd/loom/cmd_agent_event_emit.go` and `internal/hud/bridge/spawn_telemetry.go`
// guarantees redaction at TierPublic before publication).

import { eventStore, type SSEEvent } from './events.svelte.ts';
import { groupSessionsByConversation } from '../utils/agents.ts';
import { sessionsToEnd, type SnapshotSessionLite } from '../utils/sessionReconcile.ts';
import { createPoller } from '../utils/poller.ts';

/** Maximum tool calls retained per session in the ring buffer. */
export const RECENT_CALLS_PER_SESSION = 20;

/** A session entry stays "recently ended" this many ms before disappearing. */
const ENDED_RETENTION_MS = 30_000;

export type AgentStatus = 'active' | 'idle' | 'offline' | 'expired' | 'unknown';

export interface ToolCall {
  call_id: string;
  tool_name: string;
  /** Server name for MCP-routed tools; empty for builtin/native tools. */
  server_name?: string;
  args_redacted?: Record<string, unknown>;
  /** Set on tool.call.end — undefined while the call is in flight. */
  duration_ms?: number;
  exit_code?: number;
  result_summary?: string;
  error?: string;
  status?: string;
  /** Wall-clock string from the producer (ISO-8601). */
  started_at?: string;
  ended_at?: string;
  /** True until tool.call.end arrives. */
  in_flight: boolean;
  /** Backfilled activity is not always a literal MCP tool call. */
  source?: 'tool' | 'context' | 'event' | 'trace';
}

export interface LiveSession {
  session_id: string;
  agent_id: string;
  agent_status: AgentStatus;
  /** Most recent first. Capped at RECENT_CALLS_PER_SESSION. */
  recent_calls: ToolCall[];
  /** Wall-clock when this session entry was first seen by the store. */
  first_seen: number;
  /** Wall-clock of most recent activity (call/start/status change). */
  last_activity: number;
  /** Set when a session.end event arrives; entry is reaped after ENDED_RETENTION_MS. */
  ended_at?: number;
  // --- Presence enrichment (joined from the fleet snapshot's agents array) ---
  // These make a session row useful even when no tool-call telemetry is
  // captured: they answer "what is this, what's it doing, how fresh is it".
  /** Human label, e.g. "Claude Code · services/flexinfer". */
  description?: string;
  /** The agent's self-reported current task (heartbeat data), when set. */
  current_task?: string;
  /** Working branch from presence. */
  branch?: string;
  /** Count of files the agent has touched/open this turn. */
  active_files?: number;
  /** Seconds since the last presence heartbeat (freshness). */
  heartbeat_age_seconds?: number;
  /** Presence status (active/idle/offline) — more reliable than SSE status. */
  presence_status?: string;
  /** Telemetry pipeline status (real/stub/…) for diagnosing missing activity. */
  telemetry_status?: string;
}

/**
 * A group of live sessions that belong to one conversation. The lifecycle
 * hooks mint a distinct agent_id per (workspace, conversation)
 * (`<base>-<WS_HASH>-<SESSION_SCOPE>`), so one chat that hopped across
 * repos/worktrees shows up as several sessions with different WS_HASHes; this
 * collapses them under one header keyed by `conversationId`. See
 * `groupedSessions`.
 */
export interface LiveSessionGroup {
  /** Conversation id — the group key and display label. */
  root: string;
  /** Sessions in this group, most-recent activity first. */
  sessions: LiveSession[];
  /** Most "live" status across the group's sessions (active > idle > …). */
  status: AgentStatus;
  /** Max last_activity across the group, for sorting groups. */
  last_activity: number;
}

class LiveSessionsStore {
  /** Keyed by session_id. Sessions appear when any event mentions them. */
  sessions = $state<Map<string, LiveSession>>(new Map());

  /**
   * Monotonic event counter. Used by the card to render a "last update X ago"
   * indicator and by tests to assert events flowed through.
   */
  eventCount = $state(0);

  private unsubs: Array<() => void> = [];
  private activityRefreshes = new Map<string, Promise<void>>();
  // 5s reaper — prunes ended sessions past their retention window. Uses the
  // shared poller purely for timer lifecycle (visibility-pause is harmless
  // for a local cleanup; visibleSessions already filters by retention).
  private reaper = createPoller(() => this.reapEnded(), 5_000);

  /** Connect to the SSE event stream (idempotent). */
  connect() {
    if (this.unsubs.length > 0) return;

    this.unsubs.push(eventStore.on('session.start', (e) => this.onSessionStart(e)));
    this.unsubs.push(eventStore.on('session.end', (e) => this.onSessionEnd(e)));
    this.unsubs.push(eventStore.on('agent.status.change', (e) => this.onStatusChange(e)));
    this.unsubs.push(eventStore.on('tool.call.start', (e) => this.onToolCallStart(e)));
    this.unsubs.push(eventStore.on('tool.call.end', (e) => this.onToolCallEnd(e)));

    // HUD lifecycle routes use the agent.* vocabulary while the embedded
    // daemon uses session.*. Normalize both into the same session map.
    this.unsubs.push(eventStore.on('agent.session.start', (e) => this.onSessionStart(e)));
    this.unsubs.push(eventStore.on('agent.session.bootstrap', (e) => this.onSessionStart(e)));
    this.unsubs.push(eventStore.on('agent.session.end', (e) => this.onSessionEnd(e)));
    this.unsubs.push(eventStore.on('agent.session.reaped', (e) => this.onSessionEnd(e)));
    this.unsubs.push(eventStore.on('agent.heartbeat', (e) => this.onHeartbeat(e)));
    this.unsubs.push(eventStore.on('agent.context.added', (e) => this.onContextAdded(e)));
    this.unsubs.push(eventStore.on('agent.session.stats.updated', (e) => this.onContextAdded(e)));

    // hud.fleet carries the canonical session list. Without this subscription
    // the panel would only ever learn about sessions that emit session.start
    // *after* mount — already-running sessions never appear (the one-shot
    // seed below races against fleet load and silently misses late arrivals).
    this.unsubs.push(eventStore.on('hud.fleet', (e) => this.onFleetSnapshot(e)));

    this.reaper.start();

    // Seed from currently-active sessions so the panel isn't empty when an
    // operator opens the HUD mid-flight. SSE events take over and overlay
    // status/calls as they arrive; getOrCreate dedups on session_id so a
    // backfill entry and a later session.start for the same id collapse.
    void this.seedFromActiveSessions();
  }

  disconnect() {
    for (const u of this.unsubs) u();
    this.unsubs = [];
    this.reaper.stop();
  }

  /** Active sessions sorted by most-recent activity desc. */
  get visibleSessions(): LiveSession[] {
    return Array.from(this.sessions.values())
      .filter((s) => s.ended_at === undefined || Date.now() - s.ended_at < ENDED_RETENTION_MS)
      .sort((a, b) => b.last_activity - a.last_activity);
  }

  get activeSessionCount(): number {
    return this.visibleSessions.filter((s) => s.ended_at === undefined).length;
  }

  /**
   * Visible sessions collapsed into per-conversation groups so one chat that
   * hopped across repos/worktrees renders under a single header instead of as
   * flat, unrelated rows (and distinct chats sharing a repo stay separate).
   * Matches the Fleet "Live Agents" table's conversation-first grouping. Groups
   * and the sessions within them stay sorted by most recent activity, matching
   * `visibleSessions`.
   */
  get groupedSessions(): LiveSessionGroup[] {
    return groupSessionsByConversation(this.visibleSessions) as LiveSessionGroup[];
  }

  /** Distinct conversations among visible sessions. */
  get conversationGroupCount(): number {
    return this.groupedSessions.length;
  }

  /** In-flight tool calls across all visible sessions. */
  get inFlightCallCount(): number {
    let n = 0;
    for (const s of this.visibleSessions) {
      for (const c of s.recent_calls) {
        if (c.in_flight) n++;
      }
    }
    return n;
  }

  /** Reset state — used by tests. */
  reset() {
    this.sessions = new Map();
    this.eventCount = 0;
  }

  /**
   * Backfill the panel with currently-active sessions from `/api/fleet`.
   * Called once on `connect()`; runs in the background and silently no-ops
   * on failure. SSE events arriving during the fetch are not lost because
   * getOrCreate dedups by session_id and the latest activity timestamp wins.
   *
   * Exposed for tests; production code calls it via connect().
   */
  async seedFromActiveSessions(): Promise<void> {
    try {
      const res = await globalThis.fetch('/api/fleet');
      if (!res.ok) return;
      const data = (await res.json()) as {
        sessions?: Array<Record<string, unknown>>;
        agents?: Array<Record<string, unknown>>;
      };
      await this.mergeActiveSessions(data.sessions ?? []);
      this.applyPresence(data.agents ?? []);
    } catch {
      // Best-effort: SSE will populate as turns happen.
    }
  }

  /**
   * mergeActiveSessions reconciles the live-sessions map against the
   * canonical session list from a fleet snapshot, in both directions:
   *
   *   - status=active sessions not yet tracked are inserted (existing
   *     entries are left alone — SSE-sourced entries have richer state we
   *     don't want to clobber);
   *   - tracked entries the snapshot reports as ended (or that have gone
   *     quiet AND dropped out of the snapshot) are marked ended so missed
   *     session.end SSE events can't leave zombie rows that drift away
   *     from the fleet-snapshot count the card header displays.
   *
   * Called from both the one-shot mount seed and the hud.fleet SSE handler.
   */
  async mergeActiveSessions(sessions: Array<Record<string, unknown>>): Promise<void> {
    let dirty = 0;
    const backfills: Array<Promise<void>> = [];
    for (const s of sessions) {
      const sid = stringField(s, 'id');
      const status = stringField(s, 'status');
      const ended = stringField(s, 'ended_at');
      if (!sid || status !== 'active' || ended) continue;
      if (this.sessions.has(sid)) continue;
      const aid = stringField(s, 'agent_id');
      const startedMs = Date.parse(stringField(s, 'started_at')) || Date.now();
      const session: LiveSession = {
        session_id: sid,
        agent_id: aid,
        agent_status: 'unknown',
        recent_calls: [],
        first_seen: startedMs,
        last_activity: startedMs,
      };
      this.sessions.set(sid, session);
      backfills.push(this.backfillSessionActivity(session));
      dirty++;
    }

    const lite: SnapshotSessionLite[] = sessions.map((s) => ({
      id: stringField(s, 'id'),
      status: stringField(s, 'status'),
      ended_at: stringField(s, 'ended_at'),
    }));
    const now = Date.now();
    for (const sid of sessionsToEnd(this.sessions.values(), lite, now)) {
      const session = this.sessions.get(sid);
      if (!session) continue;
      session.ended_at = now;
      session.last_activity = now;
      dirty++;
    }

    if (dirty > 0) this.touch();
    if (backfills.length > 0) {
      await Promise.allSettled(backfills);
    }
  }

  private onFleetSnapshot(e: SSEEvent): void {
    const data = e.data as { sessions?: unknown; agents?: unknown };
    const sessions = Array.isArray(data?.sessions) ? (data.sessions as Array<Record<string, unknown>>) : [];
    const agents = Array.isArray(data?.agents) ? (data.agents as Array<Record<string, unknown>>) : [];
    if (sessions.length === 0 && agents.length === 0) return;
    // Enrich after merge so newly-added sessions also pick up presence in the
    // same tick. applyPresence is a no-op when agents is empty.
    void this.mergeActiveSessions(sessions).then(() => this.applyPresence(agents));
  }

  /**
   * applyPresence joins the fleet snapshot's `agents` (presence) array onto the
   * tracked sessions by session_id (falling back to agent_id), enriching each
   * row with description / current_task / branch / active-file count / heartbeat
   * freshness. This is what makes a session legible even with zero captured
   * tool calls — the data the daemon already has, just surfaced.
   */
  applyPresence(agents: Array<Record<string, unknown>>): void {
    if (!Array.isArray(agents) || agents.length === 0) return;
    const bySession = new Map<string, Record<string, unknown>>();
    const byAgent = new Map<string, Record<string, unknown>>();
    for (const a of agents) {
      const sid = stringField(a, 'session_id');
      const aid = stringField(a, 'agent_id');
      if (sid) bySession.set(sid, a);
      if (aid && !byAgent.has(aid)) byAgent.set(aid, a);
    }
    let dirty = 0;
    for (const [sid, s] of this.sessions) {
      const p = bySession.get(sid) || (s.agent_id ? byAgent.get(s.agent_id) : undefined);
      if (!p) continue;
      const desc = stringField(p, 'description');
      if (desc) s.description = desc;
      const task = stringField(p, 'current_task');
      s.current_task = task || undefined;
      const branch = stringField(p, 'branch');
      s.branch = branch || undefined;
      const af = p['active_files'];
      s.active_files = Array.isArray(af) ? af.length : numberField(p, 'active_files');
      const hb = numberField(p, 'heartbeat_age_seconds');
      if (hb !== undefined) s.heartbeat_age_seconds = hb;
      const pstatus = stringField(p, 'status');
      if (pstatus) s.presence_status = pstatus;
      const tstatus = stringField(p, 'telemetry_status');
      if (tstatus) s.telemetry_status = tstatus;
      dirty++;
    }
    if (dirty > 0) this.touch();
  }

  private touch() {
    this.eventCount++;
    // Replace the map reference so $state reactivity picks up the change.
    this.sessions = new Map(this.sessions);
  }

  private async backfillSessionActivity(session: LiveSession): Promise<void> {
    if (!session.session_id || session.recent_calls.length > 0) return;
    await this.refreshSessionActivity(session.session_id);
  }

  private refreshSessionActivity(sessionID: string): Promise<void> {
    const active = this.activityRefreshes.get(sessionID);
    if (active) return active;

    const refresh = this.fetchSessionActivity(sessionID).finally(() => {
      this.activityRefreshes.delete(sessionID);
    });
    this.activityRefreshes.set(sessionID, refresh);
    return refresh;
  }

  private async fetchSessionActivity(sessionID: string): Promise<void> {
    const session = this.sessions.get(sessionID);
    if (!session) return;
    try {
      const params = new URLSearchParams({ limit: '8' });
      if (session.agent_id) params.set('agent_id', session.agent_id);
      const res = await globalThis.fetch(
        `/api/sessions/${encodeURIComponent(sessionID)}/trace?${params.toString()}`,
      );
      if (!res.ok) return;
      const trace = (await res.json()) as Record<string, unknown>;
      const calls = traceActivityToCalls(trace);
      const current = this.sessions.get(sessionID);
      if (!current || calls.length === 0) return;
      const recent = mergeSessionCalls(current.recent_calls, calls);
      const latest = latestCallTime(recent);
      this.sessions.set(sessionID, {
        ...current,
        recent_calls: recent,
        last_activity: latest > 0 ? Math.max(current.last_activity, latest) : current.last_activity,
      });
      if (!current.agent_id && stringField(trace, 'agent_id')) {
        this.sessions.get(sessionID)!.agent_id = stringField(trace, 'agent_id');
      }
      this.touch();
    } catch {
      // Best-effort: live SSE activity will still populate the card.
    }
  }

  private getOrCreate(sessionID: string, agentID: string): LiveSession {
    const existing = this.sessions.get(sessionID);
    if (existing) {
      // Late-arriving agent_id wins over the empty placeholder we may have
      // created when the first event for this session lacked one.
      if (!existing.agent_id && agentID) {
        existing.agent_id = agentID;
      }
      return existing;
    }
    const now = Date.now();
    const fresh: LiveSession = {
      session_id: sessionID,
      agent_id: agentID,
      agent_status: 'unknown',
      recent_calls: [],
      first_seen: now,
      last_activity: now,
    };
    this.sessions.set(sessionID, fresh);
    return fresh;
  }

  private onSessionStart(e: SSEEvent) {
    const sid = stringField(e.data, 'session_id');
    const aid = stringField(e.data, 'agent_id');
    if (!sid) return;
    const session = this.getOrCreate(sid, aid);
    session.last_activity = Date.now();
    session.ended_at = undefined; // re-opened
    this.touch();
  }

  private onSessionEnd(e: SSEEvent) {
    const sid = stringField(e.data, 'session_id');
    if (!sid) return;
    const session = this.sessions.get(sid);
    if (!session) return; // never seen — ignore
    session.ended_at = Date.now();
    session.last_activity = Date.now();
    this.touch();
  }

  private onStatusChange(e: SSEEvent) {
    const aid = stringField(e.data, 'agent_id');
    const status = (stringField(e.data, 'status') || stringField(e.data, 'new_status')) as AgentStatus;
    if (!aid || !status) return;
    // agent.status.change is keyed on agent_id, not session_id. Update every
    // session belonging to this agent.
    let dirty = false;
    for (const s of this.sessions.values()) {
      if (s.agent_id === aid) {
        s.agent_status = status;
        s.last_activity = Date.now();
        dirty = true;
      }
    }
    if (dirty) this.touch();
  }

  private onHeartbeat(e: SSEEvent) {
    const aid = stringField(e.data, 'agent_id');
    if (!aid) return;
    const status = (stringField(e.data, 'status') || 'active') as AgentStatus;
    let dirty = false;
    for (const s of this.sessions.values()) {
      if (s.agent_id !== aid) continue;
      s.agent_status = status;
      s.presence_status = status;
      s.current_task = stringField(e.data, 'current_task') || undefined;
      s.branch = stringField(e.data, 'branch') || undefined;
      const files = e.data['active_files'];
      if (Array.isArray(files)) s.active_files = files.length;
      s.heartbeat_age_seconds = 0;
      s.last_activity = Date.now();
      dirty = true;
    }
    if (dirty) this.touch();
  }

  private onContextAdded(e: SSEEvent) {
    const sid = stringField(e.data, 'session_id');
    if (!sid) return;
    this.getOrCreate(sid, stringField(e.data, 'agent_id')).last_activity = Date.now();
    this.touch();
    void this.refreshSessionActivity(sid);
  }

  private onToolCallStart(e: SSEEvent) {
    const sid = stringField(e.data, 'session_id');
    const aid = stringField(e.data, 'agent_id');
    const callID = stringField(e.data, 'call_id');
    const toolName = stringField(e.data, 'tool_name');
    if (!sid || !callID) return;
    const session = this.getOrCreate(sid, aid);
    const call: ToolCall = {
      call_id: callID,
      tool_name: toolName,
      server_name: stringField(e.data, 'server_name') || undefined,
      args_redacted:
        (e.data.args_redacted as Record<string, unknown>) ?? undefined,
      started_at: stringField(e.data, 'started_at') || undefined,
      in_flight: true,
      source: 'tool',
    };
    // Push at front (most recent first) and trim the tail.
    session.recent_calls.unshift(call);
    if (session.recent_calls.length > RECENT_CALLS_PER_SESSION) {
      session.recent_calls.length = RECENT_CALLS_PER_SESSION;
    }
    session.last_activity = Date.now();
    this.touch();
  }

  private onToolCallEnd(e: SSEEvent) {
    const sid = stringField(e.data, 'session_id');
    const callID = stringField(e.data, 'call_id');
    if (!sid) return;
    const session = this.sessions.get(sid);
    if (!session) return;
    const idx = callID ? session.recent_calls.findIndex((c) => c.call_id === callID) : -1;
    if (idx >= 0) {
      const call = session.recent_calls[idx];
      call.in_flight = false;
      const dur = numberField(e.data, 'duration_ms');
      if (dur !== undefined) call.duration_ms = dur;
      const ec = numberField(e.data, 'exit_code');
      if (ec !== undefined) call.exit_code = ec;
      call.result_summary = stringField(e.data, 'result_summary') || undefined;
      call.error = stringField(e.data, 'error') || undefined;
      call.status = stringField(e.data, 'status') || undefined;
      call.ended_at = stringField(e.data, 'ended_at') || undefined;
    } else {
      // No matching start — could be a coarse codex.turn event without prior
      // start. Synthesize a closed entry so the user sees activity.
      const tool = stringField(e.data, 'tool_name') || 'unknown';
      const synthetic: ToolCall = {
        call_id: callID || `synthetic-${Date.now()}`,
        tool_name: tool,
        duration_ms: numberField(e.data, 'duration_ms'),
        exit_code: numberField(e.data, 'exit_code'),
        result_summary: stringField(e.data, 'result_summary') || undefined,
        error: stringField(e.data, 'error') || undefined,
        status: stringField(e.data, 'status') || undefined,
        ended_at: stringField(e.data, 'ended_at') || undefined,
        in_flight: false,
        source: 'tool',
      };
      session.recent_calls.unshift(synthetic);
      if (session.recent_calls.length > RECENT_CALLS_PER_SESSION) {
        session.recent_calls.length = RECENT_CALLS_PER_SESSION;
      }
    }
    session.last_activity = Date.now();
    this.touch();
  }

  private reapEnded() {
    let dirty = false;
    const now = Date.now();
    for (const [sid, s] of this.sessions) {
      if (s.ended_at !== undefined && now - s.ended_at >= ENDED_RETENTION_MS) {
        this.sessions.delete(sid);
        dirty = true;
      }
    }
    if (dirty) this.touch();
  }
}

export const liveSessionsStore = new LiveSessionsStore();

// --- Internal helpers ---

function stringField(data: Record<string, unknown>, key: string): string {
  const v = data?.[key];
  return typeof v === 'string' ? v : '';
}

function numberField(data: Record<string, unknown>, key: string): number | undefined {
  const v = data?.[key];
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  return undefined;
}

function traceActivityToCalls(trace: Record<string, unknown>): ToolCall[] {
  const out: ToolCall[] = [];
  for (const raw of arrayField(trace, 'traces')) {
    const item = objectField(raw);
    const server = stringField(item, 'server');
    const tool = stringField(item, 'tool');
    if (!tool && !server) continue;
    const ts = stringField(item, 'timestamp');
    out.push({
      call_id: stableBackfillID('trace', item, out.length),
      tool_name: tool || server || 'trace',
      server_name: server || undefined,
      duration_ms: numberField(item, 'duration_ms'),
      error: stringField(item, 'error') || undefined,
      status: stringField(item, 'status') || undefined,
      result_summary: stringField(item, 'target') || stringField(item, 'pipeline_stage') || undefined,
      started_at: ts || undefined,
      ended_at: ts || undefined,
      in_flight: false,
      source: 'trace',
    });
  }
  for (const raw of arrayField(trace, 'events')) {
    const item = objectField(raw);
    const eventType = stringField(item, 'event_type');
    if (!eventType) continue;
    const ts = stringField(item, 'timestamp');
    out.push({
      call_id: stableBackfillID('event', item, out.length),
      tool_name: eventType,
      result_summary: eventSummary(item),
      started_at: ts || undefined,
      ended_at: ts || undefined,
      in_flight: false,
      source: 'event',
    });
  }
  for (const raw of arrayField(trace, 'entries')) {
    const item = objectField(raw);
    const entryType = stringField(item, 'entry_type') || 'context';
    const title = stringField(item, 'title');
    const content = stringField(item, 'content');
    const ts = stringField(item, 'timestamp');
    out.push({
      call_id: stableBackfillID('context', item, out.length),
      tool_name: entryType,
      result_summary: title || truncateSummary(content),
      started_at: ts || undefined,
      ended_at: ts || undefined,
      in_flight: false,
      source: 'context',
    });
  }
  return out
    .sort((a, b) => callTime(b) - callTime(a))
    .filter((call, idx, calls) => calls.findIndex((c) => c.call_id === call.call_id) === idx);
}

function arrayField(data: Record<string, unknown>, key: string): unknown[] {
  const v = data?.[key];
  return Array.isArray(v) ? v : [];
}

function objectField(v: unknown): Record<string, unknown> {
  return v && typeof v === 'object' ? (v as Record<string, unknown>) : {};
}

function eventSummary(item: Record<string, unknown>): string | undefined {
  const data = objectField(item.data);
  return (
    stringField(data, 'tool_name') ||
    stringField(data, 'status') ||
    stringField(data, 'summary') ||
    undefined
  );
}

function stableBackfillID(prefix: string, item: Record<string, unknown>, fallback: number): string {
  const id =
    stringField(item, 'id') ||
    stringField(item, 'call_id') ||
    [stringField(item, 'timestamp'), stringField(item, 'server'), stringField(item, 'tool'), stringField(item, 'event_type'), stringField(item, 'entry_type')]
      .filter(Boolean)
      .join(':');
  return `${prefix}-${id || fallback}`;
}

function truncateSummary(s: string): string | undefined {
  if (!s) return undefined;
  const singleLine = s.replace(/\s+/g, ' ').trim();
  return singleLine.length > 140 ? `${singleLine.slice(0, 137)}...` : singleLine;
}

function latestCallTime(calls: ToolCall[]): number {
  return calls.reduce((latest, call) => Math.max(latest, callTime(call)), 0);
}

function callTime(call: ToolCall): number {
  return Date.parse(call.ended_at || call.started_at || '') || 0;
}

export function mergeSessionCalls(existing: ToolCall[], incoming: ToolCall[]): ToolCall[] {
  const merged = new Map<string, ToolCall>();
  for (const call of existing) merged.set(call.call_id, call);
  for (const call of incoming) {
    if (!merged.has(call.call_id)) merged.set(call.call_id, call);
  }
  return Array.from(merged.values())
    .sort((a, b) => callTime(b) - callTime(a))
    .slice(0, RECENT_CALLS_PER_SESSION);
}
