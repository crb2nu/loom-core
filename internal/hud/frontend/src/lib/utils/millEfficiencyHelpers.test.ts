import { describe, expect, it } from 'vitest';
import type { PipelineRun } from '../stores/mills.svelte.ts';
import { boltCosts, escalatedWaste, firstPassYield, mendingPile } from './millEfficiencyHelpers.ts';

const NOW = new Date('2026-07-15T12:00:00Z');

// firstPassYield buckets by viewer-local midnights, so timestamps asserting bucket
// placement must be derived from NOW instead of hardcoded UTC strings.
function localDayStart(offsetDays: number): number {
  const day = new Date(NOW);
  day.setHours(0, 0, 0, 0);
  day.setDate(day.getDate() + offsetDays);
  return day.getTime();
}

function localIso(offsetDays: number, hours: number): string {
  return new Date(localDayStart(offsetDays) + hours * 60 * 60 * 1_000).toISOString();
}

function run(id: string, state: string, endedAt: string, extra: Partial<PipelineRun> = {}): PipelineRun {
  return {
    ID: id,
    BacklogID: `bk-${id}`,
    Template: 'standard',
    State: state,
    Attempts: 1,
    EndedAt: endedAt,
    ...extra,
  };
}

describe('firstPassYield', () => {
  it('returns seven oldest-to-newest daily yields and today\'s ratio', () => {
    const runs = [
      run('old-done', 'done', localIso(-6, 8)),
      run('old-spark', 'escalated', localIso(-6, 9)),
      run('merged', 'merged', localIso(-1, 8)),
      run('today-done', 'DONE', localIso(0, 8)),
      run('today-spark', 'escalated', localIso(0, 9)),
      run('ignored', 'running', localIso(0, 10)),
    ];

    expect(firstPassYield(runs, NOW)).toEqual({
      daily: [0.5, 0, 0, 0, 0, 1, 0.5],
      today: 0.5,
    });
  });

  it('uses StartedAt as the timestamp fallback and safely handles empty days', () => {
    const fallback = run('fallback', 'done', '', { StartedAt: localIso(0, 7) });
    expect(firstPassYield([fallback], NOW)).toEqual({ daily: [0, 0, 0, 0, 0, 0, 1], today: 1 });
    expect(firstPassYield([], NOW)).toEqual({ daily: [0, 0, 0, 0, 0, 0, 0], today: undefined });
  });

  it('ignores invalid, future, and out-of-window timestamps', () => {
    const invalid = run('invalid', 'done', 'not-a-date');
    const stale = run('stale', 'done', new Date(localDayStart(-6) - 1).toISOString());
    const future = run('future', 'escalated', new Date(localDayStart(1)).toISOString());
    expect(firstPassYield([invalid, stale, future], NOW).today).toBeUndefined();
  });
});

describe('boltCosts', () => {
  it('includes escalated spend in true cost but not raw successful-run mean', () => {
    const runs = [
      run('a', 'done', '2026-07-15T08:00:00Z', { CostUSD: 2 }),
      run('b', 'merged', '2026-07-14T08:00:00Z', { CostUSD: 4 }),
      run('c', 'escalated', '2026-07-13T08:00:00Z', { CostUSD: 6 }),
    ];
    expect(boltCosts(runs, NOW)).toEqual({ trueCostPerBolt: 6, rawCostPerBolt: 3, bolts: 2 });
  });

  it('returns unavailable costs instead of dividing by zero bolts', () => {
    expect(boltCosts([run('spark', 'escalated', '2026-07-15T08:00:00Z', { CostUSD: 9 })], NOW))
      .toEqual({ trueCostPerBolt: undefined, rawCostPerBolt: undefined, bolts: 0 });
    expect(boltCosts([], NOW)).toEqual({ trueCostPerBolt: undefined, rawCostPerBolt: undefined, bolts: 0 });
  });
});

describe('escalatedWaste', () => {
  it('sums only valid seven-day escalated spend', () => {
    const runs = [
      run('a', 'escalated', '2026-07-15T08:00:00Z', { CostUSD: 2.5 }),
      run('b', 'escalated', '2026-07-10T08:00:00Z', { CostUSD: 3.5 }),
      run('done', 'done', '2026-07-15T08:00:00Z', { CostUSD: 20 }),
      run('stale', 'escalated', '2026-07-01T08:00:00Z', { CostUSD: 100 }),
      run('bad-cost', 'escalated', '2026-07-15T08:00:00Z', { CostUSD: Number.NaN }),
    ];
    expect(escalatedWaste(runs, NOW)).toBe(6);
    expect(escalatedWaste([], NOW)).toBe(0);
  });
});

describe('mendingPile', () => {
  it('counts only escalated runs whose retryable tri-state is exactly true', () => {
    const runs = [
      run('yes', 'escalated', '2026-07-15T08:00:00Z', { EscalationRetryable: true }),
      run('no', 'escalated', '2026-07-15T08:00:00Z', { EscalationRetryable: false }),
      run('unknown', 'escalated', '2026-07-15T08:00:00Z', { EscalationRetryable: null }),
      run('done', 'done', '2026-07-15T08:00:00Z', { EscalationRetryable: true }),
      run('stale', 'escalated', '2026-07-01T08:00:00Z', { EscalationRetryable: true }),
    ];
    expect(mendingPile(runs, NOW)).toBe(1);
    expect(mendingPile([], NOW)).toBe(0);
  });
});
