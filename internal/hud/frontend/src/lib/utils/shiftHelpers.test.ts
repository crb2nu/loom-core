import { describe, expect, it } from 'vitest';
import {
  shiftMarkdown,
  shiftNarrative,
  shiftStats,
  shiftWindow,
  type ShiftRun,
} from './shiftHelpers.ts';
import type { BacklogItem, PipelineRun } from '../stores/mills.svelte.ts';
import type { PatternInfo } from '../stores/patterns.svelte.ts';

// Fixed anchor so window edges are unambiguous in any TZ.
const NOW = new Date(2026, 6, 8, 12, 0, 0); // Wed 2026-07-08 12:00 local

function run(over: Partial<PipelineRun>): PipelineRun {
  return {
    ID: 'run-x',
    BacklogID: 'psl-x',
    Template: 'implement',
    State: 'merged',
    Attempts: 1,
    ...over,
  } as PipelineRun;
}

/** ISO stamp `hoursAgo` hours before NOW. */
function ago(hoursAgo: number): string {
  return new Date(NOW.getTime() - hoursAgo * 3_600_000).toISOString();
}

function pattern(over: Partial<PatternInfo>): PatternInfo {
  return {
    id: 'pat-1',
    slug: 'go-rest-service',
    name: 'go-rest-service',
    makes: 'a Go REST service',
    version: '1',
    status: 'approved',
    ...over,
  } as PatternInfo;
}

function sr(over: Partial<ShiftRun>): ShiftRun {
  return {
    kind: 'bolt',
    runID: 'run-x',
    backlogID: 'psl-x',
    template: 'implement',
    attempts: 1,
    endedAt: NOW.getTime() - 3_600_000,
    ...over,
  };
}

describe('shiftWindow', () => {
  it('keeps terminal runs inside the window, oldest first', () => {
    const runs = shiftWindow(
      [
        run({ ID: 'a', State: 'merged', EndedAt: ago(2) }),
        run({ ID: 'b', State: 'escalated', EndedAt: ago(23) }),
        run({ ID: 'c', State: 'done', EndedAt: ago(10) }),
      ],
      NOW,
    );
    expect(runs.map((r) => r.runID)).toEqual(['b', 'c', 'a']);
    expect(runs.map((r) => r.kind)).toEqual(['spark', 'bolt', 'bolt']);
  });

  it('drops runs outside the window, non-woven states, and missing stamps', () => {
    const runs = shiftWindow(
      [
        run({ ID: 'old', EndedAt: ago(25) }),
        run({ ID: 'future', EndedAt: ago(-1) }),
        run({ ID: 'paused', State: 'paused', EndedAt: ago(1) }),
        run({ ID: 'running', State: 'running', EndedAt: ago(1) }),
        run({ ID: 'nostamp', EndedAt: undefined, StartedAt: undefined }),
        run({ ID: 'bad', EndedAt: 'not-a-date' }),
      ],
      NOW,
    );
    expect(runs).toEqual([]);
  });

  it('falls back to StartedAt when EndedAt is missing', () => {
    const runs = shiftWindow([run({ ID: 'a', EndedAt: undefined, StartedAt: ago(3) })], NOW);
    expect(runs).toHaveLength(1);
    expect(runs[0].endedAt).toBe(Date.parse(ago(3)));
  });

  it('honors a custom window length', () => {
    const runs = shiftWindow(
      [run({ ID: 'in', EndedAt: ago(5) }), run({ ID: 'out', EndedAt: ago(9) })],
      NOW,
      8,
    );
    expect(runs.map((r) => r.runID)).toEqual(['in']);
  });

  it('preserves grade state for interactive departure rows', () => {
    const runs = shiftWindow(
      [run({ ID: 'graded', Grade: 'regret', GradeNote: 'wrong direction', EndedAt: ago(1) })],
      NOW,
    );
    expect(runs[0]).toMatchObject({ grade: 'regret', gradeNote: 'wrong direction' });
  });
});

describe('shiftStats', () => {
  it('splits bolts/sparks, sums cost, and ranks retries worst-first', () => {
    const stats = shiftStats(
      [
        sr({ runID: 'a', costUSD: 1.5 }),
        sr({ runID: 'b', kind: 'spark', attempts: 3, costUSD: 2 }),
        sr({ runID: 'c', attempts: 2 }),
      ],
      [],
      [],
    );
    expect(stats.bolts.map((r) => r.runID)).toEqual(['a', 'c']);
    expect(stats.sparks.map((r) => r.runID)).toEqual(['b']);
    expect(stats.costUSD).toBeCloseTo(3.5);
    expect(stats.retried.map((r) => r.runID)).toEqual(['b', 'c']);
  });

  it('finds the busiest local hour', () => {
    const h14 = new Date(2026, 6, 8, 14, 10).getTime();
    const h9 = new Date(2026, 6, 8, 9, 5).getTime();
    const stats = shiftStats(
      [sr({ runID: 'a', endedAt: h14 }), sr({ runID: 'b', endedAt: h14 + 60_000 }), sr({ runID: 'c', endedAt: h9 })],
      [],
      [],
    );
    expect(stats.busiestHour).toEqual({ hour: 14, count: 2 });
  });

  it('attributes pattern stamps via run → backlog → PlanID → slug', () => {
    const backlog: BacklogItem[] = [
      { ID: 'psl-1', PlanID: 'plan-stamp-go-rest-service-x1' } as BacklogItem,
      { ID: 'psl-2', PlanID: 'plan-stamp-go-rest-service-x2' } as BacklogItem,
      { ID: 'psl-3', PlanID: 'plan-organic' } as BacklogItem,
    ];
    const stats = shiftStats(
      [
        sr({ runID: 'a', backlogID: 'psl-1' }),
        sr({ runID: 'b', backlogID: 'psl-2', kind: 'spark' }),
        sr({ runID: 'c', backlogID: 'psl-3' }),
      ],
      [pattern({})],
      backlog,
    );
    expect(stats.stamps).toEqual([
      { slug: 'go-rest-service', name: 'go-rest-service', bolts: 1, sparks: 1 },
    ]);
  });

  it('ignores unapproved patterns', () => {
    const backlog = [{ ID: 'psl-1', PlanID: 'plan-stamp-go-rest-service-x1' } as BacklogItem];
    const stats = shiftStats(
      [sr({ backlogID: 'psl-1' })],
      [pattern({ status: 'draft' })],
      backlog,
    );
    expect(stats.stamps).toEqual([]);
  });
});

describe('shiftNarrative', () => {
  it('says a quiet shift plainly', () => {
    const stats = shiftStats([], [], []);
    expect(shiftNarrative(stats)).toEqual([
      'The loom sat quiet — no cloth came off the beam in the last 24 hours.',
    ]);
  });

  it('leads with the bolt/spark headline, singular and plural correct', () => {
    const one = shiftStats([sr({})], [], []);
    expect(shiftNarrative(one)[0]).toBe('The floor wove 1 bolt and struck no sparks over the last 24 hours.');

    const many = shiftStats(
      [sr({ runID: 'a' }), sr({ runID: 'b' }), sr({ runID: 'c', kind: 'spark' })],
      [],
      [],
    );
    expect(shiftNarrative(many)[0]).toBe('The floor wove 2 bolts and struck 1 spark over the last 24 hours.');
  });

  it('narrates pattern stamps with once/twice phrasing and outcomes', () => {
    const backlog: BacklogItem[] = [
      { ID: 'psl-1', PlanID: 'plan-stamp-go-rest-service-a' } as BacklogItem,
      { ID: 'psl-2', PlanID: 'plan-stamp-go-rest-service-b' } as BacklogItem,
    ];
    const stats = shiftStats(
      [sr({ runID: 'a', backlogID: 'psl-1' }), sr({ runID: 'b', backlogID: 'psl-2' })],
      [pattern({})],
      backlog,
    );
    expect(shiftNarrative(stats)).toContain(
      'Pattern go-rest-service stamped twice — all merged on green.',
    );
  });

  it('reports retries with the worst run named', () => {
    const stats = shiftStats(
      [sr({ runID: 'a', backlogID: 'psl-a', attempts: 4 }), sr({ runID: 'b', attempts: 2 })],
      [],
      [],
    );
    expect(shiftNarrative(stats)).toContain(
      '2 runs needed extra passes (worst: psl-a at 4 attempts).',
    );
  });

  it('narrates the busiest hour only when it has more than one departure', () => {
    const h14 = new Date(2026, 6, 8, 14, 10).getTime();
    const spread = shiftNarrative(
      shiftStats([sr({ runID: 'a', endedAt: h14 }), sr({ runID: 'b', endedAt: h14 - 4 * 3_600_000 })], [], []),
    );
    expect(spread.some((l) => l.startsWith('Busiest hour'))).toBe(false);

    const peaked = shiftNarrative(
      shiftStats([sr({ runID: 'a', endedAt: h14 }), sr({ runID: 'b', endedAt: h14 + 60_000 })], [], []),
    );
    expect(peaked).toContain('Busiest hour 14:00–15:00 — 2 departures.');
  });

  it('includes cost only when spend is nonzero', () => {
    const free = shiftNarrative(shiftStats([sr({})], [], []));
    expect(free.some((l) => l.includes('$'))).toBe(false);
    const paid = shiftNarrative(shiftStats([sr({ costUSD: 1.25 })], [], []));
    expect(paid).toContain('The shift burned $1.25 of pipeline fuel.');
  });

  it('is deterministic — same runs, same words', () => {
    const runs = [sr({ runID: 'a' }), sr({ runID: 'b', kind: 'spark', attempts: 2 })];
    const a = shiftNarrative(shiftStats(runs, [], []));
    const b = shiftNarrative(shiftStats(runs, [], []));
    expect(a).toEqual(b);
  });
});

describe('shiftMarkdown', () => {
  const GEN = new Date(Date.UTC(2026, 6, 8, 19, 0));

  it('renders headline, sparks with failing gates, and the departures table', () => {
    const runs = [
      sr({ runID: 'a', backlogID: 'psl-a', endedAt: Date.UTC(2026, 6, 8, 9, 30) }),
      sr({ runID: 'b', backlogID: 'psl-b', kind: 'spark', attempts: 2, endedAt: Date.UTC(2026, 6, 8, 11, 0) }),
    ];
    const stats = shiftStats(runs, [], []);
    const md = shiftMarkdown(stats, shiftNarrative(stats), GEN, [
      { runID: 'b', failedGates: ['judge_gate', 'tests'] },
    ]);
    expect(md).toContain('# Mills shift report — 2026-07-08 19:00 UTC');
    expect(md).toContain('- `psl-b` (implement, 2 attempts) — failed judge_gate, tests');
    expect(md).toContain('| 09:30 | 🟢 bolt | psl-a | implement | 1 |');
    expect(md).toContain('| 11:00 | 🟡 spark | psl-b | implement | 2 |');
  });

  it('omits the sparks and departures sections on a quiet shift', () => {
    const stats = shiftStats([], [], []);
    const md = shiftMarkdown(stats, shiftNarrative(stats), GEN);
    expect(md).toContain('The loom sat quiet');
    expect(md).not.toContain('## Sparks');
    expect(md).not.toContain('## Departures');
  });
});
