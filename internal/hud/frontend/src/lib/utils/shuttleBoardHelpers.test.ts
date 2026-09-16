import { describe, expect, it } from 'vitest';
import type { PipelineRun, PipelineRunDetail, StageResult } from '../stores/mills.svelte.ts';
import {
  buildLane,
  currentStageRecord,
  lastLogLine,
  overseerRows,
  pressRows,
  sortLanes,
  sparkSplit,
  stageNodes,
  weaverFor,
} from './shuttleBoardHelpers.ts';

const NOW = Date.parse('2026-09-04T00:30:00Z');

function run(over: Partial<PipelineRun>): PipelineRun {
  return {
    ID: 'PIPE-bl-x-20260903-01a069ab-08a6-71a9-953a-c01a6b16190a',
    BacklogID: 'bl-x-20260903',
    Template: 'mills-default-pipeline',
    State: 'testing',
    CurrentStage: 'tests',
    Attempts: 1,
    CostUSD: 1.22,
    StartedAt: '2026-09-03T23:46:51Z',
    ...over,
  };
}

function stage(over: Partial<StageResult>): StageResult {
  return {
    ID: 1,
    PipelineRunID: 'r',
    Stage: 'tests',
    Attempt: 1,
    StartedAt: '2026-09-04T00:00:00Z',
    CostUSD: 0,
    ...over,
  };
}

describe('lastLogLine', () => {
  it('returns the last non-empty line, trimmed and clipped', () => {
    expect(lastLogLine('a\n  b  \n\n')).toBe('b');
    expect(lastLogLine('')).toBeNull();
    expect(lastLogLine(undefined)).toBeNull();
    expect(lastLogLine('x'.repeat(200))?.length).toBe(160);
  });
});

describe('currentStageRecord', () => {
  it('picks the highest attempt for the stage', () => {
    const recs = [
      stage({ ID: 1, Attempt: 1, Outcome: 'error' }),
      stage({ ID: 2, Attempt: 2 }),
      stage({ ID: 3, Stage: 'implement', Attempt: 3 }),
    ];
    expect(currentStageRecord(recs, 'tests')?.ID).toBe(2);
    expect(currentStageRecord(recs, 'merge')).toBeNull();
    expect(currentStageRecord(undefined, 'tests')).toBeNull();
  });
});

describe('stageNodes', () => {
  it('marks done/active/pending around the current stage', () => {
    const nodes = stageNodes(run({ CurrentStage: 'tests' }));
    expect(nodes.map((n) => n.state)).toEqual([
      'done', 'done', 'done', 'active', 'pending', 'pending', 'pending', 'pending', 'pending',
    ]);
    expect(nodes[3].label).toBe('counting picks');
  });
  it('leaves every node pending for an unknown stage and fails the active one when escalated', () => {
    expect(stageNodes(run({ CurrentStage: undefined })).every((n) => n.state === 'pending')).toBe(true);
    expect(stageNodes(run({ State: 'escalated', CurrentStage: 'mr' }))[5].state).toBe('failed');
  });
});

describe('buildLane', () => {
  it('reads who is weaving and what it is doing from the in-flight stage record', () => {
    const detail: PipelineRunDetail = {
      run: run({}),
      stages: [
        stage({ ID: 1, Attempt: 1, Outcome: 'error', LogTail: 'old' }),
        stage({
          ID: 2,
          Attempt: 2,
          Outcome: null,
          Model: 'codex',
          Backend: 'spawn',
          Artifacts: { agent_routing: { agent: 'codex', model: 'gpt-5.6-sol' } },
          LogTail: 'go test ./...\nok  pkg/mills 12.3s\n',
        }),
      ],
      gates: [
        { ID: 1, PipelineRunID: 'r', AfterStage: 'post_implement_gate', GateName: 'scope', Outcome: 'pass', EvaluatedAt: '' },
        { ID: 2, PipelineRunID: 'r', AfterStage: 'post_implement_gate', GateName: 'diff_size', Outcome: 'fail', EvaluatedAt: '' },
      ],
    };
    const lane = buildLane(
      run({}),
      [{ ID: 'bl-x-20260903', Title: 'Fix the thing', State: 'running', Priority: 'P1', TargetProject: 'services/loom-core' }],
      { status: 'loaded', detail },
      { stage: 'tests', since: NOW - 5 * 60_000 },
      NOW,
    );
    expect(lane.title).toBe('Fix the thing');
    expect(lane.priority).toBe('P1');
    expect(lane.project).toBe('services/loom-core');
    expect(lane.flight).toBe('…c01a6b16190a');
    expect(lane.stageLabel).toBe('counting picks');
    expect(lane.stageAgeMs).toBe(5 * 60_000);
    expect(lane.delayed).toBe(false);
    expect(lane.stageAttempt).toBe(2);
    expect(lane.model).toBe('gpt-5.6-sol');
    expect(lane.agent).toBe('codex');
    expect(lane.now).toBe('ok  pkg/mills 12.3s');
    expect(lane.gatesPassed).toBe(1);
    expect(lane.gatesFailed).toBe(1);
    expect(lane.lastFailedGate).toBe('diff_size');
  });

  it('falls back to the last finished pick when the current stage has no record yet', () => {
    // Stage records land at pick end: a run in `research` right after
    // `plan_slice` finished has only the plan_slice record.
    const detail: PipelineRunDetail = {
      run: run({ CurrentStage: 'research' }),
      stages: [stage({ Stage: 'plan_slice', Outcome: 'success', Model: 'codex', LogTail: 'slice planned' })],
      gates: [],
    };
    const lane = buildLane(run({ CurrentStage: 'research' }), [], { status: 'loaded', detail }, undefined, NOW);
    // The log line borrows the last finished pick (labelled); the weaver
    // does not — plan_slice's model says nothing about who runs research.
    expect(lane.model).toBeNull();
    expect(lane.weaverSource).toBeNull();
    expect(lane.stageAttempt).toBeNull();
    expect(lane.now).toBe('slice planned');
    expect(lane.nowStage).toBe('plan_slice');
    expect(lane.stage).toBe('research');
  });

  it('degrades without detail', () => {
    const pending = buildLane(run({}), [], { status: 'pending' }, undefined, NOW);
    expect(pending.now).toBeNull();
    expect(pending.nowStage).toBeNull();
    expect(pending.model).toBeNull();
    expect(pending.title).toBe('bl-x-20260903');
    expect(pending.stageAgeMs).toBeNull();
    expect(pending.detail).toBe('pending');
  });

  it('calls a run delayed past the stage fuse, honoring the longer ci_watch fuse', () => {
    const tests = buildLane(run({}), [], { status: 'pending' }, { stage: 'tests', since: NOW - 13 * 60_000 }, NOW);
    expect(tests.delayed).toBe(true);
    const ci = buildLane(run({ CurrentStage: 'ci_watch' }), [], { status: 'pending' }, { stage: 'ci_watch', since: NOW - 13 * 60_000 }, NOW);
    expect(ci.delayed).toBe(false);
  });
});

describe('weaverFor', () => {
  it('reads a complete record first', () => {
    const rec = stage({ Model: 'codex', Backend: 'spawn', Artifacts: { agent_routing: { agent: 'codex', model: 'gpt-5.6-sol' } } });
    expect(weaverFor(rec, 'implement', null)).toEqual({ model: 'gpt-5.6-sol', agent: 'codex', source: 'record' });
  });
  it('names the operator for picks no model runs', () => {
    for (const s of ['tests', 'mr', 'ci_watch', 'merge', 'cleanup']) {
      expect(weaverFor(stage({ Stage: s, Model: '', Backend: '', SpawnID: '' }), s, null)).toEqual({ model: null, agent: 'operator', source: 'operator' });
    }
  });
  it('reads a flexinfer research pick from its weaver- spawn id', () => {
    const rec = stage({ Stage: 'research', Model: '', Backend: '', SpawnID: 'weaver-moonshotai/kimi-k3' });
    expect(weaverFor(rec, 'research', null)).toEqual({ model: 'moonshotai/kimi-k3', agent: 'flexinfer', source: 'record' });
  });
  it('falls back to provenance routing for an in-flight agent pick', () => {
    const rec = stage({ Stage: 'implement', Model: '', Backend: '', SpawnID: 'spawn-d528105d7ce4', Outcome: null });
    expect(weaverFor(rec, 'implement', { implement: 'gpt-5.6-sol' })).toEqual({ model: 'gpt-5.6-sol', agent: 'spawn', source: 'routed' });
    expect(weaverFor(null, 'implement', { implement: 'gpt-5.6-sol' })).toEqual({ model: 'gpt-5.6-sol', agent: null, source: 'routed' });
  });
  it('reports a bare spawn when no routing is known, and nothing when nothing is', () => {
    const rec = stage({ Stage: 'plan_slice', Model: '', Backend: '', SpawnID: 'spawn-1272ce5e091a' });
    expect(weaverFor(rec, 'plan_slice', {})).toEqual({ model: null, agent: 'spawn', source: 'spawn' });
    expect(weaverFor(null, 'plan_slice', null)).toEqual({ model: null, agent: null, source: null });
  });
  it('answers for the CURRENT pick in buildLane, not the last finished one', () => {
    const detail: PipelineRunDetail = {
      run: run({ CurrentStage: 'implement' }),
      stages: [
        stage({ Stage: 'plan_slice', Outcome: 'success', Model: 'codex', Backend: 'spawn' }),
        stage({ Stage: 'implement', Outcome: null, Model: '', Backend: '', SpawnID: 'spawn-abc' }),
      ],
      gates: [],
      evidence: { verdicts: [], provenance: { stage_models: { implement: 'gpt-5.6-sol' } }, regression: null },
    };
    const lane = buildLane(run({ CurrentStage: 'implement' }), [], { status: 'loaded', detail }, undefined, NOW);
    expect(lane.model).toBe('gpt-5.6-sol');
    expect(lane.weaverSource).toBe('routed');
    // Operator pick with only a finished agent record behind it: never
    // borrow that record's model.
    const testsDetail: PipelineRunDetail = {
      run: run({ CurrentStage: 'tests' }),
      stages: [stage({ Stage: 'implement', Outcome: 'success', Model: 'codex', Backend: 'spawn' })],
      gates: [],
    };
    const tests = buildLane(run({ CurrentStage: 'tests' }), [], { status: 'loaded', detail: testsDetail }, undefined, NOW);
    expect(tests.model).toBeNull();
    expect(tests.agent).toBe('operator');
    expect(tests.weaverSource).toBe('operator');
  });
});

describe('sortLanes', () => {
  it('puts delayed lanes first, then priority, then longest in stage', () => {
    const mk = (id: string, delayed: boolean, priority: string, age: number | null) =>
      ({ ...buildLane(run({ ID: id }), [], { status: 'pending' }, undefined, NOW), delayed, priority, stageAgeMs: age });
    const sorted = sortLanes([mk('a', false, 'P3', 10), mk('b', true, 'P2', 1), mk('c', false, 'P0', 5), mk('d', false, 'P0', 50)]);
    expect(sorted.map((l) => l.runID)).toEqual(['b', 'd', 'c', 'a']);
  });
});

describe('pressRows', () => {
  it('phrases active entries in queue order and appends recent settled', () => {
    const rows = pressRows(
      {
        active: [
          { id: 1, pipeline_run_id: 'r1', backlog_id: 'bl-1', project: 'services/loom-core', mr_iid: 1821, source_branch: '', target_branch: 'main', enqueued_sha: '', current_sha: '', state: 'awaiting_pipeline', attempts: 2, enqueued_at: '2026-09-04T00:20:00Z', updated_at: '' },
          { id: 2, pipeline_run_id: 'r2', backlog_id: 'bl-2', project: 'services/loom-core', mr_iid: 1822, source_branch: '', target_branch: 'main', enqueued_sha: '', current_sha: '', state: 'queued', attempts: 0, enqueued_at: '2026-09-04T00:25:00Z', updated_at: '' },
        ],
        recent_settled: [
          { id: 3, pipeline_run_id: 'r3', backlog_id: 'bl-3', project: 'p', mr_iid: 1800, source_branch: '', target_branch: 'main', enqueued_sha: '', current_sha: '', state: 'evicted', eviction_reason: 'ci_timeout', attempts: 3, enqueued_at: '', updated_at: '', settled_at: '2026-09-04T00:00:00Z' },
        ],
        summary: { depth: 2, lanes: {}, enabled: true },
      },
      NOW,
    );
    expect(rows.map((r) => [r.mrIID, r.phrase, r.settled])).toEqual([
      [1821, 're-proving on CI', false],
      [1822, 'waiting for the press', false],
      [1800, 'evicted · ci_timeout', true],
    ]);
    expect(rows[0].ageMs).toBe(10 * 60_000);
    expect(rows[2].ageMs).toBe(30 * 60_000);
    expect(pressRows(null, NOW)).toEqual([]);
  });
});

describe('sparkSplit', () => {
  it('splits 24h escalations into infra and real, largest class first', () => {
    const split = sparkSplit({ infra: 3, code: 5, transient: 1, config: 0 });
    expect(split).toEqual({
      total: 9,
      infra: 4,
      real: 5,
      classes: [
        { cls: 'code', n: 5, infra: false },
        { cls: 'infra', n: 3, infra: true },
        { cls: 'transient', n: 1, infra: true },
      ],
    });
    expect(sparkSplit({})).toBeNull();
    expect(sparkSplit(undefined)).toBeNull();
  });
});

describe('overseerRows', () => {
  const base = { enabled: true, paused: false, dry_run: false, last_result: { inspected: 4, acted: 1, planned: 0, skipped: 3, errored: 0 } };
  it('classifies ticking, silent, paused, suppressed, error, and disabled', () => {
    const rows = overseerRows(
      {
        enabled: true,
        recent_actions: {},
        agents: [
          { ...base, name: 'groomer', last_tick_at: new Date(NOW - 60_000).toISOString() },
          { ...base, name: 'sentinel', last_tick_at: new Date(NOW - 30 * 60_000).toISOString() },
          { ...base, name: 'foreman', paused: true, last_tick_at: new Date(NOW).toISOString() },
          { ...base, name: 'a', suppression: { reason: 'budget', until: '' }, last_tick_at: new Date(NOW).toISOString() },
          { ...base, name: 'b', last_error: 'boom', last_tick_at: new Date(NOW).toISOString() },
          { ...base, name: 'c', enabled: false },
        ],
      },
      NOW,
    );
    expect(rows.map((r) => [r.name, r.state])).toEqual([
      ['groomer', 'ticking'],
      ['sentinel', 'silent'],
      ['foreman', 'paused'],
      ['a', 'suppressed'],
      ['b', 'error'],
      ['c', 'disabled'],
    ]);
    expect(rows[0].tickAgeMs).toBe(60_000);
    expect(rows[0].inspected).toBe(4);
    expect(rows[3].note).toBe('budget');
    expect(rows[4].note).toBe('boom');
    expect(overseerRows(null, NOW)).toEqual([]);
  });
});

import { overseerReadiness } from './shuttleBoardHelpers.ts';
import type { OverseerAgent, OverseerSoak, OverseerEvent } from '../stores/mills_overseers.svelte.ts';
import type { PromotionReport, PromotionAction } from '../stores/mills_staff.svelte.ts';

describe('overseerReadiness', () => {
  const agent = (name: string, dry_run = true): OverseerAgent => ({ name, dry_run, enabled: true, paused: false,
    last_result: { inspected: 0, acted: 0, planned: 0, skipped: 0, errored: 0 } });
  const soak: OverseerSoak = { mills_overseer_soak_elapsed_days: 7, mills_overseer_soak_dry_run_decisions: 100,
    mills_overseer_soak_would_have_acted: 55, mills_overseer_soak_divergences: 0, promotable: true, fail_closed: false };
  const action = (over: Partial<PromotionAction> = {}): PromotionAction => ({ action: 'pause', dry_run: 7,
    executed: 0, unique_subjects: 1, first: '2026-09-06T01:00:00Z', last: '2026-09-12T23:59:59Z',
    subject_sample: ['backlog_item/a', 'backlog_item/z'], ...over });
  // Reconstructed from the supplied review, not a captured API response.
  // Times/subject IDs and the shepherd's exact action/count were not supplied.
  const founding: PromotionReport = { actor_prefix: 'overseer.', window_start: '2026-09-06T12:00:00Z',
    window_end: '2026-09-13T12:00:00Z', total_actions: 56, total_dry_run: 55, total_executed: 1, zero_evidence: false,
    per_actor: [
      { actor: 'overseer.groomer', per_action: [action({ action: 'dedup_close', dry_run: 4, unique_subjects: 4,
        first: '2026-09-13T01:00:00Z', last: '2026-09-13T02:00:00Z' })] },
      { actor: 'overseer.foreman', per_action: [action({ dry_run: 51 })] },
      { actor: 'overseer.shepherd', per_action: [action({ action: 'synthetic_execution', dry_run: 0, executed: 1 })] },
    ] };
  // Explicitly synthetic closed-window fixture: no claim that the founding review passed.
  const closed: PromotionReport = { ...founding, window_start: '2026-09-06T00:00:00Z', window_end: '2026-09-13T00:00:00Z',
    per_actor: [{ actor: 'overseer.foreman', per_action: [action()] }] };
  it('keeps the founding false positives/anomalies in soaking, sentinel empty, and shepherd live', () => {
    const groomer = overseerReadiness(agent('groomer'), founding, soak);
    expect(groomer.actions[0]).toMatchObject({ dry: 4, subjects: 4 });
    expect(groomer.label).toMatch(/^soaking/);
    const foreman = overseerReadiness(agent('foreman'), founding, soak);
    expect(foreman.actions[0]).toMatchObject({ dry: 51, subjects: 1 });
    expect(foreman.label).toMatch(/^soaking/);
    expect(overseerReadiness(agent('sentinel'), founding, soak).label).toBe('no evidence');
    expect(overseerReadiness(agent('shepherd', false), founding, soak).label).toBe('promoted');
    expect(groomer.actions[0].reasons.join(' ')).toContain('zero false positives');
  });
  it('requires human review even with seven days and a positive aggregate verdict', () => {
    const r = overseerReadiness(agent('foreman'), closed, soak);
    expect(r.label).toBe('review');
    expect(r.actions[0].evidenceDays).toBe(7);
    expect(r.actions[0].reasons.join(' ')).toContain('Human review');
    expect(JSON.stringify(r)).not.toContain('promotable');
  });
  it('does not certify partial UTC windows, short soak, missing telemetry or divergences', () => {
    expect(overseerReadiness(agent('foreman'), { ...closed, window_end: '2026-09-13T00:00:01Z' }, soak).label).toMatch(/^soaking/);
    expect(overseerReadiness(agent('foreman'), closed, { ...soak, mills_overseer_soak_elapsed_days: 6 }).label).toBe('soaking (6/7d)');
    expect(overseerReadiness(agent('foreman'), closed, undefined).label).toBe('soaking (0/7d)');
    expect(overseerReadiness(agent('foreman'), closed, { ...soak, mills_overseer_soak_divergences: 1 }).label).toMatch(/^soaking/);
    expect(overseerReadiness(agent('foreman'), null, soak).label).toBe('no evidence');
  });
  it('isolates action classes and flags execution while currently dry', () => {
    const report = { ...closed, per_actor: [{ actor: 'overseer.foreman', per_action: [action(),
      action({ action: 'other', executed: 1 }), action({ action: 'empty', dry_run: 0 }),
      action({ action: 'new', first: '2026-09-12T01:00:00Z' })] }] };
    const r = overseerReadiness(agent('foreman'), report, soak);
    expect(r.actions.map((a) => a.label)).toEqual(['review', 'soaking (7/7d)', 'no evidence', 'soaking (1/7d)']);
    expect(r.actions[1].reasons.join(' ')).toContain('Execution occurred');
    expect(overseerReadiness(agent('foreman', false), closed, soak).label).toMatch(/^soaking/);
  });
  it('rejects malformed/out-of-window timestamps and excludes the current partial date', () => {
    for (const first of ['invalid', '2026-09-05T23:59:59Z', '2026-09-14T00:00:00Z']) {
      const report = { ...closed, per_actor: [{ actor: 'overseer.foreman', per_action: [action({ first })] }] };
      expect(overseerReadiness(agent('foreman'), report, soak).actions[0].evidenceDays).toBe(0);
    }
    expect(overseerReadiness(agent('groomer'), founding, soak).actions[0].evidenceDays).toBe(0);
  });
  it('only calls a subject newest when a matching latest timestamp corroborates it', () => {
    const event: OverseerEvent = { ID: 1, Actor: 'overseer.foreman', Kind: 'overseer.foreman.pause.dryrun',
      OccurredAt: action().last, SubjectKind: 'backlog_item', SubjectID: 'z', Payload: null };
    expect(overseerReadiness(agent('foreman'), closed, soak).actions[0]).toMatchObject({ sample: 'backlog_item/a', sampleNewest: false });
    expect(overseerReadiness(agent('foreman'), closed, soak, [event]).actions[0]).toMatchObject({ sample: 'backlog_item/z', sampleNewest: true });
    expect(overseerReadiness(agent('foreman'), closed, soak, [{ ...event, Kind: 'other' }]).actions[0].sampleNewest).toBe(false);
    expect(overseerReadiness(agent('foreman'), closed, soak, [{ ...event, OccurredAt: action().first }]).actions[0].sampleNewest).toBe(false);
  });
});

// Render the strip alongside its pure model tests to keep this slice's scope
// bounded. Native details/summary supplies keyboard and pointer expansion.
import { render } from 'svelte/server';
import OverseerStrip from '../components/mills/OverseerStrip.svelte';
import { millsOverseersStore } from '../stores/mills_overseers.svelte.ts';

it('renders expandable cached evidence and an encoded backlog route without fetching', () => {
  const previous = { status: millsOverseersStore.status, report: millsOverseersStore.report, error: millsOverseersStore.reportError };
  const originalFetch = globalThis.fetch;
  globalThis.fetch = () => { throw new Error('Rendering must not fetch'); };
  try {
    millsOverseersStore.status = { enabled: true, recent_actions: {}, agents: [{ name: 'groomer', enabled: true,
      paused: false, dry_run: true, last_result: { inspected: 4, acted: 0, planned: 4, skipped: 0, errored: 0 } }] };
    millsOverseersStore.report = { actor_prefix: 'overseer.', window_start: '2026-09-06T12:00:00Z',
      window_end: '2026-09-13T12:00:00Z', total_actions: 4, total_dry_run: 4, total_executed: 0, zero_evidence: false,
      per_actor: [{ actor: 'overseer.groomer', per_action: [{ action: 'dedup_close', dry_run: 4, executed: 0,
        unique_subjects: 4, first: '2026-09-13T01:00:00Z', last: '2026-09-13T02:00:00Z', subject_sample: ['backlog_item/a b'] }] }] };
    millsOverseersStore.reportError = '503';
    const html = render(OverseerStrip, { props: { rows: overseerRows(millsOverseersStore.status, NOW) } }).body;
    expect(html).toContain('<details');
    expect(html).toContain('<summary');
    expect(html).toContain('href="#mills/warps/a%20b"');
    expect(html).toContain('Cached evidence; report refresh failed');
    expect(html).toContain('dedup_close');
    expect(html).toContain('recency unknown');
    expect(html).toContain('human approval pending per action class');
  } finally {
    globalThis.fetch = originalFetch;
    millsOverseersStore.status = previous.status;
    millsOverseersStore.report = previous.report;
    millsOverseersStore.reportError = previous.error;
  }
});
