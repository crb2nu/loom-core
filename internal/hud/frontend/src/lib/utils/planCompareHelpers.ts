// Pure, unit-tested logic for the Plans compare/merge editor
// (PlansComparePanel.svelte). Two (or more) competing DRAFT plans, spun from
// the same brief, are aligned slice-by-slice so the operator can see where the
// frames agreed (`shared` themes) vs where each frame went its own way
// (`unique`), then cherry-pick the best slice from each into one merged plan.
//
// Everything here is a pure function of the input plans so the Svelte component
// stays presentation-only — same split as plansHelpers.ts / spinRunsHelpers.ts.

import type { Plan, PlanSlice } from './plansHelpers.ts';
import type { CompetitiveGroup } from './spinRunsHelpers.ts';

/** How a slice compares against the OTHER plans in the comparison. */
export type SliceKind = 'shared' | 'unique';

/** A plan's slice annotated with its cross-plan classification. */
export interface AlignedSlice {
  planId: string;
  slice: PlanSlice;
  kind: SliceKind;
  /**
   * Stable id of the cross-plan theme this slice belongs to (shared slices
   * only). Slices in DIFFERENT plans that matched each other carry the SAME
   * key, so the UI can link counterparts (hover-highlight) and detect when
   * the operator cherry-picks the same theme from more than one frame.
   */
  themeKey?: string;
}

/** One competing plan's aligned slices. */
export interface AlignedPlan {
  planId: string;
  plan: Plan;
  slices: AlignedSlice[];
}

/** Rollup of the alignment for the diff-summary strip. */
export interface DiffSummary {
  /** Distinct themes present in 2+ plans. */
  shared: number;
  /** Per-plan count of slices unique to that plan. */
  uniquePerPlan: Record<string, number>;
}

export interface AlignmentResult {
  plans: AlignedPlan[];
  diffSummary: DiffSummary;
}

/**
 * Normalize a slice name for cross-plan matching: lowercase, collapse any run
 * of non-alphanumeric characters to a single space, trim. So "Cost & time-sink
 * analyzer" and "Cost / time-sink metrics" both reduce toward a comparable
 * token stream. Empty when the name is only punctuation.
 */
export function normalizeSliceName(name: string): string {
  return (name ?? '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, ' ')
    .trim();
}

/** Set of significant tokens (drops 1-char noise) from a normalized name. */
function nameTokens(name: string): Set<string> {
  return new Set(
    normalizeSliceName(name)
      .split(' ')
      .filter((t) => t.length > 1),
  );
}

// A "distinctive" token (≥5 chars) is domain-specific enough that a single
// shared one signals the same theme (e.g. "derivatives", "normalize"). Short
// tokens ("cost", "data", "graph") are too generic to match on alone.
const DISTINCTIVE_TOKEN_LEN = 5;

/**
 * Two slice names are "similar" when one normalized name contains the other, or
 * they share enough significant tokens: ≥1 when either name is a single token
 * or they share a distinctive (≥5-char) token, else ≥2. A pure
 * containment/overlap heuristic — good enough to catch "Cost & time-sink
 * analyzer" vs "Cost / time-sink metrics" and "Derivatives engine" vs
 * "Derivatives + rate-of-change targeting" without a fuzzy-match dependency,
 * while keeping unrelated slices ("Handoff graph" vs "Standardized data model")
 * apart.
 */
export function namesSimilar(a: string, b: string): boolean {
  const na = normalizeSliceName(a);
  const nb = normalizeSliceName(b);
  if (!na || !nb) return false;
  if (na === nb) return true;
  if (na.includes(nb) || nb.includes(na)) return true;
  const ta = nameTokens(a);
  const tb = nameTokens(b);
  if (ta.size === 0 || tb.size === 0) return false;
  let overlap = 0;
  let sharedDistinctive = false;
  for (const t of ta) {
    if (tb.has(t)) {
      overlap++;
      if (t.length >= DISTINCTIVE_TOKEN_LEN) sharedDistinctive = true;
    }
  }
  const singleToken = ta.size === 1 || tb.size === 1;
  const threshold = singleToken || sharedDistinctive ? 1 : 2;
  return overlap >= threshold;
}

/** Normalize + de-dupe a slice's file list for overlap checks. */
function fileSet(files?: string[]): Set<string> {
  const out = new Set<string>();
  for (const f of files ?? []) {
    const t = (f ?? '').trim().toLowerCase();
    if (t) out.add(t);
  }
  return out;
}

/** Any file in common between two slices. */
export function filesOverlap(a: PlanSlice, b: PlanSlice): boolean {
  const fa = fileSet(a.files);
  if (fa.size === 0) return false;
  for (const f of fileSet(b.files)) if (fa.has(f)) return true;
  return false;
}

/** Whether two slices should be treated as the same theme. */
function slicesMatch(a: PlanSlice, b: PlanSlice): boolean {
  return namesSimilar(a.name, b.name) || filesOverlap(a, b);
}

/**
 * Align the slices of the competing plans. A slice is `shared` when a matching
 * slice (similar name OR overlapping files) exists in at least one OTHER plan;
 * otherwise it's `unique` to its plan. `diffSummary.shared` counts DISTINCT
 * shared themes (each cross-plan match group counted once), not the number of
 * shared slices — so two plans each contributing one slice to a theme count as
 * one shared theme. With fewer than two plans every slice is unique (nothing to
 * compare against).
 */
export function alignSlices(plans: Plan[]): AlignmentResult {
  const alignedPlans: AlignedPlan[] = plans.map((p) => ({
    planId: p.id,
    plan: p,
    slices: (p.slices ?? []).map((slice) => ({
      planId: p.id,
      slice,
      kind: 'unique' as SliceKind,
    })),
  }));

  const uniquePerPlan: Record<string, number> = {};
  for (const p of plans) uniquePerPlan[p.id] = 0;

  if (plans.length < 2) {
    // Nothing to compare against — every slice is unique.
    for (const ap of alignedPlans) uniquePerPlan[ap.planId] = ap.slices.length;
    return { plans: alignedPlans, diffSummary: { shared: 0, uniquePerPlan } };
  }

  // Classify each slice against every other plan's slices, and assign a stable
  // theme id per shared cross-plan match so distinct themes can be counted.
  // themeKey: `${planIndex}:${sliceIndex}` of the FIRST plan that participates
  // in a given theme; a matching slice in a later plan adopts the same key.
  const themeOf = new Map<AlignedSlice, string>();

  for (let i = 0; i < alignedPlans.length; i++) {
    for (let si = 0; si < alignedPlans[i].slices.length; si++) {
      const a = alignedPlans[i].slices[si];
      for (let j = 0; j < alignedPlans.length; j++) {
        if (j === i) continue;
        const other = alignedPlans[j].slices.find((b) => slicesMatch(a.slice, b.slice));
        if (other) {
          a.kind = 'shared';
          // Adopt an existing theme key from either slice, else mint one keyed
          // to the lower plan index so both sides converge on the same id.
          const existing = themeOf.get(a) ?? themeOf.get(other);
          const key = existing ?? `${Math.min(i, j)}:${a.planId}:${si}`;
          themeOf.set(a, key);
          themeOf.set(other, key);
        }
      }
    }
  }

  // Reconcile theme keys transitively (a↔b, b↔c should collapse to one theme)
  // and expose each shared slice's theme id for cross-column linking.
  const distinctThemes = new Set<string>();
  for (const ap of alignedPlans) {
    for (const as of ap.slices) {
      if (as.kind === 'shared') {
        const key = themeOf.get(as) ?? `${as.planId}:${as.slice.id}`;
        as.themeKey = key;
        distinctThemes.add(key);
      } else {
        uniquePerPlan[ap.planId]++;
      }
    }
  }

  return {
    plans: alignedPlans,
    diffSummary: { shared: distinctThemes.size, uniquePerPlan },
  };
}

/**
 * Resolve the frame label for a plan from its competitive spin group. The
 * group's `frames[]` is parallel to `planIds[]` (frame i spun draft i), so the
 * frame is the label at the plan's index. Returns '' when the plan isn't in a
 * group or the arrays don't line up.
 */
export function frameForPlan(
  planId: string,
  spinGroups: Map<string, CompetitiveGroup>,
): string {
  const group = spinGroups.get(planId);
  if (!group) return '';
  const idx = group.planIds.indexOf(planId);
  if (idx < 0 || idx >= group.frames.length) return '';
  return group.frames[idx] ?? '';
}

/** Stable key for a picked slice: `${planId}::${sliceId}`. */
export function sliceKey(planId: string, sliceId: string): string {
  return `${planId}::${sliceId}`;
}
