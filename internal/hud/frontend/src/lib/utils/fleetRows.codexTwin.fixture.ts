// Runnable fixture / smoke test for the Codex twin collapse in the Fleet
// "Live Agents" table. Regression guard for the "2 repos" / "same conversation"
// bug: Codex is workspace-anchored, so its notify hook mints `codex-<WS>` (no
// per-conversation scope) while the fleet also sees a scoped twin
// `codex-<WS>-<SCOPE>` for the SAME app in the SAME workspace. conversationId
// folds both into one bucket (good), but the bucket then had two member-lists
// and the renderer mislabeled them "2 repos" with a nested "same conversation"
// child — implying two repos and two agents where there is one codex in one
// repo. buildFleetRows now collapses bucket members that share a workspace
// identity (rootAgentId = base+WS_HASH), keeping only the freshest. See
// fleetRows.ts.
//
//   pnpm dlx tsx src/lib/utils/fleetRows.codexTwin.fixture.ts

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

// The twin collapses: 3 agent_ids → 2 rows (one per distinct codex app).
allOk = expect('twin collapses to 2 rows', result.rows.length, 2) && allOk;
allOk = expect('distinct conversations = 2', result.rootGroupCount, 2) && allOk;
allOk = expect('nothing ungrouped', result.ungroupedCount, 0) && allOk;

// The freshest twin (14:18, scoped) is the surviving row, at depth 0.
allOk = expect('row0 is the freshest twin', result.rows[0]?.agent.agent_id, 'codex-1713039686-180612849') && allOk;
allOk = expect('row0 depth 0', result.rows[0]?.depth, 0) && allOk;
// No "2 repos" pill: a singleton workspace count is left undefined.
allOk = expect('row0 NOT tagged with a member count', result.rows[0]?.conversationMemberCount, undefined) && allOk;
allOk = expect('row0 not a conversation sibling', result.rows[0]?.conversationSibling, false) && allOk;

// The dropped twin never resurfaces as its own row anywhere.
allOk =
  expect(
    'scopeless twin does not appear as a separate row',
    result.rows.some((r) => r.agent.agent_id === 'codex-1713039686'),
    false,
  ) && allOk;

// The genuinely-separate codex in another workspace stays its own row.
allOk = expect('row1 is the other-workspace codex', result.rows[1]?.agent.agent_id, 'codex-389747459-3485468849') && allOk;
allOk = expect('row1 depth 0', result.rows[1]?.depth, 0) && allOk;
allOk = expect('row1 not a conversation sibling', result.rows[1]?.conversationSibling, false) && allOk;

if (!allOk) {
  console.error('fleetRows.codexTwin fixture: FAILURES detected');
  throw new Error('fixture failed');
}
console.log('fleetRows.codexTwin fixture: all cases pass');
