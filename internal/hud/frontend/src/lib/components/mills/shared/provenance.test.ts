import { describe, it, expect } from 'vitest';
import {
  originOf,
  originFacet,
  originTone,
  repoLabel,
  repoTitle,
  repoFacet,
  isCrossRepo,
  whyLine,
  HOME_REPO,
} from './provenance.ts';

// Fixtures use CreatedBy spellings taken verbatim from the live backlog
// (GET /api/mills/backlog, 544 items, 2026-08-16) rather than idealized ones —
// the whole point of normalizeActor is surviving the fleet's 19 spellings.
const item = (createdBy?: string, labels?: string[]) => ({
  CreatedBy: createdBy,
  Labels: labels,
});

describe('originOf', () => {
  it('classifies the live CreatedBy spellings', () => {
    expect(originOf(item('mills:plan-slice-emitter')).kind).toBe('plan');
    expect(originOf(item('claude-code')).kind).toBe('agent');
    expect(originOf(item('claude-code:millsreconcilerslow-investigation')).kind).toBe('agent');
    expect(originOf(item('claude-code ralph-loop')).kind).toBe('agent');
    expect(originOf(item('codex mills review')).kind).toBe('agent');
    expect(originOf(item('council')).kind).toBe('council');
    expect(originOf(item('operator-directed')).kind).toBe('operator');
    expect(originOf(item('operator-seed:claude-code')).kind).toBe('operator');
    expect(originOf(item('hud-user')).kind).toBe('operator');
    expect(originOf(item('portfolio-sensor')).kind).toBe('sensor');
    expect(originOf(item('mills canary autopilot')).kind).toBe('canary');
    expect(originOf(item('loom mills pipelines canary')).kind).toBe('canary');
    expect(originOf(item('api')).kind).toBe('automation');
    expect(originOf(item('mrwatch_shepherd')).kind).toBe('automation');
    expect(originOf(item('mills:gitlab-importer')).kind).toBe('automation');
  });

  it('honours precedence when several rungs match', () => {
    // A canary is authored by automation; canary must win or synthetic
    // heartbeat traffic hides inside the automation bucket.
    expect(originOf(item('mills canary autopilot', ['mills-canary'])).kind).toBe('canary');
    // The live sensor item is labelled maintenance-loop AND portfolio-sensor.
    expect(
      originOf(item('portfolio-sensor', ['maintenance-loop', 'portfolio-sensor', 'roadmap'])).kind,
    ).toBe('sensor');
    // Human intent outranks the plan machinery that carried it out.
    expect(originOf(item('mills:plan-slice-emitter', ['operator-directed'])).kind).toBe('operator');
  });

  it('classifies from labels when CreatedBy is absent', () => {
    expect(originOf(item(undefined, ['mills-canary'])).kind).toBe('canary');
    expect(originOf(item(undefined, ['mills-from-plan-slice'])).kind).toBe('plan');
    expect(originOf(item('', ['maintenance-loop'])).kind).toBe('sensor');
  });

  it('falls back to unknown rather than guessing', () => {
    expect(originOf(item(undefined)).kind).toBe('unknown');
    expect(originOf(item('  ')).kind).toBe('unknown');
    expect(originOf(item('some-new-thing-2027')).kind).toBe('unknown');
    expect(originOf(null).kind).toBe('unknown');
    expect(originOf(undefined).kind).toBe('unknown');
  });

  it('carries the evidence for its verdict', () => {
    // A chip that asserts provenance must be able to say why.
    expect(originOf(item('operator-directed')).detail).toContain('created_by=operator-directed');
    expect(originOf(item('x', ['mills-canary'])).detail).toContain('label=mills-canary');
    expect(originOf(item(undefined)).detail).toContain('created_by unset');
  });

  it('is case- and separator-insensitive', () => {
    expect(originOf(item('MILLS:PLAN-SLICE-EMITTER')).kind).toBe('plan');
    expect(originOf(item('mills_plan_slice_emitter')).kind).toBe('plan');
    expect(originOf(item('Claude-Code')).kind).toBe('agent');
    expect(originOf(item('x', ['MILLS-CANARY'])).kind).toBe('canary');
  });
});

describe('originTone', () => {
  it('maps every kind to a shared badge variant', () => {
    const kinds = [
      'operator',
      'council',
      'sensor',
      'plan',
      'agent',
      'canary',
      'automation',
      'unknown',
    ] as const;
    const allowed = ['info', 'success', 'warning', 'error', 'accent', 'muted'];
    for (const k of kinds) expect(allowed).toContain(originTone(k));
  });
});

describe('repoLabel', () => {
  it('collapses the two encodings of the home repo', () => {
    // 460 live items carry an empty TargetProject and 63 carry the explicit
    // path. Both are loom-core; if they split, the facet counts lie.
    expect(repoLabel(undefined)).toBe(HOME_REPO);
    expect(repoLabel('')).toBe(HOME_REPO);
    expect(repoLabel('   ')).toBe(HOME_REPO);
    expect(repoLabel('services/loom-core')).toBe(HOME_REPO);
    expect(repoLabel('loom-core')).toBe(HOME_REPO);
  });

  it('shortens bucket-qualified paths', () => {
    expect(repoLabel('services/flexdeck')).toBe('flexdeck');
    expect(repoLabel('services/procmodel')).toBe('procmodel');
    expect(repoLabel('libs/fi-fhir')).toBe('fi-fhir');
    expect(repoLabel('services/flexdeck/')).toBe('flexdeck');
  });

  it('names the home repo in its tooltip instead of showing a blank', () => {
    expect(repoTitle('')).toContain('home repo');
    expect(repoTitle('services/flexdeck')).toBe('services/flexdeck');
  });
});

describe('isCrossRepo', () => {
  it('treats both home encodings as not cross-repo', () => {
    expect(isCrossRepo('')).toBe(false);
    expect(isCrossRepo('services/loom-core')).toBe(false);
    expect(isCrossRepo('services/flexdeck')).toBe(true);
    expect(isCrossRepo('libs/fi-fhir')).toBe(true);
  });
});

describe('whyLine', () => {
  it('prefers the title over the plan id', () => {
    // Inverts the old plan/book precedence, which suppressed the human-readable
    // reason on exactly the plan-linked items that dominate the floor.
    expect(whyLine({ Title: 'Harden failure classifier', PlanID: 'plan-123' })).toBe(
      'Harden failure classifier',
    );
    expect(whyLine({ Title: '', PlanID: 'plan-123' })).toBe('plan-123');
    expect(whyLine({ Title: undefined, PlanID: undefined })).toBe('');
    expect(whyLine(null)).toBe('');
  });
});

describe('repoFacet', () => {
  it('pins home first and orders the rest by count', () => {
    const facet = repoFacet([
      { TargetProject: 'services/flexdeck' },
      { TargetProject: 'services/procmodel' },
      { TargetProject: 'services/procmodel' },
      { TargetProject: '' },
      { TargetProject: 'services/loom-core' },
    ]);
    expect(facet.map((f) => f.value)).toEqual([HOME_REPO, 'procmodel', 'flexdeck']);
    // The two home encodings must have merged into one count of 2.
    expect(facet[0].count).toBe(2);
    expect(facet[1].count).toBe(2);
  });

  it('handles an empty list', () => {
    expect(repoFacet([])).toEqual([]);
  });
});

describe('originFacet', () => {
  it('returns canonical order and drops empty buckets', () => {
    const facet = originFacet([
      { CreatedBy: 'mills:plan-slice-emitter' },
      { CreatedBy: 'mills:plan-slice-emitter' },
      { CreatedBy: 'operator-directed' },
      { CreatedBy: 'claude-code' },
    ]);
    // Canonical order is operator → council → sensor → plan → agent → …
    expect(facet.map((f) => f.value)).toEqual(['operator', 'plan', 'agent']);
    expect(facet.find((f) => f.value === 'plan')?.count).toBe(2);
    expect(facet.some((f) => f.value === 'canary')).toBe(false);
  });

  it('handles an empty list', () => {
    expect(originFacet([])).toEqual([]);
  });
});
