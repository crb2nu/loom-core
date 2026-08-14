import { describe, expect, it } from 'vitest';
import {
  MIRROR_PLACEHOLDER_DESCRIPTION,
  agentRows,
  findRow,
  laneSummary,
  millEfficiency,
  millsRunRows,
  mrRows,
  rowKey,
  sortRowsPinned,
  sortRowsStable,
} from './operatorHelpers.ts';
import type { BacklogItem, PipelineRun } from '../stores/mills.svelte.ts';
import type { MRWatchMergeRequest } from '../stores/mrwatch.svelte.ts';
import type { UnifiedAgent } from './agents.ts';

function run(over: Partial<PipelineRun>): PipelineRun {
  return {
    ID: 'run-1',
    BacklogID: 'psl-1',
    Template: 'implement',
    State: 'running',
    Attempts: 1,
    ...over,
  } as PipelineRun;
}

function backlog(over: Partial<BacklogItem>): BacklogItem {
  return {
    ID: 'psl-1',
    Title: 'Ship the widget',
    State: 'strung',
    Priority: 'P2',
    ...over,
  } as BacklogItem;
}

function mr(over: Partial<MRWatchMergeRequest>): MRWatchMergeRequest {
  return {
    repo: 'services/loom-core',
    iid: 42,
    title: 'feat: widget',
    source_branch: 'feat/widget',
    state: 'ok',
    last_transition_at: new Date().toISOString(),
    stale: false,
    ...over,
  } as MRWatchMergeRequest;
}

function agent(over: Partial<UnifiedAgent>): UnifiedAgent {
  return {
    agent_id: 'claude-code-abc123',
    agent_type: 'claude-code',
    status: 'active',
    source: 'presence+session',
    description: 'Claude Code · loom-core',
    current_task: 'Building the deck',
    branch: 'feat/deck',
    last_heartbeat: new Date().toISOString(),
    registered_at: new Date().toISOString(),
    active_files: [],
    active_file_count: 0,
    entry_count: 0,
    total_tokens: 0,
    task_count: 0,
    blocked_tasks: 0,
    claim_count: 0,
    heartbeat_age_seconds: 5,
    session_age_seconds: 60,
    telemetry_status: 'real',
    has_presence: true,
    has_session: true,
    has_spawn: false,
    is_orphan: false,
    orphan_age_seconds: 0,
    ...over,
  } as UnifiedAgent;
}

describe('millsRunRows', () => {
  it('titles a run from its backlog item and keeps stage/attempts in the subtitle', () => {
    const rows = millsRunRows(
      [run({ CurrentStage: 'implement', Attempts: 2 })],
      [backlog({})],
    );
    expect(rows).toHaveLength(1);
    expect(rows[0].title).toBe('Ship the widget');
    expect(rows[0].subtitle).toContain('implement');
    expect(rows[0].subtitle).toContain('attempt 2');
    expect(rows[0].key).toBe(rowKey('mills', 'run-1'));
  });

  it('degrades to the backlog id when the item is missing, never dropping the run', () => {
    const rows = millsRunRows([run({ BacklogID: 'psl-gone' })], []);
    expect(rows).toHaveLength(1);
    expect(rows[0].title).toBe('psl-gone');
  });

  it('maps escalated to error and queued to idle', () => {
    const rows = millsRunRows(
      [run({ ID: 'a', State: 'escalated' }), run({ ID: 'b', State: 'queued' })],
      [],
    );
    expect(rows[0].severity).toBe('error');
    expect(rows[1].severity).toBe('idle');
  });

  it.each(['merged', 'merged_after_escalation'])(
    'downgrades a %s verdict to settled severity',
    (verdictClass) => {
      const rows = millsRunRows([
        { ...run({ State: 'escalated' }), Verdict: { class: verdictClass } },
      ], []);
      expect(rows[0].severity).toBe('ok');
    },
  );
});

describe('mrRows', () => {
  it('builds the repo!iid ref and carries the web link', () => {
    const rows = mrRows([mr({ web_url: 'https://gitlab.example/x/-/merge_requests/42' })]);
    expect(rows[0].id).toBe('services/loom-core!42');
    expect(rows[0].href).toContain('merge_requests/42');
  });

  it('lets a failed head pipeline outrank an ok classification', () => {
    const rows = mrRows([mr({ state: 'ok', pipeline_status: 'failed' })]);
    expect(rows[0].severity).toBe('error');
  });

  it('renders ok+running as busy and conflict as error', () => {
    const rows = mrRows([
      mr({ iid: 1, state: 'ok', pipeline_status: 'running' }),
      mr({ iid: 2, state: 'conflict' }),
    ]);
    expect(rows[0].severity).toBe('busy');
    expect(rows[1].severity).toBe('error');
  });

  it('maps every actionable taxonomy state and excludes terminal records', () => {
    const rows = mrRows([
      mr({ iid: 1, state: 'awaiting_pipeline' }),
      mr({ iid: 2, state: 'ci_running' }),
      mr({ iid: 3, state: 'ci_failed_flaky' }),
      mr({ iid: 4, state: 'ci_failed_deterministic' }),
      mr({ iid: 5, state: 'automerge_unarmed' }),
      mr({ iid: 6, state: 'pipeline_skipped' }),
      mr({ iid: 7, state: 'stale_branch' }),
      mr({ iid: 8, state: 'draft_idle' }),
      mr({ iid: 9, state: 'merged', merged: true }),
      mr({ iid: 10, state: 'closed' }),
    ]);
    expect(rows.map((row) => row.severity)).toEqual([
      'busy', 'busy', 'warn', 'error', 'warn', 'warn', 'warn', 'idle',
    ]);
    expect(rows).toHaveLength(8);
  });
});

describe('agentRows', () => {
  it('prefers current_task, then branch, for the subtitle', () => {
    const rows = agentRows([
      agent({}),
      agent({ agent_id: 'b', current_task: '', branch: 'fix/x' }),
    ]).sort((a, b) => a.id.localeCompare(b.id));
    expect(rows[0].subtitle).toBe('on fix/x');
    expect(rows[1].subtitle).toBe('Building the deck');
  });

  it('collapses one conversation across workspaces into a single stable row', () => {
    // Same SESSION_SCOPE (999) under two WS_HASHes — one chat, two worktrees.
    const rows = agentRows([
      agent({ agent_id: 'claude-code-111-999', status: 'idle' }),
      agent({ agent_id: 'claude-code-222-999', status: 'active', current_task: 'lead work' }),
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0].id).toBe('claude-code-999');
    expect(rows[0].subtitle).toBe('lead work · 2 sessions');
    // Drill-down carries the most-live member's concrete agent_id.
    expect(rows[0].drillId).toBe('claude-code-222-999');
    expect(rows[0].severity).toBe('busy');
  });

  it('warns only when EVERY member reads orphaned — one healthy member wins', () => {
    const solo = agentRows([agent({ is_orphan: true })]);
    expect(solo[0].state).toBe('orphan');
    expect(solo[0].severity).toBe('warn');

    const mixed = agentRows([
      agent({ agent_id: 'claude-code-111-999', is_orphan: true }),
      agent({ agent_id: 'claude-code-222-999', is_orphan: false }),
    ]);
    expect(mixed).toHaveLength(1);
    expect(mixed[0].severity).toBe('busy');
  });

  it('lingers a conversation whose status flapped offline but heartbeat is recent', () => {
    const lingering = agentRows([
      agent({ status: 'offline', heartbeat_age_seconds: 60 }),
    ]);
    expect(lingering).toHaveLength(1);

    const gone = agentRows([
      agent({ status: 'offline', heartbeat_age_seconds: 3600 }),
    ]);
    expect(gone).toHaveLength(0);
  });

  // Mirror placeholder: federation-mirrored workspace bases with nothing in
  // flight (mirror.go's fallback description, no current_task).
  function mirrorAgent(over: Partial<UnifiedAgent>): UnifiedAgent {
    return agent({
      description: MIRROR_PLACEHOLDER_DESCRIPTION,
      current_task: '',
      branch: '',
      ...over,
    });
  }

  it('collapses mirror placeholders into one summary row, leaving real rows alone', () => {
    // Both live shapes: fully scoped hook presences (distinct SESSION_SCOPEs,
    // like the 15-flat-row incident) and a workspace proxy base.
    const rows = agentRows([
      mirrorAgent({ agent_id: 'claude-code-3475321877-1256110271', heartbeat_age_seconds: 40 }),
      mirrorAgent({ agent_id: 'claude-code-2725212065-2856110897', status: 'idle', heartbeat_age_seconds: 10 }),
      mirrorAgent({ agent_id: 'gemini-cli-333', heartbeat_age_seconds: 90 }),
      agent({ agent_id: 'claude-code-444-999', current_task: 'real work' }),
    ]);
    expect(rows).toHaveLength(2);

    const collapsed = findRow(rows, rowKey('agent', 'mirrored-presences'))!;
    expect(collapsed.title).toBe('3 mirrored presences');
    expect(collapsed.state).toBe('mirror');
    expect(collapsed.severity).toBe('idle');
    // Drill-down lands on the freshest placeholder's concrete agent_id.
    expect(collapsed.drillId).toBe('claude-code-2725212065-2856110897');

    const real = rows.find((r) => r.id === 'claude-code-999')!;
    expect(real.subtitle).toBe('real work');
  });

  it('folds codex workspace-anchored twins into the summary as ONE presence', () => {
    // codex-<WS> and codex-<WS>-<SCOPE> are one conversation group
    // (workspace-anchored) — the bucket counts the group once, not twice.
    const rows = agentRows([
      mirrorAgent({ agent_id: 'codex-1713039686' }),
      mirrorAgent({ agent_id: 'codex-1713039686-2303432733' }),
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0].title).toBe('1 mirrored presence');
  });

  it('keeps a mirror-described agent visible when it carries real work', () => {
    const rows = agentRows([
      mirrorAgent({ agent_id: 'claude-code-111', current_task: 'shipping a fix' }),
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0].id).toBe('claude-code-111');
    expect(rows[0].subtitle).toBe('shipping a fix');
    expect(rows[0].state).not.toBe('mirror');
  });

  it('keeps a conversation individual when only SOME members are placeholders', () => {
    // Same conversation scope: one placeholder twin + one real member. The
    // group must render as its normal row, not fold into the mirror bucket.
    const rows = agentRows([
      mirrorAgent({ agent_id: 'claude-code-111-999', status: 'idle' }),
      agent({ agent_id: 'claude-code-222-999', current_task: 'lead work' }),
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0].id).toBe('claude-code-999');
    expect(rows[0].subtitle).toBe('lead work · 2 sessions');
  });

  it('drops dead mirror placeholders instead of counting them', () => {
    const rows = agentRows([
      mirrorAgent({ agent_id: 'claude-code-111', status: 'offline', heartbeat_age_seconds: 3600 }),
    ]);
    expect(rows).toHaveLength(0);
  });
});

describe('sortRowsStable', () => {
  it('orders by severity bucket, then a stable id key — immune to input order', () => {
    const shuffleA = agentRows([
      agent({ agent_id: 'zed', status: 'active' }),
      agent({ agent_id: 'alpha', status: 'active' }),
      agent({ agent_id: 'mid', is_orphan: true }),
    ]);
    const shuffleB = agentRows([
      agent({ agent_id: 'alpha', status: 'active' }),
      agent({ agent_id: 'mid', is_orphan: true }),
      agent({ agent_id: 'zed', status: 'active' }),
    ]);
    const orderedA = sortRowsStable(shuffleA).map((r) => r.id);
    const orderedB = sortRowsStable(shuffleB).map((r) => r.id);
    // Same membership in a different poll order must render identically —
    // this is the anti-jumpy-board guarantee.
    expect(orderedA).toEqual(orderedB);
    expect(orderedA).toEqual(['mid', 'alpha', 'zed']); // warn bucket first, then busy a→z
  });

  it('does not mutate the input array', () => {
    const rows = agentRows([agent({ agent_id: 'b' }), agent({ agent_id: 'a' })]);
    const before = rows.map((r) => r.id);
    sortRowsStable(rows);
    expect(rows.map((r) => r.id)).toEqual(before);
  });
});

describe('sortRowsPinned', () => {
  it('ignores severity entirely so a flapping orphan flag cannot move a row', () => {
    const before = sortRowsPinned(
      agentRows([
        agent({ agent_id: 'beta', status: 'active' }),
        agent({ agent_id: 'alpha', status: 'active' }),
      ]),
    ).map((r) => r.id);
    const afterFlap = sortRowsPinned(
      agentRows([
        agent({ agent_id: 'beta', is_orphan: true }),
        agent({ agent_id: 'alpha', status: 'active' }),
      ]),
    ).map((r) => r.id);
    expect(before).toEqual(['alpha', 'beta']);
    expect(afterFlap).toEqual(before);
  });
});

describe('laneSummary / findRow', () => {
  it('counts attention rows (error+warn) per lane', () => {
    const rows = millsRunRows(
      [run({ ID: 'a', State: 'escalated' }), run({ ID: 'b', State: 'running' })],
      [],
    );
    const summary = laneSummary('mills', rows);
    expect(summary.total).toBe(2);
    expect(summary.attention).toBe(1);
  });

  it('resolves a live selection and folds a vanished one to null', () => {
    const rows = mrRows([mr({})]);
    expect(findRow(rows, rows[0].key)?.id).toBe('services/loom-core!42');
    expect(findRow(rows, 'mr:gone!1')).toBeNull();
    expect(findRow(rows, null)).toBeNull();
  });
});

describe('millEfficiency', () => {
  it('computes first-pass yield and cost per bolt from the KPI window', () => {
    const eff = millEfficiency({
      pipeline_merged_runs: 9,
      pipeline_escalated_runs: 2,
      cost_per_merged_pipeline_usd: 1.49,
    });
    expect(eff).not.toBeNull();
    expect(eff!.yieldPct).toBe(82);
    expect(eff!.costPerBolt).toBe('$1.49');
    expect(eff!.detail).toBe('82% first-pass · $1.49/bolt');
    expect(eff!.tone).toBe('ok');
  });

  it('tones warn below 70% and error below 40%', () => {
    expect(millEfficiency({ pipeline_merged_runs: 1, pipeline_escalated_runs: 1 })!.tone).toBe('warn');
    expect(millEfficiency({ pipeline_merged_runs: 1, pipeline_escalated_runs: 4 })!.tone).toBe('error');
  });

  it('omits the cost segment when the KPI key is absent', () => {
    const eff = millEfficiency({ pipeline_merged_runs: 3, pipeline_escalated_runs: 0 });
    expect(eff!.costPerBolt).toBeNull();
    expect(eff!.detail).toBe('100% first-pass');
  });

  it('returns null for idle windows and missing metrics', () => {
    expect(millEfficiency(null)).toBeNull();
    expect(millEfficiency({})).toBeNull();
    expect(millEfficiency({ pipeline_merged_runs: 0, pipeline_escalated_runs: 0 })).toBeNull();
  });
});
