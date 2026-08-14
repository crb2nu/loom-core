// Pure helpers for the Factory panel's pattern shelf — the Pattern Loom
// catalog as a shelf of labeled card-chains (Jacquard card chains were
// interchangeable programs, and works of art in their own right).
// Attribution is derived, never guessed: a stamped plan's id embeds its
// pattern slug (svc_pattern_stamp.go mints `plan-stamp-<slug>-<primary>`),
// backlog rows carry PlanID, runs carry BacklogID. Rune-free so vitest
// covers the chain without a Svelte runtime.

import type { BacklogItem, PipelineRun } from '../stores/mills.svelte.ts';
import type { PatternInfo } from '../stores/patterns.svelte.ts';

const STAMP_PREFIX = 'plan-stamp-';

/**
 * Which catalog pattern stamped this plan, by longest-slug prefix match
 * (`go-rest` must not swallow `go-rest-service`). Returns null for
 * non-stamp plans and for slugs missing from the catalog — an unknown
 * book attributes to nothing rather than the wrong shelf spot.
 */
export function stampedPatternSlug(
  planID: string | undefined | null,
  slugs: readonly string[],
): string | null {
  if (!planID || !planID.startsWith(STAMP_PREFIX)) return null;
  const rest = planID.slice(STAMP_PREFIX.length);
  let best: string | null = null;
  for (const slug of slugs) {
    if (!slug) continue;
    if (rest === slug || rest.startsWith(slug + '-')) {
      if (!best || slug.length > best.length) best = slug;
    }
  }
  return best;
}

/** One book on the shelf: a pattern plus what the floor wove from it. */
export interface PatternBook {
  slug: string;
  name: string;
  makes: string;
  /** Runs weaving from this book right now. */
  active: number;
  /** Recent terminal outcomes (from the history window). */
  merged: number;
  escalated: number;
}

/**
 * Build the shelf: one book per approved catalog pattern, with run
 * counts attributed via run → backlog → PlanID → slug. Books are
 * ordered working-first (active desc, then merged desc, then name) so
 * the live books sit at the front of the shelf.
 */
export function patternBooks(
  patterns: PatternInfo[],
  backlog: BacklogItem[],
  activeRuns: PipelineRun[],
  history: PipelineRun[],
): PatternBook[] {
  const approved = patterns.filter((p) => p.status === 'approved' && p.slug);
  if (approved.length === 0) return [];
  const slugs = approved.map((p) => p.slug);

  const planByBacklog = new Map<string, string>();
  for (const item of backlog) {
    if (item?.ID && item.PlanID) planByBacklog.set(item.ID, item.PlanID);
  }
  const slugForRun = (run: PipelineRun): string | null =>
    stampedPatternSlug(planByBacklog.get(run?.BacklogID ?? ''), slugs);

  const books = new Map<string, PatternBook>();
  for (const p of approved) {
    books.set(p.slug, { slug: p.slug, name: p.name, makes: p.makes, active: 0, merged: 0, escalated: 0 });
  }
  for (const run of activeRuns) {
    const slug = slugForRun(run);
    if (slug) books.get(slug)!.active++;
  }
  for (const run of history) {
    const slug = slugForRun(run);
    if (!slug) continue;
    const state = (run.State ?? '').toLowerCase();
    if (state === 'done' || state === 'merged') books.get(slug)!.merged++;
    else if (state === 'escalated') books.get(slug)!.escalated++;
  }
  return [...books.values()].sort(
    (a, b) => b.active - a.active || b.merged - a.merged || a.name.localeCompare(b.name),
  );
}

/**
 * Per-run book names for the shuttle's pick labels: runID → pattern
 * name, only for attributable runs.
 */
export function bookNamesByRun(
  patterns: PatternInfo[],
  backlog: BacklogItem[],
  activeRuns: PipelineRun[],
): Map<string, string> {
  const approved = patterns.filter((p) => p.status === 'approved' && p.slug);
  const slugs = approved.map((p) => p.slug);
  const nameBySlug = new Map(approved.map((p) => [p.slug, p.name]));
  const planByBacklog = new Map<string, string>();
  for (const item of backlog) {
    if (item?.ID && item.PlanID) planByBacklog.set(item.ID, item.PlanID);
  }
  const out = new Map<string, string>();
  for (const run of activeRuns) {
    if (!run?.ID) continue;
    const slug = stampedPatternSlug(planByBacklog.get(run.BacklogID ?? ''), slugs);
    if (slug) out.set(run.ID, nameBySlug.get(slug)!);
  }
  return out;
}
