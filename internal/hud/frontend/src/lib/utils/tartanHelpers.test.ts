import { describe, expect, it } from 'vitest';
import { archiveDays, archiveTotals, tartanSVG, type TartanColors } from './tartanHelpers.ts';
import type { PipelineRun } from '../stores/mills.svelte.ts';

// Local-noon anchor so day boundaries are unambiguous in any TZ the CI
// runner happens to use.
const NOW = new Date(2026, 6, 8, 12, 0, 0); // Wed 2026-07-08 local

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

/** Local-time RFC3339-ish stamp for a given local Y/M/D/h. */
function localISO(y: number, m: number, d: number, h: number): string {
  return new Date(y, m, d, h).toISOString();
}

const COLORS: TartanColors = {
  bg: '#0b0f14',
  bolt: 'rgb(34, 224, 118)',
  spark: 'rgb(255, 184, 48)',
  fog: 'rgb(212, 238, 244)',
  dim: 'rgba(212, 238, 244, 0.35)',
};

describe('archiveDays', () => {
  it('produces one band per day, oldest first, covering the window', () => {
    const days = archiveDays([], 7, NOW);
    expect(days).toHaveLength(7);
    expect(days[0].date).toBe('2026-07-02');
    expect(days[6].date).toBe('2026-07-08');
    expect(days[6].label).toBe('Wed 7/8');
    expect(days.every((d) => d.runs.length === 0)).toBe(true);
  });

  it('lands runs on their local end-day and maps state → kind', () => {
    const days = archiveDays(
      [
        run({ ID: 'a', State: 'merged', EndedAt: localISO(2026, 6, 7, 9) }),
        run({ ID: 'b', State: 'done', EndedAt: localISO(2026, 6, 7, 15) }),
        run({ ID: 'c', State: 'escalated', EndedAt: localISO(2026, 6, 8, 8) }),
      ],
      7,
      NOW,
    );
    const tue = days.find((d) => d.date === '2026-07-07')!;
    const wed = days.find((d) => d.date === '2026-07-08')!;
    expect(tue.runs.map((r) => r.kind)).toEqual(['bolt', 'bolt']);
    expect(wed.runs.map((r) => r.kind)).toEqual(['spark']);
  });

  it('sorts within a day chronologically even when input is newest-first', () => {
    const days = archiveDays(
      [
        run({ ID: 'late', EndedAt: localISO(2026, 6, 7, 18) }),
        run({ ID: 'early', EndedAt: localISO(2026, 6, 7, 6) }),
      ],
      7,
      NOW,
    );
    const tue = days.find((d) => d.date === '2026-07-07')!;
    expect(tue.runs.map((r) => r.runID)).toEqual(['early', 'late']);
  });

  it('drops non-woven states, missing timestamps, and out-of-window runs', () => {
    const days = archiveDays(
      [
        run({ ID: 'held', State: 'paused', EndedAt: localISO(2026, 6, 7, 9) }),
        run({ ID: 'no-ts', State: 'merged', EndedAt: undefined, StartedAt: undefined }),
        run({ ID: 'ancient', State: 'merged', EndedAt: localISO(2026, 5, 1, 9) }),
        run({ ID: 'ok', State: 'merged', EndedAt: localISO(2026, 6, 8, 9) }),
      ],
      7,
      NOW,
    );
    expect(days.flatMap((d) => d.runs).map((r) => r.runID)).toEqual(['ok']);
  });

  it('falls back to StartedAt when EndedAt is missing', () => {
    const days = archiveDays(
      [run({ ID: 'started-only', EndedAt: undefined, StartedAt: localISO(2026, 6, 6, 10) })],
      7,
      NOW,
    );
    expect(days.find((d) => d.date === '2026-07-06')!.runs).toHaveLength(1);
  });

  it('preserves grade state for the interactive archive rows', () => {
    const days = archiveDays(
      [run({ ID: 'graded', Grade: 'keep', GradeNote: 'more like this', EndedAt: localISO(2026, 6, 8, 9) })],
      7,
      NOW,
    );
    expect(days.flatMap((day) => day.runs)[0]).toMatchObject({
      grade: 'keep',
      gradeNote: 'more like this',
    });
  });
});

describe('archiveTotals', () => {
  it('counts bolts/sparks and sums cost across the week', () => {
    const days = archiveDays(
      [
        run({ ID: 'a', State: 'merged', EndedAt: localISO(2026, 6, 7, 9), CostUSD: 1.25 }),
        run({ ID: 'b', State: 'escalated', EndedAt: localISO(2026, 6, 7, 10), CostUSD: 0.5 }),
        run({ ID: 'c', State: 'merged', EndedAt: localISO(2026, 6, 8, 9) }),
      ],
      7,
      NOW,
    );
    expect(archiveTotals(days)).toEqual({ bolts: 2, sparks: 1, costUSD: 1.75 });
  });
});

describe('tartanSVG', () => {
  const days = archiveDays(
    [
      run({ ID: 'a', State: 'merged', EndedAt: localISO(2026, 6, 7, 9), CostUSD: 2 }),
      run({ ID: 'b', State: 'escalated', EndedAt: localISO(2026, 6, 8, 9) }),
    ],
    7,
    NOW,
  );

  it('is deterministic — same days and options weave byte-identical cloth', () => {
    const first = tartanSVG(days, { colors: COLORS });
    const second = tartanSVG(days, { colors: COLORS });
    expect(first).toBe(second);
  });

  it('weaves bolts and sparks in their tones with per-run hover titles', () => {
    const svg = tartanSVG(days, { colors: COLORS });
    expect(svg).toContain(COLORS.bolt);
    expect(svg).toContain(COLORS.spark);
    expect(svg).toContain('<title>bolt psl-x · merged on green</title>');
    expect(svg).toContain('<title>spark psl-x · escalated</title>');
  });

  it('renders empty days as "no cloth" bands and totals in the caption', () => {
    const svg = tartanSVG(days, { colors: COLORS });
    expect(svg).toContain('no cloth');
    expect(svg).toContain('1 bolt · 1 spark · $2.00');
    expect(svg).toContain('Thu 7/2 – Wed 7/8');
  });

  it('changes when a run id changes — the pattern is the run, not decoration', () => {
    const other = archiveDays(
      [run({ ID: 'zzz', State: 'merged', EndedAt: localISO(2026, 6, 7, 9), CostUSD: 2 })],
      7,
      NOW,
    );
    const a = tartanSVG(days, { colors: COLORS });
    const b = tartanSVG(other, { colors: COLORS });
    expect(a).not.toBe(b);
  });

  it('escapes XML-hostile ids instead of breaking the document', () => {
    const hostile = archiveDays(
      [run({ ID: 'r<&>"', BacklogID: 'psl-<evil>', EndedAt: localISO(2026, 6, 8, 9) })],
      7,
      NOW,
    );
    const svg = tartanSVG(hostile, { colors: COLORS });
    expect(svg).toContain('psl-&lt;evil&gt;');
    expect(svg).not.toContain('psl-<evil>');
  });
});
