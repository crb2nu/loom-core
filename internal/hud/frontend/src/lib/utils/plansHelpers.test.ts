import { describe, expect, it } from 'vitest';
import {
  buildMillsBacklogItem,
  normalizePlan,
  sliceDependsLabels,
  type Plan,
  type PlanSlice,
} from './plansHelpers.ts';

function plan(over: Partial<Plan> & { id: string; title: string }): Plan {
  return { phase: 'planned', ...over };
}

function slice(over: Partial<PlanSlice> & { id: string; name: string }): PlanSlice {
  return { phase: 'pending', ...over };
}

describe('buildMillsBacklogItem', () => {
  // Regression: the operator decodes POST /api/mills/backlog into
  // store.BacklogItem (no top-level json tags) with DisallowUnknownFields(), so
  // snake_case keys (plan_id/spec_doc/created_by) are rejected with 400
  // "unknown field". The body MUST use PascalCase top-level keys.
  it('emits PascalCase top-level keys the strict operator decoder accepts', () => {
    const item = buildMillsBacklogItem(
      plan({ id: 'plan-1', title: 'Model a process', slug: 'procmodel-plan', mirror_path: '.loom/x.md' }),
    );
    // Present (PascalCase, matching store.BacklogItem field names).
    expect(Object.keys(item)).toEqual(
      expect.arrayContaining(['ID', 'Title', 'PlanID', 'State', 'SpecDoc', 'Slices', 'CreatedBy']),
    );
    // Absent (snake_case would be an unknown field → 400).
    for (const bad of ['plan_id', 'spec_doc', 'created_by', 'id', 'title']) {
      expect(item).not.toHaveProperty(bad);
    }
    expect(item.ID).toBe('bl-procmodel-plan');
    expect(item.PlanID).toBe('plan-1');
    expect(item.SpecDoc).toBe('.loom/x.md');
    expect(item.CreatedBy).toBe('hud-user');
    expect(item.State).toBe('queued');
  });

  // Regression for the reported bug: a plan minted into a non-home repo
  // (bootstrapped services/procmodel) must route cross-repo via TargetProject,
  // not silently run against the operator's home repo.
  it('routes to the minted repo via TargetProject when the plan has a project', () => {
    const item = buildMillsBacklogItem(
      plan({ id: 'plan-pm', title: 'procmodel', project: 'services/procmodel' }),
    );
    expect(item.TargetProject).toBe('services/procmodel');
  });

  it('omits TargetProject for a home-repo (project-less) plan', () => {
    const item = buildMillsBacklogItem(plan({ id: 'plan-h', title: 'home' }));
    expect(item).not.toHaveProperty('TargetProject');
  });

  it('scopes a slice hand-off to just that slice with a deterministic id', () => {
    const item = buildMillsBacklogItem(
      plan({ id: 'plan-2', title: 'Big plan', slug: 'big', slices: [slice({ id: 's1', name: 'a' })] }),
      slice({ id: 's2', name: 'scaffold', files: ['cmd/procmodel/main.go'] }),
    );
    expect(item.ID).toBe('bl-big-s2');
    expect(item.Title).toBe('Big plan — scaffold');
    expect(item.Slices).toEqual([{ name: 'scaffold', files: ['cmd/procmodel/main.go'] }]);
  });

  it('maps a whole-plan hand-off across all plan slices with snake_case nested keys', () => {
    const item = buildMillsBacklogItem(
      plan({
        id: 'plan-3',
        title: 'Whole',
        slices: [slice({ id: 's1', name: 'one', files: ['a.go'] }), slice({ id: 's2', name: 'two' })],
      }),
    );
    // Nested slices keep their snake_case json tags (Slice struct is tagged).
    expect(item.Slices).toEqual([
      { name: 'one', files: ['a.go'] },
      { name: 'two', files: [] },
    ]);
  });
});

describe('normalizePlan', () => {
  it('coerces objective + slice tissue to null-safe shape (older plans → empty)', () => {
    const out = normalizePlan(plan({ id: 'p1', title: 'Old plan', slices: [slice({ id: 'p1#1', name: 'a' })] }))!;
    expect(out.objective).toBe(''); // undefined → '' so it never renders "undefined"
    expect(out.slices![0].depends_on).toEqual([]);
    expect(out.slices![0].interface_contracts).toBe('');
    expect(out.slices![0].acceptance_criteria).toBe('');
  });

  it('preserves populated fields and defaults a plan with no slices', () => {
    const out = normalizePlan(
      plan({ id: 'p2', title: 'New plan', objective: 'One coherent goal.' }),
    )!;
    expect(out.objective).toBe('One coherent goal.');
    expect(out.slices).toEqual([]);
  });

  it('returns null for a nullish input', () => {
    expect(normalizePlan(null)).toBeNull();
    expect(normalizePlan(undefined)).toBeNull();
  });
});

describe('sliceDependsLabels', () => {
  const slices: PlanSlice[] = [
    slice({ id: 'plan-x#1', name: 'schema', order: 1 }),
    slice({ id: 'plan-x#2', name: 'api', order: 2 }),
    slice({ id: 'plan-x#3', name: 'ui', order: 3 }),
  ];

  it('maps resolved slice_ids to #order labels', () => {
    expect(sliceDependsLabels(['plan-x#2', 'plan-x#1'], slices)).toEqual(['#2', '#1']);
  });

  it('returns [] for empty/undefined depends_on so nothing renders', () => {
    expect(sliceDependsLabels(undefined, slices)).toEqual([]);
    expect(sliceDependsLabels([], slices)).toEqual([]);
  });

  it('falls back to the #N suffix when the slice is not in the list', () => {
    expect(sliceDependsLabels(['plan-y#4'], slices)).toEqual(['#4']);
  });

  it('falls back to the raw id when it carries no #N suffix', () => {
    expect(sliceDependsLabels(['loose-ref'], slices)).toEqual(['loose-ref']);
  });
});
