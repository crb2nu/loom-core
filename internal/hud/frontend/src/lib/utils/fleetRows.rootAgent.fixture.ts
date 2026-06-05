// Runnable fixture / smoke test for workspace-agent grouping in the Fleet
// "Live Agents" table. Sibling conversations of one agent have NO session
// parent/root linkage in production (parent_session_id is null), so the
// session-tree grouping treated each as its own "root session". buildFleetRows
// now buckets the session-tree roots by rootAgentId so siblings nest under one
// lead. See fleetRows.ts.
//
//   pnpm dlx tsx src/lib/utils/fleetRows.rootAgent.fixture.ts

import { buildFleetRows, type FleetRowsInput } from './fleetRows';
import type { UnifiedAgent } from './agents';

function expect(label: string, actual: unknown, want: unknown): boolean {
  const ok = actual === want;
  if (ok) console.log(`PASS ${label}: got=${String(actual)}`);
  else console.error(`FAIL ${label}: got=${String(actual)} want=${String(want)}`);
  return ok;
}

let allOk = true;

function agent(id: string, sessionId: string): UnifiedAgent {
  return {
    agent_id: id,
    agent_type: id.startsWith('codex') ? 'codex' : 'claude',
    status: 'active',
    source: 'session',
    description: '',
    current_task: '',
    branch: '',
    last_heartbeat: '',
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
function session(id: string, agentId: string, startedAt: string): any {
  return {
    id,
    agent_id: agentId,
    agent: agentId,
    namespace: 'services/loom-core/feat/x',
    started_at: startedAt,
    ended_at: null,
    status: 'active',
  };
}

// Two sibling Claude conversations (same workspace root claude-code-552019522,
// no parent linkage) + one codex. This mirrors the live fleet.
const a1 = agent('claude-code-552019522-2804496862', 's1');
const a2 = agent('claude-code-552019522-3116397616', 's2');
const a3 = agent('codex-4188162495', 's3');
const agents = [a1, a2, a3];

const s1 = session('s1', a1.agent_id, '2026-06-05T17:00:00Z');
const s2 = session('s2', a2.agent_id, '2026-06-05T17:01:00Z');
const s3 = session('s3', a3.agent_id, '2026-06-05T17:02:00Z');
const sessionById = new Map<string, ReturnType<typeof session>>([
  ['s1', s1],
  ['s2', s2],
  ['s3', s3],
]);

// Production shape: every session is its own root node (no parent/children).
const sessionTree = [
  { session: s1, depth: 0, children: [] },
  { session: s2, depth: 0, children: [] },
  { session: s3, depth: 0, children: [] },
];

const agentLookup = new Map<string, UnifiedAgent>(agents.map((a) => [a.agent_id, a]));

const input: FleetRowsInput = {
  agents,
  sortKey: 'agent',
  sortDir: 'asc',
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

// 3 agents, 2 logical roots (claude bucket has 2 siblings, codex has 1).
allOk = expect('all 3 agents still rendered', result.rows.length, 3) && allOk;
allOk = expect('distinct root groups = 2', result.rootGroupCount, 2) && allOk;
allOk = expect('nothing ungrouped (all have sessions)', result.ungroupedCount, 0) && allOk;

// Lead of the claude bucket at depth 0, sibling nested at depth 1.
allOk = expect('row0 is claude lead', result.rows[0]?.agent.agent_id, 'claude-code-552019522-2804496862') && allOk;
allOk = expect('row0 depth 0', result.rows[0]?.depth, 0) && allOk;
allOk = expect('row0 not an agent-root child', result.rows[0]?.agentRootChild, false) && allOk;

allOk = expect('row1 is the sibling conversation', result.rows[1]?.agent.agent_id, 'claude-code-552019522-3116397616') && allOk;
allOk = expect('row1 nested one level', result.rows[1]?.depth, 1) && allOk;
allOk = expect('row1 flagged agentRootChild (→ "same agent")', result.rows[1]?.agentRootChild, true) && allOk;

// Codex is its own separate root group at depth 0.
allOk = expect('row2 is codex root', result.rows[2]?.agent.agent_id, 'codex-4188162495') && allOk;
allOk = expect('row2 depth 0', result.rows[2]?.depth, 0) && allOk;

if (!allOk) {
  console.error('fleetRows.rootAgent fixture: FAILURES detected');
  throw new Error('fixture failed');
}
console.log('fleetRows.rootAgent fixture: all cases pass');
