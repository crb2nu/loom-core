import { describe, expect, it } from 'vitest';
import fixture from './telemetryHelpers.fixture.json';
import {
  aggregateGatePassRate,
  escalationFunnel,
  escalationRate,
  failurePareto,
  fmtDurationSeconds,
  fmtMinutes,
  fmtPct,
  fmtUSD,
  gateHealth,
  HIGH_ERROR_RATE,
  isTelemetryWindow,
  modelEconomics,
  stageWaterfall,
  windowLabel,
  windowSeconds,
  type ModelEconomicsEntry,
  type StageTelemetryReport,
} from './telemetryHelpers.ts';

// The fixture is a captured real response (production aggregates from
// 2026-07-16) and exists ONLY for this test — no component imports it, so it
// never reaches the bundle or stands in for live data. Typing it as the report
// shape here asserts the interface still matches the bytes the API produces.
const report = fixture as StageTelemetryReport;

describe('window param mapping', () => {
  it('maps each window to its second count', () => {
    expect(windowSeconds('1d')).toBe(86_400);
    expect(windowSeconds('7d')).toBe(604_800);
    expect(windowSeconds('30d')).toBe(2_592_000);
  });

  it('matches the fixture window_seconds for 7d', () => {
    expect(windowSeconds('7d')).toBe(report.window_seconds);
  });

  it('labels windows for the selector', () => {
    expect(windowLabel('1d')).toBe('24h');
    expect(windowLabel('7d')).toBe('7d');
    expect(windowLabel('30d')).toBe('30d');
  });

  it('guards accepted window params', () => {
    expect(isTelemetryWindow('7d')).toBe(true);
    expect(isTelemetryWindow('1d')).toBe(true);
    expect(isTelemetryWindow('90d')).toBe(false);
    expect(isTelemetryWindow(7)).toBe(false);
    expect(isTelemetryWindow(null)).toBe(false);
  });
});

describe('formatting', () => {
  it('formats durations by magnitude', () => {
    expect(fmtDurationSeconds(0)).toBe('0s');
    expect(fmtDurationSeconds(0.4)).toBe('<1s');
    expect(fmtDurationSeconds(20)).toBe('20s');
    expect(fmtDurationSeconds(731)).toBe('12.2m');
    expect(fmtDurationSeconds(7200)).toBe('2.0h');
    expect(fmtDurationSeconds(undefined)).toBe('—');
    expect(fmtDurationSeconds(NaN)).toBe('—');
  });

  it('formats retry-burn minutes whole', () => {
    expect(fmtMinutes(29_313)).toBe('489m');
    expect(fmtMinutes(0)).toBe('0m');
    expect(fmtMinutes(null)).toBe('—');
  });

  it('formats USD with sensible precision', () => {
    expect(fmtUSD(0)).toBe('$0');
    expect(fmtUSD(0.004)).toBe('<$0.01');
    expect(fmtUSD(5.35)).toBe('$5.35');
    expect(fmtUSD(17.59)).toBe('$17.6');
    expect(fmtUSD(117)).toBe('$117');
    expect(fmtUSD(undefined)).toBe('—');
  });

  it('formats percentages', () => {
    expect(fmtPct(0.61)).toBe('61%');
    expect(fmtPct(0.429, 1)).toBe('42.9%');
    expect(fmtPct(undefined)).toBe('—');
  });
});

describe('stageWaterfall', () => {
  const bars = stageWaterfall(report.stages);

  it('sorts by p50 descending', () => {
    const p50s = bars.map((b) => b.p50_seconds);
    const sorted = [...p50s].sort((a, b) => b - a);
    expect(p50s).toEqual(sorted);
    // implement (731s) is the slowest stage in the fixture.
    expect(bars[0].stage).toBe('implement');
  });

  it('scales every bar within [0,1] against the widest p90', () => {
    for (const b of bars) {
      expect(b.p50Frac).toBeGreaterThanOrEqual(0);
      expect(b.p50Frac).toBeLessThanOrEqual(1);
      expect(b.p90Frac).toBeGreaterThanOrEqual(0);
      expect(b.p90Frac).toBeLessThanOrEqual(1);
      // p90 >= p50 in the data, so its overlay is never shorter.
      expect(b.p90Frac).toBeGreaterThanOrEqual(b.p50Frac);
    }
    // The widest p90 (implement, 1385s) fills the axis.
    const widest = bars.find((b) => b.stage === 'implement');
    expect(widest?.p90Frac).toBe(1);
  });

  it('flags stages over the 25% error high-water mark', () => {
    for (const b of bars) {
      expect(b.highError).toBe(b.error_rate > HIGH_ERROR_RATE);
    }
    // research (0.429) is red; tests (0.129) is not.
    expect(bars.find((b) => b.stage === 'research')?.highError).toBe(true);
    expect(bars.find((b) => b.stage === 'tests')?.highError).toBe(false);
  });

  it('does not mutate the input array order', () => {
    const before = report.stages.map((s) => s.stage);
    stageWaterfall(report.stages);
    expect(report.stages.map((s) => s.stage)).toEqual(before);
  });
});

describe('gateHealth', () => {
  const segs = gateHealth(report.gates);

  it('treats unparseable as a subset of fails, never double-counted', () => {
    const review = segs.find((s) => s.gate === 'pr_self_review');
    // fixture: evaluations 32, passes 24, fails 8, unparseable 3.
    expect(review).toBeDefined();
    // genuine fails = 8 - 3 = 5.
    expect(review!.fails).toBe(5);
    expect(review!.unparseable).toBe(3);
    // pass + skip + genuineFail + unparseable == evaluations.
    const summed = review!.passes + review!.skips + review!.fails + review!.unparseable;
    expect(summed).toBe(review!.evaluations);
  });

  it('produces fractions that stack to <= 1', () => {
    for (const s of segs) {
      const total = s.passFrac + s.failFrac + s.skipFrac + s.unparseableFrac;
      expect(total).toBeLessThanOrEqual(1 + 1e-9);
      expect(s.passFrac).toBeGreaterThanOrEqual(0);
    }
  });

  it('surfaces the unparseable segment distinctly for spec_conformance', () => {
    const spec = segs.find((s) => s.gate === 'spec_conformance');
    // fixture: fails 4, unparseable 4 → all fails are unparseable.
    expect(spec!.fails).toBe(0);
    expect(spec!.unparseable).toBe(4);
    expect(spec!.unparseableFrac).toBeGreaterThan(0);
  });
});

describe('escalationFunnel', () => {
  const bars = escalationFunnel(report.escalation_funnel);

  it('sorts by count descending', () => {
    const counts = bars.map((b) => b.count);
    expect(counts).toEqual([...counts].sort((a, b) => b - a));
    // ci_watch:error (5) is the largest escalation sink in the fixture.
    expect(bars[0].last_stage).toBe('ci_watch');
    expect(bars[0].outcome).toBe('error');
  });

  it('scales the top entry to a full bar', () => {
    expect(bars[0].frac).toBe(1);
    for (const b of bars) {
      expect(b.frac).toBeGreaterThanOrEqual(0);
      expect(b.frac).toBeLessThanOrEqual(1);
    }
  });
});

describe('failurePareto', () => {
  const rows = failurePareto(report.failure_classes);

  it('sorts by count descending with the top class first', () => {
    const counts = rows.map((r) => r.count);
    expect(counts).toEqual([...counts].sort((a, b) => b - a));
    // research/model_unavailable (24) dominates the fixture.
    expect(rows[0].stage).toBe('research');
    expect(rows[0].class).toBe('model_unavailable');
    expect(rows[0].frac).toBe(1);
  });

  it('shares sum to 1 and cumulative reaches 1', () => {
    const shareSum = rows.reduce((s, r) => s + r.share, 0);
    expect(shareSum).toBeCloseTo(1, 6);
    expect(rows[rows.length - 1].cumulative).toBeCloseTo(1, 6);
    // Cumulative is monotonically non-decreasing.
    for (let i = 1; i < rows.length; i++) {
      expect(rows[i].cumulative).toBeGreaterThanOrEqual(rows[i - 1].cumulative);
    }
  });

  it('handles an empty failure list', () => {
    expect(failurePareto([])).toEqual([]);
  });
});

describe('aggregateGatePassRate', () => {
  it('is total passes over total evaluations across gates', () => {
    const totalPasses = report.gates.reduce((s, g) => s + g.passes, 0);
    const totalEvals = report.gates.reduce((s, g) => s + g.evaluations, 0);
    expect(aggregateGatePassRate(report.gates)).toBeCloseTo(totalPasses / totalEvals, 6);
    // Sanity: the fixture's gates mostly pass, so the rate is high.
    expect(aggregateGatePassRate(report.gates)).toBeGreaterThan(0.9);
  });

  it('guards an empty gate list', () => {
    expect(aggregateGatePassRate([])).toBe(0);
  });
});

describe('escalationRate', () => {
  it('computes escalated / total from the runs block', () => {
    // fixture: 22 escalated / 36 total ≈ 0.611.
    expect(escalationRate(report.runs)).toBeCloseTo(22 / 36, 6);
  });

  it('guards a zero-run window', () => {
    expect(
      escalationRate({
        total: 0,
        done: 0,
        escalated: 0,
        retry_burn_cost_usd: 0,
        retry_burn_seconds: 0,
      }),
    ).toBe(0);
  });
});

describe('modelEconomics', () => {
  it('sorts tiers by cost descending and scales the cost bar against the top tier', () => {
    const rows = modelEconomics(report.model_economics);
    expect(rows.length).toBe(report.model_economics.length);
    // Fixture's costliest tier is the spawn agent; cheapest attributed is qwen.
    expect(rows[0].model).toBe('claude-opus-4-8');
    for (let i = 1; i < rows.length; i++) {
      expect(rows[i - 1].cost_usd).toBeGreaterThanOrEqual(rows[i].cost_usd);
    }
    // Top tier fills the bar; a zero-cost tier has an empty bar.
    expect(rows[0].costFrac).toBe(1);
    const zero = rows.find((r) => r.cost_usd === 0);
    expect(zero?.costFrac).toBe(0);
  });

  it('flags a tier whose error rate exceeds the high-water mark', () => {
    const entries: ModelEconomicsEntry[] = [
      { model: 'flaky', backend: 'flexinfer', calls: 10, cost_usd: 1, errors: 6, error_rate: 0.6, avg_seconds: 5 },
      { model: 'steady', backend: 'spawn', calls: 10, cost_usd: 2, errors: 1, error_rate: 0.1, avg_seconds: 5 },
    ];
    const rows = modelEconomics(entries);
    // Sorted by cost: steady (2) then flaky (1).
    expect(rows[0].model).toBe('steady');
    expect(rows[0].highError).toBe(false);
    expect(rows[1].highError).toBe(true);
    expect(HIGH_ERROR_RATE).toBeGreaterThan(0);
  });

  it('does not mutate the input array and tolerates an empty window', () => {
    const input: ModelEconomicsEntry[] = [
      { model: 'a', backend: 'x', calls: 1, cost_usd: 1, errors: 0, error_rate: 0, avg_seconds: 1 },
      { model: 'b', backend: 'y', calls: 1, cost_usd: 5, errors: 0, error_rate: 0, avg_seconds: 1 },
    ];
    const snapshot = JSON.stringify(input);
    modelEconomics(input);
    expect(JSON.stringify(input)).toBe(snapshot);
    expect(modelEconomics([])).toEqual([]);
  });
});
