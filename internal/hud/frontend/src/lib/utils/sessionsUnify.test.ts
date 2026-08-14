import { describe, expect, it } from 'vitest';
import type { UnifiedAgent } from './agents.ts';
import type { VendorSession } from '../clients/vendorSessions.ts';
import {
  groupSessions,
  linkLiveAgents,
  repoFromCwd,
  sessionTitle,
  transcriptKey,
} from './sessionsUnify.ts';

function agent(id: string, status: 'active' | 'idle' | 'offline' = 'active'): UnifiedAgent {
  return {
    agent_id: id,
    agent_type: 'claude',
    status,
    source: 'presence',
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
    telemetry_status: 'unknown',
    has_presence: true,
    has_session: false,
    has_spawn: false,
    is_orphan: false,
    orphan_age_seconds: 0,
  };
}

function transcript(over: Partial<VendorSession> & Pick<VendorSession, 'vendor' | 'id'>): VendorSession {
  return {
    path: `/x/${over.id}.jsonl`,
    modified_at: '2026-07-30T12:00:00Z',
    size_bytes: 1000,
    ...over,
  };
}

describe('repoFromCwd', () => {
  it('reduces workspace paths to bucket/repo', () => {
    expect(repoFromCwd('/Users/u/workspace/services/loom-core')).toEqual({
      repo: 'services/loom-core',
    });
  });

  it('splits linked worktrees out as a facet of the repo, not a new group', () => {
    expect(
      repoFromCwd(
        '/Users/u/workspace/services/loom-core/.claude/worktrees/mac-session-grouping-65cf',
      ),
    ).toEqual({ repo: 'services/loom-core', worktree: 'mac-session-grouping-65cf' });
    expect(repoFromCwd('/Users/u/workspace/libs/game/.worktrees/feat-jump')).toEqual({
      repo: 'libs/game',
      worktree: 'feat-jump',
    });
  });

  it('falls back to the last two segments outside a workspace', () => {
    expect(repoFromCwd('/Users/u/Documents/Codex/2026-07-30')).toEqual({
      repo: 'Codex/2026-07-30',
    });
    expect(repoFromCwd('')).toEqual({ repo: '(unknown)' });
  });
});

describe('linkLiveAgents', () => {
  // Real vectors: cksum("be2382e8-…") = 1735870880 (SESSION_SCOPE),
  // cksum("/Users/cblevins/workspace/services/familyforge") = 1503458259
  // (WS_HASH) — see cksum.test.ts.
  const claudeTranscript = transcript({
    vendor: 'claude',
    id: 'be2382e8-d62e-40d2-9ed4-abeac9078fbe',
    cwd: '/Users/cblevins/workspace/services/loom-core/.claude/worktrees/mac-session-grouping-transcripts-65cfce',
    title: 'the ui still has some issues with session grouping',
  });
  const codexInteractive = transcript({
    vendor: 'codex',
    id: '019fb52e-b889-7570-937a-628dc7c6039e',
    cwd: '/Users/cblevins/workspace/services/familyforge',
    title: "Let's continue building out the app",
    modified_at: '2026-07-30T12:30:00Z',
  });
  const codexWorker = transcript({
    vendor: 'codex',
    id: '019fb52e-92f5-7ea2-9ed0-e6af8b8dd470',
    cwd: '/Users/cblevins/workspace/services/familyforge',
    title: 'Erdos the 2nd · explorer',
    kind: 'sidechain',
    modified_at: '2026-07-30T13:00:00Z',
  });

  it('links claude agents by SESSION_SCOPE exactly', () => {
    const link = linkLiveAgents(
      [agent('claude-code-3363428570-1735870880')],
      [claudeTranscript, codexInteractive],
    );
    expect(link.liveKeys.has(transcriptKey(claudeTranscript))).toBe(true);
    expect(link.liveKeys.has(transcriptKey(codexInteractive))).toBe(false);
    expect(link.byConversation.get('claude-code-1735870880')).toMatchObject({
      title: 'the ui still has some issues with session grouping',
      repo: 'services/loom-core',
      vendor: 'claude',
    });
  });

  it('links scopeless codex agents by WS_HASH, ignoring background transcripts', () => {
    const link = linkLiveAgents(
      [agent('codex-1503458259')],
      [codexWorker, codexInteractive],
    );
    // The worker is NEWER but kind-tagged; the interactive transcript must
    // carry the workspace identity.
    expect(link.liveKeys.has(transcriptKey(codexInteractive))).toBe(true);
    expect(link.liveKeys.has(transcriptKey(codexWorker))).toBe(false);
    expect(link.byConversation.get('codex-1503458259')?.title).toBe(
      "Let's continue building out the app",
    );
  });

  it('never links offline agents', () => {
    const link = linkLiveAgents(
      [agent('claude-code-3363428570-1735870880', 'offline')],
      [claudeTranscript],
    );
    expect(link.liveKeys.size).toBe(0);
  });
});

describe('groupSessions', () => {
  it('groups by repo, collapses background kinds, and leads with live rows', () => {
    const live = transcript({
      vendor: 'claude',
      id: 'be2382e8-d62e-40d2-9ed4-abeac9078fbe',
      cwd: '/Users/u/workspace/services/loom-core',
      title: 'live one',
      modified_at: '2026-07-30T10:00:00Z',
    });
    const newerButIdle = transcript({
      vendor: 'claude',
      id: 'aaaa1111-0000-0000-0000-000000000001',
      cwd: '/Users/u/workspace/services/loom-core/.claude/worktrees/wt-a',
      title: 'newer but idle',
      modified_at: '2026-07-30T12:00:00Z',
    });
    const worker = transcript({
      vendor: 'codex',
      id: '019f-worker',
      cwd: '/Users/u/workspace/services/loom-core',
      kind: 'sidechain',
      modified_at: '2026-07-30T13:00:00Z',
    });
    const otherRepo = transcript({
      vendor: 'codex',
      id: '019f-other',
      cwd: '/Users/u/workspace/services/familyforge',
      modified_at: '2026-07-29T09:00:00Z',
      host: 'Codys-MacBook-Air.local',
    });

    const linkage = {
      liveKeys: new Set([transcriptKey(live)]),
      byConversation: new Map(),
    };
    const groups = groupSessions([live, newerButIdle, worker, otherRepo], linkage);

    expect(groups.map((g) => g.repo)).toEqual(['services/loom-core', 'services/familyforge']);
    const loom = groups[0];
    // Live row leads despite being older; worktree facet preserved.
    expect(loom.rows.map((r) => r.session.title)).toEqual(['live one', 'newer but idle']);
    expect(loom.rows[1].worktree).toBe('wt-a');
    // Background collapsed out of the interactive flow.
    expect(loom.background).toHaveLength(1);
    expect(loom.background[0].session.kind).toBe('sidechain');
    // Federated host surfaces on its group.
    expect(groups[1].hosts).toEqual(['Codys-MacBook-Air.local']);
  });
});

describe('sessionTitle', () => {
  it('prefers title, then source, then a truncated id', () => {
    expect(sessionTitle(transcript({ vendor: 'codex', id: 'x'.repeat(20), title: 't' }))).toBe('t');
    expect(sessionTitle(transcript({ vendor: 'codex', id: 'x'.repeat(20), source: 'vscode' }))).toBe(
      'vscode',
    );
    expect(sessionTitle(transcript({ vendor: 'codex', id: 'x'.repeat(20) }))).toBe('x'.repeat(12));
  });
});
