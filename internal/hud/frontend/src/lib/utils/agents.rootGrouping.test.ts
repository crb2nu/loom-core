// Workspace-scoped agent grouping — the fix for the HUD "Live Sessions not
// grouping" bug. The lifecycle hooks mint a distinct agent_id per conversation
// (`<base>-<WS_HASH>-<SESSION_SCOPE>`, pkg/generator/configs_hooks.go), and
// parent_session_id is null for sibling conversations, so the HUD rendered each
// conversation as an unrelated row and over-counted agents. rootAgentId /
// groupSessionsByRootAgent collapse them.
//
// Agent ids below are the real values observed live on hud.flexinfer.ai
// (/api/mobile/v1/sessions) at the time of the bug report.

import { describe, expect, it } from 'vitest';
import {
  rootAgentId,
  conversationId,
  groupSessionsByRootAgent,
  groupSessionsByConversation,
  summarizeUnifiedAgents,
  type RootGroupableSession,
  type UnifiedAgent,
} from './agents.ts';

// ── rootAgentId: <base>-<WS_HASH>-<SESSION_SCOPE> → <base>-<WS_HASH> ──
describe('agents — rootAgentId', () => {
  it('two-suffix claude id strips session scope', () => {
    expect(rootAgentId('claude-code-552019522-2804496862')).toBe('claude-code-552019522');
  });

  it('sibling claude conversation maps to same root', () => {
    expect(rootAgentId('claude-code-552019522-3116397616')).toBe('claude-code-552019522');
  });

  it('bare ws-hash id is its own root', () => {
    expect(rootAgentId('codex-4188162495')).toBe('codex-4188162495');
  });

  it('codex session-scope strips to ws-hash root', () => {
    expect(rootAgentId('codex-4188162495-2303882182')).toBe('codex-4188162495');
  });

  it('non-numeric suffix id passes through (no ws-hash)', () => {
    expect(rootAgentId('codex-7b28')).toBe('codex-7b28');
  });

  it('spawn pod id (hex suffix) passes through', () => {
    expect(rootAgentId('spawn-claude-code-10fa8a6eb214')).toBe('spawn-claude-code-10fa8a6eb214');
  });

  it('empty id is empty root', () => {
    expect(rootAgentId('')).toBe('');
  });

  it('bare base with no hash is its own root', () => {
    expect(rootAgentId('claude-code')).toBe('claude-code');
  });
});

// ── conversationId: <base>-<WS_HASH>-<SESSION_SCOPE> → <base>-<SESSION_SCOPE> ──
// Drops the WS_HASH, keeps the conversation scope, so one chat that hopped
// repos groups together (the inverse axis of rootAgentId).
describe('agents — conversationId', () => {
  it('flightdeck member of a cross-repo chat keeps its scope', () => {
    expect(conversationId('claude-code-3749726816-1105899468')).toBe('claude-code-1105899468');
  });

  it('gitops member of the SAME chat maps to the same conversation', () => {
    expect(conversationId('claude-code-401508988-1105899468')).toBe('claude-code-1105899468');
  });

  it('same-repo but different scope is a different conversation', () => {
    expect(conversationId('claude-code-552019522-2804496862')).toBe('claude-code-2804496862');
  });

  // Codex is workspace-anchored: scoped and scopeless ids of one app fold by
  // WS_HASH (NOT by scope, which would fragment the codex app into many rows).
  it('codex scoped id folds to its workspace anchor', () => {
    expect(conversationId('codex-401508988-2992486099')).toBe('codex-401508988');
  });

  it('codex scopeless id folds with the scoped variant above', () => {
    expect(conversationId('codex-401508988')).toBe('codex-401508988');
  });

  it('non-numeric suffix id passes through', () => {
    expect(conversationId('codex-7b28')).toBe('codex-7b28');
  });

  it('empty id is empty conversation', () => {
    expect(conversationId('')).toBe('');
  });
});

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

describe('agents — groupSessionsByRootAgent', () => {
  const groups = groupSessionsByRootAgent(liveSessions);
  const shared = groups.find((g) => g.root === 'claude-code-552019522');

  it('6 sessions collapse to 5 agent groups', () => {
    expect(groups.length).toBe(5);
  });

  it('shared root groups both sibling sessions', () => {
    expect(shared?.sessions.length).toBe(2);
  });

  it('group status is the most-live member', () => {
    // idle + active in the group → group status is the most-live (active).
    expect(shared?.status).toBe('active');
  });

  it('groups sort by most-recent activity', () => {
    // Group order: the group containing the most-recent session (700) comes first.
    expect(groups[0]?.root).toBe('claude-code-552019522');
  });

  it('distinct agents stay separate', () => {
    // Every other id is a singleton group.
    expect(groups.filter((g) => g.sessions.length === 1).length).toBe(4);
  });
});

// ── conversation grouping: the inverse axis of root-agent grouping ──
// The Live Sessions card buckets by conversation (groupSessionsByConversation),
// matching the Fleet "Live Agents" table. The SAME six sessions group
// differently under each axis: a cross-repo chat (one SESSION_SCOPE seen under
// three WS_HASHes) collapses to ONE conversation but SPLITS into three root
// agents; a same-repo pair (one WS_HASH, two SESSION_SCOPEs) stays TWO
// conversations but MERGES into one root agent.
const mixedSessions: RootGroupableSession[] = [
  // One chat (scope 1105899468) that worked in flightdeck, gitops, and an
  // agents namespace — three WS_HASHes, one conversation.
  {
    session_id: 'aa',
    agent_id: 'claude-code-3749726816-1105899468',
    agent_status: 'idle',
    last_activity: 100,
  },
  {
    session_id: 'bb',
    agent_id: 'claude-code-401508988-1105899468',
    agent_status: 'active',
    last_activity: 800, // most-recent overall → its conversation sorts first
  },
  {
    session_id: 'cc',
    agent_id: 'claude-code-1305365710-1105899468',
    agent_status: 'idle',
    last_activity: 300,
  },
  // Two distinct chats in the SAME repo (WS_HASH 552019522, two scopes).
  {
    session_id: 'dd',
    agent_id: 'claude-code-552019522-2804496862',
    agent_status: 'active',
    last_activity: 500,
  },
  {
    session_id: 'ee',
    agent_id: 'claude-code-552019522-3116397616',
    agent_status: 'idle',
    last_activity: 400,
  },
  // An unrelated singleton.
  { session_id: 'ff', agent_id: 'codex-7b28', agent_status: 'active', last_activity: 200 },
];

describe('agents — groupSessionsByConversation (inverse axis)', () => {
  const convGroups = groupSessionsByConversation(mixedSessions);
  const crossRepoChat = convGroups.find((g) => g.root === 'claude-code-1105899468');

  it('6 sessions collapse to 4 conversations', () => {
    expect(convGroups.length).toBe(4);
  });

  it('cross-repo chat unifies its 3 sessions', () => {
    expect(crossRepoChat?.sessions.length).toBe(3);
  });

  it('conversation status is the most-live member', () => {
    // idle + active + idle across the group → most-live wins.
    expect(crossRepoChat?.status).toBe('active');
  });

  it('conversations sort by most-recent activity', () => {
    // The conversation holding the most-recent session (800) leads.
    expect(convGroups[0]?.root).toBe('claude-code-1105899468');
  });

  it('same-repo distinct chats stay separate conversations', () => {
    // The same-repo pair are TWO separate conversations here (they MERGE under root).
    expect(
      convGroups.filter(
        (g) => g.root === 'claude-code-2804496862' || g.root === 'claude-code-3116397616',
      ).length,
    ).toBe(2);
  });

  // Same data, root-agent axis: cross-repo chat SPLITS to 3, same-repo pair
  // MERGES to 1 → 5 groups. Proves the two groupers are genuine inverses.
  const rootGroups = groupSessionsByRootAgent(mixedSessions);

  it('same 6 sessions are 5 root-agent groups', () => {
    expect(rootGroups.length).toBe(5);
  });

  it('cross-repo chat splits into 3 distinct root agents', () => {
    const crossRepoRoots = [
      'claude-code-3749726816',
      'claude-code-401508988',
      'claude-code-1305365710',
    ];
    expect(
      crossRepoRoots.filter((r) => rootGroups.some((g) => g.root === r && g.sessions.length === 1))
        .length,
    ).toBe(3);
  });

  it('same-repo pair merges under one root agent', () => {
    const sameRepoRoot = rootGroups.find((g) => g.root === 'claude-code-552019522');
    expect(sameRepoRoot?.sessions.length).toBe(2);
  });
});

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

describe('agents — summarizeUnifiedAgents live_agents dedup', () => {
  const summary = summarizeUnifiedAgents([
    agent('claude-code-552019522-2804496862', 'idle'),
    agent('claude-code-552019522-3116397616', 'active'),
    agent('codex-4188162495', 'active'),
  ]);

  it('live_agents dedupes per-conversation rows', () => {
    // Two of the three rows are the same logical agent → 2 live agents, not 3.
    expect(summary.live_agents).toBe(2);
  });

  it('active_agents still counts rows', () => {
    // Per-row status tallies are unchanged (they measure rows, not agents).
    expect(summary.active_agents).toBe(2);
  });

  it('idle_agents still counts rows', () => {
    expect(summary.idle_agents).toBe(1);
  });
});
