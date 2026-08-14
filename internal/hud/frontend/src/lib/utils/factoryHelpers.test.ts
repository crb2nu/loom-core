import { describe, it, expect } from 'vitest';
import {
  diffStagePicks,
  diffTerminalRuns,
  fuelReading,
  policyTapeSeed,
  seededPattern,
  stageLabel,
  tapeHole,
  warpCountFor,
} from './factoryHelpers.ts';
import type { PipelineRun } from '../stores/mills.svelte.ts';

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
