// Runnable fixture / smoke test for workspace-scoped agent grouping — the fix
// for the HUD "Live Sessions not grouping" bug. The lifecycle hooks mint a
// distinct agent_id per conversation (`<base>-<WS_HASH>-<SESSION_SCOPE>`,
// pkg/generator/configs_hooks.go), and parent_session_id is null for sibling
// conversations, so the HUD rendered each conversation as an unrelated row and
// over-counted agents. rootAgentId / groupSessionsByRootAgent collapse them.
//
// The HUD frontend ships no test runner, so this is a self-check:
//   pnpm dlx tsx src/lib/utils/agents.rootGrouping.fixture.ts
//
// Agent ids below are the real values observed live on hud.flexinfer.ai
// (/api/mobile/v1/sessions) at the time of the bug report.

import {
  rootAgentId,
  conversationId,
  groupSessionsByRootAgent,
  summarizeUnifiedAgents,
  type RootGroupableSession,
  type UnifiedAgent,
} from './agents';

function expect(label: string, actual: unknown, want: unknown): boolean {
  const ok = actual === want;
  if (ok) console.log(`PASS ${label}: got=${String(actual)}`);
  else console.error(`FAIL ${label}: got=${String(actual)} want=${String(want)}`);
  return ok;
}

let allOk = true;

// ── rootAgentId: <base>-<WS_HASH>-<SESSION_SCOPE> → <base>-<WS_HASH> ──
allOk =
  expect(
    'two-suffix claude id strips session scope',
    rootAgentId('claude-code-552019522-2804496862'),
    'claude-code-552019522',
  ) && allOk;
allOk =
  expect(
    'sibling claude conversation maps to same root',
    rootAgentId('claude-code-552019522-3116397616'),
    'claude-code-552019522',
  ) && allOk;
allOk =
  expect('bare ws-hash id is its own root', rootAgentId('codex-4188162495'), 'codex-4188162495') &&
  allOk;
allOk =
  expect(
    'codex session-scope strips to ws-hash root',
    rootAgentId('codex-4188162495-2303882182'),
    'codex-4188162495',
  ) && allOk;
allOk =
  expect(
    'non-numeric suffix id passes through (no ws-hash)',
    rootAgentId('codex-7b28'),
    'codex-7b28',
  ) && allOk;
allOk =
  expect(
    'spawn pod id (hex suffix) passes through',
    rootAgentId('spawn-claude-code-10fa8a6eb214'),
    'spawn-claude-code-10fa8a6eb214',
  ) && allOk;
allOk = expect('empty id is empty root', rootAgentId(''), '') && allOk;
allOk =
  expect('bare base with no hash is its own root', rootAgentId('claude-code'), 'claude-code') &&
  allOk;

// ── conversationId: <base>-<WS_HASH>-<SESSION_SCOPE> → <base>-<SESSION_SCOPE> ──
// Drops the WS_HASH, keeps the conversation scope, so one chat that hopped
// repos groups together (the inverse axis of rootAgentId).
allOk =
  expect(
    'flightdeck member of a cross-repo chat keeps its scope',
    conversationId('claude-code-3749726816-1105899468'),
    'claude-code-1105899468',
  ) && allOk;
allOk =
  expect(
    'gitops member of the SAME chat maps to the same conversation',
    conversationId('claude-code-401508988-1105899468'),
    'claude-code-1105899468',
  ) && allOk;
allOk =
  expect(
    'same-repo but different scope is a different conversation',
    conversationId('claude-code-552019522-2804496862'),
    'claude-code-2804496862',
  ) && allOk;
allOk =
  expect(
    'codex session-scope strips the ws-hash',
    conversationId('codex-401508988-2992486099'),
    'codex-2992486099',
  ) && allOk;
allOk =
  expect(
    'bare ws-hash id (no scope) is its own conversation',
    conversationId('codex-401508988'),
    'codex-401508988',
  ) && allOk;
allOk =
  expect(
    'non-numeric suffix id passes through',
    conversationId('codex-7b28'),
    'codex-7b28',
  ) && allOk;
allOk = expect('empty id is empty conversation', conversationId(''), '') && allOk;

// ── grouping: the live 6-session fleet collapses to 5 logical agents ──
const liveSessions: RootGroupableSession[] = [
  { session_id: '5f51bf06', agent_id: 'codex-7b28', agent_status: 'active', last_activity: 600 },
  { session_id: '1605b23e', agent_id: 'codex-4188162495', agent_status: 'active', last_activity: 500 },
  {
    session_id: '53deaf1d',
    agent_id: 'claude-code-1570571821-1796354763',
    agent_status: 'active',
    last_activity: 400,
  },
  {
    session_id: '60b83756',
    agent_id: 'claude-code-552019522-2804496862',
    agent_status: 'idle',
    last_activity: 300,
  },
  {
    session_id: '7fd44262',
    agent_id: 'claude-code-552019522-3116397616',
    agent_status: 'active',
    last_activity: 700, // most-recent overall → its group sorts first
  },
  {
    session_id: '11bdb2ac',
    agent_id: 'codex-1713039686-683244154',
    agent_status: 'active',
    last_activity: 200,
  },
];

const groups = groupSessionsByRootAgent(liveSessions);
allOk = expect('6 sessions collapse to 5 agent groups', groups.length, 5) && allOk;

const shared = groups.find((g) => g.root === 'claude-code-552019522');
allOk = expect('shared root groups both sibling sessions', shared?.sessions.length, 2) && allOk;
// idle + active in the group → group status is the most-live (active).
allOk = expect('group status is the most-live member', shared?.status, 'active') && allOk;
// Group order: the group containing the most-recent session (700) comes first.
allOk = expect('groups sort by most-recent activity', groups[0]?.root, 'claude-code-552019522') && allOk;
// Every other id is a singleton group.
allOk =
  expect(
    'distinct agents stay separate',
    groups.filter((g) => g.sessions.length === 1).length,
    4,
  ) && allOk;

// ── count dedup: summarizeUnifiedAgents.live_agents counts logical agents ──
function agent(id: string, status: 'active' | 'idle' | 'offline'): UnifiedAgent {
  return {
    agent_id: id,
    agent_type: 'claude',
    status,
    source: 'session',
    description: '',
    current_task: '',
    branch: '',
    last_heartbeat: '',
    registered_at: '',
    active_files: [],
    active_file_count: 0,
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

const summary = summarizeUnifiedAgents([
  agent('claude-code-552019522-2804496862', 'idle'),
  agent('claude-code-552019522-3116397616', 'active'),
  agent('codex-4188162495', 'active'),
]);
// Two of the three rows are the same logical agent → 2 live agents, not 3.
allOk = expect('live_agents dedupes per-conversation rows', summary.live_agents, 2) && allOk;
// Per-row status tallies are unchanged (they measure rows, not agents).
allOk = expect('active_agents still counts rows', summary.active_agents, 2) && allOk;
allOk = expect('idle_agents still counts rows', summary.idle_agents, 1) && allOk;

if (!allOk) {
  console.error('agents.rootGrouping fixture: FAILURES detected');
  throw new Error('fixture failed');
}
console.log('agents.rootGrouping fixture: all cases pass');
