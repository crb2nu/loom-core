// Codex twin MERGE in buildUnifiedAgents. Regression guard for the
// presence-only variant of the twin problem: codex is workspace-anchored, so its
// notify hook mints a scopeless `codex-<WS>` id while session/telemetry can
// surface a scoped twin `codex-<WS>-<SCOPE>` for the same app. When one twin has
// NO active session (presence-only) it bypassed the Fleet table's session-tree
// conversation fold and rendered as a separate ungrouped row. buildUnifiedAgents
// now merges same-workspace codex twins into one UnifiedAgent before any
// grouping, so the app is one row regardless of which evidence each twin
// carried. See agents.ts (mergeWorkspaceAnchoredTwins).

import { describe, expect, it } from 'vitest';
import { buildUnifiedAgents, summarizeUnifiedAgents } from './agents.ts';

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

describe('agents — codex twin merge in buildUnifiedAgents', () => {
  const unified = buildUnifiedAgents({ sessions, agents });
  const ids = unified.map((a) => a.agent_id).sort();
  const merged = unified.find((a) => a.agent_id === 'codex-1713039686-2004540290');

  // 4 input agent_ids → 3 unified agents (the two codex-1713039686 twins merge).
  it('twins merge: 3 unified agents', () => {
    expect(unified.length).toBe(3);
  });

  it('scopeless presence-only twin is gone', () => {
    expect(ids.includes('codex-1713039686')).toBe(false);
  });

  it('merged row keeps the session-bearing id', () => {
    expect(ids.includes('codex-1713039686-2004540290')).toBe(true);
  });

  it('other-workspace codex survives', () => {
    expect(ids.includes('codex-389747459')).toBe(true);
  });

  it('claude agent untouched', () => {
    expect(ids.includes('claude-code-552019522-2804496862')).toBe(true);
  });

  it('merged carries presence', () => {
    expect(merged?.has_presence).toBe(true);
  });

  it('merged carries session', () => {
    expect(merged?.has_session).toBe(true);
  });

  it('merged source is presence+session', () => {
    expect(merged?.source).toBe('presence+session');
  });

  it('merged keeps the session namespace', () => {
    expect(merged?.namespace).toBe('services/flexdeck/main');
  });

  it('merged keeps the freshest heartbeat', () => {
    expect(merged?.last_heartbeat).toBe('2026-06-16T14:18:00Z');
  });

  it('merged is not an orphan', () => {
    expect(merged?.is_orphan).toBe(false);
  });

  it('live_agents = 3', () => {
    // The live-agent count (distinct workspace roots) is unchanged by the merge:
    // the twins were already one root. Three live agents across three workspaces.
    const summary = summarizeUnifiedAgents(unified);
    expect(summary.live_agents).toBe(3);
  });
});
