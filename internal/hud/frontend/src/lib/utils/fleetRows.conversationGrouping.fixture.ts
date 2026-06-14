// Runnable fixture / smoke test for conversation grouping in the Fleet
// "Live Agents" table. The lifecycle hooks mint a distinct agent_id per
// (workspace, conversation) — `<base>-<WS_HASH>-<SESSION_SCOPE>` — so one chat
// that moves across repos/worktrees keeps its SESSION_SCOPE but changes its
// WS_HASH, with NO session parent/root linkage (parent_session_id is null).
// buildFleetRows now buckets the session-tree roots by conversationId so those
// cross-repo members nest under a single lead, while distinct chats that merely
// share a repo (different SESSION_SCOPE) stay separate. See fleetRows.ts.
//
//   pnpm dlx tsx src/lib/utils/fleetRows.conversationGrouping.fixture.ts

import { buildFleetRows, type FleetRowsInput } from './fleetRows';
import type { UnifiedAgent } from './agents';

function expect(label: string, actual: unknown, want: unknown): boolean {
  const ok = actual === want;
  if (ok) console.log(`PASS ${label}: got=${String(actual)}`);
  else console.error(`FAIL ${label}: got=${String(actual)} want=${String(want)}`);
  return ok;
}

let allOk = true;

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

// deno-lint-ignore no-explicit-any
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

// 4 agents → 3 conversations (the 1105899468 chat has 2 members; loom-core chat
// and codex are singletons).
allOk = expect('all 4 agents still rendered', result.rows.length, 4) && allOk;
allOk = expect('distinct conversations = 3', result.rootGroupCount, 3) && allOk;
allOk = expect('nothing ungrouped (all have sessions)', result.ungroupedCount, 0) && allOk;

// The fresher member (gitops, 14:20) leads the conversation; the flightdeck
// member (14:16) nests under it as "same conversation".
allOk = expect('row0 is the freshest member (gitops)', result.rows[0]?.agent.agent_id, 'claude-code-401508988-1105899468') && allOk;
allOk = expect('row0 depth 0', result.rows[0]?.depth, 0) && allOk;
allOk = expect('row0 is not a conversation sibling', result.rows[0]?.conversationSibling, false) && allOk;
allOk = expect('row0 tagged with member count 2 (→ "2 repos")', result.rows[0]?.conversationMemberCount, 2) && allOk;

allOk = expect('row1 is the older member (flightdeck)', result.rows[1]?.agent.agent_id, 'claude-code-3749726816-1105899468') && allOk;
allOk = expect('row1 nested one level', result.rows[1]?.depth, 1) && allOk;
allOk = expect('row1 flagged conversationSibling (→ "same conversation")', result.rows[1]?.conversationSibling, true) && allOk;

// The same-repo-but-different-chat conversation does NOT merge with anything.
allOk = expect('row2 is the separate loom-core chat', result.rows[2]?.agent.agent_id, 'claude-code-552019522-2804496862') && allOk;
allOk = expect('row2 depth 0', result.rows[2]?.depth, 0) && allOk;
allOk = expect('row2 has no member count (singleton)', result.rows[2]?.conversationMemberCount, undefined) && allOk;

// Codex is its own conversation at depth 0.
allOk = expect('row3 is codex', result.rows[3]?.agent.agent_id, 'codex-4188162495') && allOk;
allOk = expect('row3 depth 0', result.rows[3]?.depth, 0) && allOk;

if (!allOk) {
  console.error('fleetRows.conversationGrouping fixture: FAILURES detected');
  throw new Error('fixture failed');
}
console.log('fleetRows.conversationGrouping fixture: all cases pass');
