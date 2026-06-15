export type UnifiedAgentStatus = 'active' | 'idle' | 'offline';

interface SessionLike {
  id: string;
  agent_id: string;
  namespace?: string;
  status?: string;
  description?: string;
  started_at?: string;
  entry_count?: number;
  total_tokens?: number;
  tokens_used?: number;
  task_count?: number;
  memory_items?: number;
}

interface PresenceLike {
  agent_id: string;
  session_id?: string;
  status?: string;
  agent_type?: string;
  description?: string;
  current_task?: string;
  active_files?: string[];
  branch?: string;
  pr_url?: string;
  worktree_id?: string;
  last_heartbeat?: string;
  registered_at?: string;
  source?: string;
  has_presence?: boolean;
  has_session?: boolean;
  session_status?: string;
  session_started_at?: string;
  heartbeat_age_seconds?: number;
  session_age_seconds?: number;
  telemetry_status?: string;
  is_orphan?: boolean;
  orphan_age_seconds?: number;
}

interface TaskLike {
  agent_id?: string;
  status?: string;
}

interface FileClaimLike {
  agent_id?: string;
}

interface SpawnLike {
  spawn_id: string;
  agent_id: string;
  status?: string;
  request?: {
    project?: string;
    branch?: string;
    task_description?: string;
    agent_type?: string;
  };
}

export interface UnifiedAgent {
  agent_id: string;
  agent_type: string;
  status: UnifiedAgentStatus;
  source: 'presence' | 'presence+session' | 'session' | 'spawn';
  description: string;
  current_task: string;
  branch: string;
  last_heartbeat: string;
  registered_at: string;
  active_files: string[];
  active_file_count: number;
  pr_url?: string;
  worktree_id?: string;
  session_id?: string;
  namespace?: string;
  session_status?: string;
  session_started_at?: string;
  entry_count: number;
  total_tokens: number;
  task_count: number;
  blocked_tasks: number;
  claim_count: number;
  spawn_id?: string;
  spawn_status?: string;
  project?: string;
  heartbeat_age_seconds: number;
  session_age_seconds: number;
  telemetry_status: string;
  has_presence: boolean;
  has_session: boolean;
  has_spawn: boolean;
  is_orphan: boolean;
  orphan_age_seconds: number;
}

export interface UnifiedAgentSummary {
  total_agents: number;
  active_agents: number;
  idle_agents: number;
  offline_agents: number;
  live_agents: number;
  with_sessions: number;
  with_presence: number;
  with_spawns: number;
  orphans: number;
}

export function inferAgentType(agentId: string | null | undefined, declaredType?: string | null): string {
  const raw = (declaredType ?? '').trim();
  if (raw && raw.toLowerCase() !== 'unknown') return raw;
  const id = (agentId ?? '').trim().toLowerCase();
  if (!id) return 'unknown';
  if (id.startsWith('claude')) return 'claude';
  if (id.startsWith('codex')) return 'codex';
  if (id.startsWith('gemini')) return 'gemini';
  if (id.startsWith('copilot')) return 'copilot';
  if (id.startsWith('kilocode')) return 'kilocode';
  return id.split('-')[0] || 'unknown';
}

// rootAgentId collapses a per-conversation agent_id down to its stable,
// workspace-scoped identity so sibling conversations of one agent group
// together. The lifecycle hooks mint agent_ids as
// `<base>-<WS_HASH>[-<SESSION_SCOPE>]` (pkg/generator/configs_hooks.go),
// where WS_HASH is `cksum` of the git workspace root (stable per workspace)
// and SESSION_SCOPE is `cksum` of the conversation/session id (changes every
// conversation). Both suffixes are all-decimal; the base prefix
// (`claude-code`, `codex`, `gemini-cli`, …) never is. So the root identity is
// everything up to and including the FIRST all-numeric segment (the WS_HASH);
// any later numeric segment is the session scope we strip. Ids with no
// numeric segment (`codex-7b28`, `spawn-claude-code-<hex>`, a bare
// `claude-code`) are already roots and pass through unchanged.
//
// Examples:
//   claude-code-552019522-2804496862 → claude-code-552019522
//   claude-code-552019522-3116397616 → claude-code-552019522  (groups w/ above)
//   codex-4188162495                 → codex-4188162495
//   codex-4188162495-2303882182      → codex-4188162495        (groups w/ above)
//   codex-7b28                       → codex-7b28              (no WS_HASH suffix)
export function rootAgentId(agentId: string | null | undefined): string {
  const id = (agentId ?? '').trim();
  if (!id) return '';
  const parts = id.split('-');
  for (let i = 0; i < parts.length; i += 1) {
    if (parts[i].length > 0 && /^[0-9]+$/.test(parts[i])) {
      return parts.slice(0, i + 1).join('-');
    }
  }
  return id;
}

// conversationId collapses a per-(workspace,conversation) agent_id down to the
// CONVERSATION it belongs to, so one chat that moved across repos/worktrees
// groups as a single identity. Where rootAgentId keeps the WS_HASH (grouping by
// workspace), conversationId keeps the SESSION_SCOPE and drops the WS_HASH —
// grouping by chat. The hooks mint `<base>-<WS_HASH>-<SESSION_SCOPE>`
// (pkg/generator/configs_hooks.go): `base` is non-numeric (`claude-code`,
// `codex`, `gemini-cli`), WS_HASH is the FIRST all-numeric segment (the git
// workspace-root cksum, which changes per repo/worktree), and SESSION_SCOPE is
// the trailing all-numeric segment (the conversation/session-id cksum, stable
// for the life of one chat). The conversation identity is `base-SESSION_SCOPE`.
//
// Live evidence: scope 1105899468 appears under three WS_HASHes
// (claude-code-3749726816-…, -401508988-…, -1305365710-…) — one chat that
// worked in flightdeck, gitops, and an agents namespace. rootAgentId splits it
// into three; conversationId unifies it.
//
// Codex is the exception: its notify hook mints WORKSPACE-anchored ids
// (`codex-<WS_HASH>`, no scope) and the fleet also sees scoped variants
// (`codex-<WS_HASH>-<SCOPE>`) for the same app. Codex's "conversation" is the
// workspace, so for it we KEEP the WS_HASH (folding scopeless + scoped) instead
// of fragmenting by scope. See WORKSPACE_ANCHORED_BASES.
//
// Examples:
//   claude-code-3749726816-1105899468 → claude-code-1105899468
//   claude-code-401508988-1105899468  → claude-code-1105899468  (same chat, other repo)
//   codex-401508988-2992486099        → codex-401508988  (codex: workspace-anchored)
//   codex-401508988                   → codex-401508988  (folds with the scoped variant above)
//   codex-7b28                        → codex-7b28       (no numeric suffix: already a root)
export function conversationId(agentId: string | null | undefined): string {
  const id = (agentId ?? '').trim();
  if (!id) return '';
  const parts = id.split('-');
  const isNumeric = (p: string) => p.length > 0 && /^[0-9]+$/.test(p);
  // WS_HASH is the first all-numeric segment; the base is everything before it.
  let wsIdx = -1;
  for (let i = 0; i < parts.length; i += 1) {
    if (isNumeric(parts[i])) {
      wsIdx = i;
      break;
    }
  }
  if (wsIdx < 0) return id; // no numeric suffix — already a conversation root
  const base = parts.slice(0, wsIdx).join('-');
  // Workspace-anchored vendors (codex): keep the WS_HASH so scopeless and scoped
  // ids for one app fold into a single conversation.
  if (WORKSPACE_ANCHORED_BASES.has(base)) {
    return parts.slice(0, wsIdx + 1).join('-'); // base-WSHASH
  }
  // SESSION_SCOPE is the last all-numeric segment AFTER the WS_HASH.
  let scope = '';
  for (let i = parts.length - 1; i > wsIdx; i -= 1) {
    if (isNumeric(parts[i])) {
      scope = parts[i];
      break;
    }
  }
  if (!scope) return id; // base-WSHASH only, no scope — can't separate the chat
  return base ? `${base}-${scope}` : scope;
}

// Bases whose agent ids are workspace-anchored rather than conversation-scoped:
// conversationId keeps their WS_HASH. Codex's notify hook mints `codex-<WS_HASH>`
// (one app per workspace); Claude/Gemini chats instead carry a stable
// SESSION_SCOPE across repos and are keyed by scope.
const WORKSPACE_ANCHORED_BASES = new Set<string>(['codex']);

/** Minimal shape needed to group live sessions by their owning agent. */
export interface RootGroupableSession {
  session_id: string;
  agent_id: string;
  agent_status: string;
  last_activity: number;
}

/**
 * A set of sessions that share one grouping identity. The `root` field holds
 * the group key — a workspace-scoped root agent id when grouped via
 * `groupSessionsByRootAgent`, or a conversation id when grouped via
 * `groupSessionsByConversation`. Both are `<base>-<numeric>` shaped, so the
 * field doubles as the display label either way.
 */
export interface RootAgentSessionGroup<T extends RootGroupableSession> {
  /** Group key + display label (root agent id OR conversation id). */
  root: string;
  /** Member sessions, in their incoming order (callers pre-sort). */
  sessions: T[];
  /** Most "live" status across the group (active > idle > unknown > offline). */
  status: string;
  /** Max last_activity across the group, for ordering groups. */
  last_activity: number;
}

/** Status rank: higher wins when merging a group's session statuses. */
export function liveStatusRank(status: string | null | undefined): number {
  switch ((status ?? '').trim().toLowerCase()) {
    case 'active':
      return 3;
    case 'idle':
      return 2;
    case 'unknown':
    case '':
      return 1;
    default:
      return 0; // offline / expired / anything else terminal
  }
}

// groupSessionsByKey collapses a list of live sessions into groups keyed by an
// agent_id-derived identity (rootAgentId or conversationId). Input order is
// preserved within each group (callers sort by recency first); groups come back
// ordered by most-recent activity. Pure + rune-free so it is unit-testable.
function groupSessionsByKey<T extends RootGroupableSession>(
  sessions: T[],
  keyFn: (agentId: string | null | undefined) => string,
): RootAgentSessionGroup<T>[] {
  const groups = new Map<string, RootAgentSessionGroup<T>>();
  for (const session of sessions) {
    const root = keyFn(session.agent_id) || session.agent_id || session.session_id;
    let group = groups.get(root);
    if (!group) {
      group = { root, sessions: [], status: 'unknown', last_activity: 0 };
      groups.set(root, group);
    }
    group.sessions.push(session);
    group.last_activity = Math.max(group.last_activity, session.last_activity);
    if (liveStatusRank(session.agent_status) > liveStatusRank(group.status)) {
      group.status = session.agent_status;
    }
  }
  return Array.from(groups.values()).sort((a, b) => b.last_activity - a.last_activity);
}

// groupSessionsByRootAgent buckets sessions by their workspace-scoped root
// agent (rootAgentId), so sibling conversations running in the SAME workspace
// render together. This is the "how many logical agents/CLIs are live" lens —
// the same one the Active-Agents headline uses. Use it where workspace identity
// is the right grouping; use groupSessionsByConversation where chat identity is.
export function groupSessionsByRootAgent<T extends RootGroupableSession>(
  sessions: T[],
): RootAgentSessionGroup<T>[] {
  return groupSessionsByKey(sessions, rootAgentId);
}

// groupSessionsByConversation buckets sessions by their conversation
// (conversationId), so one chat that hopped across repos/worktrees — distinct
// agent_ids sharing a SESSION_SCOPE but differing in WS_HASH — renders as a
// single group, while distinct chats that merely share a repo stay separate.
// This is the inverse axis of groupSessionsByRootAgent and matches the Fleet
// "Live Agents" table's conversation-first grouping (see fleetRows.ts).
export function groupSessionsByConversation<T extends RootGroupableSession>(
  sessions: T[],
): RootAgentSessionGroup<T>[] {
  return groupSessionsByKey(sessions, conversationId);
}

export function normalizeUnifiedStatus(raw: string | null | undefined): UnifiedAgentStatus {
  const status = (raw ?? '').trim().toLowerCase();
  if (status === 'active') return 'active';
  if (status === 'idle') return 'idle';
  return 'offline';
}

// Terminal spawn statuses — any spawn in one of these states is "done"
// and the agent that owned it has no current work, even if a stale CLI
// keepalive may still be heartbeating its presence row. Used to keep the
// Fleet view honest: the previous build of the unified row marked every
// spawn `status: 'active'` unconditionally (line 317 below), so closed
// CI pipelines like `spawn-claude-code-10fa8a6eb214` (pipeline 9839
// failed 2026-05-16) kept showing up as live agents on hud.flexinfer.ai.
const TERMINAL_SPAWN_STATUSES = new Set([
  'completed',
  'failed',
  'escalated',
  'stopped',
  'paused',
  'cancelled',
  'canceled',
]);

export function isTerminalSpawnStatus(raw: string | null | undefined): boolean {
  return TERMINAL_SPAWN_STATUSES.has((raw ?? '').trim().toLowerCase());
}

export function isLiveSession(raw: string | null | undefined): boolean {
  return (raw ?? '').trim().toLowerCase() === 'active';
}

function agentSortTime(agent: UnifiedAgent): number {
  const ts = agent.last_heartbeat || agent.session_started_at || agent.registered_at;
  const parsed = ts ? new Date(ts).getTime() : 0;
  return Number.isFinite(parsed) ? parsed : 0;
}

export function buildUnifiedAgents(input: {
  sessions: SessionLike[];
  agents: PresenceLike[];
  tasks?: TaskLike[];
  fileClaims?: FileClaimLike[];
  spawns?: SpawnLike[];
}): UnifiedAgent[] {
  const sessions = input.sessions ?? [];
  const agents = input.agents ?? [];
  const tasks = input.tasks ?? [];
  const fileClaims = input.fileClaims ?? [];
  const spawns = input.spawns ?? [];

  const byAgent = new Map<string, UnifiedAgent>();
  const liveSessionsByID = new Map<string, SessionLike>();
  const liveSessionsByAgent = new Map<string, SessionLike>();

  for (const session of sessions) {
    if (!isLiveSession(session.status) || !session.agent_id) continue;
    liveSessionsByID.set(session.id, session);
    const existing = liveSessionsByAgent.get(session.agent_id);
    const sessionTime = new Date(session.started_at ?? 0).getTime();
    const existingTime = new Date(existing?.started_at ?? 0).getTime();
    if (!existing || sessionTime >= existingTime) {
      liveSessionsByAgent.set(session.agent_id, session);
    }
  }

  const taskCounts = new Map<string, { total: number; blocked: number }>();
  for (const task of tasks) {
    const agentID = task.agent_id?.trim();
    if (!agentID) continue;
    const current = taskCounts.get(agentID) ?? { total: 0, blocked: 0 };
    current.total += 1;
    if ((task.status ?? '').trim().toLowerCase() === 'blocked') {
      current.blocked += 1;
    }
    taskCounts.set(agentID, current);
  }

  const claimCounts = new Map<string, number>();
  for (const claim of fileClaims) {
    const agentID = claim.agent_id?.trim();
    if (!agentID) continue;
    claimCounts.set(agentID, (claimCounts.get(agentID) ?? 0) + 1);
  }

  for (const agent of agents) {
    if (!agent.agent_id) continue;
    const session =
      (agent.session_id ? liveSessionsByID.get(agent.session_id) : undefined) ??
      liveSessionsByAgent.get(agent.agent_id);
    // hasSession is derived from the live sessions array only. The server
    // also computes this in fleetview.Join (internal/hud/fleetview), so the
    // two agree by construction. We deliberately do NOT fall back to
    // `agent.has_session` here — treating it as authoritative was the source
    // of the "SESSION badge lit but 0 sessions in counter" divergence.
    const hasSession = !!session;
    const hasPresence = agent.has_presence ?? agent.source !== 'session';
    const source =
      agent.source === 'session' ? 'session' : hasSession ? 'presence+session' : 'presence';

    byAgent.set(agent.agent_id, {
      agent_id: agent.agent_id,
      agent_type: inferAgentType(agent.agent_id, agent.agent_type),
      status: normalizeUnifiedStatus(agent.status),
      source,
      description: agent.description ?? session?.description ?? '',
      current_task: agent.current_task ?? '',
      branch: agent.branch ?? '',
      last_heartbeat: agent.last_heartbeat ?? '',
      registered_at: agent.registered_at ?? '',
      active_files: agent.active_files ?? [],
      active_file_count: agent.active_files?.length ?? 0,
      pr_url: agent.pr_url,
      worktree_id: agent.worktree_id,
      session_id: session?.id ?? agent.session_id,
      namespace: session?.namespace,
      session_status: agent.session_status ?? session?.status,
      session_started_at: agent.session_started_at ?? session?.started_at,
      entry_count: session?.entry_count ?? 0,
      total_tokens: session?.total_tokens ?? session?.tokens_used ?? 0,
      task_count: taskCounts.get(agent.agent_id)?.total ?? session?.task_count ?? 0,
      blocked_tasks: taskCounts.get(agent.agent_id)?.blocked ?? 0,
      claim_count: claimCounts.get(agent.agent_id) ?? 0,
      heartbeat_age_seconds: agent.heartbeat_age_seconds ?? 0,
      session_age_seconds: agent.session_age_seconds ?? 0,
      telemetry_status: agent.telemetry_status ?? (source === 'session' ? 'session_only' : 'unknown'),
      has_presence: hasPresence,
      has_session: hasSession,
      has_spawn: false,
      // is_orphan must agree with the locally-computed hasSession. The
      // server applies the age threshold in fleetview.Join, but presence
      // and sessions can arrive at the client out of order: a snapshot
      // can carry agent.is_orphan=true alongside a now-matching session
      // when raw presence was sampled before the session was created.
      // Mirror the server's invariant ("orphan ⇒ no active session") on
      // the client so the badge can never contradict the SESSION pill.
      is_orphan: hasSession ? false : (agent.is_orphan ?? false),
      orphan_age_seconds: hasSession ? 0 : (agent.orphan_age_seconds ?? 0),
    });
  }

  for (const session of liveSessionsByAgent.values()) {
    const existing = byAgent.get(session.agent_id);
    if (existing) {
      existing.session_id = session.id;
      existing.namespace = session.namespace;
      existing.session_status = session.status;
      existing.session_started_at = session.started_at;
      existing.entry_count = session.entry_count ?? existing.entry_count;
      existing.total_tokens = session.total_tokens ?? session.tokens_used ?? existing.total_tokens;
      existing.task_count = Math.max(existing.task_count, session.task_count ?? 0);
      if (!existing.description) existing.description = session.description ?? '';
      existing.has_session = true;
      // Same invariant as in the first loop: an agent with a matched
      // active session is never an orphan, regardless of what the server
      // flag said.
      existing.is_orphan = false;
      existing.orphan_age_seconds = 0;
      if (existing.source === 'presence') existing.source = 'presence+session';
      if (!existing.session_started_at) existing.session_started_at = session.started_at;
      continue;
    }

    byAgent.set(session.agent_id, {
      agent_id: session.agent_id,
      agent_type: inferAgentType(session.agent_id),
      status: 'active',
      source: 'session',
      description: session.description ?? '',
      current_task: '',
      branch: '',
      last_heartbeat: '',
      registered_at: '',
      active_files: [],
      active_file_count: 0,
      session_id: session.id,
      namespace: session.namespace,
      session_status: session.status,
      session_started_at: session.started_at,
      entry_count: session.entry_count ?? 0,
      total_tokens: session.total_tokens ?? session.tokens_used ?? 0,
      task_count: taskCounts.get(session.agent_id)?.total ?? session.task_count ?? 0,
      blocked_tasks: taskCounts.get(session.agent_id)?.blocked ?? 0,
      claim_count: claimCounts.get(session.agent_id) ?? 0,
      heartbeat_age_seconds: 0,
      session_age_seconds: 0,
      telemetry_status: 'session_only',
      has_presence: false,
      has_session: true,
      has_spawn: false,
      is_orphan: false,
      orphan_age_seconds: 0,
    });
  }

  for (const spawn of spawns) {
    if (!spawn.agent_id) continue;
    const spawnTerminal = isTerminalSpawnStatus(spawn.status);
    const existing = byAgent.get(spawn.agent_id);
    if (existing) {
      existing.spawn_id = spawn.spawn_id;
      existing.spawn_status = spawn.status ?? existing.spawn_status;
      existing.project = existing.project || spawn.request?.project || '';
      existing.branch = existing.branch || spawn.request?.branch || '';
      if (!existing.description) existing.description = spawn.request?.task_description ?? '';
      if (existing.agent_type === 'unknown') {
        existing.agent_type = inferAgentType(spawn.agent_id, spawn.request?.agent_type);
      }
      existing.has_spawn = true;
      // If the spawn is terminal, the agent that owned it has no current
      // work — even if its presence/session rows are still heartbeating
      // (a vendor CLI keepalive can outlive the spawn it was started
      // for). Downgrade the unified row to "offline" so the Fleet view's
      // "live agents" counter reflects active work, not historical
      // spawns. Preserves the SPAWN badge and underlying spawn detail.
      if (spawnTerminal && (existing.status === 'active' || existing.status === 'idle')) {
        existing.status = 'offline';
      }
      continue;
    }

    byAgent.set(spawn.agent_id, {
      agent_id: spawn.agent_id,
      agent_type: inferAgentType(spawn.agent_id, spawn.request?.agent_type),
      // Spawn-only rows (no presence, no session) reflect the spawn's
      // own state: still running → active, terminal → offline. Previous
      // build hardcoded 'active' regardless, so reaped spawn pods like
      // `spawn-claude-code-10fa8a6eb214` (CI pipeline 9839 failed) kept
      // showing up as live agents long after the work ended.
      status: spawnTerminal ? 'offline' : 'active',
      source: 'spawn',
      description: spawn.request?.task_description ?? '',
      current_task: '',
      branch: spawn.request?.branch ?? '',
      last_heartbeat: '',
      registered_at: '',
      active_files: [],
      active_file_count: 0,
      entry_count: 0,
      total_tokens: 0,
      task_count: taskCounts.get(spawn.agent_id)?.total ?? 0,
      blocked_tasks: taskCounts.get(spawn.agent_id)?.blocked ?? 0,
      claim_count: claimCounts.get(spawn.agent_id) ?? 0,
      heartbeat_age_seconds: 0,
      session_age_seconds: 0,
      telemetry_status: 'spawn',
      spawn_id: spawn.spawn_id,
      spawn_status: spawn.status ?? '',
      project: spawn.request?.project ?? '',
      has_presence: false,
      has_session: false,
      has_spawn: true,
      is_orphan: false,
      orphan_age_seconds: 0,
    });
  }

  const unified = [...byAgent.values()];
  unified.sort((left, right) => {
    const statusOrder: Record<UnifiedAgentStatus, number> = { active: 0, idle: 1, offline: 2 };
    const statusDelta = statusOrder[left.status] - statusOrder[right.status];
    if (statusDelta !== 0) return statusDelta;
    const timeDelta = agentSortTime(right) - agentSortTime(left);
    if (timeDelta !== 0) return timeDelta;
    return left.agent_id.localeCompare(right.agent_id);
  });
  return unified;
}

export function summarizeUnifiedAgents(agents: UnifiedAgent[]): UnifiedAgentSummary {
  const summary: UnifiedAgentSummary = {
    total_agents: agents.length,
    active_agents: 0,
    idle_agents: 0,
    offline_agents: 0,
    live_agents: 0,
    with_sessions: 0,
    with_presence: 0,
    with_spawns: 0,
    orphans: 0,
  };

  // live_agents counts distinct *logical* agents (workspace-scoped roots),
  // not per-conversation agent_ids. Two conversations of the same agent
  // (e.g. claude-code-552019522-2804496862 and -3116397616) are one live
  // agent, so the "Active Agents" headline and footer read the agent count,
  // not the session count. See rootAgentId.
  const liveRoots = new Set<string>();
  for (const agent of agents) {
    if (agent.status === 'active') summary.active_agents += 1;
    else if (agent.status === 'idle') summary.idle_agents += 1;
    else summary.offline_agents += 1;

    if (agent.status === 'active' || agent.status === 'idle') {
      liveRoots.add(rootAgentId(agent.agent_id));
    }
    if (agent.has_session) summary.with_sessions += 1;
    if (agent.has_presence) summary.with_presence += 1;
    if (agent.has_spawn) summary.with_spawns += 1;
    if (agent.is_orphan) summary.orphans += 1;
  }
  summary.live_agents = liveRoots.size;

  return summary;
}
