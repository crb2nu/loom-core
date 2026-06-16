// Runnable fixture for the Codex twin MERGE in buildUnifiedAgents. Regression
// guard for the presence-only variant of the twin problem: codex is
// workspace-anchored, so its notify hook mints a scopeless `codex-<WS>` id while
// session/telemetry can surface a scoped twin `codex-<WS>-<SCOPE>` for the same
// app. When one twin has NO active session (presence-only) it bypassed the Fleet
// table's session-tree conversation fold and rendered as a separate ungrouped
// row. buildUnifiedAgents now merges same-workspace codex twins into one
// UnifiedAgent before any grouping, so the app is one row regardless of which
// evidence each twin carried. See agents.ts (mergeWorkspaceAnchoredTwins).
//
//   pnpm dlx tsx src/lib/utils/agents.codexTwinMerge.fixture.ts

import { buildUnifiedAgents, summarizeUnifiedAgents } from './agents';

function expect(label: string, actual: unknown, want: unknown): boolean {
  const ok = actual === want;
  if (ok) console.log(`PASS ${label}: got=${String(actual)}`);
  else console.error(`FAIL ${label}: got=${String(actual)} want=${String(want)}`);
  return ok;
}

let allOk = true;

// One codex app in workspace WS_HASH 1713039686, seen as a presence-only
// scopeless twin (`codex-1713039686`, heartbeat 14:17) AND a scoped twin with an
// active session (`codex-1713039686-2004540290`, heartbeat 14:18). Plus a
// genuinely-separate codex in another workspace, and a claude agent — neither may
// be folded in.
const sessions = [
  { id: 's-scoped', agent_id: 'codex-1713039686-2004540290', status: 'active', started_at: '2026-06-16T14:18:00Z', namespace: 'services/flexdeck/main' },
  { id: 's-other', agent_id: 'codex-389747459', status: 'active', started_at: '2026-06-16T14:10:00Z', namespace: 'services/loom-core/main' },
  { id: 's-claude', agent_id: 'claude-code-552019522-2804496862', status: 'active', started_at: '2026-06-16T14:12:00Z', namespace: 'services/loom-core/main' },
];

const agents = [
  { agent_id: 'codex-1713039686', status: 'active', has_presence: true, last_heartbeat: '2026-06-16T14:17:00Z' },
  { agent_id: 'codex-1713039686-2004540290', status: 'active', session_id: 's-scoped', has_presence: true, last_heartbeat: '2026-06-16T14:18:00Z' },
  { agent_id: 'codex-389747459', status: 'active', has_presence: true, last_heartbeat: '2026-06-16T14:10:00Z' },
  { agent_id: 'claude-code-552019522-2804496862', status: 'active', has_presence: true, last_heartbeat: '2026-06-16T14:12:00Z' },
];

const unified = buildUnifiedAgents({ sessions, agents });

// 4 input agent_ids → 3 unified agents (the two codex-1713039686 twins merge).
allOk = expect('twins merge: 3 unified agents', unified.length, 3) && allOk;

const ids = unified.map((a) => a.agent_id).sort();
allOk = expect('scopeless presence-only twin is gone', ids.includes('codex-1713039686'), false) && allOk;
allOk = expect('merged row keeps the session-bearing id', ids.includes('codex-1713039686-2004540290'), true) && allOk;
allOk = expect('other-workspace codex survives', ids.includes('codex-389747459'), true) && allOk;
allOk = expect('claude agent untouched', ids.includes('claude-code-552019522-2804496862'), true) && allOk;

const merged = unified.find((a) => a.agent_id === 'codex-1713039686-2004540290');
allOk = expect('merged carries presence', merged?.has_presence, true) && allOk;
allOk = expect('merged carries session', merged?.has_session, true) && allOk;
allOk = expect('merged source is presence+session', merged?.source, 'presence+session') && allOk;
allOk = expect('merged keeps the session namespace', merged?.namespace, 'services/flexdeck/main') && allOk;
allOk = expect('merged keeps the freshest heartbeat', merged?.last_heartbeat, '2026-06-16T14:18:00Z') && allOk;
allOk = expect('merged is not an orphan', merged?.is_orphan, false) && allOk;

// The live-agent count (distinct workspace roots) is unchanged by the merge: the
// twins were already one root. Three live agents across three workspaces.
const summary = summarizeUnifiedAgents(unified);
allOk = expect('live_agents = 3', summary.live_agents, 3) && allOk;

if (!allOk) {
  console.error('agents.codexTwinMerge fixture: FAILURES detected');
  throw new Error('fixture failed');
}
console.log('agents.codexTwinMerge fixture: all cases pass');
