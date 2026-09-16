// Conversation grouping in the Fleet "Live Agents" table. The lifecycle hooks
// mint a distinct agent_id per (workspace, conversation) —
// `<base>-<WS_HASH>-<SESSION_SCOPE>` — so one chat that moves across
// repos/worktrees keeps its SESSION_SCOPE but changes its WS_HASH, with NO
// session parent/root linkage (parent_session_id is null). buildFleetRows now
// buckets the session-tree roots by conversationId so those cross-repo members
// nest under a single lead, while distinct chats that merely share a repo
// (different SESSION_SCOPE) stay separate. See fleetRows.ts.

import { describe, expect, it } from 'vitest';
import { buildFleetRows, type FleetRowsInput } from './fleetRows';
import type { UnifiedAgent } from './agents';

function agent(id: string, sessionId: string, lastHeartbeat: string): UnifiedAgent {
  return {
    agent_id: id,
    agent_type: id.startsWith('codex') ? 'codex' : 'claude',
    status: 'active',
    source: 'session',
    description: '',
    current_task: '',
    branch: '',
    last_heartbeat: lastHeartbeat,
    registered_at: '',
    active_files: [],
    active_file_count: 0,
    session_id: sessionId,
    entry_count: 0,
    total_tokens: 0,
    task_count: 0,
    blocked_tasks: 0,
    claim_count: 0,
    heartbeat_age_seconds: 0,
    session_age_seconds: 0,
    telemetry_status: 'session_only',
    has_presence: false,
    has_session: true,
    has_spawn: false,
    is_orphan: false,
    orphan_age_seconds: 0,
  };
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function session(id: string, agentId: string, namespace: string, startedAt: string): any {
  return {
    id,
    agent_id: agentId,
    agent: agentId,
    namespace,
    started_at: startedAt,
    ended_at: null,
    status: 'active',
  };
}

// One Claude conversation (SESSION_SCOPE 1105899468) that worked in two repos:
// flightdeck (WS_HASH 3749726816, heartbeat 14:16) and gitops (WS_HASH
// 401508988, heartbeat 14:20 — fresher). A SEPARATE Claude chat in loom-core
// (different scope 2804496862). Plus a codex. Mirrors the live fleet.
const a1 = agent('claude-code-3749726816-1105899468', 's1', '2026-06-14T14:16:01Z');
const a2 = agent('claude-code-401508988-1105899468', 's2', '2026-06-14T14:20:30Z');
const a3 = agent('claude-code-552019522-2804496862', 's3', '2026-06-14T14:18:00Z');
const a4 = agent('codex-4188162495', 's4', '2026-06-14T14:17:00Z');
const agents = [a1, a2, a3, a4];

const s1 = session('s1', a1.agent_id, 'services/loom-flightdeck/main', '2026-06-14T14:16:01Z');
const s2 = session('s2', a2.agent_id, 'platform/gitops/main', '2026-06-14T14:20:30Z');
const s3 = session('s3', a3.agent_id, 'services/loom-core/main', '2026-06-14T14:18:00Z');
const s4 = session('s4', a4.agent_id, 'services/loom-core/main', '2026-06-14T14:17:00Z');
const sessionById = new Map<string, ReturnType<typeof session>>([
  ['s1', s1],
  ['s2', s2],
  ['s3', s3],
  ['s4', s4],
]);

// Production shape: every session is its own root node (no parent/children).
const sessionTree = [
  { session: s1, depth: 0, children: [] },
  { session: s2, depth: 0, children: [] },
  { session: s3, depth: 0, children: [] },
  { session: s4, depth: 0, children: [] },
];

const agentLookup = new Map<string, UnifiedAgent>(agents.map((a) => [a.agent_id, a]));

const input: FleetRowsInput = {
  agents,
  sortKey: 'heartbeat',
  sortDir: 'desc',
  groupByRootSession: true,
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  sessionById: sessionById as any,
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  sessionTree: sessionTree as any,
  parentSession: () => null,
  rootSession: (id: string) => sessionById.get(id) ?? null,
  childSessions: () => [],
  sessionLineage: (id: string) => {
    const s = sessionById.get(id);
    return s ? [s] : [];
  },
  agentLookup,
};

const result = buildFleetRows(input);

describe('fleetRows — conversation grouping', () => {
  // 4 agents → 3 conversations (the 1105899468 chat has 2 members; loom-core
  // chat and codex are singletons).
  it('all 4 agents still rendered', () => {
    expect(result.rows.length).toBe(4);
  });

  it('distinct conversations = 3', () => {
    expect(result.rootGroupCount).toBe(3);
  });

  it('nothing ungrouped (all have sessions)', () => {
    expect(result.ungroupedCount).toBe(0);
  });

  // The fresher member (gitops, 14:20) leads the conversation; the flightdeck
  // member (14:16) nests under it as "same conversation".
  it('row0 is the freshest member (gitops)', () => {
    expect(result.rows[0]?.agent.agent_id).toBe('claude-code-401508988-1105899468');
  });

  it('row0 depth 0', () => {
    expect(result.rows[0]?.depth).toBe(0);
  });

  it('row0 is not a conversation sibling', () => {
    expect(result.rows[0]?.conversationSibling).toBe(false);
  });

  it('row0 tagged with member count 2 (→ "2 repos")', () => {
    expect(result.rows[0]?.conversationMemberCount).toBe(2);
  });

  it('row1 is the older member (flightdeck)', () => {
    expect(result.rows[1]?.agent.agent_id).toBe('claude-code-3749726816-1105899468');
  });

  it('row1 nested one level', () => {
    expect(result.rows[1]?.depth).toBe(1);
  });

  it('row1 flagged conversationSibling (→ "same conversation")', () => {
    expect(result.rows[1]?.conversationSibling).toBe(true);
  });

  // The same-repo-but-different-chat conversation does NOT merge with anything.
  it('row2 is the separate loom-core chat', () => {
    expect(result.rows[2]?.agent.agent_id).toBe('claude-code-552019522-2804496862');
  });

  it('row2 depth 0', () => {
    expect(result.rows[2]?.depth).toBe(0);
  });

  it('row2 has no member count (singleton)', () => {
    expect(result.rows[2]?.conversationMemberCount).toBeUndefined();
  });

  // Codex is its own conversation at depth 0.
  it('row3 is codex', () => {
    expect(result.rows[3]?.agent.agent_id).toBe('codex-4188162495');
  });

  it('row3 depth 0', () => {
    expect(result.rows[3]?.depth).toBe(0);
  });
});
