import { describe, it, expect } from 'vitest';
import {
  diffStagePicks,
  diffTerminalRuns,
  fuelReading,
  fuelReadings,
  policyTapeSeed,
  seededPattern,
  stageLabel,
  tapeHole,
  warpCountFor,
} from './factoryHelpers.ts';
import type { MillsStatus, PipelineRun } from '../stores/mills.svelte.ts';

function run(id: string, state: string, extra: Partial<PipelineRun> = {}): PipelineRun {
  return { ID: id, BacklogID: `bk-${id}`, Template: 't', State: state, Attempts: 1, ...extra };
}

describe('diffTerminalRuns', () => {
  it('emits bolt for done and spark for escalated, oldest first', () => {
    // History is newest-first; events must come out chronological.
    const history = [run('c', 'escalated'), run('b', 'done'), run('a', 'merged')];
    const { events, seen } = diffTerminalRuns(new Set(), history);
    expect(events.map((e) => e.runID)).toEqual(['a', 'b', 'c']);
    expect(events.map((e) => e.kind)).toEqual(['bolt', 'bolt', 'spark']);
    expect(seen.size).toBe(3);
  });

  it('skips already-seen runs and does not mutate the input set', () => {
    const prev = new Set(['a']);
    const { events, seen } = diffTerminalRuns(prev, [run('b', 'done'), run('a', 'done')]);
    expect(events.map((e) => e.runID)).toEqual(['b']);
    expect(prev.has('b')).toBe(false);
    expect(seen.has('b')).toBe(true);
  });

  it('marks paused runs seen without weaving a row', () => {
    const { events, seen } = diffTerminalRuns(new Set(), [run('p', 'paused')]);
    expect(events).toEqual([]);
    expect(seen.has('p')).toBe(true);
  });
});

describe('diffStagePicks', () => {
  it('emits a pick on first sighting and on stage advance, none when unchanged', () => {
    const r1 = run('r1', 'running', { CurrentStage: 'implement' });
    const first = diffStagePicks(new Map(), [r1]);
    expect(first.picks).toEqual([{ runID: 'r1', backlogID: 'bk-r1', stage: 'implement' }]);

    const unchanged = diffStagePicks(first.stages, [r1]);
    expect(unchanged.picks).toEqual([]);

    const advanced = diffStagePicks(unchanged.stages, [run('r1', 'running', { CurrentStage: 'tests' })]);
    expect(advanced.picks).toEqual([{ runID: 'r1', backlogID: 'bk-r1', stage: 'tests' }]);
  });

  it('drops vanished runs from the observation map', () => {
    const seenBoth = diffStagePicks(new Map(), [
      run('a', 'running', { CurrentStage: 'implement' }),
      run('b', 'running', { CurrentStage: 'tests' }),
    ]);
    const onlyB = diffStagePicks(seenBoth.stages, [run('b', 'running', { CurrentStage: 'tests' })]);
    expect(onlyB.picks).toEqual([]);
    expect(onlyB.stages.has('a')).toBe(false);
    // If 'a' reappears later it earns a fresh pick.
    const back = diffStagePicks(onlyB.stages, [run('a', 'running', { CurrentStage: 'merge' })]);
    expect(back.picks.map((p) => p.runID)).toEqual(['a']);
  });
});

describe('seededPattern', () => {
  it('is deterministic per seed and differs across seeds', () => {
    const a1 = seededPattern('PIPE-x-123', 32);
    const a2 = seededPattern('PIPE-x-123', 32);
    const b = seededPattern('PIPE-y-456', 32);
    expect(a1).toEqual(a2);
    expect(a1).toHaveLength(32);
    expect(a1).not.toEqual(b);
  });

  it('re-derives cleanly at a different warp width', () => {
    expect(seededPattern('PIPE-x-123', 48)).toHaveLength(48);
    // Same prefix behavior isn't required — only determinism at each width.
    expect(seededPattern('PIPE-x-123', 48)).toEqual(seededPattern('PIPE-x-123', 48));
  });

  it('weaves runs of threads, not pure noise', () => {
    const cells = seededPattern('run-length-check', 64);
    let flips = 0;
    for (let i = 1; i < cells.length; i++) if (cells[i] !== cells[i - 1]) flips++;
    // Run-length encoding means far fewer flips than a coin toss (~32).
    expect(flips).toBeLessThan(32);
    expect(flips).toBeGreaterThan(4);
  });
});

describe('policy tape', () => {
  it('seed is stable for a policy and changes on version bump or kill-switch flip', () => {
    const v3 = policyTapeSeed({ version: 3, enabled: true });
    expect(policyTapeSeed({ version: 3, enabled: true })).toBe(v3);
    expect(policyTapeSeed({ version: 4, enabled: true })).not.toBe(v3);
    expect(policyTapeSeed({ version: 3, enabled: false })).not.toBe(v3);
    expect(policyTapeSeed(null)).toBe(0);
  });

  it('holes are deterministic and the pattern shifts with the policy seed', () => {
    const s1 = policyTapeSeed({ version: 1, enabled: true });
    const s2 = policyTapeSeed({ version: 2, enabled: true });
    const grid = (seed: number) =>
      Array.from({ length: 12 }, (_, r) => Array.from({ length: 4 }, (_, c) => tapeHole(seed, r, c)));
    expect(grid(s1)).toEqual(grid(s1));
    expect(grid(s1)).not.toEqual(grid(s2));
  });
});

describe('stageLabel', () => {
  it('maps known stages to loom vocabulary and falls back readably', () => {
    expect(stageLabel('implement')).toBe('laying weft');
    expect(stageLabel('ci_watch')).toBe('under the inspection lamp');
    expect(stageLabel('some_new_stage')).toBe('some new stage');
    expect(stageLabel(undefined)).toBe('in the shed');
  });
});

describe('warpCountFor', () => {
  it('returns the floor when the beam is empty', () => {
    expect(warpCountFor(0, 100)).toBe(24);
  });
  it('scales with backlog and saturates at the ceiling', () => {
    expect(warpCountFor(5, 100)).toBe(34);
    expect(warpCountFor(500, 100)).toBe(72);
  });
  it('never exceeds what the viewport fits', () => {
    expect(warpCountFor(500, 40)).toBe(40);
  });
});

describe('fuelReading', () => {
  it('renders an em dash when the operator omitted the tier — never a guessed level', () => {
    expect(fuelReading(undefined)).toEqual({ frac: null, label: '—', tone: 'cy' });
    expect(fuelReading(null)).toEqual({ frac: null, label: '—', tone: 'cy' });
    expect(fuelReading({ cap_usd: 75 })).toEqual({ frac: null, label: '—', tone: 'cy' });
  });

  it('an uncapped tier shows spend but no level', () => {
    expect(fuelReading({ spent_usd: 12.5, cap_usd: 0 })).toEqual({
      frac: null,
      label: '$12.50 · no cap',
      tone: 'cy',
    });
  });

  it('maps remaining fraction to tones across the thresholds', () => {
    const at = (spent: number) => fuelReading({ spent_usd: spent, cap_usd: 100 });
    expect(at(10).frac).toBeCloseTo(0.9);
    expect(at(10).tone).toBe('ok');
    expect(at(60).frac).toBeCloseTo(0.4);
    expect(at(60).tone).toBe('wr');
    expect(at(90).frac).toBeCloseTo(0.1);
    expect(at(90).tone).toBe('er');
  });

  it('clamps overspend to an empty tank and formats big caps without cents', () => {
    const r = fuelReading({ spent_usd: 120, cap_usd: 100 });
    expect(r.frac).toBe(0);
    expect(r.tone).toBe('er');
    expect(r.label).toBe('$120 / $100');
  });
});


describe('fuelReadings', () => {
  it('splits full pipeline and council status payloads into independent tanks', () => {
    const status: MillsStatus = { budget: {
      pipeline: { spent_usd: 2.43, cap_usd: 75, runs: 1, runs_cap: 0,
        total_spent_usd: 70.92, subscription_spent_usd: 68.48, subscription_cap_usd: 300 },
      council: { spent_usd: 6, cap_usd: 10, runs: 1, runs_cap: 0,
        total_spent_usd: 96, subscription_spent_usd: 90, subscription_cap_usd: 100 },
    } };
    const [api, sub] = fuelReadings(status.budget?.pipeline);
    expect(api).toMatchObject({ kind: 'api', label: '$2.43 / $75.00', tone: 'ok', unbounded: false });
    expect(api.frac).toBeCloseTo(1 - 2.43 / 75);
    expect(sub).toMatchObject({ kind: 'sub', label: '$68.48 / $300', tone: 'ok', unbounded: false });
    expect(sub.frac).toBeCloseTo(1 - 68.48 / 300);
    expect(api.title).toBe('API metered spend: $2.43 / $75.00; total spend: $70.92 (rolling 24h)');
    expect(sub.title).toBe('Subscription list-price equivalent: $68.48 / $300.00; total spend: $70.92 (rolling 24h)');
    const council = fuelReadings(status.budget?.council);
    expect(council.map(t => t.tone)).toEqual(['wr', 'er']);
    expect(council[0].frac).toBeCloseTo(0.4);
    expect(council[1].frac).toBeCloseTo(0.1);
  });

  it('keeps one legacy tank and an em dash for absent metered data', () => {
    expect(fuelReadings({ spent_usd: 12.5, cap_usd: 75 })).toHaveLength(1);
    for (const usage of [undefined, null, {}, { cap_usd: 75 }]) {
      expect(fuelReadings(usage)).toEqual([
        expect.objectContaining({ kind: 'api', label: '—', frac: null, unbounded: false }),
      ]);
    }
  });

  it.each([0, undefined])('renders an unbounded sub tank for cap %s, including zero spend', cap => {
    for (const spent of [0, 12.5]) {
      const sub = fuelReadings({ subscription_spent_usd: spent, subscription_cap_usd: cap })[1];
      expect(sub).toMatchObject({ unbounded: true, frac: null, tone: 'cy', label: `$${spent.toFixed(2)} · no cap` });
      expect(sub.title).toContain(`$${spent.toFixed(2)} / no cap`);
    }
  });

  it('does not invent missing subscription spend or total', () => {
    for (const spent of [undefined, NaN, Infinity]) {
      const sub = fuelReadings({ subscription_spent_usd: spent, subscription_cap_usd: 0 })[1];
      expect(sub).toMatchObject({ label: '—', frac: null, unbounded: false, tone: 'cy' });
      expect(sub.title).toContain('—; total spend: —');
    }
    expect(fuelReadings({ total_spent_usd: 0 })[1].title).toContain('total spend: $0.00');
  });

  it('rounds compact labels but preserves cents in tooltips', () => {
    const sub = fuelReadings({ subscription_spent_usd: 123.456, subscription_cap_usd: 300.12, total_spent_usd: 125.886 })[1];
    expect(sub.label).toBe('$123 / $300');
    expect(sub.title).toContain('$123.46 / $300.12; total spend: $125.89');
  });

  it.each([[-10, 1, 'ok'], [0, 1, 'ok'], [60, 0.4, 'wr'], [120, 0, 'er']] as const)(
    'clamps and colors subscription spend %s', (spent, frac, tone) => {
      expect(fuelReadings({ subscription_spent_usd: spent, subscription_cap_usd: 100 })[1])
        .toMatchObject({ frac, tone, unbounded: false });
    },
  );

  it('subscription changes never affect metered readings used by Andon', () => {
    const usage = { spent_usd: 60, cap_usd: 75, subscription_spent_usd: 0, subscription_cap_usd: 300 };
    const metered = fuelReading(usage);
    for (const spend of [0, 150, 600]) {
      const changed = { ...usage, subscription_spent_usd: spend };
      expect(fuelReading(changed)).toEqual(metered);
      expect(fuelReadings(changed)[0]).toMatchObject(metered);
    }
  });
});
