// Codex twin collapse in the Fleet "Live Agents" table. Regression guard for
// the "2 repos" / "same conversation" bug: Codex is workspace-anchored, so its
// notify hook mints `codex-<WS>` (no per-conversation scope) while the fleet
// also sees a scoped twin `codex-<WS>-<SCOPE>` for the SAME app in the SAME
// workspace. conversationId folds both into one bucket (good), but the bucket
// then had two member-lists and the renderer mislabeled them "2 repos" with a
// nested "same conversation" child — implying two repos and two agents where
// there is one codex in one repo. buildFleetRows now collapses bucket members
// that share a workspace identity (rootAgentId = base+WS_HASH), keeping only
// the freshest. See fleetRows.ts.

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
  return { id, agent_id: agentId, agent: agentId, namespace, started_at: startedAt, ended_at: null, status: 'active' };
}

// One Codex app in workspace WS_HASH 1713039686, seen under both its scopeless
// notify-hook id (`codex-1713039686`, heartbeat 14:17) and a scoped telemetry
// twin (`codex-1713039686-180612849`, heartbeat 14:18 — fresher). Plus a SECOND,
// genuinely-different Codex app in another workspace (WS_HASH 389747459) to prove
// distinct workspaces still stay separate.
const c1 = agent('codex-1713039686', 's1', '2026-06-14T14:17:00Z');
const c2 = agent('codex-1713039686-180612849', 's2', '2026-06-14T14:18:00Z');
const c3 = agent('codex-389747459-3485468849', 's3', '2026-06-14T14:15:00Z');
const agents = [c1, c2, c3];

const s1 = session('s1', c1.agent_id, '', '2026-06-14T14:17:00Z');
const s2 = session('s2', c2.agent_id, '', '2026-06-14T14:18:00Z');
const s3 = session('s3', c3.agent_id, 'services/flexdeck/main', '2026-06-14T14:15:00Z');
const sessionById = new Map<string, ReturnType<typeof session>>([
  ['s1', s1],
  ['s2', s2],
  ['s3', s3],
]);

const sessionTree = [
  { session: s1, depth: 0, children: [] },
  { session: s2, depth: 0, children: [] },
  { session: s3, depth: 0, children: [] },
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

describe('fleetRows — codex twin collapse', () => {
  // The twin collapses: 3 agent_ids → 2 rows (one per distinct codex app).
  it('twin collapses to 2 rows', () => {
    expect(result.rows.length).toBe(2);
  });

  it('distinct conversations = 2', () => {
    expect(result.rootGroupCount).toBe(2);
  });

  it('nothing ungrouped', () => {
    expect(result.ungroupedCount).toBe(0);
  });

  // The freshest twin (14:18, scoped) is the surviving row, at depth 0.
  it('row0 is the freshest twin', () => {
    expect(result.rows[0]?.agent.agent_id).toBe('codex-1713039686-180612849');
  });

  it('row0 depth 0', () => {
    expect(result.rows[0]?.depth).toBe(0);
  });

  // No "2 repos" pill: a singleton workspace count is left undefined.
  it('row0 NOT tagged with a member count', () => {
    expect(result.rows[0]?.conversationMemberCount).toBeUndefined();
  });

  it('row0 not a conversation sibling', () => {
    expect(result.rows[0]?.conversationSibling).toBe(false);
  });

  // The dropped twin never resurfaces as its own row anywhere.
  it('scopeless twin does not appear as a separate row', () => {
    expect(result.rows.some((r) => r.agent.agent_id === 'codex-1713039686')).toBe(false);
  });

  // The genuinely-separate codex in another workspace stays its own row.
  it('row1 is the other-workspace codex', () => {
    expect(result.rows[1]?.agent.agent_id).toBe('codex-389747459-3485468849');
  });

  it('row1 depth 0', () => {
    expect(result.rows[1]?.depth).toBe(0);
  });

  it('row1 not a conversation sibling', () => {
    expect(result.rows[1]?.conversationSibling).toBe(false);
  });
});
