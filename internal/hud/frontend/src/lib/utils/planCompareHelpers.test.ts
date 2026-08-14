import { describe, expect, it } from 'vitest';
import {
  alignSlices,
  frameForPlan,
  filesOverlap,
  namesSimilar,
  normalizeSliceName,
  sliceKey,
} from './planCompareHelpers.ts';
import type { Plan, PlanSlice } from './plansHelpers.ts';
import type { CompetitiveGroup } from './spinRunsHelpers.ts';

function slice(over: Partial<PlanSlice> & { id: string; name: string }): PlanSlice {
  return { phase: 'pending', ...over };
}

function plan(id: string, slices: PlanSlice[], over: Partial<Plan> = {}): Plan {
  return { id, title: id, phase: 'draft', slices, ...over };
}

describe('normalizeSliceName', () => {
  it('lowercases, strips punctuation, collapses whitespace', () => {
    expect(normalizeSliceName('Cost & time-sink analyzer')).toBe('cost time sink analyzer');
    expect(normalizeSliceName('  Derivatives / rate-of-change  ')).toBe('derivatives rate of change');
    expect(normalizeSliceName('!!!')).toBe('');
  });
});

describe('namesSimilar', () => {
  it('matches on containment and token overlap', () => {
    expect(namesSimilar('Cost & time-sink analyzer', 'Cost / time-sink metrics')).toBe(true);
    expect(namesSimilar('Derivatives engine', 'Derivatives + rate-of-change targeting')).toBe(true);
    expect(namesSimilar('Ingest events', 'Ingest & normalize process events')).toBe(true);
  });
  it('rejects unrelated names', () => {
    expect(namesSimilar('Handoff graph', 'Standardized data model')).toBe(false);
    expect(namesSimilar('', 'anything')).toBe(false);
  });
});

describe('filesOverlap', () => {
  it('true when any file is shared (case-insensitive), false otherwise', () => {
    expect(
      filesOverlap(slice({ id: 'a', name: 'a', files: ['calc/Derivatives.py'] }), slice({ id: 'b', name: 'b', files: ['calc/derivatives.py'] })),
    ).toBe(true);
    expect(
      filesOverlap(slice({ id: 'a', name: 'a', files: ['x.ts'] }), slice({ id: 'b', name: 'b', files: ['y.ts'] })),
    ).toBe(false);
    expect(filesOverlap(slice({ id: 'a', name: 'a' }), slice({ id: 'b', name: 'b', files: ['y.ts'] }))).toBe(false);
  });
});

describe('alignSlices — shared/unique classification', () => {
  const mule = plan('plan-mule', [
    slice({ id: 'm1', name: 'Ingest & normalize process events', files: ['ingest/events.py'] }),
    slice({ id: 'm2', name: 'Cost & time-sink analyzer', files: ['analysis/cost_time.py'] }),
    slice({ id: 'm3', name: 'Handoff & feedback-loop graph', files: ['graph/handoffs.py'] }),
    slice({ id: 'm4', name: 'Rate-of-change / derivatives engine', files: ['calc/derivatives.py'] }),
  ]);
  const flyer = plan('plan-flyer', [
    slice({ id: 'f1', name: 'Parse raw process notes → events', files: ['parse/notes.ts'] }),
    slice({ id: 'f2', name: 'Standardized data model + validation', files: ['model/standard.ts'] }),
    slice({ id: 'f3', name: 'Cost / time-sink metrics', files: ['metrics/costTime.ts'] }),
    slice({ id: 'f4', name: 'Derivatives + rate-of-change targeting', files: ['calc/derivatives.py'] }),
  ]);

  it('classifies matching themes shared and one-offs unique', () => {
    const { plans, diffSummary } = alignSlices([mule, flyer]);
    const muleKinds = Object.fromEntries(plans[0].slices.map((s) => [s.slice.id, s.kind]));
    const flyerKinds = Object.fromEntries(plans[1].slices.map((s) => [s.slice.id, s.kind]));

    // Cost/time-sink (name), and derivatives (file + name) are shared.
    expect(muleKinds.m2).toBe('shared');
    expect(muleKinds.m4).toBe('shared');
    expect(flyerKinds.f3).toBe('shared');
    expect(flyerKinds.f4).toBe('shared');

    // Handoff graph (mule) and standardized data model (flyer) are unique.
    expect(muleKinds.m3).toBe('unique');
    expect(flyerKinds.f2).toBe('unique');

    // Ingest (mule) vs Parse notes → events (flyer): both mention "events"
    // (single-token-ish overlap threshold) — treated shared.
    expect(muleKinds.m1).toBe('shared');
    expect(flyerKinds.f1).toBe('shared');

    // uniquePerPlan should count the true one-offs.
    expect(diffSummary.uniquePerPlan['plan-mule']).toBe(1);
    expect(diffSummary.uniquePerPlan['plan-flyer']).toBe(1);
    // shared themes are counted distinctly (ingest, cost/time, derivatives = 3).
    expect(diffSummary.shared).toBe(3);
  });

  it('no overlap → all unique, zero shared themes', () => {
    const a = plan('a', [slice({ id: 'a1', name: 'Alpha widget' })]);
    const b = plan('b', [slice({ id: 'b1', name: 'Bravo pipeline' })]);
    const { plans, diffSummary } = alignSlices([a, b]);
    expect(plans[0].slices[0].kind).toBe('unique');
    expect(plans[1].slices[0].kind).toBe('unique');
    expect(diffSummary.shared).toBe(0);
    expect(diffSummary.uniquePerPlan).toEqual({ a: 1, b: 1 });
  });

  it('single plan → everything unique (nothing to compare)', () => {
    const { plans, diffSummary } = alignSlices([mule]);
    expect(plans[0].slices.every((s) => s.kind === 'unique')).toBe(true);
    expect(diffSummary.shared).toBe(0);
    expect(diffSummary.uniquePerPlan['plan-mule']).toBe(4);
  });

  it('handles plans with no slices', () => {
    const { plans, diffSummary } = alignSlices([plan('empty', []), flyer]);
    expect(plans[0].slices).toHaveLength(0);
    expect(diffSummary.uniquePerPlan['empty']).toBe(0);
  });
});

describe('alignSlices — themeKey cross-plan linking', () => {
  const mule = plan('plan-mule', [
    slice({ id: 'm2', name: 'Cost & time-sink analyzer' }),
    slice({ id: 'm3', name: 'Handoff & feedback-loop graph' }),
    slice({ id: 'm4', name: 'Rate-of-change / derivatives engine', files: ['calc/derivatives.py'] }),
  ]);
  const flyer = plan('plan-flyer', [
    slice({ id: 'f3', name: 'Cost / time-sink metrics' }),
    slice({ id: 'f4', name: 'Derivatives + rate-of-change targeting', files: ['calc/derivatives.py'] }),
  ]);

  it('counterpart slices in different plans carry the SAME themeKey', () => {
    const { plans } = alignSlices([mule, flyer]);
    const bySlice = new Map(
      plans.flatMap((ap) => ap.slices.map((s) => [s.slice.id, s] as const)),
    );
    expect(bySlice.get('m2')!.themeKey).toBeDefined();
    expect(bySlice.get('m2')!.themeKey).toBe(bySlice.get('f3')!.themeKey);
    expect(bySlice.get('m4')!.themeKey).toBe(bySlice.get('f4')!.themeKey);
    // Distinct themes get distinct keys.
    expect(bySlice.get('m2')!.themeKey).not.toBe(bySlice.get('m4')!.themeKey);
  });

  it('unique slices carry no themeKey', () => {
    const { plans } = alignSlices([mule, flyer]);
    const m3 = plans[0].slices.find((s) => s.slice.id === 'm3')!;
    expect(m3.kind).toBe('unique');
    expect(m3.themeKey).toBeUndefined();
  });

  it('a theme spanning three plans converges on one key', () => {
    const a = plan('a', [slice({ id: 'a1', name: 'Derivatives engine' })]);
    const b = plan('b', [slice({ id: 'b1', name: 'Derivatives + rate-of-change targeting' })]);
    const c = plan('c', [slice({ id: 'c1', name: 'Derivatives metrics' })]);
    const { plans, diffSummary } = alignSlices([a, b, c]);
    const keys = plans.map((ap) => ap.slices[0].themeKey);
    expect(new Set(keys).size).toBe(1);
    expect(diffSummary.shared).toBe(1);
  });
});

describe('frameForPlan', () => {
  const groups = new Map<string, CompetitiveGroup>();
  const g: CompetitiveGroup = { spinId: 's1', frames: ['mule', 'flyer'], planIds: ['plan-mule', 'plan-flyer'] };
  groups.set('plan-mule', g);
  groups.set('plan-flyer', g);

  it('resolves the frame at the plan index', () => {
    expect(frameForPlan('plan-mule', groups)).toBe('mule');
    expect(frameForPlan('plan-flyer', groups)).toBe('flyer');
  });
  it('returns empty when the plan is not in a group', () => {
    expect(frameForPlan('plan-x', groups)).toBe('');
  });
  it('returns empty when frames and ids do not line up', () => {
    const short = new Map<string, CompetitiveGroup>();
    short.set('p', { spinId: 's', frames: [], planIds: ['p', 'q'] });
    expect(frameForPlan('p', short)).toBe('');
  });
});

describe('sliceKey', () => {
  it('joins plan and slice ids stably', () => {
    expect(sliceKey('plan-a', 's1')).toBe('plan-a::s1');
  });
});
