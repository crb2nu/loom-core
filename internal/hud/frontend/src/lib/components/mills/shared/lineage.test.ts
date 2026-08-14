import { describe, expect, it } from 'vitest';
import {
  PIPELINE_STAGES,
  WARP_PRIORITIES,
  isBoltState,
  isSparkState,
  lineageFor,
  priorityTone,
  spineSegments,
  stageNodeLabel,
  type LineageSegment,
} from './lineage.ts';
import type { BacklogItem, PipelineRun } from '../../../stores/mills.svelte.ts';

function item(over: Partial<BacklogItem>): BacklogItem {
  return {
    ID: 'b-1',
    Title: 'Fix auth timeout',
    State: 'queued',
    Priority: 'P1',
    ...over,
  } as BacklogItem;
}

function run(over: Partial<PipelineRun>): PipelineRun {
  return {
    ID: 'r-1',
    BacklogID: 'b-1',
    Template: 'implement',
    State: 'running',
    Attempts: 1,
    ...over,
  } as PipelineRun;
}

function kinds(segs: LineageSegment[]): string[] {
  return segs.map((s) => s.kind);
}

describe('priorityTone', () => {
  it('maps the warm→cool ramp and defaults unknown to muted', () => {
    expect(priorityTone('P0')).toBe('error');
    expect(priorityTone('P1')).toBe('accent');
    expect(priorityTone('P2')).toBe('warning');
    expect(priorityTone('P3')).toBe('info');
    expect(priorityTone('p0')).toBe('error'); // case-insensitive
    expect(priorityTone('')).toBe('muted');
    expect(priorityTone(undefined)).toBe('muted');
    expect(priorityTone('P9')).toBe('muted');
  });
});

describe('isBoltState / isSparkState', () => {
  it('classifies terminal states', () => {
    expect(isBoltState('done')).toBe(true);
    expect(isBoltState('merged')).toBe(true);
    expect(isBoltState('MERGED')).toBe(true);
    expect(isBoltState('escalated')).toBe(false);
    expect(isBoltState(undefined)).toBe(false);

    expect(isSparkState('escalated')).toBe(true);
    expect(isSparkState('paused')).toBe(true);
    expect(isSparkState('done')).toBe(false);
    expect(isSparkState(undefined)).toBe(false);
  });
});

describe('lineageFor', () => {
  it('threads warp → all stages → active current for an in-flight run', () => {
    const segs = lineageFor(run({ CurrentStage: 'implement' }), [item({})]);
    // warp + 9 stages, no terminal node while in flight.
    expect(segs).toHaveLength(1 + PIPELINE_STAGES.length);
    expect(segs[0]).toMatchObject({ kind: 'warp', tone: 'accent' });
    expect((segs[0] as { label: string }).label).toContain('P1');

    const stages = segs.filter((s): s is Extract<LineageSegment, { kind: 'stage' }> => s.kind === 'stage');
    // plan_slice, research done; implement active; rest pending.
    expect(stages[0].state).toBe('done'); // plan_slice
    expect(stages[1].state).toBe('done'); // research
    expect(stages[2].state).toBe('active'); // implement (current)
    expect(stages[3].state).toBe('pending'); // tests
    expect(kinds(segs)).not.toContain('bolt');
    expect(kinds(segs)).not.toContain('spark');
  });

  it('joins backlog by BacklogID for priority + plan label', () => {
    const segs = lineageFor(
      run({ BacklogID: 'b-7' }),
      [item({ ID: 'b-7', Priority: 'P0', PlanID: 'plan-stamp-go-rest-auth' })],
    );
    expect(segs[0]).toMatchObject({ kind: 'warp', tone: 'error' });
    expect((segs[0] as { label: string }).label).toBe('P0 · plan-stamp-go-rest-auth');
    expect((segs[0] as { href?: string }).href).toBe('#mills/warps/b-7');
  });

  it('degrades to the raw backlog id when no item is joined', () => {
    const segs = lineageFor(run({ BacklogID: 'b-x' }), []);
    expect((segs[0] as { label: string }).label).toBe('b-x');
    expect((segs[0] as { kind: string; tone: string }).tone).toBe('muted');
  });

  it('marks every stage done and appends a bolt for a merged run', () => {
    const segs = lineageFor(run({ State: 'merged', MRIID: 482, CurrentStage: 'merge' }), [item({})]);
    const stages = segs.filter((s) => s.kind === 'stage');
    expect(stages.every((s) => (s as { state: string }).state === 'done')).toBe(true);
    const terminal = segs[segs.length - 1];
    expect(terminal).toMatchObject({ kind: 'bolt', mriid: 482 });
    expect((terminal as { href?: string }).href).toBe('#mills/bolts/r-1');
  });

  it('appends a spark with class + reasons and marks the current stage failed', () => {
    const segs = lineageFor(
      run({ State: 'escalated', CurrentStage: 'ci_watch', EscalationClass: 'infra' }),
      [item({})],
      ['pipeline 500'],
    );
    const stages = segs.filter((s): s is Extract<LineageSegment, { kind: 'stage' }> => s.kind === 'stage');
    const ciWatch = stages[PIPELINE_STAGES.indexOf('ci_watch')];
    expect(ciWatch.state).toBe('failed');
    const terminal = segs[segs.length - 1];
    expect(terminal).toMatchObject({ kind: 'spark', class: 'infra', reasons: ['pipeline 500'] });
    expect((terminal as { href?: string }).href).toBe('#mills/sparks/r-1');
  });

  it('falls back to unclassified spark class and omits empty reasons', () => {
    const segs = lineageFor(run({ State: 'escalated', CurrentStage: 'tests' }), [item({})], []);
    const terminal = segs[segs.length - 1] as Extract<LineageSegment, { kind: 'spark' }>;
    expect(terminal.class).toBe('unclassified');
    expect(terminal.reasons).toBeUndefined();
  });

  it('renders paused as held without marking the current stage failed', () => {
    const segs = lineageFor(
      run({ State: 'paused', CurrentStage: 'implement', EscalationClass: 'infra' }),
      [item({})],
      ['ignored for held runs'],
    );
    const stages = segs.filter((s): s is Extract<LineageSegment, { kind: 'stage' }> => s.kind === 'stage');
    const implement = stages[PIPELINE_STAGES.indexOf('implement')];
    expect(implement.state).toBe('active');
    const terminal = segs[segs.length - 1] as Extract<LineageSegment, { kind: 'spark' }>;
    expect(terminal).toMatchObject({ kind: 'spark', class: 'held' });
    expect(terminal.reasons).toBeUndefined();
  });

  it('leaves all stages pending when the current stage is unknown', () => {
    const segs = lineageFor(run({ CurrentStage: 'nonsense' }), [item({})]);
    const stages = segs.filter((s) => s.kind === 'stage');
    expect(stages.every((s) => (s as { state: string }).state === 'pending')).toBe(true);
  });

  it('tolerates a null backlog array without throwing', () => {
    expect(() => lineageFor(run({}), undefined as unknown as BacklogItem[])).not.toThrow();
  });
});

describe('spineSegments', () => {
  it('emits one warp node per priority bucket plus shuttle/bolt/spark tallies', () => {
    const segs = spineSegments({
      backlogByPriority: {
        P0: [item({}), item({}), item({})],
        P1: [item({}), item({})],
        P2: [],
        // P3 absent → treated as empty
        other: [item({})],
      },
      activeShuttles: 4,
      bolts: 12,
      sparks: 2,
    });

    const warps = segs.filter((s): s is Extract<LineageSegment, { kind: 'warp' }> => s.kind === 'warp');
    expect(warps).toHaveLength(WARP_PRIORITIES.length);
    expect(warps.map((w) => w.count)).toEqual([3, 2, 0, 0]);
    expect(warps.every((w) => w.href === '#mills/warps')).toBe(true);

    const shuttle = segs.find((s) => s.kind === 'shuttle') as Extract<LineageSegment, { kind: 'shuttle' }>;
    expect(shuttle).toMatchObject({ count: 4, active: true, href: '#mills/shuttles' });

    const bolt = segs.find((s) => s.kind === 'bolt') as Extract<LineageSegment, { kind: 'bolt' }>;
    expect(bolt).toMatchObject({ count: 12, href: '#mills/bolts' });

    const spark = segs.find((s) => s.kind === 'spark') as Extract<LineageSegment, { kind: 'spark' }>;
    expect(spark).toMatchObject({ count: 2, href: '#mills/sparks' });
  });

  it('is []-safe on empty/missing inputs and reports an idle shuttle', () => {
    const segs = spineSegments({
      backlogByPriority: {},
      activeShuttles: 0,
      bolts: 0,
      sparks: 0,
    });
    const warps = segs.filter((s) => s.kind === 'warp');
    expect(warps.every((w) => (w as { count: number }).count === 0)).toBe(true);
    const shuttle = segs.find((s) => s.kind === 'shuttle') as Extract<LineageSegment, { kind: 'shuttle' }>;
    expect(shuttle.active).toBe(false);
  });
});

describe('stageNodeLabel', () => {
  it('speaks the loom vocabulary with the node state', () => {
    expect(stageNodeLabel('implement', 'active')).toBe('laying weft — active');
    expect(stageNodeLabel('merge', 'done')).toBe('winding the take-up roll — done');
  });
});
