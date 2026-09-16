import { describe, expect, it } from 'vitest';
import { readinessOf, describeCode, type MillsProjectEntry } from './millsProjects.svelte.ts';

function entry(over: Partial<MillsProjectEntry>): MillsProjectEntry {
  return {
    project: 'labs/x', sources: [], protected_paths: 'global', protected_path_count: 1,
    ready: true, blockers: [], pending: [], ...over,
  };
}

describe('readinessOf', () => {
  it('maps the operator verdict onto one chip per state', () => {
    expect(readinessOf(undefined).kind).toBe('unknown');
    expect(readinessOf(entry({ sources: ['home'] })).kind).toBe('home');
    expect(readinessOf(entry({ project: 'services/loom-core' }), 'services/loom-core').kind).toBe('home');
    expect(readinessOf(entry({ sources: ['demand_projects'] })).kind).toBe('weaving');
    const reg = readinessOf(entry({ sources: ['bootstrapped'], pending: ['git_policy_missing'] }));
    expect(reg.kind).toBe('registered');
    expect(reg.detail).toContain('runtime registry');
    const blocked = readinessOf(entry({ ready: false, blockers: ['protected_paths_unknown', 'not_in_demand'] }));
    expect(blocked.kind).toBe('blocked');
    expect(blocked.detail).toContain('every touched file');
    expect(blocked.detail).toContain('not in cross_repo.demand_projects');
  });
  it('says "unknown", not "not onboarded", when the registry never loaded', () => {
    const noRegistry = readinessOf(undefined, '', false);
    expect(noRegistry.kind).toBe('unknown');
    expect(noRegistry.label).toBe('unknown');
    expect(noRegistry.detail).toContain('registry is unavailable');
    expect(readinessOf(undefined, '', true).label).toBe('not onboarded');
  });
  it('never renders a raw code without copy', () => {
    expect(describeCode('some_new_code')).toBe('some new code');
  });
});
